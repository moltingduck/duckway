package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

// LoanHandler issues short-lived real tokens to a trusted duckway-client
// sidecar. The sidecar caches the token in RAM and forwards requests
// directly to upstream — the gateway is only involved at loan time, not
// per-request. This is the "loan_proxy" delivery mode.
type LoanHandler struct {
	resolver  *services.KeyResolver
	services  *queries.ServiceQueries
	approvals *queries.ApprovalQueries
	logs      *queries.RequestLogQueries
	notifier  *services.Notifier
	defaultTTLSeconds int
}

func NewLoanHandler(
	resolver *services.KeyResolver,
	svcQ *queries.ServiceQueries,
	approvalQ *queries.ApprovalQueries,
	logQ *queries.RequestLogQueries,
	notifier *services.Notifier,
) *LoanHandler {
	return &LoanHandler{
		resolver:          resolver,
		services:          svcQ,
		approvals:         approvalQ,
		logs:              logQ,
		notifier:          notifier,
		defaultTTLSeconds: 60, // 60s window — long enough for any single git op
	}
}

// GET /client/loan?service=<name>
//
// Resolves the calling client's phantom binding for the given service and
// returns a short-lived "loan" of the real token plus the auth scheme the
// sidecar should use when injecting it into upstream requests.
//
// The sidecar is expected to:
//   - Cache (real_token, auth_*) keyed by service for at most ttl_seconds
//   - Replace the agent's auth header with `auth_prefix + real_token`
//     (or set auth_header to real_token directly when auth_type is "header")
//   - Forward the request DIRECTLY to upstream — gateway is out of the data path
//   - Re-fetch via this endpoint when the cache entry expires
func (h *LoanHandler) Issue(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}

	serviceName := r.URL.Query().Get("service")
	if serviceName == "" {
		jsonError(w, "service query parameter required", http.StatusBadRequest)
		return
	}

	svc, err := h.services.GetByName(serviceName)
	if err != nil {
		jsonError(w, "unknown service: "+serviceName, http.StatusNotFound)
		return
	}
	if svc.DeliveryMode != "loan_proxy" {
		jsonError(w, "service is not configured for loan_proxy delivery", http.StatusForbidden)
		return
	}

	result, err := h.resolver.ResolveForService(client.ID, svc.ID)
	if err != nil {
		jsonError(w, "key resolution failed", http.StatusInternalServerError)
		return
	}
	if result.Error != "" {
		jsonError(w, result.Error, http.StatusForbidden)
		return
	}

	// Coarse approval gate: if the binding requires approval and there's no
	// valid one, block. The user accepted coarse-grained ACL/approval for
	// loan_proxy, so we only check at the loan level — not per request.
	if result.NeedApproval {
		approvalID, _ := CreatePendingApproval(h.approvals, result.PlaceholderID, "LOAN", "/proxy/"+serviceName)
		if h.notifier != nil {
			h.notifier.NotifyApprovalNeeded(services.ApprovalNotification{
				ApprovalID:    approvalID,
				PlaceholderID: result.PlaceholderID,
				ClientName:    client.Name,
				ServiceName:   serviceName,
				Method:        "LOAN",
				Path:          "/proxy/" + serviceName,
				AdminURL:      "/admin/approvals",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error":       "duckway_approval_pending",
			"message":     "Loan requires admin approval. Retry after approval.",
			"approval_id": approvalID,
		})
		return
	}

	// Refreshable (OAuth) keys still go in Authorization: Bearer regardless of
	// the service's configured auth_type — same special-case as the proxy path.
	authType := svc.AuthType
	authHeader := svc.AuthHeader
	authPrefix := svc.AuthPrefix
	if result.IsRefreshable {
		authType = "bearer"
		authHeader = "Authorization"
		authPrefix = "Bearer "
	}

	if h.logs != nil {
		h.logs.Log(client.ID, result.PlaceholderID, serviceName, "LOAN", "/loan", 200)
	}

	jsonResponse(w, map[string]interface{}{
		"real_token":   result.RealKey,
		"ttl_seconds":  h.defaultTTLSeconds,
		"auth_type":    authType,
		"auth_header":  authHeader,
		"auth_prefix":  authPrefix,
		"placeholder_id": result.PlaceholderID,
	})
}

// POST /client/audit
//
// Sidecar batches request entries it observed in loan_proxy mode and posts
// them here so the gateway can keep a single source of truth in request_logs.
// The gateway does not see the bodies; only the metadata the sidecar provides.
func (h *LoanHandler) Audit(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}

	var entries []struct {
		PlaceholderID string `json:"placeholder_id"`
		Service       string `json:"service"`
		Method        string `json:"method"`
		Path          string `json:"path"`
		Status        int    `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	logged := 0
	if h.logs != nil {
		for _, e := range entries {
			h.logs.Log(client.ID, e.PlaceholderID, e.Service, e.Method, e.Path, e.Status)
			logged++
		}
	}
	jsonResponse(w, map[string]int{"logged": logged})
}
