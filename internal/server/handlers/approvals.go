package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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

	list, err := h.approvals.ListEnriched(statuses, limit)
	if err != nil {
		jsonError(w, "failed to list approvals", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []queries.ApprovalListItem{}
	}

	// Apply client_id / service_id filters in Go since the query already
	// joined those tables — keeps SQL composition simple.
	clientID := q.Get("client_id")
	serviceID := q.Get("service_id")
	if clientID != "" || serviceID != "" {
		filtered := make([]queries.ApprovalListItem, 0, len(list))
		for _, a := range list {
			if clientID != "" && a.ClientID != clientID {
				continue
			}
			if serviceID != "" && a.ServiceID != serviceID {
				continue
			}
			filtered = append(filtered, a)
		}
		list = filtered
	}

	jsonResponse(w, list)
}

func (h *ApprovalHandler) Approve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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

	expiresAt := fmt.Sprintf("datetime('now', '+%d minutes')", req.DurationMinutes)
	if err := h.approvals.Approve(id, expiresAt); err != nil {
		jsonError(w, "failed to approve", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "approved"})
}

func (h *ApprovalHandler) Reject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.approvals.Reject(id); err != nil {
		jsonError(w, "failed to reject", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "rejected"})
}

// CreatePendingApproval creates an approval request for a placeholder key.
// Called by the proxy handler when approval is needed.
func CreatePendingApproval(approvals *queries.ApprovalQueries, placeholderID, method, path string) (string, error) {
	id, _ := svc.GenerateToken(16)
	requestInfo, _ := json.Marshal(map[string]string{
		"method": method,
		"path":   path,
	})

	approval := &models.Approval{
		ID:            id,
		PlaceholderID: placeholderID,
		Status:        "pending",
		RequestInfo:   strPtr(string(requestInfo)),
	}

	if err := approvals.Create(approval); err != nil {
		return "", err
	}
	return id, nil
}

func strPtr(s string) *string {
	return &s
}
