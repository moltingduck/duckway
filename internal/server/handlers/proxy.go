package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

type ProxyHandler struct {
	services    *queries.ServiceQueries
	apiKeys     *queries.APIKeyQueries
	resolver    *services.KeyResolver
	requestLog  *queries.RequestLogQueries
	approvals   *queries.ApprovalQueries
	settings    *queries.SettingsQueries
	convUsage   *queries.ConversationUsageQueries
	permissions *services.PermissionChecker
	notifier    *services.Notifier
	httpClient  *http.Client
}

func NewProxyHandler(svcQueries *queries.ServiceQueries, apiKeys *queries.APIKeyQueries, resolver *services.KeyResolver, requestLog *queries.RequestLogQueries, approvals *queries.ApprovalQueries, settings *queries.SettingsQueries, notifier *services.Notifier) *ProxyHandler {
	return &ProxyHandler{
		services:    svcQueries,
		apiKeys:     apiKeys,
		resolver:    resolver,
		requestLog:  requestLog,
		approvals:   approvals,
		settings:    settings,
		permissions: services.NewPermissionChecker(),
		notifier:    notifier,
		// No full Timeout: LLM streaming responses can run for many minutes.
		// ResponseHeaderTimeout caps the time to first byte from upstream;
		// once headers arrive the body streams until the client disconnects.
		httpClient: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 2 * time.Minute,
			},
		},
	}
}

// WithConversationUsage wires the per-request token-usage recorder.
// Optional: when nil, the proxy skips token capture entirely.
func (h *ProxyHandler) WithConversationUsage(q *queries.ConversationUsageQueries) *ProxyHandler {
	h.convUsage = q
	return h
}

const maxCapturedBytes = 64 * 1024 // 64 KB cap per body

// shouldCaptureContentType returns true for text-like content types where
// capturing the body in the request log is useful (and safe to store as a
// string). Binary blobs (images, packfiles, gzip) are skipped.
func shouldCaptureContentType(ct string) bool {
	ct = strings.ToLower(ct)
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	if ct == "" {
		// Many JSON APIs omit Content-Type on small responses; assume capturable.
		return true
	}
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/json", "application/xml", "application/x-www-form-urlencoded",
		"application/javascript", "application/ld+json", "application/problem+json":
		return true
	}
	return false
}

// captureDetailEnabledFor checks the toggle + per-client filter settings.
// Filter empty == capture for everyone.
func (h *ProxyHandler) captureDetailEnabledFor(clientID string) bool {
	if h.settings == nil {
		return false
	}
	if h.settings.Get(queries.SettingRequestLogCaptureOn) != "1" {
		return false
	}
	raw := h.settings.Get(queries.SettingRequestLogCaptureClients)
	if raw == "" || raw == "[]" {
		return true
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return true // fail-open if filter is malformed — toggle still gates
	}
	if len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if id == clientID {
			return true
		}
	}
	return false
}

// capLimitedWriter is like an io.MultiWriter target that records only the
// first `limit` bytes and silently discards the rest, but reports success on
// Write so io.Copy keeps streaming. Used to capture a prefix of a streaming
// response (e.g. SSE from /v1/messages) without buffering the whole thing.
type capLimitedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (lw *capLimitedWriter) Write(p []byte) (int, error) {
	remaining := lw.limit - lw.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) <= remaining {
		lw.buf.Write(p)
		return len(p), nil
	}
	lw.buf.Write(p[:remaining])
	return len(p), nil
}

// flushingWriter wraps an http.ResponseWriter so every Write is followed by
// an explicit Flush. Without this, the http server's bufio buffer holds up
// to ~4KB of body before sending. For streaming responses (SSE on
// /v1/messages, OpenAI streaming chat, etc.) this delays small events
// (keepalive pings, partial tokens) past the agent's idle-timeout window.
//
// The non-capture path uses Go's optimised ReadFrom which has internal
// flushing; the capture path goes through MultiWriter which doesn't, so we
// need to flush explicitly here to keep SSE working.
type flushingWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushingWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil && n > 0 {
		fw.f.Flush()
	}
	return n, err
}

