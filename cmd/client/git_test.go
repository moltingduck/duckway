package main

import (
	"reflect"
	"testing"

	"github.com/hackerduck/duckway/internal/client"
)

func TestNormalizeGitRepoArg(t *testing.T) {
	cases := map[string]string{
		"OWNER/REPO":                         "OWNER/REPO",
		"https://github.com/OWNER/REPO":      "OWNER/REPO",
		"https://github.com/OWNER/REPO.git/": "OWNER/REPO",
		"git@github.com:OWNER/REPO.git":      "OWNER/REPO",
	}
	for in, want := range cases {
		got, err := normalizeGitRepoArg(in)
		if err != nil {
			t.Fatalf("normalizeGitRepoArg(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("normalizeGitRepoArg(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "OWNER", "OWNER/REPO/EXTRA", "../REPO", "https://evil.example/OWNER/REPO"} {
		if got, err := normalizeGitRepoArg(bad); err == nil {
			t.Fatalf("normalizeGitRepoArg(%q) = %q, want error", bad, got)
		}
	}
}

func TestConfiguredGitReposFromPermissionConfig(t *testing.T) {
	acl := `{"version":"1","provider":"github","rules":[{"name":"dev-read-write","endpoints":[{"method":"GET","path":"/OWNER/REPO.git/info/refs","allow":true},{"method":"POST","path":"/OWNER/REPO.git/git-upload-pack","allow":true},{"method":"POST","path":"/OWNER/REPO.git/git-receive-pack","allow":true},{"method":"GET","path":"/repos/OWNER/REPO","allow":true},{"method":"GET","path":"/ORG/DEPLOY.git/info/refs","allow":true},{"method":"POST","path":"/ORG/DEPLOY.git/git-upload-pack","allow":true}],"deny_all_other":true}]}`
	got := configuredGitRepos([]client.PlaceholderKeyInfo{
		{ServiceName: "github", PermissionConfig: acl},
		{ServiceName: "openai", PermissionConfig: acl},
	})
	want := []configuredGitRepo{
		{Repo: "ORG/DEPLOY", Mode: "deploy"},
		{Repo: "OWNER/REPO", Mode: "dev"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredGitRepos = %#v, want %#v", got, want)
	}
}
