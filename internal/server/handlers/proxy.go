package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

type ProxyHandler struct {
	services     *queries.ServiceQueries
	apiKeys      *queries.APIKeyQueries
	resolver     *services.KeyResolver
	requestLog   *queries.RequestLogQueries
	approvals    *queries.ApprovalQueries
	settings     *queries.SettingsQueries
	convUsage    *queries.ConversationUsageQueries
	permissions  *services.PermissionChecker
	notifier     *services.Notifier
	crypto       *services.Crypto
	httpClient   *http.Client
	proxyClients *services.UpstreamProxyClientCache

	githubAppMu     sync.Mutex
	githubAppTokens map[string]githubAppTokenCache
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
		proxyClients: services.NewUpstreamProxyClientCache(),
	}
}

// WithConversationUsage wires the per-request token-usage recorder.
// Optional: when nil, the proxy skips token capture entirely.
func (h *ProxyHandler) WithConversationUsage(q *queries.ConversationUsageQueries) *ProxyHandler {
	h.convUsage = q
	return h
}

// WithCrypto lets the proxy persist OAuth refreshes that pass through virtual
// auth services such as openai-auth.
func (h *ProxyHandler) WithCrypto(c *services.Crypto) *ProxyHandler {
	h.crypto = c
	return h
}

const maxCapturedBytes = 64 * 1024 // 64 KB cap per body
const maxInspectableRequestBodyBytes = 16 * 1024 * 1024

var capturedBodySecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bgh[opsru]_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}\b`),
	regexp.MustCompile(`\bxai-[A-Za-z0-9_-]{12,}\b`),
	regexp.MustCompile(`\brt\.[A-Za-z0-9._-]{20,}\b`),
	regexp.MustCompile(`\b[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
}

type proxyACLLayer struct {
	name   string
	config string
}

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
	safe := h.Clone()
	for _, key := range []string{
		"Authorization",
		"Proxy-Authorization",
		"X-Duckway-Token",
		"X-Api-Key",
		"Cookie",
		"Set-Cookie",
	} {
		if _, ok := safe[key]; ok {
			safe.Set(key, "[redacted]")
		}
	}
	out, _ := json.Marshal(safe)
	return string(out)
}

func redactCapturedBody(body, contentType string) string {
	if body == "" {
		return body
	}
	ct := strings.ToLower(contentType)
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	if ct == "application/json" || strings.HasSuffix(ct, "+json") || (ct == "" && strings.HasPrefix(strings.TrimSpace(body), "{")) {
		var v interface{}
		if json.Unmarshal([]byte(body), &v) == nil {
			redactJSONValue(v)
			if out, err := json.Marshal(v); err == nil {
				return redactTextSecrets(string(out))
			}
		}
	}
	if ct == "application/x-www-form-urlencoded" {
		vals, err := url.ParseQuery(body)
		if err == nil {
			for key := range vals {
				if isSensitiveBodyKey(key) {
					vals[key] = []string{"[redacted]"}
				}
			}
			return redactTextSecrets(vals.Encode())
		}
	}
	return redactTextSecrets(body)
}

func redactTextSecrets(body string) string {
	for _, pattern := range capturedBodySecretPatterns {
		body = pattern.ReplaceAllString(body, "[redacted]")
	}
	return body
}

func redactJSONValue(v interface{}) {
	switch obj := v.(type) {
	case map[string]interface{}:
		for key, val := range obj {
			if isSensitiveBodyKey(key) {
				obj[key] = "[redacted]"
				continue
			}
			redactJSONValue(val)
		}
	case []interface{}:
		for _, val := range obj {
			redactJSONValue(val)
		}
	}
}

