package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// AuditHandler serves the read-only audit log API.
type AuditHandler struct{ q *queries.AuditQueries }

func NewAuditHandler(q *queries.AuditQueries) *AuditHandler { return &AuditHandler{q: q} }

// parseAuditSince accepts RFC 3339, "2006-01-02 15:04:05" (UTC) or a date,
// and returns the stored UTC form.
func parseAuditSince(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return queries.UTCNow(t), true
		}
	}
	return "", false
}

// List — GET /api/audit. Filters: actor, kind, since, q, resource_type,
// resource_id; cursor paging with limit (default 50, max 200) and cursor
// (the previous response's next_cursor).
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	f := queries.AuditFilter{
		Actor: strings.TrimSpace(qs.Get("actor")), Kind: qs.Get("kind"), Query: strings.TrimSpace(qs.Get("q")),
		ResourceType: qs.Get("resource_type"), ResourceID: qs.Get("resource_id"),
	}
	if f.Kind != "" {
		if _, ok := queries.AuditKinds[f.Kind]; !ok && f.Kind != "approvals" {
			jsonError(w, "unknown kind", http.StatusBadRequest)
			return
		}
	}
	since, ok := parseAuditSince(qs.Get("since"))
	if !ok {
		jsonError(w, "since must be an RFC 3339 time or a date", http.StatusBadRequest)
		return
	}
	f.Since = since
	if c := qs.Get("cursor"); c != "" {
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil || n < 1 {
			jsonError(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		f.BeforeID = n
	}
	if l := qs.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			jsonError(w, "invalid limit", http.StatusBadRequest)
			return
		}
		f.Limit = n
	}
	rows, next, err := h.q.List(f)
	if err != nil {
		jsonError(w, "audit query failed", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []queries.AuditRow{}
	}
	var cursor interface{}
	if next > 0 {
		cursor = strconv.FormatInt(next, 10)
	}
	jsonResponse(w, map[string]interface{}{"entries": rows, "next_cursor": cursor})
}

// Get — GET /api/audit/{id}: one entry with its redacted before/after JSON.
func (h *AuditHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	row, err := h.q.Get(id)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	raw := func(s string) json.RawMessage {
		if s == "" {
			return nil
		}
		return json.RawMessage(s)
	}
	jsonResponse(w, map[string]interface{}{
		"id": row.ID, "created_at": row.CreatedAt, "actor": row.Actor, "via": row.Via, "remote_addr": row.RemoteAddr,
		"action": row.Action, "resource_type": row.ResourceType, "resource_id": row.ResourceID,
		"resource_name": row.ResourceName, "summary": row.Summary, "status": row.Status,
		"before": raw(row.Before), "after": raw(row.After),
	})
}
