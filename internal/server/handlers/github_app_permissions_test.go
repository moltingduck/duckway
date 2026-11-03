package handlers

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/server/services"
)

func TestGitHubPermissionScopeUsesLeastPrivilege(t *testing.T) {
	tests := []struct {
		method, requestPath, query, scope, level string
	}{
		{"GET", "/repos/o/r/issues/1", "", "issues", "read"},
		{"POST", "/repos/o/r/issues", "", "issues", "write"},
		{"PATCH", "/repos/o/r/pulls/1", "", "pull_requests", "write"},
		{"GET", "/repos/o/r/releases", "", "contents", "read"},
		{"POST", "/repos/o/r/releases", "", "contents", "write"},
		{"GET", "/repos/o/r/actions/runs", "", "actions", "read"},
		{"POST", "/repos/o/r/actions/workflows/ci.yml/dispatches", "", "actions", "write"},
		{"GET", "/o/r.git/info/refs", "service=git-receive-pack", "contents", "write"},
	}
	for _, tc := range tests {
		scope, level := githubPermissionScope(tc.method, tc.requestPath, tc.query)
		if scope != tc.scope || level != tc.level {
			t.Errorf("%s %s got %s:%s, want %s:%s", tc.method, tc.requestPath, scope, level, tc.scope, tc.level)
		}
	}
}

func TestGitHubAppCacheKeyIncludesPolicyIdentity(t *testing.T) {
	cred := &githubAppCredential{AppID: 1, InstallationID: 2, PrivateKey: "private"}
	permissions := map[string]string{"contents": "read"}
	a := githubAppCacheKey(cred, "o", "r", permissions, `{"version":"2","repositories":{"o/r":{}}}`)
	b := githubAppCacheKey(cred, "o", "r", permissions, `{"version":"2","repositories":{"o/r":{"changed":true}}}`)
	if a == b {
		t.Fatal("cache key did not change with assignment policy")
	}
}

// The GitHub repositories builder compiles capability rows with
// services.GitHubCapabilityTable. Every endpoint it can open must request, when
// a token is minted for it, exactly the GitHub App permission and level the row
// promises, so the builder and githubPermissionScope can never drift apart.
func TestGitHubCapabilityTableMatchesPermissionScope(t *testing.T) {
	sample := func(tpl string) string {
		p := strings.ReplaceAll(tpl, "{git}", "/acme/app.git")
		p = strings.ReplaceAll(p, "{repo}", "acme/app")
		return strings.ReplaceAll(p, "*", "x")
	}
	for _, c := range services.GitHubCapabilityTable {
		check := func(level string, eps []services.GitHubCapEndpoint) {
			for _, e := range eps {
				path := sample(e.Path)
				wantScope, wantLevel := c.Scope, level
				if e.Method == "GET" || strings.HasSuffix(path, "git-upload-pack") {
					wantLevel = "read"
				}
				gotScope, gotLevel := githubPermissionScope(e.Method, path, "")
				if gotScope != wantScope || gotLevel != wantLevel {
					t.Errorf("%s %s %s: githubPermissionScope = %s/%s, table says %s/%s", c.Key, e.Method, path, gotScope, gotLevel, wantScope, wantLevel)
				}
			}
		}
		check("read", c.Read)
		check("write", c.Write)
	}
	// Git over HTTPS: clone/fetch is contents read, push is contents write.
	for _, tc := range []struct{ method, path, query, level string }{
		{"GET", "/acme/app.git/info/refs", "service=git-upload-pack", "read"},
		{"POST", "/acme/app.git/git-upload-pack", "", "read"},
		{"GET", "/acme/app.git/info/refs", "service=git-receive-pack", "write"},
		{"POST", "/acme/app.git/git-receive-pack", "", "write"},
	} {
		scope, level := githubPermissionScope(tc.method, tc.path, tc.query)
		if scope != "contents" || level != tc.level {
			t.Errorf("%s %s?%s = %s/%s, want contents/%s", tc.method, tc.path, tc.query, scope, level, tc.level)
		}
	}
}

// The compiled rule must let git discovery through exactly as the proxy
// normalises it: upload-pack discovery needs contents read, receive-pack
// discovery needs contents write.
func TestGitHubCapabilityRuleGitOverHTTPS(t *testing.T) {
	compile := func(level string) string {
		rule, err := services.CompileGitHubCapabilityRule(services.GitHubCapabilityRows{"Acme/App": {"contents": level}})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(services.PermissionConfig{Version: "1", Provider: "github", Rules: []services.PermissionRule{rule}})
		return string(b)
	}
	pc := services.NewPermissionChecker()
	allowed := func(cfg, method, path, query string) bool {
		m, p := proxyACLRequest(method, path, query)
		return pc.Check(cfg, "p", m, p, nil).Allowed
	}
	read, write := compile("read"), compile("write")
	for _, tc := range []struct {
		name, method, path, query string
		read, write               bool
	}{
		{"upload discovery", "GET", "/Acme/App.git/info/refs", "service=git-upload-pack", true, true},
		{"upload-pack", "POST", "/Acme/App.git/git-upload-pack", "", true, true},
		{"receive discovery", "GET", "/Acme/App.git/info/refs", "service=git-receive-pack", false, true},
		{"receive-pack", "POST", "/Acme/App.git/git-receive-pack", "", false, true},
		{"case-insensitive repo", "POST", "/acme/app.git/git-upload-pack", "", true, true},
		{"other repo", "POST", "/acme/other.git/git-upload-pack", "", false, false},
	} {
		if got := allowed(read, tc.method, tc.path, tc.query); got != tc.read {
			t.Errorf("%s with contents read: allowed=%v want %v", tc.name, got, tc.read)
		}
		if got := allowed(write, tc.method, tc.path, tc.query); got != tc.write {
			t.Errorf("%s with contents write: allowed=%v want %v", tc.name, got, tc.write)
		}
	}
}

// Mint audit guard: an empty token request body makes GitHub grant the whole
// installation's permissions on every repository. No handler may mint that
// way; every mint must carry an explicit permissions object.
func TestNoEmptyBodyInstallationTokenMints(t *testing.T) {
	for _, name := range []string{"api_keys.go", "github_app.go", "proxy.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "mintGitHubInstallationToken(") && !strings.HasPrefix(strings.TrimSpace(line), "func ") &&
				(strings.Contains(line, "`{}`") || strings.Contains(line, `"{}"`)) {
				t.Errorf("%s:%d mints an installation token with an empty body: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