func isSensitiveBodyKey(key string) bool {
	k := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	for _, marker := range []string{"token", "authorization", "password", "private_key", "client_secret", "refresh_secret", "api_key"} {
		if k == marker || strings.Contains(k, marker) {
			return true
		}
	}
	return false
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

	upstreamServiceName := serviceName
	upstreamBaseURL := ""
	switch serviceName {
	case "openai-chatgpt":
		upstreamServiceName = "openai"
		upstreamBaseURL = "https://chatgpt.com"
	case "xai-grok":
		upstreamServiceName = "xai"
		upstreamBaseURL = "https://cli-chat-proxy.grok.com"
	case "xai-api":
		upstreamServiceName = "xai"
		upstreamBaseURL = "https://api.x.ai"
	}

	svc, err := h.services.GetByName(upstreamServiceName)
	if err != nil {
		jsonError(w, "unknown service: "+serviceName, http.StatusNotFound)
		return
	}
	if serviceName == "xai-grok" && !proxyHostPatternAllows(svc.HostPattern, "cli-chat-proxy.grok.com") {
		jsonError(w, "xai service does not allow Grok CLI host", http.StatusForbidden)
		return
	}
	if serviceName == "xai-api" && !proxyHostPatternAllows(svc.HostPattern, "api.x.ai") {
		jsonError(w, "xai service does not allow xAI API host", http.StatusForbidden)
		return
	}

	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client authentication required", http.StatusUnauthorized)
		return
	}
	websocketUpgrade := serviceName == "openai-chatgpt" && isProxyWebSocketUpgrade(r)
	if websocketUpgrade {
		if err := validateCodexWebSocketRequest(r, upstreamPath); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	result, err := h.resolveProxyKey(r, client.ID, svc, websocketUpgrade || serviceName == "xai-grok" || serviceName == "xai-api")
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
	if websocketUpgrade {
		if _, ok := w.(http.Hijacker); !ok {
			jsonError(w, "websocket bridge unavailable", http.StatusInternalServerError)
			return
		}
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

	// Three-level ACL: request must pass ALL non-empty layers.
	// Each layer can only shrink (restrict further), never widen.
	//   1. Service default_acl (widest)
	//   2. API Key acl
	//   3. Placeholder permission_config (narrowest)
	aclLayers := []proxyACLLayer{
		{"service", svc.DefaultACL},
		{"api_key", result.APIKeyACL},
		{"placeholder", result.PermissionConfig},
	}
	aclMethod, aclPath := proxyACLRequest(r.Method, upstreamPath, r.URL.RawQuery)
	bodyRequired := requestBodyRequiredForProxyACL(aclLayers, aclMethod, aclPath)

	var bodyBytes []byte
	if bodyRequired && r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxInspectableRequestBodyBytes))
		r.Body.Close()
		if err != nil {
			jsonError(w, "request body too large or unreadable for permission check", http.StatusRequestEntityTooLarge)
			return
		}
	}

	for _, layer := range aclLayers {
		if layer.config == "" {
			continue
		}
		permResult := h.permissions.Check(layer.config, result.PlaceholderID, aclMethod, aclPath, bodyBytes)
		if !permResult.Allowed {
			jsonError(w, "permission denied ("+layer.name+"): "+permResult.Reason, http.StatusForbidden)
			return
		}
	}

	// Build upstream URL
	if upstreamBaseURL == "" {
		upstreamBaseURL = svc.UpstreamURL
	}
	upstreamBaseURL = effectiveProxyUpstreamBaseURL(serviceName, upstreamBaseURL, upstreamPath)
	upstreamURL := strings.TrimRight(upstreamBaseURL, "/") + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	var bodyReader io.Reader
	if bodyRequired {
		bodyReader = bytes.NewReader(bodyBytes)
	} else {
		bodyReader = r.Body
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
		if shouldStripHeader(key) || (websocketUpgrade && !shouldForwardCodexWebSocketHeader(key)) {
			continue
		}
		for _, v := range values {
			upstreamReq.Header.Add(key, v)
		}
	}
	if websocketUpgrade {
		upstreamReq.Header.Set("Connection", "Upgrade")
		upstreamReq.Header.Set("Upgrade", "websocket")
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
	realKey := result.RealKey
	if serviceName == "github" {
		ghAppCred, ok, err := parseGitHubAppCredential(result.RealKey)
		if err != nil {
			jsonError(w, "invalid github app credential", http.StatusBadGateway)
			return
		}
		if ok {
			realKey, err = h.mintGitHubInstallationToken(r.Context(), ghAppCred, r.Method, upstreamPath, r.URL.RawQuery)
			if err != nil {
				log.Printf("github app token mint failed for placeholder %s: %v", result.PlaceholderID, err)
				jsonError(w, "github app token mint failed", http.StatusBadGateway)
				return
			}
		}
	}
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
		if serviceName == "github" && strings.EqualFold(authHeader, "Authorization") {
			if rewritten, ok := rewriteGitHubBasicAuth(r.Header.Get("Authorization"), result.Placeholder, realKey); ok {
				upstreamReq.Header.Set(authHeader, rewritten)
			} else {
				upstreamReq.Header.Set(authHeader, authPrefix+realKey)
			}
		} else {
			upstreamReq.Header.Set(authHeader, authPrefix+realKey)
		}
	case "header":
		upstreamReq.Header.Set(authHeader, realKey)
	case "query":
		q := upstreamReq.URL.Query()
		q.Set(authHeader, realKey)
		upstreamReq.URL.RawQuery = q.Encode()
	}

	upstreamClient, err := h.httpClientForUpstream(result.UpstreamProxyURL)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := upstreamClient.Do(upstreamReq)
	if err != nil {
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			// Client disconnected before upstream responded — not a gateway fault.
			// Still log the row so the audit panel shows the attempt.
			if h.requestLog != nil {
				h.requestLog.Log(client.ID, result.PlaceholderID, serviceName, r.Method, upstreamPath, 0)
			}
			return
		}
		log.Printf("upstream error for %s via %s: %s", serviceName, services.RedactProxyURL(result.UpstreamProxyURL), services.RedactProxyError(result.UpstreamProxyURL, err))
		jsonError(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if websocketUpgrade && resp.StatusCode == http.StatusSwitchingProtocols {
		if !isProxyWebSocketUpgradeResponse(resp) {
			jsonError(w, "invalid upstream websocket response", http.StatusBadGateway)
			return
		}
		if h.requestLog != nil {
			h.requestLog.Log(client.ID, result.PlaceholderID, serviceName, r.Method, upstreamPath, resp.StatusCode)
		}
		h.relayProxyWebSocket(w, resp)
		return
	}

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
		if reqSize == 0 && r.ContentLength > 0 {
			reqSize = r.ContentLength
		}
		reqCT := r.Header.Get("Content-Type")
		if len(bodyBytes) > 0 && shouldCaptureContentType(reqCT) {
			toStore := bodyBytes
			if decoded, ok := decodeBody(bodyBytes, r.Header.Get("Content-Encoding")); ok {
				toStore = decoded
			}
			if int64(len(toStore)) > maxCapturedBytes {
				reqBodyStored = redactCapturedBody(string(toStore[:maxCapturedBytes]), reqCT)
				reqTrunc = true
			} else {
				reqBodyStored = redactCapturedBody(string(toStore), reqCT)
			}
		}

		if captureDetail {
			_ = h.requestLog.StoreDetail(&queries.RequestLogDetail{
				LogID:           logID,
				RequestHeaders:  formatHeaders(r.Header),
				RequestBody:     reqBodyStored,
				RequestSize:     reqSize,
				ResponseHeaders: formatHeaders(resp.Header),
				ResponseBody:    redactCapturedBody(string(respBodyStored), resp.Header.Get("Content-Type")),
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

func isProxyWebSocketUpgrade(req *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, value := range req.Header.Values("Connection") {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
				return true
			}
		}
	}
	return false
}

func isProxyWebSocketUpgradeResponse(resp *http.Response) bool {
	if !strings.EqualFold(strings.TrimSpace(resp.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, value := range resp.Header.Values("Connection") {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
				_, ok := resp.Body.(io.ReadWriteCloser)
				return ok
			}
		}
	}
	return false
}

func validateCodexWebSocketRequest(req *http.Request, path string) error {
	if req.Method != http.MethodGet {
		return fmt.Errorf("codex websocket requires GET")
	}
	if path != "/backend-api/codex/responses" {
		return fmt.Errorf("unsupported codex websocket path")
	}
	if len(req.Header.Values("Authorization")) != 1 {
		return fmt.Errorf("codex websocket requires one authorization header")
	}
	if req.Header.Get("Sec-WebSocket-Version") != "13" {
		return fmt.Errorf("unsupported websocket version")
	}
	keys := req.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return fmt.Errorf("codex websocket requires one key")
	}
	rawKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0]))
	if err != nil || len(rawKey) != 16 {
		return fmt.Errorf("invalid websocket key")
	}
	return nil
}

