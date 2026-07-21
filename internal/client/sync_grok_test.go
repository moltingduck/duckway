package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncGrokConfigWritesQuotedDefaultModelAPIKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := SyncGrokConfig([]PlaceholderKeyInfo{
		{ServiceName: "xai", EnvName: "XAI_API_KEY", Placeholder: "xai-dw_fake_placeholder"},
	}); err != nil {
		t.Fatalf("SyncGrokConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".grok", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`[model."grok-4.5"]`,
		`api_key = "xai-dw_fake_placeholder"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
}

func TestSyncGrokConfigPreservesOtherConfigAndUpdatesAPIKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	grokDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(grokDir, 0700); err != nil {
		t.Fatal(err)
	}
	existing := `[ui]
screen_mode = "minimal"

[ model."grok-4.5" ]
api_key = "xai-dw_old"
api_backend = "responses"
max_retries = 3
`
	if err := os.WriteFile(filepath.Join(grokDir, "config.toml"), []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SyncGrokConfig([]PlaceholderKeyInfo{
		{ServiceName: "xai", EnvName: "XAI_API_KEY", Placeholder: "xai-dw_new"},
	}); err != nil {
		t.Fatalf("SyncGrokConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(grokDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, `[ui]`) || !strings.Contains(got, `screen_mode = "minimal"`) {
		t.Fatalf("unrelated Grok config was not preserved:\n%s", got)
	}
	if !strings.Contains(got, `api_backend = "responses"`) || !strings.Contains(got, `max_retries = 3`) {
		t.Fatalf("existing Grok model settings were not preserved:\n%s", got)
	}
	if !strings.Contains(got, `api_key = "xai-dw_new"`) || strings.Contains(got, "xai-dw_old") {
		t.Fatalf("Grok API key was not updated cleanly:\n%s", got)
	}
	if strings.Count(got, `model."grok-4.5"`) != 1 {
		t.Fatalf("Grok model section was duplicated:\n%s", got)
	}
}

func TestSyncGrokConfigNoXAIKeyIsNoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := SyncGrokConfig([]PlaceholderKeyInfo{
		{ServiceName: "openai", EnvName: "OPENAI_API_KEY", Placeholder: "sk-dw_fake"},
	}); err != nil {
		t.Fatalf("SyncGrokConfig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".grok", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("config should not be written without xai key, stat err=%v", err)
	}
}

func TestSyncGrokConfigReturnsReadError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".grok", "config.toml"), 0700); err != nil {
		t.Fatal(err)
	}

	err := SyncGrokConfig([]PlaceholderKeyInfo{
		{ServiceName: "xai", EnvName: "XAI_API_KEY", Placeholder: "xai-dw_fake_placeholder"},
	})
	if err == nil || !strings.Contains(err.Error(), "read ") {
		t.Fatalf("SyncGrokConfig error = %v, want read error", err)
	}
}
