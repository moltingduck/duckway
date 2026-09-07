package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/client"
)

func TestConfigCommandSetsRetentionAndRequiresManualRestart(t *testing.T) {
	dir := t.TempDir()
	if err := client.SaveConfig(dir, &client.Config{ServerURL: "https://duckway.example", ClientName: "host", Token: "secret", ProxyPort: 18080}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runConfigCommand(dir, []string{"set", "pty_log_retention_days", "14"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "7 -> 14") || !strings.Contains(output.String(), "no automatic restart") {
		t.Fatalf("output=%q", output.String())
	}
	cfg, err := client.LoadConfig(dir)
	if err != nil || cfg.PTYLogRetentionDays != 14 {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
	info, err := os.Stat(filepath.Join(dir, "config.yaml"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
	output.Reset()
	if err := runConfigCommand(dir, []string{"get", "pty_log_retention_days"}, &output); err != nil || output.String() != "14\n" {
		t.Fatalf("get=%q err=%v", output.String(), err)
	}
}

func TestConfigCommandRejectsUnknownOrUnsafeValuesWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	if err := client.SaveConfig(dir, &client.Config{PTYLogRetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	for _, args := range [][]string{{"set", "unknown", "7"}, {"set", "pty_log_retention_days", "0"}, {"set", "pty_log_retention_days", "3651"}} {
		if err := runConfigCommand(dir, args, &bytes.Buffer{}); err == nil {
			t.Fatalf("args %q accepted", args)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("rejected config command modified config.yaml")
	}
}

func TestConfigCommandRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "sentinel.yaml")
	before := []byte("server_url: https://sentinel.example\npty_log_retention_days: 7\n")
	if err := os.WriteFile(sentinel, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := runConfigCommand(dir, []string{"set", "pty_log_retention_days", "14"}, &bytes.Buffer{}); err == nil {
		t.Fatal("symlink config accepted")
	}
	after, err := os.ReadFile(sentinel)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("sentinel=%q err=%v", after, err)
	}
}