func shouldForwardCodexWebSocketHeader(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "sec-websocket-") || strings.HasPrefix(lower, "x-stainless-") {
		return true
	}
	switch lower {
	case "origin", "user-agent", "openai-beta", "chatgpt-account-id", "x-openai-client-user-agent":
		return true
	default:
		return false
	}
}

func (h *ProxyHandler) relayProxyWebSocket(w http.ResponseWriter, upstreamResp *http.Response) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	upstream, ok := upstreamResp.Body.(io.ReadWriteCloser)
	if !ok {
		return
	}
	clientConn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if err := writeProxyUpgradeResponse(rw.Writer, upstreamResp); err != nil {
		clientConn.Close()
		return
	}
	relayProxyWebSocketStreams(clientConn, rw.Reader, upstream)
}

func writeProxyUpgradeResponse(writer *bufio.Writer, resp *http.Response) error {
	proto := resp.Proto
	if proto == "" {
		proto = "HTTP/1.1"
	}
	if _, err := fmt.Fprintf(writer, "%s %d %s\r\n", proto, resp.StatusCode, http.StatusText(resp.StatusCode)); err != nil {
		return err
	}
	if err := resp.Header.Write(writer); err != nil {
		return err
	}
	if _, err := writer.WriteString("\r\n"); err != nil {
		return err
	}
	return writer.Flush()
}

