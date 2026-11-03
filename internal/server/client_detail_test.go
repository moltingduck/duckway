package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/server/services"
)

func newClientDetailServer(t *testing.T) (*Server, *SharedServices) {
	t.Helper()
	s, shared := newShellTestServer(t)
	s.SetupGatewayRoutes(shared)
	return s, shared
}

func seedDetailClient(t *testing.T, s *Server, id, name, token string) {
	t.Helper()
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO clients (id, short_id, name, token_hash, is_active) VALUES (?, ?, ?, ?, 1)`, id, "sh"+id, name, services.HashToken(token))
}

func adminGet(s *Server, shared *SharedServices, path string, authed bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authed {
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	}
	rec := httptest.NewRecorder()
	testSameOrigin(req)
	s.ServeHTTP(rec, req)
	return rec
}

func TestClientDetailPageAuthShellAndNotFound(t *testing.T) {
	s, shared := newClientDetailServer(t)
	hostile := `<img src=x onerror=alert(1)>"'`
	seedDetailClient(t, s, "c-detail", hostile, "tok-detail")

	// Anonymous requests never reach the page.
	if rec := adminGet(s, shared, "/admin/clients/c-detail", false); rec.Code == http.StatusOK {
		t.Fatalf("anonymous detail page status=%d, want redirect or 401", rec.Code)
	}

	rec := adminGet(s, shared, "/admin/clients/c-detail", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%.200s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "</html>") || !strings.Contains(body, `src="/static/shell.js"`) {
		t.Fatal("detail page is not a complete console shell page")
	}
	if strings.Contains(body, "<img src=x") {
		t.Fatal("client name rendered unescaped")
	}
	if !strings.Contains(body, `data-client-id="c-detail"`) {
		t.Fatal("client id attribute missing")
	}
	checkPage(t, rec)

	// Unknown ids render the shell with a 404 notice, not a bare error.
	rec = adminGet(s, shared, "/admin/clients/does-not-exist", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown client status=%d, want 404", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, "Client not found") || !strings.Contains(body, `src="/static/shell.js"`) || !strings.Contains(body, `href="/admin/clients"`) {
		t.Fatalf("404 page must render inside the shell with a way back: %.300s", body)
	}
	checkPage(t, rec)
	if rec := adminGet(s, shared, "/admin/clients/does-not-exist", false); rec.Code == http.StatusNotFound {
		t.Fatal("anonymous request must not learn whether a client id exists")
	}
}

