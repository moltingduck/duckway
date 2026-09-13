package ducklord

import (
	"bytes"
	"crypto/sha256"
	"fmt"
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
	fixture := []byte("#!/bin/sh\nif [ \"$1\" = management ]; then mv \"$0\" \"$3\"; printf 'DUCKLION_INSTALLED\\t%s\\n' \"$3\"; elif [ \"$1\" = daemon ]; then echo running=true; else echo ducklion fake; fi\n")
	cmd := exec.Command("sh", "-lc", remoteDucklionInstallScript, "ducklord-install-ducklion", "~/.local/bin/ducklion", fmt.Sprintf("%x", sha256.Sum256(fixture)))
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = bytes.NewReader(fixture)
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
