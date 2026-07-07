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

	if err := deployGitHubCredential(home, "github_pat_dw_new"); err != nil {
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
