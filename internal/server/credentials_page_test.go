package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/server/handlers"
)

const (
	secretStaticKey   = "sk-STATIC-SECRET-VALUE-0123456789abcdef"
	secretAccess      = "ACCESS-SECRET-TOKEN-0123456789xyz"
	secretRefresh     = "REFRESH-SECRET-TOKEN-0123456789xyz"
	secretIDToken     = "eyJID.TOKEN.SECRET-0123456789"
	secretClientSecr  = "CLIENT-SECRET-0123456789"
	secretPrivateKey  = "PRIVATE-KEY-MATERIAL-0123456789"
	secretGitHubToken = "ghs_MINTED_SECRET_0123456789"
)

func credAdminRequest(t *testing.T, s *Server, shared *SharedServices, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	rec := httptest.NewRecorder()
	testSameOrigin(req)
	s.ServeHTTP(rec, req)
	return rec
}

// seedCredentials stores one static, one refreshable (with provider metadata
// that carries secret-looking fields) and one GitHub App credential, all
// encrypted with the server's crypto exactly like production data.
func seedCredentials(t *testing.T, s *Server, shared *SharedServices) (staticID, refreshID, appID string) {
	t.Helper()
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	enc := func(v string) string {
		t.Helper()
		out, err := shared.Crypto.Encrypt(v)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-c', 'cred-svc', 'Cred Service', 'https://c.example', 'c.example')`)
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-gh', 'github', 'GitHub', 'https://api.github.com', 'api.github.com')`)
	sub := `{"subscriptionType":"max","account_id":"acct-1","id_token":"` + secretIDToken + `","client_secret":"` + secretClientSecr + `","nested":{"refresh_token":"` + secretRefresh + `","kept":"visible"}}`
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k-static', 'svc-c', 'static key', ?)`, enc(secretStaticKey))
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, refresh_token, token_endpoint, subscription_info, expires_at) VALUES ('k-refresh', 'svc-c', 'refreshable key', ?, ?, 'https://c.example/token', ?, 4102444800000)`, enc(secretAccess), enc(secretRefresh), sub)
	app := `{"type":"github_app","app_id":42,"installation_id":7,"private_key":"` + secretPrivateKey + `"}`
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k-app', 'svc-gh', 'github app', ?)`, enc(app))
	exec(`INSERT INTO clients (id, short_id, name, token_hash) VALUES ('c1', 'c1short', 'client one', 'h')`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('ph1', 'CRED_KEY', 'ph-1', 'svc-c', 'k-static', 'c1')`)
	return "k-static", "k-refresh", "k-app"
}

func assertNoSecrets(t *testing.T, label, body string) {
	t.Helper()
	for _, secret := range []string{secretStaticKey, secretAccess, secretRefresh, secretIDToken, secretClientSecr, secretPrivateKey, secretGitHubToken, "PRIVATE KEY"} {
		if strings.Contains(body, secret) {
			t.Errorf("%s leaked a stored secret %q: %.300s", label, secret, body)
		}
	}
	for _, field := range []string{`"id_token"`, `"client_secret"`, `"refresh_token"`, `"private_key"`, `"key_encrypted"`} {
		if strings.Contains(body, field) {
			t.Errorf("%s exposes secret field %s: %.300s", label, field, body)
		}
	}
}

func TestCredentialAPIResponsesNeverContainStoredSecrets(t *testing.T) {
	s, shared := newShellTestServer(t)
	staticID, refreshID, appID := seedCredentials(t, s, shared)

	calls := []struct{ method, path, body string }{
		{http.MethodGet, "/api/keys", ""},
		{http.MethodGet, "/api/keys?service_id=svc-c", ""},
		{http.MethodGet, "/api/keys/" + staticID, ""},
		{http.MethodGet, "/api/keys/" + refreshID, ""},
		{http.MethodGet, "/api/keys/" + appID, ""},
		{http.MethodGet, "/api/oauth/" + refreshID, ""},
		{http.MethodGet, "/api/keys/" + staticID + "/acl-templates", ""},
		{http.MethodDelete, "/api/keys/" + staticID, `{"confirm":false}`},
		{http.MethodDelete, "/api/keys/" + refreshID, `{"confirm":false}`},
		{http.MethodDelete, "/api/keys/" + appID, `{"confirm":false}`},
		{http.MethodDelete, "/api/oauth/" + refreshID, `{"confirm":false}`},
		{http.MethodPut, "/api/keys/" + staticID, `{"name":"static key renamed"}`},
		{http.MethodGet, "/api/keys", ""},
	}
	for _, c := range calls {
		rec := credAdminRequest(t, s, shared, c.method, c.path, c.body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status %d body=%.200s", c.method, c.path, rec.Code, rec.Body.String())
			continue
		}
		assertNoSecrets(t, c.method+" "+c.path, rec.Body.String())
	}

	// Create responses (static, github app) and the oauth upload response must
	// not echo the submitted secret back either.
	rec := credAdminRequest(t, s, shared, http.MethodPost, "/api/keys", `{"service_id":"svc-c","name":"created","key":"`+secretStaticKey+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, "POST /api/keys", rec.Body.String())
	rec = credAdminRequest(t, s, shared, http.MethodPost, "/api/oauth/upload", `{"name":"up","service_id":"svc-c","access_token":"`+secretAccess+`","refresh_token":"`+secretRefresh+`","token_endpoint":"https://c.example/token","subscription_info":"{\"id_token\":\"`+secretIDToken+`\"}"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, "POST /api/oauth/upload", rec.Body.String())

	// Non-secret provider metadata is still shown.
	rec = credAdminRequest(t, s, shared, http.MethodGet, "/api/oauth/"+refreshID, "")
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	sub, _ := got["subscription_info"].(string)
	if !strings.Contains(sub, "subscriptionType") || !strings.Contains(sub, "visible") || !strings.Contains(sub, "acct-1") {
		t.Errorf("redaction dropped non-secret metadata: %s", sub)
	}
	if preview, _ := got["key_preview"].(string); preview == "" || strings.Contains(preview, secretAccess) {
		t.Errorf("oauth get must return only a masked preview, got %q", preview)
	}
}

func TestCredentialListIncludesUsedByCounts(t *testing.T) {
	s, shared := newShellTestServer(t)
	staticID, _, _ := seedCredentials(t, s, shared)
	rec := credAdminRequest(t, s, shared, http.MethodGet, "/api/keys", "")
	var list []struct {
		ID     string `json:"id"`
		UsedBy struct {
			Tokens int `json:"tokens"`
		} `json:"used_by"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range list {
		if k.ID == staticID {
			found = true
			if k.UsedBy.Tokens != 1 {
				t.Errorf("used_by.tokens = %d, want 1", k.UsedBy.Tokens)
			}
		}
	}
	if !found {
		t.Fatal("seeded key missing from list")
	}
}

func TestOAuthEditKeepsStoredSecretsOmittedFromRedactedJSON(t *testing.T) {
	s, shared := newShellTestServer(t)
	_, refreshID, _ := seedCredentials(t, s, shared)
	rec := credAdminRequest(t, s, shared, http.MethodPut, "/api/oauth/"+refreshID, `{"name":"renamed","subscription_info":"{\"subscriptionType\":\"pro\"}"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var stored string
	if err := s.db.QueryRow(`SELECT subscription_info FROM api_keys WHERE id = ?`, refreshID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, secretIDToken) || !strings.Contains(stored, secretClientSecr) {
		t.Errorf("edit round-trip erased stored secrets: %s", stored)
	}
	if !strings.Contains(stored, `"pro"`) {
		t.Errorf("edit did not apply: %s", stored)
	}
	// An explicit new value wins over the stored one.
	rec = credAdminRequest(t, s, shared, http.MethodPut, "/api/oauth/"+refreshID, `{"subscription_info":"{\"id_token\":\"NEW-ID-TOKEN\"}"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update 2: %d %s", rec.Code, rec.Body.String())
	}
	if err := s.db.QueryRow(`SELECT subscription_info FROM api_keys WHERE id = ?`, refreshID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, "NEW-ID-TOKEN") || strings.Contains(stored, secretIDToken) {
		t.Errorf("explicit id_token not applied: %s", stored)
	}
}

func TestStaticKeyDeleteImpactPreviewAndCleanup(t *testing.T) {
	s, shared := newShellTestServer(t)
	staticID, _, _ := seedCredentials(t, s, shared)
	if _, err := s.db.Exec(`INSERT INTO key_groups (id, name, service_name) VALUES ('kg1', 'lease pool', 'cred-svc')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO key_group_members (group_id, api_key_id, position) VALUES ('kg1', ?, 0)`, staticID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO api_key_groups (id, service_id, name) VALUES ('ag1', 'svc-c', 'rotation pool')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO api_key_group_members (group_id, api_key_id) VALUES ('ag1', ?)`, staticID); err != nil {
		t.Fatal(err)
	}

	rec := credAdminRequest(t, s, shared, http.MethodDelete, "/api/keys/"+staticID, `{"confirm":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	var prev struct {
		RequiresConfirmation bool `json:"requires_confirmation"`
		Impact               struct {
			Kind    string        `json:"kind"`
			Clients []interface{} `json:"clients"`
			Pools   []struct {
				Name string `json:"name"`
				Mode string `json:"mode"`
			} `json:"pools"`
		} `json:"impact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &prev); err != nil {
		t.Fatal(err)
	}
	if !prev.RequiresConfirmation || prev.Impact.Kind != "static" || len(prev.Impact.Clients) != 1 || len(prev.Impact.Pools) != 2 {
		t.Fatalf("unexpected preview: %s", rec.Body.String())
	}
	modes := map[string]string{}
	for _, p := range prev.Impact.Pools {
		modes[p.Mode] = p.Name
	}
	if modes["lease"] != "lease pool" || modes["rotation"] != "rotation pool" {
		t.Fatalf("pool modes = %v", modes)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id = ?`, staticID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("preview deleted the key: n=%d err=%v", n, err)
	}

	// confirm:true without the previewed counts is refused and deletes nothing.
	rec = credAdminRequest(t, s, shared, http.MethodDelete, "/api/keys/"+staticID, `{"confirm":true}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "preview required") {
		t.Fatalf("confirm without expect: %d %s", rec.Code, rec.Body.String())
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id = ?`, staticID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("refused confirm deleted the key: n=%d err=%v", n, err)
	}
	rec = credAdminRequest(t, s, shared, http.MethodDelete, "/api/keys/"+staticID,
		fmt.Sprintf(`{"confirm":true,"expect":{"key_suites":0,"clients":%d,"control_channels":0,"pools":%d}}`, len(prev.Impact.Clients), len(prev.Impact.Pools)))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	for label, q := range map[string]string{
		"key":            `SELECT COUNT(*) FROM api_keys WHERE id = 'k-static'`,
		"phantom tokens": `SELECT COUNT(*) FROM placeholder_keys WHERE api_key_id = 'k-static'`,
		"lease members":  `SELECT COUNT(*) FROM key_group_members WHERE api_key_id = 'k-static'`,
		"rot members":    `SELECT COUNT(*) FROM api_key_group_members WHERE api_key_id = 'k-static'`,
	} {
		if err := s.db.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s remaining = %d err=%v", label, n, err)
		}
	}
}

func TestStaticKeyLegacyDeleteWithoutBodyStillWorks(t *testing.T) {
	s, shared := newShellTestServer(t)
	_, _, appID := seedCredentials(t, s, shared)
	rec := credAdminRequest(t, s, shared, http.MethodDelete, "/api/keys/"+appID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy delete: %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id = ?`, appID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("key not deleted: n=%d err=%v", n, err)
	}
}

func TestCredentialsPageAuthAndMovedEntries(t *testing.T) {
	s, shared := newShellTestServer(t)

	// Auth: anonymous requests never get the page.
	anon := httptest.NewRecorder()
	s.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "/admin/credentials", nil))
	if anon.Code == http.StatusOK {
		t.Fatalf("anonymous /admin/credentials returned 200")
	}
	rec := credAdminRequest(t, s, shared, http.MethodGet, "/admin/credentials?type=refreshable", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/admin/credentials status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<h1>Credentials</h1>", `id="cr-tablist"`, `href="/admin/credentials"`, `aria-current="page"`} {
		if !strings.Contains(body, want) {
			t.Errorf("credentials page missing %q", want)
		}
	}
	if strings.Contains(body, `href="/admin/keys"`) || strings.Contains(body, `href="/admin/oauth"`) {
		t.Error("nav still links to the removed pages")
	}
	checkPage(t, rec)

	want := map[string]struct{ path, name string }{
		"/admin/keys":  {"/admin/credentials", "Credentials"},
		"/admin/oauth": {"/admin/credentials?type=refreshable", "Credentials › Refreshable"},
	}
	for old, w := range want {
		m, ok := handlers.LookupMoved(old)
		if !ok || m.NewPath != w.path || m.NewName != w.name {
			t.Errorf("moved[%s] = %+v ok=%v, want %+v", old, m, ok, w)
		}
		rec := credAdminRequest(t, s, shared, http.MethodGet, old, "")
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Update your bookmark") {
			t.Errorf("%s: status %d, want the page-moved 404 notice", old, rec.Code)
		}
	}
}

// TestServicesAndPhantomEndpointsNeverContainStoredSecrets extends the secret
// audit to the services impact/stats and phantom token APIs and pages.
func TestServicesAndPhantomEndpointsNeverContainStoredSecrets(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedCredentials(t, s, shared)

	calls := []struct{ method, path, body string }{
		{http.MethodGet, "/api/services", ""},
		{http.MethodGet, "/api/services/stats", ""},
		{http.MethodGet, "/api/services/svc-c/impact", ""},
		{http.MethodGet, "/api/services/svc-gh/impact", ""},
		{http.MethodGet, "/api/services/svc-c/pricing", ""},
		{http.MethodGet, "/api/placeholders", ""},
		{http.MethodGet, "/api/placeholders/ph1", ""},
		{http.MethodPost, "/api/placeholders/ph1/acl/test", `{"method":"GET","path":"/v1/x"}`},
		{http.MethodGet, "/api/clients/c1/impact", ""},
		{http.MethodGet, "/admin/services", ""},
		{http.MethodGet, "/admin/credentials", ""},
		{http.MethodGet, "/admin/phantom-tokens", ""},
		{http.MethodGet, "/admin/phantom-tokens/ph1", ""},
		{http.MethodGet, "/admin/key-pools", ""},
		{http.MethodGet, "/admin/key-bundles", ""},
	}
	for _, c := range calls {
		rec := credAdminRequest(t, s, shared, c.method, c.path, c.body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status %d body=%.200s", c.method, c.path, rec.Code, rec.Body.String())
			continue
		}
		if strings.HasPrefix(c.path, "/admin/") {
			// Page sources legitimately mention field names; check values only.
			for _, secret := range []string{secretStaticKey, secretAccess, secretRefresh, secretIDToken, secretClientSecr, secretPrivateKey, secretGitHubToken} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("%s leaked stored secret %q", c.path, secret)
				}
			}
			continue
		}
		assertNoSecrets(t, c.method+" "+c.path, rec.Body.String())
	}
}

func TestKeyImpactEndpointIsReadOnlyGET(t *testing.T) {
	s, shared := newShellTestServer(t)
	staticID, _, _ := seedCredentials(t, s, shared)
	rec := credAdminRequest(t, s, shared, http.MethodGet, "/api/keys/"+staticID+"/impact", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"clients"`) {
		t.Fatalf("impact: %d %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, "impact", rec.Body.String())
	if rec := credAdminRequest(t, s, shared, http.MethodGet, "/api/keys/missing/impact", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id = ?`, staticID).Scan(&n)
	if n != 1 {
		t.Fatal("preview deleted the key")
	}
}
