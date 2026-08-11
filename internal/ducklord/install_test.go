package ducklord

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeRemoteInstallPath(t *testing.T) {
	for _, path := range []string{
		"~/.local/bin/ducklion",
		"/usr/local/bin/ducklion",
		"/opt/duckway/bin/ducklion",
	} {
		if !safeRemoteInstallPath(path) {
			t.Fatalf("path %q rejected", path)
		}
	}
	for _, path := range []string{
		"",
		"ducklion",
		"-/tmp/ducklion",
		"~/bin/duck lion",
		"~/bin/ducklion;id",
		"~/bin/$(id)",
		"~/bin/`id`",
	} {
		if safeRemoteInstallPath(path) {
			t.Fatalf("path %q accepted", path)
		}
	}
}

func TestRemoteDucklionInstallScriptExpandsTildeDest(t *testing.T) {
	home := t.TempDir()
	cmd := exec.Command("sh", "-lc", remoteDucklionInstallScript, "ducklord-install-ducklion", "~/.local/bin/ducklion")
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader("#!/bin/sh\necho ducklion fake \"$1\"\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("install script failed: %v stderr=%s", err, stderr.String())
	}
	installed := filepath.Join(home, ".local", "bin", "ducklion")
	if !strings.Contains(string(out), "DUCKLION_INSTALLED\t"+installed) {
		t.Fatalf("install output = %q, want installed path %q", out, installed)
	}
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("installed file missing: %v", err)
	}
}
