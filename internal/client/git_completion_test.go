package client

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestGitCompletionSnapshot(t *testing.T) {
	dir := t.TempDir()
	keys := []PlaceholderKeyInfo{{ServiceName: "github", Placeholder: "secret-must-not-persist", PermissionConfig: `{"version":"2","provider":"github","repositories":{"org/yes":{"capabilities":{"clone":true}},"org/no":{"capabilities":{"issues_read":true}}}}`}}
	if err := saveGitCompletionRepos(dir, keys); err != nil {
		t.Fatal(err)
	}
	if got := LocalGitCompletionRepos(dir); !reflect.DeepEqual(got, []string{"org/yes"}) {
		t.Fatal(got)
	}
	body, err := os.ReadFile(filepath.Join(dir, gitCompletionFile))
	if err != nil || strings.Contains(string(body), "secret") {
		t.Fatal("secret in cache or read failed")
	}
	info, _ := os.Stat(filepath.Join(dir, gitCompletionFile))
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if err := saveGitCompletionRepos(dir, nil); err != nil {
		t.Fatal(err)
	}
	if len(LocalGitCompletionRepos(dir)) != 0 {
		t.Fatal("removed assignment persisted")
	}
}

func TestGitCompletionLegacy(t *testing.T) {
	keys := []PlaceholderKeyInfo{{ServiceName: "github", Placeholder: "dw_test", PermissionConfig: `{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"POST","path":"/a/clone.git/git-upload-pack","allow":true},{"method":"POST","path":"/a/push.git/git-receive-pack","allow":true},{"method":"POST","path":"/a/denied.git/git-upload-pack","allow":false}]}]}`}}
	if got := completionRepos(keys); !reflect.DeepEqual(got, []string{"a/clone"}) {
		t.Fatal(got)
	}
}

func TestGitCompletionUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, gitCompletionFile)
	for _, body := range []string{"broken", strings.Repeat("x", maxGitCompletionSize+1), `["ok/repo","a/$(oops)","ok/repo","OK/REPO","a/.."]`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		got := LocalGitCompletionRepos(dir)
		if strings.HasPrefix(body, "[") {
			if !reflect.DeepEqual(got, []string{"OK/REPO"}) {
				t.Fatal(got)
			}
		} else if len(got) != 0 {
			t.Fatal(got)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if got := LocalGitCompletionRepos(dir); len(got) != 0 {
		t.Fatal(got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(`["a/repo"]`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if got := LocalGitCompletionRepos(dir); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestGitCompletionLegacyIgnoresRateAccounting(t *testing.T) {
	keys := []PlaceholderKeyInfo{{ServiceName: "github", Placeholder: "dw_test", PermissionConfig: `{"version":"1","provider":"github","rules":[{"rate_limit":{"requests_per_minute":1},"endpoints":[{"method":"GET","path":"/a/one.git/info/refs","allow":true},{"method":"POST","path":"/a/one.git/git-upload-pack","allow":true},{"method":"GET","path":"/a/two.git/info/refs","allow":true},{"method":"POST","path":"/a/two.git/git-upload-pack","allow":true},{"method":"GET","path":"/a/denied.git/info/refs","allow":false},{"method":"POST","path":"/a/denied.git/git-upload-pack","allow":true}]}]}`}}
	for i := 0; i < 3; i++ {
		if got := completionRepos(keys); !reflect.DeepEqual(got, []string{"a/one", "a/two"}) {
			t.Fatal(got)
		}
	}
}
