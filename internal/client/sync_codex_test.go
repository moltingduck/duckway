package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncCodexConfig_FreshFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := SyncCodexConfig(18080); err != nil {
		t.Fatalf("SyncCodexConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`model_provider = "duckway-openai"`,
		`[model_providers.duckway-openai]`,
		`name = "Duckway OpenAI"`,
		`base_url = "http://localhost:18080/proxy/openai/v1"`,
		`env_key = "OPENAI_API_KEY"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
}

func TestSyncCodexConfig_PreservesExistingSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	existing := []byte(`model = "o4-mini"
approval_policy = "auto-edit"

[projects."/repo"]
trust_level = "trusted"
`)
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), existing, 0600); err != nil {
		t.Fatal(err)
	}

	if err := SyncCodexConfig(18081); err != nil {
		t.Fatalf("SyncCodexConfig: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	got := string(data)
	for _, want := range []string{
		`model = "o4-mini"`,
		`approval_policy = "auto-edit"`,
		`model_provider = "duckway-openai"`,
		`[projects."/repo"]`,
		`base_url = "http://localhost:18081/proxy/openai/v1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
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

	data, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "http://localhost:18080/proxy/openai/v1") {
		t.Errorf("old port still present:\n%s", got)
	}
	if !strings.Contains(got, `base_url = "http://localhost:19090/proxy/openai/v1"`) {
		t.Errorf("new port missing:\n%s", got)
	}
}

func TestDisableCodexDuckwayProviderPreservesOtherConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(codexDir, "config.toml")
	existing := `model = "gpt-5"
model_provider = "duckway-openai"

[model_providers.duckway-openai]
name = "Duckway OpenAI"
base_url = "http://localhost:18080/proxy/openai/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"

[mcp_servers.duckway-cc]
command = "duckway"
`
	if err := os.WriteFile(configPath, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	if err := DisableCodexDuckwayProvider(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, gone := range []string{`model_provider = "duckway-openai"`, `[model_providers.duckway-openai]`, `env_key = "OPENAI_API_KEY"`} {
		if strings.Contains(got, gone) {
			t.Fatalf("config still contains %q:\n%s", gone, got)
		}
	}
	for _, want := range []string{`model = "gpt-5"`, `[mcp_servers.duckway-cc]`, `command = "duckway"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
}
