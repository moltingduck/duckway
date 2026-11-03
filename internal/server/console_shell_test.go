package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/web"
)

var (
	scriptTagRE  = regexp.MustCompile(`(?i)<script\b[^>]*>`)
	scriptNonce  = regexp.MustCompile(`nonce="([^"]+)"`)
	nonceInCSPRE = regexp.MustCompile(`'nonce-([^']+)'`)
)

func newShellTestServer(t *testing.T) (*Server, *SharedServices) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &Server{
		config: &Config{DataDir: dir, EncryptionKey: bytes.Repeat([]byte{1}, 32), SessionSecret: bytes.Repeat([]byte{2}, 32)},
		db:     db, mux: http.NewServeMux(), stopCh: make(chan struct{}),
	}
	shared := s.initShared()
	s.SetupAdminRoutes(web.Content, shared)
	return s, shared
}

// checkPage asserts CSP header presence and that every <script> carries the header's nonce.
func checkPage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	csp := rec.Header().Get("Content-Security-Policy-Report-Only")
	if csp == "" {
		csp = rec.Header().Get("Content-Security-Policy")
	}
	m := nonceInCSPRE.FindStringSubmatch(csp)
	if m == nil {
		t.Fatalf("no CSP nonce header: %v", rec.Header())
	}
	nonce := m[1]
	tags := scriptTagRE.FindAllString(rec.Body.String(), -1)
	if len(tags) == 0 {
		t.Fatal("page has no script tags")
	}
	for _, tag := range tags {
		nm := scriptNonce.FindStringSubmatch(tag)
		if nm == nil || nm[1] != nonce {
			t.Errorf("script tag %q lacks header nonce %q", tag, nonce)
		}
	}
	return nonce
}

func TestAdminPagesSendCSPWithMatchingNonces(t *testing.T) {
	s, shared := newShellTestServer(t)
	get := func(path string, cookie bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie {
			req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
		}
		rec := httptest.NewRecorder()
		testSameOrigin(req)
		s.ServeHTTP(rec, req)
		return rec
	}
	for _, tc := range []struct {
		path   string
		cookie bool
	}{{"/admin/login", false}, {"/admin/", true}} {
		a, b := get(tc.path, tc.cookie), get(tc.path, tc.cookie)
		if a.Code != http.StatusOK {
			t.Fatalf("%s status %d", tc.path, a.Code)
		}
		na, nb := checkPage(t, a), checkPage(t, b)
		if na == nb {
			t.Errorf("%s nonce reused across requests", tc.path)
		}
	}
}

func TestAdminShellShowsPendingApprovalsBadge(t *testing.T) {
	s, shared := newShellTestServer(t)
	render := func() string {
		req := httptest.NewRequest(http.MethodGet, "/admin/approvals", nil)
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
		rec := httptest.NewRecorder()
		testSameOrigin(req)
		s.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if strings.Contains(render(), `class="nav-badge"`) {
		t.Fatal("badge shown with no pending approvals")
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	aq := queries.NewApprovalQueries(s.db)
	for _, id := range []string{"a1", "a2"} {
		ph := "p" // one pending token-wide approval per token: use two tokens
		if id == "a2" {
			ph = "p2"
		}
		if err := aq.Create(&models.Approval{ID: id, PlaceholderID: ph, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
	if body := render(); !strings.Contains(body, `class="nav-badge">2<span class="sr-only"> pending</span>`) {
		t.Fatalf("badge for 2 pending not rendered")
	}
}

// TestEveryAdminPageRendersWithNonceAndShell executes every admin page through
// the real render path (seeded with a key group that has members, so the
// detail template runs with a non-pageData dot) and asserts the template
// executed fully: no truncation, shell.js present, every script nonced.
func TestEveryAdminPageRendersWithNonceAndShell(t *testing.T) {
	s, shared := newShellTestServer(t)
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('svc-r', 'render-svc', 'Render', 'https://r.example', 'r.example')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k-r1', 'svc-r', 'key one', 'x')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, usage_snapshot) VALUES ('k-r2', 'svc-r', 'key two', 'x', '{"tokens_remaining":5,"tokens_limit":10}')`)
	g, err := queries.CreateKeyGroup(s.db, "render group", "d", "render-svc", "")
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"k-r1", "k-r2"} {
		if err := queries.AddKeyToGroup(s.db, g.ID, id, i); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO clients (id, short_id, name, token_hash) VALUES ('c-shell', 'shell1', 'shell client', 'h')`)
	paths := []string{
		"/admin/clients/c-shell", "/admin/", "/admin/services", "/admin/credentials", "/admin/usage", "/admin/phantom-tokens",
		"/admin/clients", "/admin/clients/rollouts", "/admin/key-pools",
		"/admin/key-pools?pool=lease:" + g.ID, "/admin/key-bundles", "/admin/approvals", "/admin/logs",
		"/admin/notifications", "/admin/canary", "/admin/credentials?type=refreshable", "/admin/cc", "/admin/clients/new",
		"/admin/settings", "/admin/supply-chain", "/admin/docs", "/admin/plugins",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
		rec := httptest.NewRecorder()
		testSameOrigin(req)
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d: %.200s", p, rec.Code, rec.Body.String())
			continue
		}
		body := rec.Body.String()
		if !strings.Contains(body, "</html>") {
			t.Errorf("%s: page truncated (template execution error)", p)
		}
		if !strings.Contains(body, `src="/static/shell.js"`) {
			t.Errorf("%s: shell.js script missing", p)
		}
		checkPage(t, rec)
	}
}
