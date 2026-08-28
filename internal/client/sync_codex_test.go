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
		`base_url = "http://127.0.0.1:18080/proxy/openai/v1"`,
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
		`base_url = "http://127.0.0.1:18081/proxy/openai/v1"`,
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
	if strings.Contains(got, "http://127.0.0.1:18080/proxy/openai/v1") {
		t.Errorf("old port still present:\n%s", got)
	}
	if !strings.Contains(got, `base_url = "http://127.0.0.1:19090/proxy/openai/v1"`) {
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
base_url = "http://127.0.0.1:18080/proxy/openai/v1"
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
	assigned := true
	unassigned := false
	hostMap := buildHostMap([]ServiceInfo{
		{Name: "openai", HostPattern: "api.openai.com", UpstreamURL: "https://api.openai.com", Assigned: &assigned},
		{Name: "xai", HostPattern: "api.x.ai,cli-chat-proxy.grok.com", UpstreamURL: "https://api.x.ai", Assigned: &unassigned},
		{Name: "discord", HostPattern: "discord.com,GATEWAY.DISCORD.GG.", UpstreamURL: "https://discord.com/api/v10"},
	})
	entry, ok := hostMap["auth.openai.com"]
	if !ok {
		t.Fatalf("auth.openai.com missing from host map: %#v", hostMap)
	}
	if entry.Service != "openai-auth" || entry.DeliveryMode != "proxy" || entry.UpstreamURL != "https://auth.openai.com" {
		t.Fatalf("unexpected auth host entry: %#v", entry)
	}
	if !entry.AssignmentKnown || !entry.Assigned {
		t.Fatalf("OpenAI auth virtual host did not inherit assignment: %#v", entry)
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
	grok, ok := hostMap["cli-chat-proxy.grok.com"]
	if !ok {
		t.Fatalf("cli-chat-proxy.grok.com missing from host map: %#v", hostMap)
	}
	if grok.Service != "xai-grok" || grok.DeliveryMode != "proxy" || grok.UpstreamURL != "https://cli-chat-proxy.grok.com" {
		t.Fatalf("unexpected Grok host entry: %#v", grok)
	}
	if !grok.AssignmentKnown || grok.Assigned {
		t.Fatalf("Grok virtual host did not inherit unassigned state: %#v", grok)
	}
	if hostMap["api.x.ai"].Service != "xai" || hostMap["api.x.ai"].UpstreamURL != "https://api.x.ai" {
		t.Fatalf("regular xAI API host was not preserved: %#v", hostMap["api.x.ai"])
	}
	discordGateway, ok := hostMap["gateway.discord.gg"]
	if !ok || !discordGateway.TunnelOnly || discordGateway.TunnelPort != "443" {
		t.Fatalf("Discord Gateway must be a port-443 transparent tunnel: %#v", discordGateway)
	}
}

func TestBuildHostMapSkipsGrokVirtualServiceWithoutXAIHostPattern(t *testing.T) {
	for _, svcs := range [][]ServiceInfo{
		{{Name: "openai", HostPattern: "api.openai.com", UpstreamURL: "https://api.openai.com"}},
		{{Name: "xai", HostPattern: "api.x.ai", UpstreamURL: "https://api.x.ai"}},
	} {
		hostMap := buildHostMap(svcs)
		if _, ok := hostMap["cli-chat-proxy.grok.com"]; ok {
			t.Fatalf("Grok virtual host should not be present for services %#v: %#v", svcs, hostMap)
		}
	}
}

func TestServiceRoutingCachePreservesAssignmentState(t *testing.T) {
	dir := t.TempDir()
	assigned := true
	services := []ServiceInfo{{
		Name: "openai", HostPattern: "api.openai.com",
		UpstreamURL: "https://api.openai.com", Assigned: &assigned,
	}}
	if err := saveServiceMetadata(dir, services); err != nil {
		t.Fatal(err)
	}
	hosts, err := cachedServiceHostMap(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"api.openai.com", "auth.openai.com", "chatgpt.com"} {
		entry := hosts[host]
		if !entry.AssignmentKnown || !entry.Assigned {
			t.Fatalf("%s did not preserve assignment: %#v", host, entry)
		}
	}
	info, err := os.Stat(filepath.Join(dir, serviceMetadataCacheFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("cache mode=%o, want 600", info.Mode().Perm())
	}
}

func TestBuildHostMapAddsGitHubSmartHTTPAliasForLegacyMetadata(t *testing.T) {
	assigned := true
	hosts := buildHostMap([]ServiceInfo{{
		Name: "github", HostPattern: "api.github.com", UpstreamURL: "https://api.github.com",
		DeliveryMode: "proxy", Assigned: &assigned,
	}})
	entry, ok := hosts["github.com"]
	if !ok || entry.Service != "github" || !entry.AssignmentKnown || !entry.Assigned {
		t.Fatalf("github.com legacy alias = %#v, ok=%v", entry, ok)
	}
}

func TestDefaultManagedServicesFailClosedForPhantom(t *testing.T) {
	hosts := buildHostMap(defaultManagedServices())
	entry := hosts["api.openai.com"]
	if entry.AssignmentKnown {
		t.Fatalf("default metadata unexpectedly has known assignment: %#v", entry)
	}
	status, _ := phantomAssignmentError(entry)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", status, http.StatusServiceUnavailable)
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
base_url = "http://127.0.0.1:18080/proxy/openai/v1"
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