func relayProxyWebSocketStreams(clientConn net.Conn, clientReader io.Reader, upstream io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, clientReader)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, upstream)
		done <- struct{}{}
	}()
	<-done
	_ = upstream.Close()
	_ = clientConn.Close()
	<-done
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
	bodyBytes, _ := io.ReadAll(r.Body)
	openAISvc, err := h.services.GetByName("openai")
	if err != nil {
		jsonError(w, "openai service unavailable", http.StatusNotFound)
		return
	}
	placeholder := openAIAuthRefreshPlaceholder(bodyBytes, r.Header.Get("Content-Type"))
	if placeholder == "" {
		jsonError(w, "duckway phantom refresh token required", http.StatusForbidden)
		return
	}
	result, err := h.resolver.Resolve(placeholder, client.ID)
	if err != nil {
		log.Printf("resolve error for openai-auth/%s: %v", client.Name, err)
		jsonError(w, "key resolution failed", http.StatusInternalServerError)
		return
	}
	if result.Error == "" && result.ServiceID != "" && result.ServiceID != openAISvc.ID {
		jsonError(w, "placeholder key is not for openai", http.StatusForbidden)
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

	upstreamURL := "https://auth.openai.com" + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	var resp *http.Response
	var respBody []byte
	errStatus := http.StatusInternalServerError
	errMessage := "openai auth refresh failed"
	if err := services.WithOAuthRefreshLock(result.APIKeyID, func() error {
		lockedResult, resolveErr := h.resolver.Resolve(placeholder, client.ID)
		if resolveErr != nil {
			log.Printf("resolve error for openai-auth/%s: %v", client.Name, resolveErr)
			errStatus = http.StatusInternalServerError
			errMessage = "key resolution failed"
			return errors.New("openai auth proxy response already selected")
		}
		if lockedResult.Error == "" && lockedResult.ServiceID != "" && lockedResult.ServiceID != openAISvc.ID {
			errStatus = http.StatusForbidden
			errMessage = "placeholder key is not for openai"
			return errors.New("openai auth proxy response already selected")
		}
		if lockedResult.NeedApproval {
			errStatus = http.StatusForbidden
			errMessage = "approval required"
			return errors.New("openai auth proxy response already selected")
		}
		if lockedResult.Error != "" {
			errStatus = http.StatusForbidden
			errMessage = lockedResult.Error
			return errors.New("openai auth proxy response already selected")
		}
		if lockedResult.RealRefreshToken == "" {
			errStatus = http.StatusForbidden
			errMessage = "openai key is not a refreshable Codex OAuth token"
			return errors.New("openai auth proxy response already selected")
		}
		result = lockedResult

		clientID := h.openAIAuthClientID(result.APIKeyID)
		rewrittenBody, contentType := rewriteCodexRefreshRequest(bodyBytes, r.Header.Get("Content-Type"), result.RealRefreshToken, clientID)
		upstreamReq, buildErr := buildOpenAIAuthUpstreamRequest(r.Context(), r.Method, upstreamURL, r.Header, rewrittenBody, contentType)
		if buildErr != nil {
			errStatus = http.StatusInternalServerError
			errMessage = "failed to create upstream request"
			return errors.New("openai auth proxy response already selected")
		}

		upstreamClient, clientErr := h.httpClientForUpstream(result.UpstreamProxyURL)
		if clientErr != nil {
			errStatus = http.StatusBadGateway
			errMessage = clientErr.Error()
			return errors.New("openai auth proxy response already selected")
		}
		var doErr error
		resp, doErr = upstreamClient.Do(upstreamReq)
		if doErr != nil {
			log.Printf("openai auth upstream error via %s: %s", services.RedactProxyURL(result.UpstreamProxyURL), services.RedactProxyError(result.UpstreamProxyURL, doErr))
			errStatus = http.StatusBadGateway
			errMessage = "upstream request failed"
			return errors.New("openai auth proxy response already selected")
		}

		respBody, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if (resp.StatusCode < 200 || resp.StatusCode >= 300) &&
			services.IsPermanentOAuthError(resp.StatusCode, respBody) &&
			h.openAIAuthRefreshTokenChanged(result.APIKeyID, result.RealRefreshToken) {
			updatedResult, resolveErr := h.resolver.Resolve(placeholder, client.ID)
			if resolveErr == nil &&
				updatedResult != nil &&
				updatedResult.Error == "" &&
				!updatedResult.NeedApproval &&
				updatedResult.APIKeyID == result.APIKeyID &&
				updatedResult.ServiceID == openAISvc.ID &&
				updatedResult.PlaceholderID == result.PlaceholderID &&
				updatedResult.RealRefreshToken != "" {
				updatedClientID := h.openAIAuthClientID(updatedResult.APIKeyID)
				updatedBody, updatedContentType := rewriteCodexRefreshRequest(bodyBytes, r.Header.Get("Content-Type"), updatedResult.RealRefreshToken, updatedClientID)
				retryReq, buildErr := buildOpenAIAuthUpstreamRequest(r.Context(), r.Method, upstreamURL, r.Header, updatedBody, updatedContentType)
				if buildErr == nil {
					retryClient, clientErr := h.httpClientForUpstream(updatedResult.UpstreamProxyURL)
					if clientErr == nil {
						retryResp, retryErr := retryClient.Do(retryReq)
						if retryErr == nil {
							resp = retryResp
							respBody, _ = io.ReadAll(resp.Body)
							_ = resp.Body.Close()
							result = updatedResult
						}
					}
				}
			} else if resolveErr != nil {
				log.Printf("openai auth retry resolve failed for %s: %v", result.APIKeyID, resolveErr)
			} else {
				log.Printf("openai auth retry skipped for %s because credential binding changed", result.APIKeyID)
			}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			h.persistOpenAIAuthRefresh(result.APIKeyID, result.RealRefreshToken, respBody)
		}
		return nil
	}); err != nil {
		jsonError(w, errMessage, errStatus)
		return
	}
	respBody = rewriteCodexRefreshResponse(respBody, result.PlaceholderID, result.Placeholder)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody = redactOpenAIAuthProxyResponse(respBody, result.RealRefreshToken)
	}
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

