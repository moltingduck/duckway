package handlers

import "testing"

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
