package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployGitHubCredentialMergesDuckwayEntry(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".git-credentials")
	existing := strings.Join([]string{
		"https://alice:real-token@github.com",
		"https://x-access-token:github_pat_dw_old@github.com",
		"https://user:token@example.com",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	if err := DeployGitHubCredentialForGit(home, "github_pat_dw_new"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "https://alice:real-token@github.com") {
		t.Fatalf("non-duckway github credential removed:\n%s", got)
	}
	if !strings.Contains(got, "https://user:token@example.com") {
		t.Fatalf("non-github credential removed:\n%s", got)
	}
	if strings.Contains(got, "github_pat_dw_old") {
		t.Fatalf("old duckway credential not replaced:\n%s", got)
	}
	if count := strings.Count(got, "github_pat_dw_new"); count != 1 {
		t.Fatalf("new duckway credential count = %d:\n%s", count, got)
	}
}

func TestDeployGitHubCredentialForGitRepoPreservesOtherRepoTokens(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".git-credentials")
	existing := strings.Join([]string{
		"https://alice:real-token@github.com",
		"https://x-access-token:github_pat_dw_old@github.com",
		"https://x-access-token:github_pat_dw_alpha@github.com/ExampleOrg/RepoAlpha.git",
		"https://x-access-token:github_pat_dw_old_beta@github.com/ExampleOrg/RepoBeta.git",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	if err := DeployGitHubCredentialForGitRepo(home, "ExampleOrg/RepoBeta", "github_pat_dw_new_beta"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "https://alice:real-token@github.com") {
		t.Fatalf("non-duckway github credential removed:\n%s", got)
	}
	if !strings.Contains(got, "github_pat_dw_alpha@github.com/ExampleOrg/RepoAlpha.git") {
		t.Fatalf("different repo duckway credential removed:\n%s", got)
	}
	if strings.Contains(got, "github_pat_dw_old@github.com") || strings.Contains(got, "github_pat_dw_old_beta") {
		t.Fatalf("old host-level or same repo duckway credential not replaced:\n%s", got)
	}
	if !strings.Contains(got, "https://x-access-token:github_pat_dw_new_beta@github.com/ExampleOrg/RepoBeta.git") {
		t.Fatalf("new repo-scoped credential missing:\n%s", got)
	}
}

func TestDeployGitHubCredentialsForKeyWritesConfiguredRepos(t *testing.T) {
	home := t.TempDir()
	acl := `{"version":"1","provider":"github","rules":[{"endpoints":[` +
		`{"method":"GET","path":"/ExampleOrg/RepoBeta.git/info/refs","allow":true},` +
		`{"method":"POST","path":"/ExampleOrg/RepoBeta.git/git-upload-pack","allow":true},` +
		`{"method":"GET","path":"/ExampleOrg/RepoAlpha.git/info/refs","allow":true}` +
		`],"deny_all_other":true}]}`

	if err := DeployGitHubCredentialsForKey(home, "github_pat_dw_repo", acl); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(home, ".git-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"https://x-access-token:github_pat_dw_repo@github.com/ExampleOrg/RepoBeta.git",
		"https://x-access-token:github_pat_dw_repo@github.com/ExampleOrg/RepoAlpha.git",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("credential store missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "github_pat_dw_repo@github.com\n") {
		t.Fatalf("unexpected host-level credential:\n%s", got)
	}
}

func TestGitHubReposFromV2PermissionConfig(t *testing.T) {
	config := `{"version":"2","provider":"github","repositories":{"ExampleOrg/RepoBeta":{"capabilities":{"clone":true}},"ExampleOrg/RepoAlpha":{"capabilities":{"actions_read":true}}}}`
	got := githubReposFromPermissionConfig(config)
	want := []string{"ExampleOrg/RepoAlpha", "ExampleOrg/RepoBeta"}
	if len(got) != len(want) {
		t.Fatalf("repositories = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("repositories = %v, want %v", got, want)
		}
	}
}
