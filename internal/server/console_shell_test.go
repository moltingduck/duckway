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
		if err := aq.Create(&models.Approval{ID: id, PlaceholderID: "p", Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
	if body := render(); !strings.Contains(body, `class="nav-badge" aria-label="2 pending">2<`) {
		t.Fatalf("badge for 2 pending not rendered")
	}
}
