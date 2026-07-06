package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestSyncCodexAuthConfigUsesNativeOAuthWhenCredentialsExist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := t.TempDir()
	writeCodexConfigWithDuckwayProvider(t, home)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/codex-credentials" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(testCodexCredentials())
	}))
	defer srv.Close()

	cfg := &Config{ServerURL: srv.URL, Token: "tok", ProxyPort: 18080}
	if err := SyncCodexAuthConfig(configDir, cfg); err != nil {
		t.Fatal(err)
	}

	assertCodexNativeOAuthMode(t, home)
}

func TestCCWatchCodexOAuthSyncUsesNativeOAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := t.TempDir()
	writeCodexConfigWithDuckwayProvider(t, home)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/codex-credentials" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(testCodexCredentials())
	}))
	defer srv.Close()

	w := &CCWatch{
		cfg:        &Config{ServerURL: srv.URL, Token: "tok", ProxyPort: 18080},
		configDir:  configDir,
		agentTypes: map[string]string{"cc-codex": "codex"},
	}
	w.syncCodexOAuthForCC("cc-codex")

	assertCodexNativeOAuthMode(t, home)
}

func TestBuildHostMapAddsOpenAIAuthVirtualService(t *testing.T) {
	hostMap := buildHostMap([]ServiceInfo{
		{Name: "openai", HostPattern: "api.openai.com", UpstreamURL: "https://api.openai.com"},
	})
	entry, ok := hostMap["auth.openai.com"]
	if !ok {
		t.Fatalf("auth.openai.com missing from host map: %#v", hostMap)
	}
	if entry.Service != "openai-auth" || entry.DeliveryMode != "proxy" || entry.UpstreamURL != "https://auth.openai.com" {
		t.Fatalf("unexpected auth host entry: %#v", entry)
	}
	if hostMap["api.openai.com"].Service != "openai" {
		t.Fatalf("regular service host was not preserved: %#v", hostMap["api.openai.com"])
	}
	chatgpt, ok := hostMap["chatgpt.com"]
	if !ok {
		t.Fatalf("chatgpt.com missing from host map: %#v", hostMap)
	}
	if chatgpt.Service != "openai-chatgpt" || chatgpt.DeliveryMode != "proxy" || chatgpt.UpstreamURL != "https://chatgpt.com" {
		t.Fatalf("unexpected chatgpt host entry: %#v", chatgpt)
	}
}

func writeCodexConfigWithDuckwayProvider(t *testing.T, home string) {
	t.Helper()
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
}

func testCodexCredentials() map[string]interface{} {
	return map[string]interface{}{
		"auth_mode": "chatgpt",
		"tokens": map[string]interface{}{
			"id_token":      "header.payload.signature",
			"access_token":  "header.payload.signature",
			"refresh_token": "rt.duckway.fake",
			"account_id":    "duckway-account",
		},
	}
}

func assertCodexNativeOAuthMode(t *testing.T, home string) {
	t.Helper()
	authData, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if !strings.Contains(string(authData), `"auth_mode": "chatgpt"`) {
		t.Fatalf("auth.json missing native Codex OAuth shape:\n%s", string(authData))
	}

	configData, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	got := string(configData)
	for _, gone := range []string{
		`model_provider = "duckway-openai"`,
		`[model_providers.duckway-openai]`,
		`env_key = "OPENAI_API_KEY"`,
	} {
		if strings.Contains(got, gone) {
			t.Fatalf("config still contains %q:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, `[mcp_servers.duckway-cc]`) {
		t.Fatalf("codex mcp config was not preserved:\n%s", got)
	}
}