func TestClientImpactEndpoint(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c-impact", "impact box", "tok-impact")
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-i', 'impact-svc', 'Impact', 'https://i.example', 'i.example')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k-i', 'svc-i', 'key', 'x')`)
	exec(`INSERT INTO key_suites (id, name) VALUES ('suite-i', 'default-dev')`)
	exec(`INSERT INTO key_suite_assignments (suite_id, client_id) VALUES ('suite-i', 'c-impact')`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, suite_id) VALUES ('p1', 'A_KEY', 'ph-a', 'svc-i', 'k-i', 'c-impact', 'suite-i')`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('p2', 'B_KEY', 'ph-b', 'svc-i', 'k-i', 'c-impact')`)
	exec(`INSERT INTO approvals (id, placeholder_id, status) VALUES ('ap1', 'p2', 'pending')`)
	exec(`INSERT INTO control_channels (id, name, service_id, api_key_id, client_id, agent_type) VALUES ('cc1', 'my-cc', 'svc-i', 'k-i', 'c-impact', 'codex')`)
	exec(`INSERT INTO canary_tokens (id, client_id, token_type, canary_token, auth_token, token_value, memo, deploy_path, deploy_content) VALUES ('ct1', 'c-impact', 'aws', 'a', 'b', 'c', 'm', 'p', 'd')`)

	if rec := adminGet(s, shared, "/api/clients/c-impact/impact", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous impact status=%d, want 401", rec.Code)
	}
	if rec := adminGet(s, shared, "/api/clients/missing/impact", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown impact status=%d, want 404", rec.Code)
	}
	rec := adminGet(s, shared, "/api/clients/c-impact/impact", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("impact status=%d body=%s", rec.Code, rec.Body.String())
	}
	var im struct {
		Name             string `json:"name"`
		IsActive         bool   `json:"is_active"`
		PhantomTokens    int    `json:"phantom_tokens"`
		BundleTokens     int    `json:"bundle_tokens"`
		ManualTokens     int    `json:"manual_tokens"`
		PendingApprovals int    `json:"pending_approvals"`
		CanaryTokens     int    `json:"canary_tokens"`
		Bundles          []struct {
			Name   string `json:"name"`
			Tokens int    `json:"tokens"`
		} `json:"bundles"`
		ControlChannel *struct {
			Name      string `json:"name"`
			AgentType string `json:"agent_type"`
		} `json:"control_channel"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &im); err != nil {
		t.Fatal(err)
	}
	if im.Name != "impact box" || !im.IsActive || im.PhantomTokens != 2 || im.BundleTokens != 1 || im.ManualTokens != 1 ||
		im.PendingApprovals != 1 || im.CanaryTokens != 1 || len(im.Bundles) != 1 || im.Bundles[0].Name != "default-dev" || im.Bundles[0].Tokens != 1 ||
		im.ControlChannel == nil || im.ControlChannel.Name != "my-cc" || im.ControlChannel.AgentType != "codex" {
		t.Fatalf("unexpected impact: %+v (%s)", im, rec.Body.String())
	}

	// A client with nothing attached reports zero counts and no channel.
	seedDetailClient(t, s, "c-empty", "empty box", "tok-empty")
	rec = adminGet(s, shared, "/api/clients/c-empty/impact", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"control_channel":null`) || !strings.Contains(rec.Body.String(), `"bundles":[]`) {
		t.Fatalf("empty impact status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestDisabledClientIsRejectedAndReenableRestores covers the Disable client
// action end to end: the admin API flips is_active and every client-token
// authenticated gateway route then rejects the client until it is re-enabled.
func TestDisabledClientIsRejectedAndReenableRestores(t *testing.T) {
	s, shared := newClientDetailServer(t)
	const token = "tok-disable"
	seedDetailClient(t, s, "c-disable", "disable me", token)

	clientCall := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("X-Duckway-Token", token)
		rec := httptest.NewRecorder()
		testSameOrigin(req)
		s.ServeHTTP(rec, req)
		return rec.Code
	}
	setActive := func(active bool) {
		t.Helper()
		body := `{"is_active":false}`
		if active {
			body = `{"is_active":true}`
		}
		req := httptest.NewRequest(http.MethodPut, "/api/clients/c-disable", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
		rec := httptest.NewRecorder()
		testSameOrigin(req)
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT is_active=%v status=%d body=%s", active, rec.Code, rec.Body.String())
		}
	}
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/client/keys", ""},
		{http.MethodGet, "/client/sync", ""},
		{http.MethodGet, "/client/plugins", ""},
		{http.MethodPost, "/client/control/heartbeat", `{}`},
	}
	for _, r := range routes {
		if code := clientCall(r.method, r.path, r.body); code == http.StatusUnauthorized {
			t.Fatalf("active client rejected on %s %s", r.method, r.path)
		}
	}

	setActive(false)
	for _, r := range routes {
		if code := clientCall(r.method, r.path, r.body); code != http.StatusUnauthorized {
			t.Fatalf("disabled client got %d on %s %s, want 401", code, r.method, r.path)
		}
	}
	// Disabling keeps the stored token: the same token works again once re-enabled.
	setActive(true)
	for _, r := range routes {
		if code := clientCall(r.method, r.path, r.body); code == http.StatusUnauthorized {
			t.Fatalf("re-enabled client rejected on %s %s", r.method, r.path)
		}
	}
}
