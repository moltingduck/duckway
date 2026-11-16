package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

// ApproverAPIHandler serves /approver/: the approvals inbox for an approver
// token (ducklord). It exposes approval fields only: never credentials, ACL
// JSON, request logs or the phantom token's name.
type ApproverAPIHandler struct {
	approvals *queries.ApprovalQueries
	decider   *svc.ApprovalDecider
}

func NewApproverAPIHandler(a *queries.ApprovalQueries, d *svc.ApprovalDecider) *ApproverAPIHandler {
	return &ApproverAPIHandler{approvals: a, decider: d}
}

// ApproverApproval is the approver API's view of an approval.
type ApproverApproval struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	ClientName string  `json:"client_name"`
	Service    string  `json:"service"`
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	Query      string  `json:"query,omitempty"` // already redacted when the approval was created
	BodyHash   string  `json:"body_hash,omitempty"`
	Rule       string  `json:"rule,omitempty"`
	HeldReason string  `json:"held_reason"`
	CreatedAt  string  `json:"created_at"`
	ApprovedAt *string `json:"approved_at"`
	ExpiresAt  *string `json:"expires_at"`
	// DefaultMinutes is how long "approve" lasts without an explicit duration.
	DefaultMinutes int    `json:"default_minutes"`
	DecidedBy      string `json:"decided_by,omitempty"`
	DecidedVia     string `json:"decided_via,omitempty"`
}

func approverView(a queries.ApprovalListItem) ApproverApproval {
	var info struct {
		Method   string `json:"method"`
		Path     string `json:"path"`
		Query    string `json:"query"`
		BodyHash string `json:"body_hash"`
		Rule     string `json:"rule"`
	}
	if a.RequestInfo != nil {
		_ = json.Unmarshal([]byte(*a.RequestInfo), &info)
	}
	reason := "The phantom token requires approval before requests are forwarded."
	if info.Rule != "" {
		reason = "An approve rule matched this request: " + info.Rule
	}
	return ApproverApproval{
		ID: a.ID, Status: a.Status, ClientName: a.ClientName, Service: a.ServiceName,
		Method: info.Method, Path: info.Path, Query: info.Query, BodyHash: info.BodyHash, Rule: info.Rule,
		HeldReason: reason, CreatedAt: a.CreatedAt, ApprovedAt: a.ApprovedAt, ExpiresAt: a.ExpiresAt,
		DefaultMinutes: a.ApprovalTTLMinutes, DecidedBy: a.DecidedBy, DecidedVia: a.DecidedVia,
	}
}

// GET /approver/approvals?status=pending[,approved,...]&limit=N
func (h *ApproverAPIHandler) List(w http.ResponseWriter, r *http.Request) {
	statuses := []string{}
	raw := r.URL.Query().Get("status")
	if raw == "" {
		raw = "pending"
	}
	for _, s := range strings.Split(raw, ",") {
		switch s = strings.TrimSpace(s); s {
		case "pending", "approved", "rejected", "ignored":
			statuses = append(statuses, s)
		case "":
		default:
			jsonError(w, "status must be pending, approved, rejected or ignored", http.StatusBadRequest)
			return
		}
	}
	limit := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}
	items, err := h.approvals.ListEnrichedFiltered(statuses, "", "", limit)
	if err != nil {
		jsonError(w, "failed to list approvals", http.StatusInternalServerError)
		return
	}
	out := make([]ApproverApproval, 0, len(items))
	for _, it := range items {
		out = append(out, approverView(it))
	}
	jsonResponse(w, out)
}

// GET /approver/approvals/{id}
func (h *ApproverAPIHandler) Get(w http.ResponseWriter, r *http.Request) {
	it, err := h.approvals.GetEnriched(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		jsonError(w, "approval not found", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "failed to load approval", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, approverView(*it))
}

// POST /approver/approvals/{id}/approve {duration_minutes?}
func (h *ApproverAPIHandler) Approve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DurationMinutes int `json:"duration_minutes"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) // an empty body means the default
	}
	id := r.PathValue("id")
	err := h.decider.Approve(r.Context(), id, req.DurationMinutes, approverActor(r), svc.ViaTUI)
	respondDecision(w, h.approvals, id, err)
}

// POST /approver/approvals/{id}/reject
func (h *ApproverAPIHandler) Reject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := h.decider.Reject(r.Context(), id, approverActor(r), svc.ViaTUI)
	respondDecision(w, h.approvals, id, err)
}

func approverActor(r *http.Request) string {
	if t := middleware.GetApprover(r); t != nil {
		return "approver:" + t.Name
	}
	return "approver:unknown"
}

// respondDecision writes the result of a decision attempt: 200 with the new
// decision, 404 for an unknown ID, 409 with the existing decider when it was
// already decided.
func respondDecision(w http.ResponseWriter, approvals *queries.ApprovalQueries, id string, err error) {
	switch {
	case err == nil:
		d, _ := approvals.GetDecision(id)
		jsonResponse(w, map[string]string{"id": id, "status": d.Status, "decided_by": d.DecidedBy, "decided_via": d.DecidedVia})
	case errors.Is(err, svc.ErrApprovalNotFound):
		jsonError(w, "approval not found", http.StatusNotFound)
	case errors.Is(err, queries.ErrApprovalNotPending):
		body := map[string]string{"error": "approval already decided"}
		var ad *svc.AlreadyDecidedError
		if errors.As(err, &ad) {
			body["status"], body["decided_by"], body["decided_via"] = ad.Decision.Status, ad.Decision.DecidedBy, ad.Decision.DecidedVia
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(body)
	default:
		jsonError(w, "failed to record the decision", http.StatusInternalServerError)
	}
}
