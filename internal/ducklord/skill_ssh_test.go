package ducklord

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeSkillSSH(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ssh")
	// The fixture executes only the final remote-command argument. SSH options
	// and the destination remain ordinary argv values, so hostile client data
	// cannot become local shell syntax.
	script := "#!/bin/sh\nlast=\"\"\nfor arg do last=\"$arg\"; done\nlast=$(printf '%s' \"$last\" | sed \"s/^'//; s/'$//\")\nlast=\"${last#/usr/local/bin/ducklion}\"\nexec go run ./../../cmd/ducklion $last\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeSkillSSHWithTarMarker(t *testing.T, marker string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ssh")
	// Keep the command execution identical to fakeSkillSSH, while putting a
	// tar wrapper first in PATH so tests can prove validation runs before tar.
	script := "#!/bin/sh\nlast=\"\"\nfor arg do last=\"$arg\"; done\nlast=$(printf '%s' \"$last\" | sed \"s/^'//; s/'$//\")\nlast=\"${last#/usr/local/bin/ducklion}\"\nPATH=\"$(dirname \"$0\"):$PATH\"\nexport PATH\nexec go run ./../../cmd/ducklion $last\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	tar := filepath.Join(dir, "tar")
	tarScript := "#!/bin/sh\ntouch " + shellQuote(marker) + "\nexec /usr/bin/tar \"$@\"\n"
	if err := os.WriteFile(tar, []byte(tarScript), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSkillSSHUploadDownloadNestedFiles(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(repo, "nested")
	if err := os.MkdirAll(filepath.Join(skill, "docs", "deep"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "docs", "deep", "example.txt"), []byte("nested"), 0600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "remote")
	if err := os.Mkdir(remote, 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Name: "safe-name", Host: "host", SSH: fakeSkillSSH(t)}
	if _, err := UploadSkillSSH(context.Background(), client, repo, "nested", remote); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(remote, "nested", "docs", "deep", "example.txt")); err != nil || string(got) != "nested" {
		t.Fatalf("uploaded nested file = %q, %v", got, err)
	}
	if err := os.RemoveAll(filepath.Join(repo, "nested")); err != nil {
		t.Fatal(err)
	}
	if _, err := DownloadSkillSSH(context.Background(), client, repo, "nested", remote); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "nested", "docs", "deep", "example.txt")); err != nil || string(got) != "nested" {
		t.Fatalf("downloaded nested file = %q, %v", got, err)
	}
}

func TestSkillSSHRejectsHostileValuesWithoutShellInjection(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Name: "client; touch " + filepath.Join(root, "name-pwned"), Host: "host; touch " + filepath.Join(root, "host-pwned"), SSH: fakeSkillSSH(t)}
	if _, err := UploadSkillSSH(context.Background(), client, repo, "bad/id", filepath.Join(root, "target; touch "+filepath.Join(root, "path-pwned"))); err == nil || !strings.Contains(err.Error(), "unsafe skill identifier") {
		t.Fatalf("hostile identifier error = %v", err)
	}
	if _, err := DownloadSkillSSH(context.Background(), client, repo, "safe", filepath.Join(root, "target; touch "+filepath.Join(root, "path-pwned"))); err == nil || !strings.Contains(err.Error(), "ssh skill transfer") {
		t.Fatalf("hostile target error = %v", err)
	}
	for _, marker := range []string{"name-pwned", "host-pwned", "path-pwned"} {
		if _, err := os.Stat(filepath.Join(root, marker)); err == nil {
			t.Fatalf("hostile value executed shell marker %s", marker)
		}
	}
}

func TestSkillSSHRejectsSymlinkedRemoteTargetAncestors(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "safe"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "safe", "SKILL.md"), []byte("manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(actual, "target"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	client := Client{Name: "safe-name", Host: "host", SSH: fakeSkillSSH(t)}
	if _, err := UploadSkillSSH(context.Background(), client, repo, "safe", filepath.Join(link, "target")); err == nil {
		t.Fatal("upload accepted symlinked remote target ancestor")
	}
	if _, err := DownloadSkillSSH(context.Background(), client, repo, "safe", filepath.Join(link, "target")); err == nil {
		t.Fatal("download accepted symlinked remote target ancestor")
	}
}

func TestSkillSSHRejectsSymlinkedRequestedRemoteSkillBeforeTar(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "remote")
	if err := os.Mkdir(remote, 0700); err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(remote, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actual, "SKILL.md"), []byte("manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	requested := filepath.Join(remote, "requested")
	if err := os.Symlink(actual, requested); err != nil {
		t.Fatal(err)
	}
	tarMarker := filepath.Join(root, "tar-ran")
	client := Client{Name: "safe-name", Host: "host", SSH: fakeSkillSSHWithTarMarker(t, tarMarker)}
	if _, err := DownloadSkillSSH(context.Background(), client, repo, "requested", remote); err == nil || !strings.Contains(err.Error(), "requested skill is invalid") {
		t.Fatalf("symlinked requested skill error = %v", err)
	}
	if _, err := os.Stat(tarMarker); err == nil {
		t.Fatal("tar ran before symlink validation rejected requested skill")
	} else if !os.IsNotExist(err) {
		t.Fatalf("tar marker stat = %v", err)
	}
}

func TestRunSkillSSHCapsStderr(t *testing.T) {
	stderr := &skillSSHStderr{limit: maxSkillSSHStderrBytes}
	if n, err := stderr.Write([]byte(strings.Repeat("x", maxSkillSSHStderrBytes*2))); err != nil || n != maxSkillSSHStderrBytes*2 {
		t.Fatalf("stderr write = (%d, %v)", n, err)
	}
	if stderr.Len() != maxSkillSSHStderrBytes || !stderr.truncated {
		t.Fatalf("stderr buffer length=%d truncated=%v", stderr.Len(), stderr.truncated)
	}
}
