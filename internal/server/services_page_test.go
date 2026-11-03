package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func svcDo(s *Server, shared *SharedServices, method, path, body string, authed bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authed {
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func svcExec(t *testing.T, s *Server, q string, args ...interface{}) {
	t.Helper()
	if _, err := s.db.Exec(q, args...); err != nil {
		t.Fatalf("seed %q: %v", q, err)
	}
}

func TestServicesPageHasNoInlineHandlersOrUnsafeMarkup(t *testing.T) {
	s, shared := newClientDetailServer(t)
	rec := svcDo(s, shared, http.MethodGet, "/admin/services", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	checkPage(t, rec)
	body := rec.Body.String()
	for _, bad := range []string{"style=\"", " onclick=", " onsubmit=", "confirm(", "alert(", "prompt(", "hx-confirm", "location.reload"} {
		if strings.Contains(body, bad) {
			t.Errorf("services page contains %q", bad)
		}
	}
	if rec := svcDo(s, shared, http.MethodGet, "/admin/services", "", false); rec.Code == http.StatusOK {
		t.Fatal("anonymous services page must not render")
	}
}

func TestServiceImpactAndStatsEndpoints(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c-a", "client a", "tok-a")
	seedDetailClient(t, s, "c-b", "client b", "tok-b")
	svcExec(t, s, `INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-x', 'xsvc', 'X', 'https://x.example', 'x.example')`)
	svcExec(t, s, `INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-y', 'ysvc', 'Y', 'https://y.example', 'y.example')`)
	svcExec(t, s, `INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k1', 'svc-x', 'static one', 'SECRET-ENC')`)
	svcExec(t, s, `INSERT INTO api_keys (id, service_id, name, key_encrypted, refresh_token) VALUES ('k2', 'svc-x', 'refresh one', 'SECRET-ENC2', 'rt')`)
	svcExec(t, s, `INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k3', 'svc-y', 'other', 'x')`)
	svcExec(t, s, `INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('p1', 'X_KEY', 'PHANTOM-SECRET-1', 'svc-x', 'k1', 'c-a')`)
	svcExec(t, s, `INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('p2', 'X_KEY', 'PHANTOM-SECRET-2', 'svc-x', 'k2', 'c-b')`)
	svcExec(t, s, `INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('p3', 'Y_KEY', 'PHANTOM-SECRET-3', 'svc-y', 'k3', 'c-b')`)
	svcExec(t, s, `INSERT INTO model_pricing (id, service_id, model, version, effective_from) VALUES ('mp1', 'svc-x', 'm', 'v1', '2026-01-01T00:00:00Z')`)
	svcExec(t, s, `INSERT INTO api_key_groups (id, service_id, name) VALUES ('g1', 'svc-x', 'pool')`)
	svcExec(t, s, `INSERT INTO key_groups (id, name, service_name) VALUES ('kg1', 'lease', 'xsvc')`)

	for _, p := range []string{"/api/services/svc-x/impact", "/api/services/stats"} {
		if rec := svcDo(s, shared, http.MethodGet, p, "", false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s status=%d, want 401", p, rec.Code)
		}
	}
	if rec := svcDo(s, shared, http.MethodGet, "/api/services/nope/impact", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown impact status=%d, want 404", rec.Code)
	}
	rec := svcDo(s, shared, http.MethodGet, "/api/services/svc-x/impact", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("impact status=%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("impact leaks secrets: %s", rec.Body.String())
	}
	var im struct {
		Credentials []struct{ Name, Type string } `json:"credentials"`
		Phantom     int                           `json:"phantom_tokens"`
		Clients     int                           `json:"clients"`
		Pools       int                           `json:"pools"`
		Pricing     int                           `json:"pricing_versions"`
		Lease       int                           `json:"lease_pools_by_name"`
		CanDelete   bool                          `json:"can_delete"`
		Tokens      []struct {
			ClientName string `json:"client_name"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &im); err != nil {
		t.Fatal(err)
	}
	if len(im.Credentials) != 2 || im.Phantom != 2 || im.Clients != 2 || im.Pools != 1 || im.Pricing != 1 || im.Lease != 1 || !im.CanDelete || len(im.Tokens) != 2 {
		t.Fatalf("unexpected impact: %s", rec.Body.String())
	}
	types := map[string]string{}
	for _, c := range im.Credentials {
		types[c.Name] = c.Type
	}
	if types["static one"] != "static" || types["refresh one"] != "refreshable" {
		t.Fatalf("credential types: %v", types)
	}

	rec = svcDo(s, shared, http.MethodGet, "/api/services/stats", "", true)
	var st map[string]struct {
		Credentials   int `json:"credentials"`
		PhantomTokens int `json:"phantom_tokens"`
		Pricing       int `json:"pricing_versions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["svc-x"].Credentials != 2 || st["svc-x"].PhantomTokens != 2 || st["svc-x"].Pricing != 1 || st["svc-y"].Credentials != 1 || st["svc-y"].PhantomTokens != 1 {
		t.Fatalf("unexpected stats: %s", rec.Body.String())
	}

	// Legacy OAuth rows block deletion; the API reports 409 instead of 500.
	svcExec(t, s, `INSERT INTO oauth_credentials (id, service_id, name, access_token, refresh_token) VALUES ('o1', 'svc-y', 'legacy', 'a', 'r')`)
	rec = svcDo(s, shared, http.MethodGet, "/api/services/svc-y/impact", "", true)
	if !strings.Contains(rec.Body.String(), `"can_delete":false`) {
		t.Fatalf("expected can_delete=false: %s", rec.Body.String())
	}
	if rec := svcDo(s, shared, http.MethodDelete, "/api/services/svc-y", "", true); rec.Code != http.StatusConflict {
		t.Fatalf("delete blocked status=%d, want 409", rec.Code)
	}
	if rec := svcDo(s, shared, http.MethodDelete, "/api/services/svc-x", "", true); rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d", rec.Code)
	}
}

func TestServicePricingEditAndDelete(t *testing.T) {
	s, shared := newClientDetailServer(t)
	svcExec(t, s, `INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-p', 'psvc', 'P', 'https://p.example', 'p.example')`)
	rec := svcDo(s, shared, http.MethodPost, "/api/services/svc-p/pricing", `{"model":"m1","version":"v1","input_usd_micros_per_mtok":1000000}`, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rec.Code, rec.Body.String())
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	path := fmt.Sprintf("/api/services/svc-p/pricing/%s", created.ID)
	if rec := svcDo(s, shared, http.MethodPut, path, `{"input_usd_micros_per_mtok":2000000}`, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon put=%d", rec.Code)
	}
	if rec := svcDo(s, shared, http.MethodPut, path, `{"input_usd_micros_per_mtok":-1}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative put=%d", rec.Code)
	}
	rec = svcDo(s, shared, http.MethodPut, path, `{"input_usd_micros_per_mtok":2000000,"model":"m2"}`, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"input_usd_micros_per_mtok":2000000`) || !strings.Contains(rec.Body.String(), `"model":"m2"`) {
		t.Fatalf("put=%d %s", rec.Code, rec.Body.String())
	}
	if rec := svcDo(s, shared, http.MethodPut, "/api/services/svc-other/pricing/"+created.ID, `{}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-service put=%d, want 404", rec.Code)
	}
	if rec := svcDo(s, shared, http.MethodDelete, path, "", true); rec.Code != http.StatusOK {
		t.Fatalf("delete=%d", rec.Code)
	}
	if rec := svcDo(s, shared, http.MethodDelete, path, "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete=%d, want 404", rec.Code)
	}
}

func TestApprovalsListClientFilterAppliesBeforeLimit(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c-busy", "busy", "tok-busy")
	seedDetailClient(t, s, "c-quiet", "quiet", "tok-quiet")
	svcExec(t, s, `INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-q', 'qsvc', 'Q', 'https://q.example', 'q.example')`)
	svcExec(t, s, `INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('kq', 'svc-q', 'kq', 'x')`)
	svcExec(t, s, `INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('pb', 'B', 'ph-b', 'svc-q', 'kq', 'c-busy')`)
	svcExec(t, s, `INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('pq', 'Q', 'ph-q', 'svc-q', 'kq', 'c-quiet')`)
	// Quiet client's approvals are old; 520 newer ones belong to the busy client.
	for i := 0; i < 3; i++ {
		svcExec(t, s, `INSERT INTO approvals (id, placeholder_id, status, created_at) VALUES (?, 'pq', 'approved', ?)`,
			fmt.Sprintf("aq%d", i), fmt.Sprintf("2026-01-01 00:00:0%d", i))
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 520; i++ {
		if _, err := tx.Exec(`INSERT INTO approvals (id, placeholder_id, status, created_at) VALUES (?, 'pb', 'approved', ?)`,
			fmt.Sprintf("ab%04d", i), fmt.Sprintf("2026-06-01 00:%02d:%02d", i/60, i%60)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rec := svcDo(s, shared, http.MethodGet, "/api/approvals/list?client_id=c-quiet&status=approved&limit=5", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var rows []struct {
		ID        string `json:"id"`
		ClientID  string `json:"client_id"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("quiet client rows=%d, want 3: %s", len(rows), rec.Body.String())
	}
	if rows[0].ID != "aq2" || rows[2].ID != "aq0" {
		t.Fatalf("not newest first: %+v", rows)
	}
	// limit applies to the filtered set, newest first.
	rec = svcDo(s, shared, http.MethodGet, "/api/approvals/list?client_id=c-busy&status=approved&limit=2", "", true)
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	if len(rows) != 2 || rows[0].ID != "ab0519" {
		t.Fatalf("busy rows: %+v", rows)
	}
}

func TestToggleCanaryRejectsDraftAndUnknown(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c-real", "real", "tok-real")
	seedDetailClient(t, s, "c-draft", "draft", "tok-draft")
	svcExec(t, s, `INSERT INTO client_drafts (client_id) VALUES ('c-draft')`)
	if rec := svcDo(s, shared, http.MethodPost, "/api/clients/c-draft/canary", `{"enabled":false}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("draft canary=%d, want 404", rec.Code)
	}
	if rec := svcDo(s, shared, http.MethodPost, "/api/clients/nope/canary", `{"enabled":false}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown canary=%d, want 404", rec.Code)
	}
	var enabled int
	_ = s.db.QueryRow(`SELECT canary_enabled FROM clients WHERE id='c-draft'`).Scan(&enabled)
	if enabled != 1 {
		t.Fatal("draft canary flag was modified")
	}
	if rec := svcDo(s, shared, http.MethodPost, "/api/clients/c-real/canary", `{"enabled":false}`, true); rec.Code != http.StatusOK {
		t.Fatalf("real canary=%d", rec.Code)
	}
}
