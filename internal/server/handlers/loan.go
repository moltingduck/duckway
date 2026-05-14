package handlers

import (
	"database/sql"
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
	resolver          *services.KeyResolver
	services          *queries.ServiceQueries
	approvals         *queries.ApprovalQueries
	logs              *queries.RequestLogQueries
	notifier          *services.Notifier
	crypto            *services.Crypto
	db                *sql.DB
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

func (h *LoanHandler) WithCrypto(crypto *services.Crypto) *LoanHandler {
	h.crypto = crypto
	return h
}

func (h *LoanHandler) WithDB(db *sql.DB) *LoanHandler {
	h.db = db
	return h
}

// GET /client/loan?service=<name>[&group=<group_id>[&exclude_key=<key_id>]]
//
// Resolves the calling client's phantom binding for the given service and
// returns a short-lived "loan" of the real token plus the auth scheme the
// sidecar should use when injecting it into upstream requests.
//
// When group is provided, the score-based key selection algorithm is used
// instead of the placeholder binding. The sidecar should pass exclude_key
// when retrying after a 429 to skip the exhausted key.
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

	// Group-based loan: score-based key selection, no phantom binding required.
	groupID := r.URL.Query().Get("group")
	if groupID != "" {
		h.issueGroupLoan(w, r, client.ID, client.Name, serviceName, groupID)
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
		"real_token":     result.RealKey,
		"ttl_seconds":    h.defaultTTLSeconds,
		"auth_type":      authType,
		"auth_header":    authHeader,
		"auth_prefix":    authPrefix,
		"placeholder_id": result.PlaceholderID,
	})
}

// issueGroupLoan performs key selection from a KeyGroup and returns a loan.
func (h *LoanHandler) issueGroupLoan(w http.ResponseWriter, r *http.Request, clientID, clientName, serviceName, groupID string) {
	if h.db == nil || h.crypto == nil {
		jsonError(w, "group loans not configured on this server", http.StatusInternalServerError)
		return
	}

	excludeKey := r.URL.Query().Get("exclude_key")

	apiKeyID, err := queries.SelectKeyForGroup(h.db, groupID, excludeKey)
	if err != nil {
		jsonError(w, "no available key in group: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	apiKeyQ := queries.NewAPIKeyQueries(h.db)
	apiKey, err := apiKeyQ.GetByID(apiKeyID)
	if err != nil {
		jsonError(w, "key lookup failed", http.StatusInternalServerError)
		return
	}
	if !apiKey.IsActive {
		jsonError(w, "selected key is inactive", http.StatusServiceUnavailable)
		return
	}

	realKey, err := h.crypto.Decrypt(apiKey.KeyEncrypted)
	if err != nil {
		jsonError(w, "decrypt failed", http.StatusInternalServerError)
		return
	}

	// Determine auth scheme from service config.
	authType := "header"
	authHeader := "x-api-key"
	authPrefix := ""
	if svc, svcErr := h.services.GetByName(serviceName); svcErr == nil {
		authType = svc.AuthType
		authHeader = svc.AuthHeader
		authPrefix = svc.AuthPrefix
	}

	if h.logs != nil {
		h.logs.Log(clientID, "", serviceName, "LOAN_GROUP", "/loan", 200)
	}

	jsonResponse(w, map[string]interface{}{
		"real_token":  realKey,
		"api_key_id":  apiKeyID,
		"group_id":    groupID,
		"ttl_seconds": h.defaultTTLSeconds,
		"auth_type":   authType,
		"auth_header": authHeader,
		"auth_prefix": authPrefix,
	})
}

// POST /client/loan/exhaust — sidecar notifies that a key got a 429.
// Body: {"group_id": "...", "api_key_id": "...", "reset_at": "2026-05-14T17:00:00Z"}
func (h *LoanHandler) MarkExhausted(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}
	if h.db == nil {
		jsonError(w, "not configured", http.StatusInternalServerError)
		return
	}

	var req struct {
		GroupID  string `json:"group_id"`
		APIKeyID string `json:"api_key_id"`
		ResetAt  string `json:"reset_at"`
	}
	if err := parseRequest(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.GroupID == "" || req.APIKeyID == "" || req.ResetAt == "" {
		jsonError(w, "group_id, api_key_id, and reset_at are required", http.StatusBadRequest)
		return
	}

	if err := queries.MarkKeyExhausted(h.db, req.GroupID, req.APIKeyID, req.ResetAt); err != nil {
		jsonError(w, "failed to mark key exhausted: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
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
