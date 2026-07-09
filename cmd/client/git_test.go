package main

import (
	"reflect"
	"strings"
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
		{ServiceName: "github", Placeholder: "github_pat_dw_repo", PermissionConfig: acl},
		{ServiceName: "openai", PermissionConfig: acl},
	})
	want := []configuredGitRepo{
		{Repo: "ORG/DEPLOY", Mode: "deploy", Placeholder: "github_pat_dw_repo"},
		{Repo: "OWNER/REPO", Mode: "dev", Placeholder: "github_pat_dw_repo"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredGitRepos = %#v, want %#v", got, want)
	}
}

func TestConfiguredGitReposSkipsEmptyPlaceholder(t *testing.T) {
	acl := `{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"GET","path":"/OWNER/REPO.git/info/refs","allow":true},{"method":"POST","path":"/OWNER/REPO.git/git-upload-pack","allow":true}]}]}`
	got := configuredGitRepos([]client.PlaceholderKeyInfo{
		{ServiceName: "github", PermissionConfig: acl},
		{ServiceName: "github", Placeholder: "   ", PermissionConfig: acl},
	})
	if len(got) != 0 {
		t.Fatalf("configuredGitRepos = %#v, want none for empty placeholder", got)
	}
}

func TestFormatGitCommand(t *testing.T) {
	got := formatGitCommand(
		"-c",
		"http.https://github.com/.proxy=http://localhost:18080",
		"-c",
		"http.https://github.com/.sslCAInfo=/home/me/.duckway/ca.pem",
		"clone",
		"https://github.com/OWNER/REPO.git",
		"my repo",
	)
	for _, want := range []string{
		"git -c",
		"'http.https://github.com/.proxy=http://localhost:18080'",
		"'http.https://github.com/.sslCAInfo=/home/me/.duckway/ca.pem'",
		"clone https://github.com/OWNER/REPO.git 'my repo'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatGitCommand missing %q in %q", want, got)
		}
	}
}
