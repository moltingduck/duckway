package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type ApprovalHandler struct {
	approvals    *queries.ApprovalQueries
	placeholders *queries.PlaceholderQueries
}

func NewApprovalHandler(approvals *queries.ApprovalQueries, placeholders *queries.PlaceholderQueries) *ApprovalHandler {
	return &ApprovalHandler{approvals: approvals, placeholders: placeholders}
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
	id := r.PathValue("id")

	existing, err := h.approvals.GetByID(id)
	if err != nil {
		jsonError(w, "approval not found", http.StatusNotFound)
		return
	}
	if existing.Status != "pending" {
		jsonError(w, "approval is already "+existing.Status, http.StatusConflict)
		return
	}

	var req struct {
		DurationMinutes int `json:"duration_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Default to 24 hours
		req.DurationMinutes = 1440
	}
	if req.DurationMinutes <= 0 {
		req.DurationMinutes = 1440
	}

	if req.DurationMinutes > 60*24*365 {
		req.DurationMinutes = 60 * 24 * 365
	}
	if err := h.approvals.Approve(id, queries.ApprovalExpiry(time.Duration(req.DurationMinutes)*time.Minute)); err != nil {
		jsonError(w, "failed to approve", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "approved"})
}

func (h *ApprovalHandler) Reject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	existing, err := h.approvals.GetByID(id)
	if err != nil {
		jsonError(w, "approval not found", http.StatusNotFound)
		return
	}
	if existing.Status != "pending" {
		jsonError(w, "approval is already "+existing.Status, http.StatusConflict)
		return
	}

	if err := h.approvals.Reject(id); err != nil {
		jsonError(w, "failed to reject", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "rejected"})
}

// CreatePendingApproval creates an approval request for a placeholder key.
// Called by the proxy handler when approval is needed.
//
// scope and rule are empty for token-wide approvals. For an approve rule,
// scope binds the approval to one request signature and rule records which
// rule (and ACL layer) asked for it; both are shown on the approvals page.
func CreatePendingApproval(approvals *queries.ApprovalQueries, placeholderID, method, path, scope, rule string) (string, error) {
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