func buildOpenAIAuthUpstreamRequest(ctx context.Context, method, upstreamURL string, headers http.Header, body []byte, contentType string) (*http.Request, error) {
	upstreamReq, err := http.NewRequestWithContext(ctx, method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
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
	return upstreamReq, nil
}

func (h *ProxyHandler) httpClientForUpstream(proxyURL string) (*http.Client, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return h.httpClient, nil
	}
	return h.proxyClients.Client(proxyURL)
}

func (h *ProxyHandler) persistOpenAIAuthRefresh(apiKeyID, currentRefreshToken string, body []byte) {
	if h.crypto == nil || h.apiKeys == nil || apiKeyID == "" || len(body) == 0 {
		return
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		log.Printf("openai auth refresh response parse failed for %s: %v", apiKeyID, err)
		return
	}
	if tokenResp.AccessToken == "" {
		return
	}
	encAccess, err := h.crypto.Encrypt(tokenResp.AccessToken)
	if err != nil {
		log.Printf("openai auth refresh access encrypt failed for %s: %v", apiKeyID, err)
		return
	}
	if err := h.apiKeys.UpdateTokens(apiKeyID, encAccess, openAIAuthRefreshExpiresAt(tokenResp.AccessToken, tokenResp.ExpiresIn)); err != nil {
		log.Printf("openai auth refresh access store failed for %s: %v", apiKeyID, err)
	}
	h.persistOpenAIAuthRefreshMetadata(apiKeyID, tokenResp.IDToken)
	if tokenResp.RefreshToken == "" || tokenResp.RefreshToken == currentRefreshToken {
		return
	}
	encRefresh, err := h.crypto.Encrypt(tokenResp.RefreshToken)
	if err != nil {
		log.Printf("openai auth refresh token encrypt failed for %s: %v", apiKeyID, err)
		return
	}
	if err := h.apiKeys.UpdateRefreshToken(apiKeyID, encRefresh); err != nil {
		log.Printf("openai auth refresh token store failed for %s: %v", apiKeyID, err)
	}
}

