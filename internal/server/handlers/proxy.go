package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

type ProxyHandler struct {
	services    *queries.ServiceQueries
	resolver    *services.KeyResolver
	requestLog  *queries.RequestLogQueries
	approvals   *queries.ApprovalQueries
	permissions *services.PermissionChecker
	notifier    *services.Notifier
	httpClient  *http.Client
}

func NewProxyHandler(svcQueries *queries.ServiceQueries, resolver *services.KeyResolver, requestLog *queries.RequestLogQueries, approvals *queries.ApprovalQueries, notifier *services.Notifier) *ProxyHandler {
	return &ProxyHandler{
		services:    svcQueries,
		resolver:    resolver,
		requestLog:  requestLog,
		approvals:   approvals,
		permissions: services.NewPermissionChecker(),
		notifier:    notifier,
		httpClient:  &http.Client{},
	}
}

func (h *ProxyHandler) Handle(w http.ResponseWriter, r *http.Request) {
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
			h.requestLog.Log(client.ID, serviceName, r.Method, upstreamPath, 200)
		}
		return
	}

	// Buffer body for permission checking
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}

	// Check permissions if configured
	if result.PermissionConfig != "" {
		permResult := h.permissions.Check(result.PermissionConfig, result.PlaceholderID, r.Method, upstreamPath, bodyBytes)
		if !permResult.Allowed {
			jsonError(w, "permission denied: "+permResult.Reason, http.StatusForbidden)
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

	// Copy headers (excluding proxy-specific ones)
	for key, values := range r.Header {
		lower := strings.ToLower(key)
		if lower == "x-duckway-token" || lower == "x-duckway-key" || lower == "host" {
			continue
		}
		for _, v := range values {
			upstreamReq.Header.Add(key, v)
		}
	}

	// Inject real API key
	switch svc.AuthType {
	case "bearer":
		upstreamReq.Header.Set(svc.AuthHeader, svc.AuthPrefix+result.RealKey)
	case "header":
		upstreamReq.Header.Set(svc.AuthHeader, result.RealKey)
	case "query":
		q := upstreamReq.URL.Query()
		q.Set(svc.AuthHeader, result.RealKey)
		upstreamReq.URL.RawQuery = q.Encode()
	}

	resp, err := h.httpClient.Do(upstreamReq)
	if err != nil {
		log.Printf("upstream error for %s: %v", serviceName, err)
		jsonError(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if h.requestLog != nil {
		h.requestLog.Log(client.ID, serviceName, r.Method, upstreamPath, resp.StatusCode)
	}

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
