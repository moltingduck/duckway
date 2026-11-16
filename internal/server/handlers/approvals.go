package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type ApprovalHandler struct {
	approvals    *queries.ApprovalQueries
	placeholders *queries.PlaceholderQueries
	decider      *svc.ApprovalDecider
}

func NewApprovalHandler(approvals *queries.ApprovalQueries, placeholders *queries.PlaceholderQueries) *ApprovalHandler {
	return &ApprovalHandler{approvals: approvals, placeholders: placeholders, decider: svc.NewApprovalDecider(approvals)}
}

// WithDecider shares the server's decider so decisions reach its observers
// (chat message updates, audit).
func (h *ApprovalHandler) WithDecider(d *svc.ApprovalDecider) *ApprovalHandler {
	h.decider = d
	return h
}

func (h *ApprovalHandler) ListPending(w http.ResponseWriter, r *http.Request) {
	list, err := h.approvals.ListPending()
	if err != nil {
		jsonError(w, "failed to list approvals", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.Approval{}
	}
	jsonResponse(w, list)
}

// List returns enriched approvals filtered by status / client / service. Query params:
//
//	status=pending,approved,rejected,ignored  (comma-separated, default: pending only)
//	client_id=<id>                            (optional)
//	service_id=<id>                           (optional)
//	limit=<n>                                 (default 500)
//
// The returned rows include client_name + service_name + env_name so the UI
// can render and filter without a second round-trip.
func (h *ApprovalHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	statusParam := q.Get("status")
	if statusParam == "" {
		statusParam = "pending"
	}
	statuses := []string{}
	for _, s := range strings.Split(statusParam, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			statuses = append(statuses, s)
		}
	}

	limit := 500
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}

	list, err := h.approvals.ListEnrichedFiltered(statuses, q.Get("client_id"), q.Get("service_id"), limit)
	if err != nil {
		jsonError(w, "failed to list approvals", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []queries.ApprovalListItem{}
	}

	jsonResponse(w, list)
}

func (h *ApprovalHandler) Approve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DurationMinutes int `json:"duration_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.DurationMinutes = 0 // no explicit duration: the token's approval time limit (24 hours by default)
	}
	id := r.PathValue("id")
	err := h.decider.Approve(r.Context(), id, req.DurationMinutes, adminActor(r), svc.ViaWeb)
	respondWebDecision(w, id, "approved", "failed to approve", err)
}

func (h *ApprovalHandler) Reject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := h.decider.Reject(r.Context(), id, adminActor(r), svc.ViaWeb)
	respondWebDecision(w, id, "rejected", "failed to reject", err)
}

// adminActor is the decided_by value for a console session: admin:<username>.
func adminActor(r *http.Request) string {
	u, _ := r.Context().Value(middleware.AdminUserKey).(string)
	if u == "" {
		u = "unknown"
	}
	return "admin:" + u
}

// respondWebDecision answers the console's decision calls. A lost race is a 409
// whose body names the existing decider so the page can say "already approved by ...".
func respondWebDecision(w http.ResponseWriter, id, status, failMsg string, err error) {
	switch {
	case err == nil:
		jsonResponse(w, map[string]string{"status": status})
	case errors.Is(err, svc.ErrApprovalNotFound):
		jsonError(w, "approval not found", http.StatusNotFound)
	case errors.Is(err, queries.ErrApprovalNotPending):
		body := map[string]string{"error": "approval already decided"}
		var ad *svc.AlreadyDecidedError
		if errors.As(err, &ad) {
			body["error"] = "approval is already " + ad.Decision.Status
			body["status"], body["decided_by"], body["decided_via"] = ad.Decision.Status, ad.Decision.DecidedBy, ad.Decision.DecidedVia
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(body)
	default:
		jsonError(w, failMsg, http.StatusInternalServerError)
	}
}

// CreatePendingApproval creates an approval request for a placeholder key.
// Called by the proxy handler when approval is needed.
//
// scope and rule are empty for token-wide approvals. For an approve rule,
// scope binds the approval to one request signature and rule records which
// rule (and ACL layer) asked for it; both are shown on the approvals page.
func CreatePendingApproval(approvals *queries.ApprovalQueries, placeholderID, method, path, scope, rule string) (string, error) {
	return CreatePendingApprovalFor(approvals, placeholderID, method, path, "", "", scope, rule)
}

// CreatePendingApprovalFor also records the query string and a short body hash
// (first 12 hex characters of the SHA-256) so the admin sees exactly what is
// being approved. The body itself is never stored.
func CreatePendingApprovalFor(approvals *queries.ApprovalQueries, placeholderID, method, path, query, bodyHash, scope, rule string) (string, error) {
	id, err := svc.GenerateToken(16)
	if err != nil {
		return "", fmt.Errorf("generate approval ID: %w", err)
	}
	info := map[string]string{
		"method": method,
		"path":   path,
	}
	if rule != "" {
		info["rule"] = rule
	}
	if query != "" {
		info["query"] = query
	}
	if bodyHash != "" {
		info["body_hash"] = bodyHash
	}
	requestInfo, _ := json.Marshal(info)

	approval := &models.Approval{
		ID:            id,
		PlaceholderID: placeholderID,
		Status:        "pending",
		RequestInfo:   strPtr(string(requestInfo)),
	}

	if scope != "" {
		if err := approvals.CreateScoped(approval, scope); err != nil {
			return "", err
		}
		return id, nil
	}
	if err := approvals.Create(approval); err != nil {
		return "", err
	}
	return id, nil
}

func strPtr(s string) *string {
	return &s
}
