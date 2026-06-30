package client

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSyncCodexConfig_FreshFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := SyncCodexConfig(18080); err != nil {
		t.Fatalf("SyncCodexConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".codex", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got map[string]interface{}
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	want := "http://localhost:18080/proxy/openai/v1"
	if got["apiBaseUrl"] != want {
		t.Errorf("apiBaseUrl = %q, want %q", got["apiBaseUrl"], want)
	}
}

func TestSyncCodexConfig_PreservesExistingSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	existing := []byte("model: o4-mini\napproval_policy: auto-edit\n")
	if err := os.WriteFile(filepath.Join(codexDir, "config.yaml"), existing, 0600); err != nil {
		t.Fatal(err)
	}

	if err := SyncCodexConfig(18081); err != nil {
		t.Fatalf("SyncCodexConfig: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(codexDir, "config.yaml"))
	var got map[string]interface{}
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["model"] != "o4-mini" {
		t.Errorf("model = %v, want o4-mini", got["model"])
	}
	if got["approval_policy"] != "auto-edit" {
		t.Errorf("approval_policy = %v, want auto-edit", got["approval_policy"])
	}
	want := "http://localhost:18081/proxy/openai/v1"
	if got["apiBaseUrl"] != want {
		t.Errorf("apiBaseUrl = %q, want %q", got["apiBaseUrl"], want)
	}
}

func TestSyncCodexConfig_UpdatesPortOnResync(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := SyncCodexConfig(18080); err != nil {
		t.Fatal(err)
	}
	if err := SyncCodexConfig(19090); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".codex", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := "http://localhost:19090/proxy/openai/v1"
	if got["apiBaseUrl"] != want {
		t.Errorf("apiBaseUrl = %q, want %q", got["apiBaseUrl"], want)
	}
}