func (h *ProxyHandler) persistOpenAIAuthRefreshMetadata(apiKeyID, idToken string) {
	key, err := h.apiKeys.GetByID(apiKeyID)
	if err != nil {
		log.Printf("openai auth refresh metadata reload failed for %s: %v", apiKeyID, err)
		return
	}
	subInfo, err := parseSubscriptionInfo(key.SubscriptionInfo)
	if err != nil {
		subInfo = map[string]interface{}{}
	}
	if idToken != "" {
		subInfo["id_token"] = idToken
	}
	subInfo["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	out, err := json.Marshal(subInfo)
	if err != nil {
		log.Printf("openai auth refresh metadata marshal failed for %s: %v", apiKeyID, err)
		return
	}
	if err := h.apiKeys.UpdateSubscriptionInfo(apiKeyID, string(out)); err != nil {
		log.Printf("openai auth refresh metadata store failed for %s: %v", apiKeyID, err)
	}
}

func openAIAuthRefreshExpiresAt(accessToken string, expiresIn int64) int64 {
	if expiresIn > 0 {
		return time.Now().UnixMilli() + expiresIn*1000
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return time.Now().UnixMilli() + 3600*1000
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Now().UnixMilli() + 3600*1000
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Now().UnixMilli() + 3600*1000
	}
	return claims.Exp * 1000
}

func (h *ProxyHandler) openAIAuthClientID(apiKeyID string) string {
	clientID := defaultCodexClientID
	if h.apiKeys == nil || apiKeyID == "" {
		return clientID
	}
	key, err := h.apiKeys.GetByID(apiKeyID)
	if err != nil {
		return clientID
	}
	if subInfo, err := parseSubscriptionInfo(key.SubscriptionInfo); err == nil {
		if v, _ := subInfo["client_id"].(string); v != "" {
			return v
		}
		if v, _ := subInfo["clientId"].(string); v != "" {
			return v
		}
	}
	if claims := parseJWTClaims(resultSafeAccessToken(h.crypto, key.KeyEncrypted)); len(claims) > 0 {
		return codexClientIDFromClaims(claims)
	}
	return clientID
}

func (h *ProxyHandler) openAIAuthRefreshTokenChanged(apiKeyID, usedRefreshToken string) bool {
	if h.crypto == nil || h.apiKeys == nil || apiKeyID == "" {
		return false
	}
	key, err := h.apiKeys.GetByID(apiKeyID)
	if err != nil || key.RefreshToken == "" {
		return false
	}
	currentRefreshToken, err := h.crypto.Decrypt(key.RefreshToken)
	if err != nil {
		return false
	}
	return currentRefreshToken != "" && currentRefreshToken != usedRefreshToken
}

func resultSafeAccessToken(crypto *services.Crypto, encrypted string) string {
	if crypto == nil || encrypted == "" {
		return ""
	}
	plain, err := crypto.Decrypt(encrypted)
	if err != nil {
		return ""
	}
	return plain
}

func rewriteCodexRefreshRequest(body []byte, contentType, realRefresh, clientID string) ([]byte, string) {
	if strings.TrimSpace(clientID) == "" {
		clientID = defaultCodexClientID
	}
	lowerCT := strings.ToLower(contentType)
	if strings.Contains(lowerCT, "application/x-www-form-urlencoded") ||
		strings.Contains(string(body), "refresh_token=") ||
		strings.Contains(string(body), "grant_type=") {
		vals, err := url.ParseQuery(string(body))
		if err == nil {
			vals.Set("refresh_token", realRefresh)
			vals.Set("client_id", clientID)
			return []byte(vals.Encode()), "application/x-www-form-urlencoded"
		}
	}
	var obj map[string]interface{}
	if json.Unmarshal(body, &obj) == nil {
		obj["refresh_token"] = realRefresh
		obj["client_id"] = clientID
		out, _ := json.Marshal(obj)
		return out, "application/json"
	}
	return body, contentType
}

func openAIAuthRefreshPlaceholder(body []byte, contentType string) string {
	refresh := ""
	lowerCT := strings.ToLower(contentType)
	if strings.Contains(lowerCT, "application/x-www-form-urlencoded") ||
		strings.Contains(string(body), "refresh_token=") ||
		strings.Contains(string(body), "grant_type=") {
		if vals, err := url.ParseQuery(string(body)); err == nil {
			refresh = vals.Get("refresh_token")
		}
	}
	if refresh == "" {
		var obj map[string]interface{}
		if json.Unmarshal(body, &obj) == nil {
			refresh, _ = obj["refresh_token"].(string)
		}
	}
	const prefix = "rt.duckway."
	if !strings.HasPrefix(refresh, prefix) {
		return ""
	}
	return strings.TrimPrefix(refresh, prefix)
}

func rewriteGitHubBasicAuth(authHeader, placeholder, realKey string) (string, bool) {
	if placeholder == "" || authHeader == "" || !strings.HasPrefix(strings.ToLower(authHeader), "basic ") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authHeader[len("Basic "):]))
	if err != nil {
		return "", false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok || pass != placeholder {
		return "", false
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+realKey)), true
}

