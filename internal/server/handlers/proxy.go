package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

type ProxyHandler struct {
	services    *queries.ServiceQueries
	resolver    *services.KeyResolver
	requestLog  *queries.RequestLogQueries
	approvals   *queries.ApprovalQueries
	settings    *queries.SettingsQueries
	permissions *services.PermissionChecker
	notifier    *services.Notifier
	httpClient  *http.Client
}

func NewProxyHandler(svcQueries *queries.ServiceQueries, resolver *services.KeyResolver, requestLog *queries.RequestLogQueries, approvals *queries.ApprovalQueries, settings *queries.SettingsQueries, notifier *services.Notifier) *ProxyHandler {
	return &ProxyHandler{
		services:    svcQueries,
		resolver:    resolver,
		requestLog:  requestLog,
		approvals:   approvals,
		settings:    settings,
		permissions: services.NewPermissionChecker(),
		notifier:    notifier,
		httpClient:  &http.Client{},
	}
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

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

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
			multi := io.MultiWriter(w, cap)
			respSize, _ = io.Copy(multi, resp.Body)
			respBodyStored = cap.buf.Bytes()
			respTrunc = respSize > int64(maxCapturedBytes)
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
			if reqSize > maxCapturedBytes {
				reqBodyStored = string(bodyBytes[:maxCapturedBytes])
				reqTrunc = true
			} else {
				reqBodyStored = string(bodyBytes)
			}
		}

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
	} else {
		io.Copy(w, resp.Body)
	}
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
