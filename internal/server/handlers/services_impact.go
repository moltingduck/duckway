package handlers

import (
	"database/sql"
	"net/http"
)

// ServiceImpactHandler serves admin-only read endpoints for the Services page:
// GET /api/services/{id}/impact lists what deleting a service removes, and
// GET /api/services/stats gives per-service counts for the list. Names and
// counts only; no secrets or placeholder values.
type ServiceImpactHandler struct {
	db *sql.DB
}

func NewServiceImpactHandler(db *sql.DB) *ServiceImpactHandler {
	return &ServiceImpactHandler{db: db}
}

type serviceImpactCredential struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // static or refreshable
	IsActive bool   `json:"is_active"`
}

type serviceImpactToken struct {
	ID         string `json:"id"`
	EnvName    string `json:"env_name"`
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`
}

// ServiceImpact is the response body of the impact endpoint.
type ServiceImpact struct {
	ServiceID        string                    `json:"service_id"`
	Name             string                    `json:"name"`
	Credentials      []serviceImpactCredential `json:"credentials"`
	PhantomTokens    int                       `json:"phantom_tokens"`
	Tokens           []serviceImpactToken      `json:"tokens"` // first 50
	Clients          int                       `json:"clients"`
	Pools            int                       `json:"pools"`
	BundleEntries    int                       `json:"bundle_entries"`
	ControlChannels  int                       `json:"control_channels"`
	PricingVersions  int                       `json:"pricing_versions"`
	OAuthCredentials int                       `json:"oauth_credentials"`
	LeasePoolsByName int                       `json:"lease_pools_by_name"`
	CanDelete        bool                      `json:"can_delete"`
	BlockedReason    string                    `json:"blocked_reason,omitempty"`
}

func (h *ServiceImpactHandler) Impact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	out := ServiceImpact{ServiceID: id, Credentials: []serviceImpactCredential{}, Tokens: []serviceImpactToken{}}
	fail := func() { jsonError(w, "failed to load service impact", http.StatusInternalServerError) }

	if err := h.db.QueryRow("SELECT name FROM services WHERE id = ?", id).Scan(&out.Name); err == sql.ErrNoRows {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	} else if err != nil {
		fail()
		return
	}

	rows, err := h.db.Query(`SELECT id, name, CASE WHEN refresh_token != '' THEN 'refreshable' ELSE 'static' END, is_active
		FROM api_keys WHERE service_id = ? ORDER BY name`, id)
	if err != nil {
		fail()
		return
	}
	for rows.Next() {
		var c serviceImpactCredential
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.IsActive); err != nil {
			rows.Close()
			fail()
			return
		}
		out.Credentials = append(out.Credentials, c)
	}
	rows.Close()
	if rows.Err() != nil {
		fail()
		return
	}

	trows, err := h.db.Query(`SELECT p.id, p.env_name, p.client_id, COALESCE(c.name, '')
		FROM placeholder_keys p LEFT JOIN clients c ON c.id = p.client_id
		WHERE p.service_id = ? ORDER BY c.name, p.env_name LIMIT 50`, id)
	if err != nil {
		fail()
		return
	}
	for trows.Next() {
		var t serviceImpactToken
		if err := trows.Scan(&t.ID, &t.EnvName, &t.ClientID, &t.ClientName); err != nil {
			trows.Close()
			fail()
			return
		}
		out.Tokens = append(out.Tokens, t)
	}
	trows.Close()

	counts := []struct {
		dst *int
		q   string
		arg string
	}{
		{&out.PhantomTokens, "SELECT COUNT(*) FROM placeholder_keys WHERE service_id = ?", id},
		{&out.Clients, "SELECT COUNT(DISTINCT client_id) FROM placeholder_keys WHERE service_id = ?", id},
		{&out.Pools, "SELECT COUNT(*) FROM api_key_groups WHERE service_id = ?", id},
		{&out.BundleEntries, "SELECT COUNT(*) FROM key_suite_entries WHERE service_id = ?", id},
		{&out.ControlChannels, "SELECT COUNT(*) FROM control_channels WHERE service_id = ?", id},
		{&out.PricingVersions, "SELECT COUNT(*) FROM model_pricing WHERE service_id = ?", id},
		{&out.OAuthCredentials, "SELECT COUNT(*) FROM oauth_credentials WHERE service_id = ?", id},
		{&out.LeasePoolsByName, "SELECT COUNT(*) FROM key_groups WHERE service_name = ?", out.Name},
	}
	for _, c := range counts {
		if err := h.db.QueryRow(c.q, c.arg).Scan(c.dst); err != nil {
			fail()
			return
		}
	}
	out.CanDelete = true
	if out.OAuthCredentials > 0 {
		out.CanDelete = false
		out.BlockedReason = "Legacy OAuth credentials still reference this service. Remove them first."
	}
	jsonResponse(w, out)
}

type serviceStat struct {
	Credentials     int `json:"credentials"`
	PhantomTokens   int `json:"phantom_tokens"`
	PricingVersions int `json:"pricing_versions"`
}

// Stats returns {service_id: counts} for every service.
func (h *ServiceImpactHandler) Stats(w http.ResponseWriter, r *http.Request) {
	out := map[string]*serviceStat{}
	get := func(id string) *serviceStat {
		if s, ok := out[id]; ok {
			return s
		}
		s := &serviceStat{}
		out[id] = s
		return s
	}
	for _, q := range []struct {
		sql string
		set func(*serviceStat, int)
	}{
		{"SELECT service_id, COUNT(*) FROM api_keys GROUP BY service_id", func(s *serviceStat, n int) { s.Credentials = n }},
		{"SELECT service_id, COUNT(*) FROM placeholder_keys GROUP BY service_id", func(s *serviceStat, n int) { s.PhantomTokens = n }},
		{"SELECT service_id, COUNT(*) FROM model_pricing GROUP BY service_id", func(s *serviceStat, n int) { s.PricingVersions = n }},
	} {
		rows, err := h.db.Query(q.sql)
		if err != nil {
			jsonError(w, "failed to load service stats", http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				jsonError(w, "failed to load service stats", http.StatusInternalServerError)
				return
			}
			q.set(get(id), n)
		}
		rows.Close()
	}
	jsonResponse(w, out)
}