func (h *ProxyHandler) resolveProxyKey(r *http.Request, clientID string, svc *models.Service, requireExplicit bool) (*services.ResolveResult, error) {
	if placeholder := explicitProxyPlaceholder(r, svc); placeholder != "" {
		result, err := h.resolver.Resolve(placeholder, clientID)
		if err != nil {
			return nil, err
		}
		if result.Error == "" && result.ServiceID != "" && result.ServiceID != svc.ID {
			return &services.ResolveResult{Error: "placeholder key is not for this service"}, nil
		}
		return result, nil
	}
	if requireExplicit {
		return &services.ResolveResult{Error: "duckway phantom token required"}, nil
	}
	return h.resolver.ResolveForService(clientID, svc.ID)
}

func explicitProxyPlaceholder(r *http.Request, svc *models.Service) string {
	authHeader := svc.AuthHeader
	if authHeader == "" {
		authHeader = "Authorization"
	}
	auth := strings.TrimSpace(r.Header.Get(authHeader))
	if auth == "" && !strings.EqualFold(authHeader, "Authorization") {
		auth = strings.TrimSpace(r.Header.Get("Authorization"))
	}
	if auth == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(auth), "basic ") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(auth[len("Basic "):]))
		if err != nil {
			return ""
		}
		_, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return ""
		}
		return explicitPlaceholderToken(strings.TrimSpace(pass))
	}
	prefix := svc.AuthPrefix
	if prefix != "" && strings.HasPrefix(strings.ToLower(auth), strings.ToLower(prefix)) {
		return explicitPlaceholderToken(strings.TrimSpace(auth[len(prefix):]))
	}
	if strings.Contains(auth, " ") {
		return ""
	}
	return explicitPlaceholderToken(auth)
}

func explicitPlaceholderToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err == nil {
			var claims map[string]interface{}
			if json.Unmarshal(payload, &claims) == nil {
				placeholder, _ := claims["https://duckway.dev/placeholder"].(string)
				if services.IsPlaceholder(placeholder) {
					return placeholder
				}
			}
		}
	}
	if services.IsPlaceholder(token) {
		return token
	}
	return ""
}

func effectiveProxyUpstreamBaseURL(serviceName, configuredBaseURL, upstreamPath string) string {
	if serviceName == "github" && strings.Contains(configuredBaseURL, "api.github.com") && isGitHubSmartHTTPPath(upstreamPath) {
		return "https://github.com"
	}
	return configuredBaseURL
}

func proxyHostPatternAllows(patterns, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, raw := range strings.Split(patterns, ",") {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case pattern == host:
			return true
		case strings.HasPrefix(pattern, "*.") && strings.HasSuffix(host, strings.TrimPrefix(pattern, "*")):
			return true
		}
	}
	return false
}

func isGitHubSmartHTTPPath(path string) bool {
	path = strings.TrimSpace(path)
	return strings.HasSuffix(path, ".git/info/refs") ||
		strings.HasSuffix(path, ".git/git-upload-pack") ||
		strings.HasSuffix(path, ".git/git-receive-pack")
}

func proxyACLRequest(method, upstreamPath, rawQuery string) (string, string) {
	if !strings.EqualFold(method, http.MethodGet) || !strings.HasSuffix(upstreamPath, ".git/info/refs") {
		return method, upstreamPath
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return method, upstreamPath
	}
	switch values.Get("service") {
	case "git-upload-pack":
		return http.MethodPost, strings.TrimSuffix(upstreamPath, ".git/info/refs") + ".git/git-upload-pack"
	case "git-receive-pack":
		return http.MethodPost, strings.TrimSuffix(upstreamPath, ".git/info/refs") + ".git/git-receive-pack"
	default:
		return method, upstreamPath
	}
}

func requestBodyRequiredForProxyACL(layers []proxyACLLayer, method, path string) bool {
	for _, layer := range layers {
		if services.RequestBodyRequiredForPermission(layer.config, method, path) {
			return true
		}
	}
	return false
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

func redactOpenAIAuthProxyResponse(body []byte, realRefreshToken string) []byte {
	text := string(body)
	if realRefreshToken != "" {
		text = strings.ReplaceAll(text, realRefreshToken, "[REDACTED_REFRESH_TOKEN]")
	}
	for _, pattern := range capturedBodySecretPatterns {
		text = pattern.ReplaceAllString(text, "[REDACTED_SECRET]")
	}
	return []byte(text)
}

func shouldStripOpenAIAuthHeader(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "x-duckway-") {
		return true
	}
	switch lower {
	case "host":
		return true
	case "accept-encoding":
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
