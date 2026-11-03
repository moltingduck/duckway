package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/server/handlers"
)

func TestMovedPagesReturn404NoticeInShell(t *testing.T) {
	s, shared := newShellTestServer(t)
	paths := handlers.MovedPaths()
	if len(paths) < 2 {
		t.Fatalf("moved map has %d entries, want at least the two fleet pages", len(paths))
	}
	for _, old := range paths {
		m, _ := handlers.LookupMoved(old)
		// Admin auth is required like every other page.
		anon := httptest.NewRecorder()
		s.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, old, nil))
		if anon.Code == http.StatusNotFound || strings.Contains(anon.Body.String(), m.NewPath) {
			t.Errorf("%s: anonymous request got the moved page (status %d)", old, anon.Code)
		}
		req := httptest.NewRequest(http.MethodGet, old, nil)
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", old, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{`class="sidebar"`, `src="/static/shell.js"`, "</html>", `href="` + m.NewPath + `"`, m.NewName, "Update your bookmark", old} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: moved page missing %q", old, want)
			}
		}
		if strings.Contains(strings.ToLower(rec.Header().Get("Location")), "http") || rec.Header().Get("Location") != "" {
			t.Errorf("%s: must not redirect", old)
		}
		checkPage(t, rec)
	}
}

func TestMovedPagesMapTargetsAreRealPages(t *testing.T) {
	s, shared := newShellTestServer(t)
	for _, old := range handlers.MovedPaths() {
		m, _ := handlers.LookupMoved(old)
		req := httptest.NewRequest(http.MethodGet, m.NewPath, nil)
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s -> %s: new location status %d", old, m.NewPath, rec.Code)
		}
	}
}
