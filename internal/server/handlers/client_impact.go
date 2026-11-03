package handlers

import (
	"database/sql"
	"net/http"
)

// ClientImpactHandler serves GET /api/clients/{id}/impact: counts of what a
// rotate, disable or delete of the client touches, so the console can show an
// impact dialog before the admin confirms. Names and counts only, no secrets.
type ClientImpactHandler struct {
	db *sql.DB
}

func NewClientImpactHandler(db *sql.DB) *ClientImpactHandler {
	return &ClientImpactHandler{db: db}
}

type clientImpactChannel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AgentType string `json:"agent_type"`
	IsActive  bool   `json:"is_active"`
}

type clientImpactBundle struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
}

// ClientImpact is the response body of the impact endpoint.
type ClientImpact struct {
	ClientID         string               `json:"client_id"`
	Name             string               `json:"name"`
	IsActive         bool                 `json:"is_active"`
	PhantomTokens    int                  `json:"phantom_tokens"`
	BundleTokens     int                  `json:"bundle_tokens"`
	ManualTokens     int                  `json:"manual_tokens"`
	PendingApprovals int                  `json:"pending_approvals"`
	CanaryTokens     int                  `json:"canary_tokens"`
	Bundles          []clientImpactBundle `json:"bundles"`
	ControlChannel   *clientImpactChannel `json:"control_channel"`
}

func (h *ClientImpactHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	out := ClientImpact{ClientID: id, Bundles: []clientImpactBundle{}}

	err := h.db.QueryRow("SELECT name, is_active FROM clients WHERE id = ? AND id NOT IN (SELECT client_id FROM client_drafts)", id).Scan(&out.Name, &out.IsActive)
	if err == sql.ErrNoRows {
		jsonError(w, "client not found", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "failed to load client impact", http.StatusInternalServerError)
		return
	}

	if err := h.db.QueryRow("SELECT COUNT(*) FROM placeholder_keys WHERE client_id = ?", id).Scan(&out.PhantomTokens); err != nil {
		jsonError(w, "failed to load client impact", http.StatusInternalServerError)
		return
	}
	if err := h.db.QueryRow("SELECT COUNT(*) FROM placeholder_keys WHERE client_id = ? AND suite_id IS NOT NULL", id).Scan(&out.BundleTokens); err != nil {
		jsonError(w, "failed to load client impact", http.StatusInternalServerError)
		return
	}
	out.ManualTokens = out.PhantomTokens - out.BundleTokens
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM approvals a JOIN placeholder_keys p ON p.id = a.placeholder_id
		WHERE p.client_id = ? AND a.status = 'pending'`, id).Scan(&out.PendingApprovals); err != nil {
		jsonError(w, "failed to load client impact", http.StatusInternalServerError)
		return
	}
	if err := h.db.QueryRow("SELECT COUNT(*) FROM canary_tokens WHERE client_id = ?", id).Scan(&out.CanaryTokens); err != nil {
		jsonError(w, "failed to load client impact", http.StatusInternalServerError)
		return
	}

	rows, err := h.db.Query(`SELECT s.id, s.name, COUNT(p.id) FROM key_suite_assignments a
		JOIN key_suites s ON s.id = a.suite_id
		LEFT JOIN placeholder_keys p ON p.suite_id = s.id AND p.client_id = a.client_id
		WHERE a.client_id = ? GROUP BY s.id, s.name ORDER BY s.name`, id)
	if err != nil {
		jsonError(w, "failed to load client impact", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var b clientImpactBundle
		if err := rows.Scan(&b.ID, &b.Name, &b.Tokens); err != nil {
			jsonError(w, "failed to load client impact", http.StatusInternalServerError)
			return
		}
		out.Bundles = append(out.Bundles, b)
	}
	if err := rows.Err(); err != nil {
		jsonError(w, "failed to load client impact", http.StatusInternalServerError)
		return
	}

	var ch clientImpactChannel
	err = h.db.QueryRow(`SELECT id, name, agent_type, is_active
		FROM control_channels WHERE client_id = ?`, id).Scan(&ch.ID, &ch.Name, &ch.AgentType, &ch.IsActive)
	switch err {
	case nil:
		out.ControlChannel = &ch
	case sql.ErrNoRows:
	default:
		jsonError(w, "failed to load client impact", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, out)
}
