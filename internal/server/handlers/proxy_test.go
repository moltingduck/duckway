package handlers_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

// proxyFixture holds all query objects and the seeded IDs for proxy tests.
type proxyFixture struct {
	svcQ         *queries.ServiceQueries
	apiKeyQ      *queries.APIKeyQueries
	placeholderQ *queries.PlaceholderQueries
	groupQ       *queries.GroupQueries
	approvalQ    *queries.ApprovalQueries
	logQ         *queries.RequestLogQueries

	crypto *services.Crypto

	serviceID     string
	clientID      string
	apiKeyID      string
	placeholderID string

	client *models.Client
}

// newProxyFixture opens a fresh SQLite DB, runs migrations, seeds one service
// (pointing at the given upstream URL), one client, one api_key, and one
// placeholder_key. It also returns the seeded *models.Client so tests can
// inject it via context.
func newProxyFixture(t *testing.T, upstreamURL string) *proxyFixture {
	t.Helper()

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	const (
		svcID = "svc-proxy-01"
		cliID = "cli-proxy-01"
		keyID = "key-proxy-01"
		phID  = "ph-proxy-01"
	)

	// Seed service
	_, err = db.Exec(
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern, auth_type, auth_header, auth_prefix)
		 VALUES (?, 'anthropic', 'Anthropic', ?, 'api.anthropic.com', 'bearer', 'Authorization', 'Bearer ')`,
		svcID, upstreamURL,
	)
	if err != nil {
		t.Fatalf("seed service: %v", err)
	}

	// Seed client. token_hash = sha256("test-token")
	tokenHash := services.HashToken("test-token")
	_, err = db.Exec(
		`INSERT INTO clients (id, name, token_hash) VALUES (?, 'test-client', ?)`,
		cliID, tokenHash,
	)
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}

	// Encrypt "sk-real-key" with a deterministic 32-byte zero key
	cryptoKey := make([]byte, 32)
	cr := services.NewCrypto(cryptoKey)
	encKey, err := cr.Encrypt("sk-real-key")
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}

	// Seed api_key
	_, err = db.Exec(
		`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES (?, ?, 'real-key', ?)`,
		keyID, svcID, encKey,
	)
	if err != nil {
		t.Fatalf("seed api_key: %v", err)
	}

	// Seed placeholder_key with requires_approval=0 (schema defaults to 1).
	_, err = db.Exec(
		`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, requires_approval)
		 VALUES (?, 'ANTHROPIC_API_KEY', 'sk-dw-fake-placeholder', ?, ?, ?, 0)`,
		phID, svcID, keyID, cliID,
	)
	if err != nil {
		t.Fatalf("seed placeholder: %v", err)
	}

	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	placeholderQ := queries.NewPlaceholderQueries(db)
	groupQ := queries.NewGroupQueries(db)
	approvalQ := queries.NewApprovalQueries(db)
	logQ := queries.NewRequestLogQueries(db)

	client := &models.Client{
		ID:       cliID,
		Name:     "test-client",
		IsActive: true,
	}

	return &proxyFixture{
		svcQ:          svcQ,
		apiKeyQ:       apiKeyQ,
		placeholderQ:  placeholderQ,
		groupQ:        groupQ,
		approvalQ:     approvalQ,
		logQ:          logQ,
		crypto:        cr,
		serviceID:     svcID,
		clientID:      cliID,
		apiKeyID:      keyID,
		placeholderID: phID,
		client:        client,
	}
}

// newProxyHandler wires up a ProxyHandler from a proxyFixture.
func newProxyHandler(f *proxyFixture) *handlers.ProxyHandler {
	resolver := services.NewKeyResolver(f.crypto, f.apiKeyQ, f.placeholderQ, f.groupQ, f.approvalQ)
	return handlers.NewProxyHandler(f.svcQ, f.apiKeyQ, resolver, f.logQ, f.approvalQ, nil, nil)
}

// withClient injects a *models.Client into a request context, bypassing the
// auth middleware. This mirrors what middleware.ClientAuth.Middleware does.
func withClient(r *http.Request, c *models.Client) *http.Request {
	ctx := context.WithValue(r.Context(), middleware.ClientKey, c)
	return r.WithContext(ctx)
}

// doProxy sends a request to the ProxyHandler and returns (statusCode, body).
func doProxy(h *handlers.ProxyHandler, r *http.Request) (int, []byte) {
	w := httptest.NewRecorder()
	http.HandlerFunc(h.Handle).ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

// ---- Tests ----

// TestProxy_UnknownService — path /proxy/nonexistent/v1/foo → 404.
func TestProxy_UnknownService(t *testing.T) {
	// Use a no-op upstream; it won't be reached.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := newProxyFixture(t, upstream.URL)
	h := newProxyHandler(f)

	r := httptest.NewRequest("GET", "/proxy/nonexistent/v1/foo", nil)
	r = withClient(r, f.client)
	code, body := doProxy(h, r)

	if code != http.StatusNotFound {
		t.Errorf("want 404, got %d; body: %s", code, body)
	}
}

// TestProxy_NoClientAuth — known service but no client in context → 401.
func TestProxy_NoClientAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := newProxyFixture(t, upstream.URL)
	h := newProxyHandler(f)

	// Do NOT inject a client — no withClient call.
	r := httptest.NewRequest("GET", "/proxy/anthropic/v1/messages", nil)
	code, body := doProxy(h, r)

	if code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d; body: %s", code, body)
	}
}

// TestProxy_NotPermitted — client has no placeholder for the service → 403.
func TestProxy_NotPermitted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := newProxyFixture(t, upstream.URL)
	h := newProxyHandler(f)

	// Use a client that has no placeholder bound to the anthropic service.
	stranger := &models.Client{
		ID:       "cli-stranger",
		Name:     "stranger",
		IsActive: true,
	}
	r := httptest.NewRequest("GET", "/proxy/anthropic/v1/messages", nil)
	r = withClient(r, stranger)
	code, body := doProxy(h, r)

	if code != http.StatusForbidden {
		t.Errorf("want 403, got %d; body: %s", code, body)
	}
}

// TestProxy_KeyInjected — happy path: real key injected upstream; client's
// fake placeholder token must NOT appear in the upstream Authorization header.
func TestProxy_KeyInjected(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := newProxyFixture(t, upstream.URL)
	h := newProxyHandler(f)

	r := httptest.NewRequest("GET", "/proxy/anthropic/v1/models", nil)
	// Simulate the client sending its fake placeholder as the auth token.
	r.Header.Set("Authorization", "Bearer sk-dw-fake-placeholder")
	r = withClient(r, f.client)
	code, _ := doProxy(h, r)

	if code != http.StatusOK {
		t.Errorf("want 200, got %d", code)
	}
	if gotAuth != "Bearer sk-real-key" {
		t.Errorf("upstream Authorization = %q, want %q", gotAuth, "Bearer sk-real-key")
	}
	if strings.Contains(gotAuth, "fake-placeholder") {
		t.Errorf("upstream Authorization must not contain the fake placeholder; got %q", gotAuth)
	}
}

func TestProxyGitHubPhantomModeInjectsFineGrainedPAT(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := newProxyFixture(t, upstream.URL)
	realPAT := "github_pat_" + strings.Repeat("r", 82)
	encPAT, err := f.crypto.Encrypt(realPAT)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := f.svcQ.GetByID(f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	svc.Name = "github"
	svc.DisplayName = "GitHub API + Git"
	svc.HostPattern = "api.github.com,github.com"
	svc.AuthType = "bearer"
	svc.AuthHeader = "Authorization"
	svc.AuthPrefix = "Bearer "
	svc.KeyPrefix = "github_pat_"
	svc.KeyLength = 93
	svc.DeliveryMode = "proxy"
	if err := f.svcQ.Update(svc); err != nil {
		t.Fatal(err)
	}
	key, err := f.apiKeyQ.GetByID(f.apiKeyID)
	if err != nil {
		t.Fatal(err)
	}
	key.KeyEncrypted = encPAT
	if err := f.apiKeyQ.Update(key); err != nil {
		t.Fatal(err)
	}
	ph, err := f.placeholderQ.GetByID(f.placeholderID)
	if err != nil {
		t.Fatal(err)
	}
	ph.EnvName = "GITHUB_TOKEN"
	ph.Placeholder = "github_pat_dw_fake"
	if err := f.placeholderQ.Update(ph); err != nil {
		t.Fatal(err)
	}

	h := newProxyHandler(f)
	r := httptest.NewRequest("GET", "/proxy/github/user", nil)
	r.Header.Set("Authorization", "Bearer github_pat_dw_fake")
	r = withClient(r, f.client)
	code, body := doProxy(h, r)

	if code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", code, body)
	}
	if gotAuth != "Bearer "+realPAT {
		t.Fatalf("upstream Authorization = %q, want real PAT", gotAuth)
	}
	if strings.Contains(gotAuth, "dw_") || strings.Contains(gotAuth, "fake") {
		t.Fatalf("upstream Authorization leaked phantom: %q", gotAuth)
	}
}

// TestProxy_StripsDuckwayHeaders — client sends X-Duckway-Token and X-Custom;
// upstream must NOT see X-Duckway-Token but MUST see X-Custom.
func TestProxy_StripsDuckwayHeaders(t *testing.T) {
	var (
		gotDuckway string
		gotCustom  string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDuckway = r.Header.Get("X-Duckway-Token")
		gotCustom = r.Header.Get("X-Custom")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := newProxyFixture(t, upstream.URL)
	h := newProxyHandler(f)

	r := httptest.NewRequest("GET", "/proxy/anthropic/v1/models", nil)
	r.Header.Set("X-Duckway-Token", "secret-internal-token")
	r.Header.Set("X-Custom", "keep-me")
	r = withClient(r, f.client)
	code, body := doProxy(h, r)

	if code != http.StatusOK {
		t.Errorf("want 200, got %d; body: %s", code, body)
	}
	if gotDuckway != "" {
		t.Errorf("upstream must not see X-Duckway-Token, but got %q", gotDuckway)
	}
	if gotCustom != "keep-me" {
		t.Errorf("upstream must see X-Custom=keep-me, got %q", gotCustom)
	}
}

// TestProxy_NeedApproval — placeholder has requires_approval=true, no approval
// row → 403 with error "duckway_approval_pending".
func TestProxy_NeedApproval(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := newProxyFixture(t, upstream.URL)

	// Flip requires_approval on the placeholder.
	ph, err := f.placeholderQ.GetByID(f.placeholderID)
	if err != nil {
		t.Fatalf("get placeholder: %v", err)
	}
	ph.RequiresApproval = true
	if err := f.placeholderQ.Update(ph); err != nil {
		t.Fatalf("update placeholder: %v", err)
	}

	h := newProxyHandler(f)

	r := httptest.NewRequest("POST", "/proxy/anthropic/v1/messages", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r = withClient(r, f.client)
	code, body := doProxy(h, r)

	if code != http.StatusForbidden {
		t.Errorf("want 403, got %d; body: %s", code, body)
	}

	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body: %s", err, body)
	}
	if resp["error"] != "duckway_approval_pending" {
		t.Errorf("error field = %q, want %q", resp["error"], "duckway_approval_pending")
	}
}

// TestProxy_UpstreamBody — upstream returns a body; response body reaches the
// client intact and the status code is forwarded.
func TestProxy_UpstreamBody(t *testing.T) {
	const wantBody = `{"hello":"world"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, wantBody)
	}))
	defer upstream.Close()

	f := newProxyFixture(t, upstream.URL)
	h := newProxyHandler(f)

	r := httptest.NewRequest("POST", "/proxy/anthropic/v1/messages", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r = withClient(r, f.client)
	code, body := doProxy(h, r)

	if code != http.StatusCreated {
		t.Errorf("want 201, got %d", code)
	}
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", string(body), wantBody)
	}
}

// TestProxy_StripsHopByHopHeaders — upstream returns Transfer-Encoding and
// Connection; those hop-by-hop headers must NOT appear in the client response.
func TestProxy_StripsHopByHopHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set hop-by-hop headers on the upstream response. net/http may
		// suppress some automatically, but the proxy's
		// shouldStripResponseHeader is exercised regardless.
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Keep", "yes")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := newProxyFixture(t, upstream.URL)
	h := newProxyHandler(f)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/proxy/anthropic/v1/models", nil)
	r = withClient(r, f.client)
	http.HandlerFunc(h.Handle).ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
	if te := rec.Header().Get("Transfer-Encoding"); te != "" {
		t.Errorf("Transfer-Encoding must be stripped, got %q", te)
	}
	if conn := rec.Header().Get("Connection"); conn != "" {
		t.Errorf("Connection must be stripped, got %q", conn)
	}
	if keep := rec.Header().Get("X-Keep"); keep != "yes" {
		t.Errorf("X-Keep must be forwarded, got %q", keep)
	}
}
