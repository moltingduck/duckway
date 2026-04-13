package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

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