// formatHeaders renders an http.Header as a JSON object string for storage.
func formatHeaders(h http.Header) string {
	if len(h) == 0 {
		return "{}"
	}
	out, _ := json.Marshal(h)
	return string(out)
}

func (h *ProxyHandler) Handle(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	path := r.URL.Path
	if !strings.HasPrefix(path, "/proxy/") {
		jsonError(w, "invalid proxy path", http.StatusBadRequest)
		return
	}

	remainder := strings.TrimPrefix(path, "/proxy/")
	parts := strings.SplitN(remainder, "/", 2)
	serviceName := parts[0]
	upstreamPath := "/"
	if len(parts) > 1 {
		upstreamPath = "/" + parts[1]
	}
	if serviceName == "openai-auth" {
		h.handleOpenAIAuthProxy(w, r, upstreamPath, startTime)
		return
	}

	svc, err := h.services.GetByName(serviceName)
	if err != nil {
		jsonError(w, "unknown service: "+serviceName, http.StatusNotFound)
		return
	}

	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client authentication required", http.StatusUnauthorized)
		return
	}

	result, err := h.resolver.ResolveForService(client.ID, svc.ID)
	if err != nil {
		log.Printf("resolve error for %s/%s: %v", serviceName, client.Name, err)
		jsonError(w, "key resolution failed", http.StatusInternalServerError)
		return
	}

	if result.NeedApproval {
		approvalID, err := CreatePendingApproval(h.approvals, result.PlaceholderID, r.Method, upstreamPath)
		if err != nil {
			log.Printf("failed to create approval: %v", err)
			jsonError(w, "failed to create approval request", http.StatusInternalServerError)
			return
		}

		// Send notification
		if h.notifier != nil {
			h.notifier.NotifyApprovalNeeded(services.ApprovalNotification{
				ApprovalID:    approvalID,
				PlaceholderID: result.PlaceholderID,
				ClientName:    client.Name,
				ServiceName:   serviceName,
				Method:        r.Method,
				Path:          upstreamPath,
				AdminURL:      "/admin/approvals",
			})
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error":       "duckway_approval_pending",
			"message":     "This API key usage requires admin approval. Retry after approval.",
			"approval_id": approvalID,
		})
		return
	}

	if result.Error != "" {
		jsonError(w, result.Error, http.StatusForbidden)
		return
	}

	// Heartbeat service: respond directly, no upstream
	if strings.HasPrefix(svc.UpstreamURL, "internal://") {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"service": "duckway-heartbeat",
			"client":  client.Name,
			"proxy":   true,
			"path":    upstreamPath,
		})
		if h.requestLog != nil {
			h.requestLog.Log(client.ID, result.PlaceholderID, serviceName, r.Method, upstreamPath, 200)
		}
		return
	}

	// Buffer body for permission checking
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}

	// Three-level ACL: request must pass ALL non-empty layers.
	// Each layer can only shrink (restrict further), never widen.
	//   1. Service default_acl (widest)
	//   2. API Key acl
	//   3. Placeholder permission_config (narrowest)
	aclLayers := []struct {
		name   string
		config string
	}{
		{"service", svc.DefaultACL},
		{"api_key", result.APIKeyACL},
		{"placeholder", result.PermissionConfig},
	}
	for _, layer := range aclLayers {
		if layer.config == "" {
			continue
		}
		permResult := h.permissions.Check(layer.config, result.PlaceholderID, r.Method, upstreamPath, bodyBytes)
		if !permResult.Allowed {
			jsonError(w, "permission denied ("+layer.name+"): "+permResult.Reason, http.StatusForbidden)
			return
		}
	}

	// Build upstream URL
	upstreamURL := strings.TrimRight(svc.UpstreamURL, "/") + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	var bodyReader io.Reader
	if len(bodyBytes) > 0 {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bodyReader)
	if err != nil {
		jsonError(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	// Copy headers, stripping anything that should not leak to upstream:
	//   - All X-Duckway-* headers (internal auth + bookkeeping)
	//   - Authorization / X-Api-Key (client sent the phantom — we inject the real one below)
	//   - Hop-by-hop headers per RFC 7230 §6.1
	//   - Host (Go sets this from the URL)
	for key, values := range r.Header {
		if shouldStripHeader(key) {
			continue
		}
		for _, v := range values {
			upstreamReq.Header.Add(key, v)
		}
	}

	// For LLM services we parse token usage out of the response body, so
	// force an uncompressed response (the agent set Accept-Encoding: gzip,
	// which would otherwise make Go's transport hand us compressed bytes
	// the scanner can't read). Slightly more bytes on the gateway↔upstream
	// hop; correctness for the usage panel.
	scanUsage := h.convUsage != nil && isLLMService(serviceName)
	if scanUsage {
		upstreamReq.Header.Set("Accept-Encoding", "identity")
	}

	// Inject real API key. Refreshable (OAuth) keys always go in
	// Authorization: Bearer regardless of the service's default auth_type,
	// since Anthropic and similar APIs require Bearer for OAuth access tokens.
	authType := svc.AuthType
	authHeader := svc.AuthHeader
	authPrefix := svc.AuthPrefix
	if result.IsRefreshable {
		authType = "bearer"
		authHeader = "Authorization"
		authPrefix = "Bearer "
	}
	switch authType {
	case "bearer":
		upstreamReq.Header.Set(authHeader, authPrefix+result.RealKey)
	case "header":
		upstreamReq.Header.Set(authHeader, result.RealKey)
	case "query":
		q := upstreamReq.URL.Query()
		q.Set(authHeader, result.RealKey)
		upstreamReq.URL.RawQuery = q.Encode()
	}

	resp, err := h.httpClient.Do(upstreamReq)
	if err != nil {
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			// Client disconnected before upstream responded — not a gateway fault.
			// Still log the row so the audit panel shows the attempt.
			if h.requestLog != nil {
				h.requestLog.Log(client.ID, result.PlaceholderID, serviceName, r.Method, upstreamPath, 0)
			}
			return
		}
		log.Printf("upstream error for %s: %v", serviceName, err)
		jsonError(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	captureDetail := h.captureDetailEnabledFor(client.ID)

	// Always log the metadata row. Use LogWithReturn so we have an id to
	// attach detail to if capture is on.
	var logID int64
	if h.requestLog != nil {
		id, lerr := h.requestLog.LogWithReturn(client.ID, result.PlaceholderID, serviceName, r.Method, upstreamPath, resp.StatusCode)
		if lerr == nil {
			logID = id
		}
	}

	// Persist rate-limit snapshot for LLM providers (Anthropic, OpenAI). The
	// upstream response carries headers like `anthropic-ratelimit-tokens-remaining`
	// — we record them on the api_key so the OAuth detail and key-group views
	// can show "X tokens used / Y remaining".
	if h.apiKeys != nil && result.APIKeyID != "" && resp.StatusCode < 500 {
		if snap := services.ParseRateLimits(resp.Header); snap != nil {
			_ = h.apiKeys.UpdateUsageSnapshot(result.APIKeyID, snap.String())
		}
	}

	for key, values := range resp.Header {
		if shouldStripResponseHeader(key) {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Token-usage scanner: tee LLM responses through it (always-on for
	// LLM services). Only count non-5xx responses — a server error has
	// no meaningful usage.
	var usageScanner *services.UsageScanner
	if scanUsage && resp.StatusCode < 500 {
		usageScanner = services.NewUsageScanner(resp.Header.Get("Content-Type"))
	}

	// agent-facing writer with per-chunk flush so SSE streams promptly.
	var agent io.Writer = w
	if fl, ok := w.(http.Flusher); ok {
		agent = &flushingWriter{w: w, f: fl}
	}

	if captureDetail && logID > 0 {
		// Tee the response body: stream it to the agent unchanged AND save
		// up to maxCapturedBytes on the side. This is critical for streaming
		// responses (SSE from /v1/messages, server-sent events in general) —
		// buffering the full body would block the agent indefinitely.
		ct := resp.Header.Get("Content-Type")
		var respBodyStored []byte
		var respTrunc bool
		var respSize int64

		if shouldCaptureContentType(ct) {
			cap := &capLimitedWriter{buf: &bytes.Buffer{}, limit: maxCapturedBytes}
			writers := []io.Writer{agent, cap}
			if usageScanner != nil {
				writers = append(writers, usageScanner)
			}
			respSize, _ = io.Copy(io.MultiWriter(writers...), resp.Body)
			respBodyStored = cap.buf.Bytes()
			respTrunc = respSize > int64(maxCapturedBytes)
			// If the upstream sent a compressed body, the agent decompresses
			// on its end (it set Accept-Encoding) — but our captured copy is
			// still the compressed bytes, which look like garbage in the UI.
			// Decompress just the captured copy. Headers are kept as-is so
			// the original Content-Encoding stays visible in the modal.
			if decoded, ok := decodeBody(respBodyStored, resp.Header.Get("Content-Encoding")); ok {
				respBodyStored = decoded
			}
		} else if usageScanner != nil {
			respSize, _ = io.Copy(io.MultiWriter(agent, usageScanner), resp.Body)
		} else {
			// Binary — skip body capture but still record metadata.
			respSize, _ = io.Copy(w, resp.Body)
		}

		// Request body: bodyBytes is what we read for ACL above. Cap it.
		var reqBodyStored string
		reqTrunc := false
		reqSize := int64(len(bodyBytes))
		reqCT := r.Header.Get("Content-Type")
		if shouldCaptureContentType(reqCT) {
			toStore := bodyBytes
			if decoded, ok := decodeBody(bodyBytes, r.Header.Get("Content-Encoding")); ok {
				toStore = decoded
			}
			if int64(len(toStore)) > maxCapturedBytes {
				reqBodyStored = string(toStore[:maxCapturedBytes])
				reqTrunc = true
			} else {
				reqBodyStored = string(toStore)
			}
		}

		if captureDetail {
			_ = h.requestLog.StoreDetail(&queries.RequestLogDetail{
				LogID:           logID,
				RequestHeaders:  formatHeaders(r.Header),
				RequestBody:     reqBodyStored,
				RequestSize:     reqSize,
				ResponseHeaders: formatHeaders(resp.Header),
				ResponseBody:    string(respBodyStored),
				ResponseSize:    respSize,
				DurationMs:      time.Since(startTime).Milliseconds(),
				Truncated:       reqTrunc || respTrunc,
			})
		}
	} else if usageScanner != nil {
		// No detail capture, but we still want token usage: tee through
		// the scanner with a flushing agent writer (LLM responses are
		// usually SSE and must not stall in Go's bufio buffer).
		io.Copy(io.MultiWriter(agent, usageScanner), resp.Body)
	} else {
		io.Copy(w, resp.Body)
	}

	// Record per-conversation token usage once the body has fully
	// streamed. conversation_id is claude's session header (empty for
	// non-claude clients, which bucket together). Best-effort.
	if usageScanner != nil && h.convUsage != nil {
		if u := usageScanner.Result(); u != nil {
			_ = h.convUsage.Insert(&queries.ConversationUsageRecord{
				ClientID:            client.ID,
				APIKeyID:            result.APIKeyID,
				ServiceName:         serviceName,
				ConversationID:      r.Header.Get("X-Claude-Code-Session-Id"),
				Model:               u.Model,
				InputTokens:         u.InputTokens,
				OutputTokens:        u.OutputTokens,
				CacheReadTokens:     u.CacheReadTokens,
				CacheCreationTokens: u.CacheCreationTokens,
			})
		}
	}
}

func (h *ProxyHandler) handleOpenAIAuthProxy(w http.ResponseWriter, r *http.Request, upstreamPath string, startTime time.Time) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client authentication required", http.StatusUnauthorized)
		return
	}
	if upstreamPath != "/oauth/token" {
		jsonError(w, "unsupported openai auth path", http.StatusForbidden)
		return
	}
	openAISvc, err := h.services.GetByName("openai")
	if err != nil {
		jsonError(w, "openai service unavailable", http.StatusNotFound)
		return
	}
	result, err := h.resolver.ResolveForService(client.ID, openAISvc.ID)
	if err != nil {
		log.Printf("resolve error for openai-auth/%s: %v", client.Name, err)
		jsonError(w, "key resolution failed", http.StatusInternalServerError)
		return
	}
	if result.NeedApproval {
		jsonError(w, "approval required", http.StatusForbidden)
		return
	}
	if result.Error != "" {
		jsonError(w, result.Error, http.StatusForbidden)
		return
	}
	if result.RealRefreshToken == "" {
		jsonError(w, "openai key is not a refreshable Codex OAuth token", http.StatusForbidden)
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	rewrittenBody, contentType := rewriteCodexRefreshRequest(bodyBytes, r.Header.Get("Content-Type"), result.RealRefreshToken)
	upstreamURL := "https://auth.openai.com" + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(rewrittenBody))
	if err != nil {
		jsonError(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}
	for key, values := range r.Header {
		if shouldStripOpenAIAuthHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, v := range values {
			upstreamReq.Header.Add(key, v)
		}
	}
	if contentType != "" {
		upstreamReq.Header.Set("Content-Type", contentType)
	}

	resp, err := h.httpClient.Do(upstreamReq)
	if err != nil {
		log.Printf("openai auth upstream error: %v", err)
		jsonError(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respBody = rewriteCodexRefreshResponse(respBody, result.PlaceholderID, result.Placeholder)
	for key, values := range resp.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	if len(respBody) > 0 {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	if h.requestLog != nil {
		h.requestLog.Log(client.ID, result.PlaceholderID, "openai-auth", r.Method, upstreamPath, resp.StatusCode)
	}
	_ = startTime
}

func rewriteCodexRefreshRequest(body []byte, contentType, realRefresh string) ([]byte, string) {
	lowerCT := strings.ToLower(contentType)
	if strings.Contains(lowerCT, "application/x-www-form-urlencoded") ||
		strings.Contains(string(body), "refresh_token=") ||
		strings.Contains(string(body), "grant_type=") {
		vals, err := url.ParseQuery(string(body))
		if err == nil {
			vals.Set("refresh_token", realRefresh)
			return []byte(vals.Encode()), "application/x-www-form-urlencoded"
		}
	}
	var obj map[string]interface{}
	if json.Unmarshal(body, &obj) == nil {
		obj["refresh_token"] = realRefresh
		out, _ := json.Marshal(obj)
		return out, "application/json"
	}
	return body, contentType
}

func rewriteCodexRefreshResponse(body []byte, placeholderID, placeholder string) []byte {
	var obj map[string]interface{}
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	subInfo := map[string]interface{}{"account_id": placeholderID}
	realAccess, _ := obj["access_token"].(string)
	realID, _ := obj["id_token"].(string)
	tokens := codexPhantomTokensFromSource(placeholder, subInfo, realAccess, realID)
	if _, ok := obj["access_token"]; ok {
		obj["access_token"] = tokens["access_token"]
	}
	if _, ok := obj["refresh_token"]; ok {
		obj["refresh_token"] = tokens["refresh_token"]
	}
	if _, ok := obj["id_token"]; ok {
		obj["id_token"] = tokens["id_token"]
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func shouldStripOpenAIAuthHeader(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "x-duckway-") {
		return true
	}
	switch lower {
	case "host":
		return true
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// shouldStripResponseHeader returns true if a response header from the upstream
// must not be forwarded to the client (hop-by-hop, RFC 7230 §6.1).
func shouldStripResponseHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// shouldStripHeader returns true if a request header from the client must NOT
// be forwarded to the upstream. This covers Duckway-internal auth headers,
// the client's own auth (which carries the phantom token — we inject the real
// one separately), and hop-by-hop headers per RFC 7230 §6.1.
func shouldStripHeader(name string) bool {
	lower := strings.ToLower(name)
	// Any X-Duckway-* header — internal-only, must never leak upstream.
	if strings.HasPrefix(lower, "x-duckway-") {
		return true
	}
	switch lower {
	case "authorization", "x-api-key":
		return true
	case "host":
		return true
	// Hop-by-hop headers — RFC 7230 §6.1
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}
