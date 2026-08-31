package server

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/web"
)

// TestAdminAPIRoutesCannotBypassAuth is an architectural guard. Admin routes
// belong on adminAPIMux, which is mounted once behind AdminAuth. Adding a new
// /api route directly to Server.mux fails this test unless it is an explicitly
// reviewed public authentication endpoint.
func TestAdminAPIRoutesCannotBypassAuth(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	allowedPublic := map[string]bool{
		"POST /api/auth/login":  true,
		"POST /api/auth/logout": true,
	}
	protectedMounts := 0

	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != repoRoot && (strings.HasPrefix(name, ".") || name == "vendor" || name == "live-credentials") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 || !isOuterMuxRegistration(call.Fun) {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Errorf("%s registers a non-literal route directly on Server.mux; route auth cannot be audited statically", path)
				return true
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Errorf("%s has invalid route literal %s", path, literal.Value)
				return true
			}
			if pattern == "/api/" {
				if len(call.Args) < 2 || !containsAdminAuthMiddleware(call.Args[1]) {
					t.Errorf("%s mounts /api/ without AdminAuth.Middleware", path)
				} else {
					protectedMounts++
				}
				return true
			}
			if routePath(pattern) == "/api" || strings.HasPrefix(routePath(pattern), "/api/") {
				if !allowedPublic[pattern] {
					t.Errorf("unprotected admin API registration %q in %s; register it on adminAPIMux or explicitly review it as public", pattern, path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if protectedMounts != 1 {
		t.Fatalf("protected /api/ mounts = %d, want exactly 1", protectedMounts)
	}
}

func TestAdminAPIRejectsAnonymousRequestsBeforeHandler(t *testing.T) {
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
	s.SetupAdminRoutes(web.Content, s.initShared())

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/clients"},
		{http.MethodPost, "/api/clients/unknown/rotate-token"},
		{http.MethodPost, "/api/settings"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s status=%d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func isOuterMuxRegistration(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
		return false
	}
	receiver, ok := sel.X.(*ast.SelectorExpr)
	return ok && receiver.Sel.Name == "mux"
}

func containsAdminAuthMiddleware(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Middleware" {
			return true
		}
		receiver, ok := sel.X.(*ast.SelectorExpr)
		if ok && receiver.Sel.Name == "AdminAuth" {
			found = true
		}
		return true
	})
	return found
}

func routePath(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}
