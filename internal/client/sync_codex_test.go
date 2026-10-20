package client

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	cfg := &Config{ServerURL: srv.URL, Token: "tok", ProxyPort: 18080}
	if err := SyncCodexAuthConfig(configDir, cfg); err != nil {
		t.Fatal(err)
	}

	assertCodexNativeOAuthMode(t, home)
	if got := logs.String(); !strings.Contains(got, "Codex phantom OAuth auth.json synced") || !strings.Contains(got, "confirm it uses this auth path") || !strings.Contains(got, "same CODEX_HOME") {
		t.Errorf("success log missing OAuth sync/path confirmation:\n%s", got)
	} else if strings.Contains(got, "falling back to API-key proxy config") {
		t.Errorf("successful OAuth sync logged fallback warning:\n%s", got)
	}
}

func TestSyncCodexOAuthCredentialsSelectsFileStore(t *testing.T) {
	for _, tc := range []struct {
		name       string
		store      string
		fresh      bool
		customHome bool
	}{
		{name: "absent config", fresh: true},
		{name: "implicit store"},
		{name: "keyring", store: "keyring"},
		{name: "auto", store: "auto"},
		{name: "ephemeral", store: "ephemeral"},
		{name: "file", store: "file"},
		{name: "CODEX_HOME", store: "keyring", customHome: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", "")
			codexDir := filepath.Join(home, ".codex")
			if tc.customHome {
				codexDir = filepath.Join(t.TempDir(), "custom-codex")
				t.Setenv("CODEX_HOME", codexDir)
			}
			if !tc.fresh {
				if err := os.MkdirAll(codexDir, 0700); err != nil {
					t.Fatal(err)
				}
				existing := "model = \"gpt-5\"\nmodel_provider = \"duckway-openai\"\n"
				if tc.store != "" {
					existing += "cli_auth_credentials_store = \"" + tc.store + "\" # select store\n"
				}
				existing += `[model_providers.duckway-openai]
name = "Duckway OpenAI"
[projects."/repo"]
model_provider = "duckway-openai"
cli_auth_credentials_store = "keyring"
trust_level = "trusted"
[mcp_servers.example] # retain this table
command = "example"
`
				if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(existing), 0644); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(testCodexCredentials())
			}))
			t.Cleanup(srv.Close)
			if !SyncCodexOAuthCredentials(t.TempDir(), &Config{ServerURL: srv.URL, Token: "tok"}) {
				t.Fatal("OAuth sync failed")
			}
			data, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			root, _, _ := strings.Cut(got, "[")
			if !strings.Contains(root, `cli_auth_credentials_store = "file"`) {
				t.Fatalf("file store missing at top level: %s", got)
			}
			if strings.Contains(root, "model_provider") || strings.Contains(got, "[model_providers.duckway-openai]") {
				t.Fatalf("Duckway API-key provider retained: %s", got)
			}
			if !tc.fresh {
				for _, want := range []string{`model = "gpt-5"`, `[projects."/repo"]
model_provider = "duckway-openai"
cli_auth_credentials_store = "keyring"
trust_level = "trusted"`, `[mcp_servers.example] # retain this table
command = "example"`} {
					if !strings.Contains(got, want) {
						t.Errorf("unrelated settings changed, missing %q: %s", want, got)
					}
				}
			}
			for _, name := range []string{"auth.json", "config.toml"} {
				info, err := os.Stat(filepath.Join(codexDir, name))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Errorf("%s mode = %o, want 600", name, info.Mode().Perm())
				}
			}
			if tc.customHome {
				if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
					t.Fatalf("default Codex home unexpectedly touched: %v", err)
				}
			}
		})
	}
}

// An open reader must keep seeing the previous complete document after sync.
// In-place truncation instead changes the contents behind that descriptor.
func TestSyncCodexOAuthCredentialsPublishesNewFiles(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	codexDir := os.Getenv("CODEX_HOME")
	previous := map[string]string{
		"auth.json":   `{"previous":"complete auth"}`,
		"config.toml": "cli_auth_credentials_store = \"keyring\"\n",
	}
	readers := map[string]*os.File{}
	for name, data := range previous {
		path := filepath.Join(codexDir, name)
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		readers[name] = f
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(testCodexCredentials())
	}))
	t.Cleanup(srv.Close)
	if !SyncCodexOAuthCredentials(t.TempDir(), &Config{ServerURL: srv.URL, Token: "tok"}) {
		t.Fatal("OAuth sync failed")
	}
	for name, f := range readers {
		data, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != previous[name] {
			t.Errorf("%s changed under an existing reader: %q", name, data)
		}
	}
	data, err := os.ReadFile(filepath.Join(codexDir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("published incomplete auth.json: %v", err)
	}
	if !reflect.DeepEqual(got, testCodexCredentials()) {
		t.Fatal("published credentials differ from server credentials")
	}
	entries, err := os.ReadDir(codexDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("unexpected publication artifacts: %v", entries)
	}
}

func TestSyncCodexOAuthCredentialsPreservesModeOnPublicationFailure(t *testing.T) {
	for _, failedFile := range []string{"auth.json", "config.toml"} {
		t.Run(failedFile, func(t *testing.T) {
			codexDir := t.TempDir()
			t.Setenv("CODEX_HOME", codexDir)
			if err := os.Mkdir(filepath.Join(codexDir, failedFile), 0700); err != nil {
				t.Fatal(err)
			}
			configDir := t.TempDir()
			modePath := CodexOAuthModePath(configDir)
			if err := os.WriteFile(modePath, []byte("oauth"), 0600); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(testCodexCredentials())
			}))
			t.Cleanup(srv.Close)
			cfg := &Config{ServerURL: srv.URL, Token: "tok", ProxyPort: 18080}
			if SyncCodexOAuthCredentials(configDir, cfg) {
				t.Fatal("sync reported success despite publication failure")
			}
			if err := SyncCodexAuthConfig(configDir, cfg); err == nil {
				t.Fatal("auth config sync swallowed publication failure")
			}
			if config, _ := os.ReadFile(filepath.Join(codexDir, "config.toml")); strings.Contains(string(config), "duckway-openai") {
				t.Fatal("publication failure switched native auth to API-key fallback")
			}
			if !CodexOAuthModeActive(configDir) {
				t.Fatal("mode marker cleared despite publication failure")
			}
			entries, err := os.ReadDir(codexDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.Contains(entry.Name(), ".tmp-") {
					t.Errorf("temporary publication artifact retained: %s", entry.Name())
				}
			}
		})
	}
}

func TestSyncCodexAuthConfigExplainsOAuthFallback(t *testing.T) {
	tests := []struct {
		name             string
		status           int
		credentials      map[string]interface{}
		body             string
		want             string
		wantConnectivity bool
	}{
		{
			name:             "credentials unavailable",
			status:           http.StatusNoContent,
			want:             "returned no usable Codex OAuth credentials",
			wantConnectivity: true,
		},
		{
			name:             "empty success response",
			status:           http.StatusOK,
			body:             "",
			want:             "cannot fetch usable Codex OAuth credentials",
			wantConnectivity: true,
		},
		{
			name:             "malformed success response",
			status:           http.StatusOK,
			body:             "not-json",
			want:             "cannot fetch usable Codex OAuth credentials",
			wantConnectivity: true,
		},
		{
			name:        "missing id token",
			status:      http.StatusOK,
			credentials: map[string]interface{}{"tokens": map[string]interface{}{"access_token": "secret-must-not-appear"}},
			want:        "missing tokens.id_token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			configDir := t.TempDir()
			writeCodexConfigWithDuckwayProvider(t, home)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/client/codex-credentials" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tt.status)
				if tt.body != "" {
					_, _ = w.Write([]byte(tt.body))
				} else if tt.credentials != nil {
					_ = json.NewEncoder(w).Encode(tt.credentials)
				}
			}))
			defer srv.Close()

			var logs bytes.Buffer
			previousOutput := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previousOutput) })
			cfg := &Config{ServerURL: srv.URL, Token: "tok", ProxyPort: 18080}
			if err := SyncCodexAuthConfig(configDir, cfg); err != nil {
				t.Fatal(err)
			}

			gotLog := logs.String()
			wants := []string{tt.want, "assigned OpenAI refreshable key", "same CODEX_HOME"}
			if tt.wantConnectivity {
				wants = append(wants, "Check server connectivity")
			}
			for _, want := range wants {
				if !strings.Contains(gotLog, want) {
					t.Errorf("log missing %q:\n%s", want, gotLog)
				}
			}
			if strings.Contains(gotLog, "secret-must-not-appear") {
				t.Errorf("log exposed credential data: %s", gotLog)
			}
			configData, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(configData), `model_provider = "duckway-openai"`) {
				t.Fatalf("API-key fallback config was not written:\n%s", configData)
			}
		})
	}
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
	root, _, _ := strings.Cut(got, "[")
	if !strings.Contains(root, `cli_auth_credentials_store = "file"`) {
		t.Fatalf("native OAuth file store was not selected: %s", got)
	}
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

// This fixture contains only synthetic Duckway claims, never live credentials.
func testManagedCodexPhantomCredentials() map[string]interface{} {
	creds := testCodexCredentials()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"duckway-phantom"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"auth0|duckway-phantom","https://duckway.dev/placeholder":"fake","https://api.openai.com/auth":{"chatgpt_account_id":"duckway-account"}}`))
	tokens := creds["tokens"].(map[string]interface{})
	tokens["access_token"] = header + "." + payload + ".signature"
	tokens["id_token"] = header + "." + payload + ".signature"
	return creds
}

func TestSyncCodexAuthConfigPreservesManagedNativeAuthOnlyOnFetchFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		preserve bool
	}{
		{"server error", 503, "secret-must-not-appear", true},
		{"rate limit", 429, "secret-must-not-appear", true},
		{"malformed", 200, "secret-must-not-appear", true},
		{"empty", 200, "", true},
		{"unassigned", 204, "", false},
		{"revoked", 403, "", false},
		{"unauthorized", 401, "", false},
		{"missing tokens", 200, `{}`, false},
		{"missing id token", 200, `{"tokens":{"access_token":"unused"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexDir := t.TempDir()
			t.Setenv("CODEX_HOME", codexDir)
			auth, err := json.Marshal(testManagedCodexPhantomCredentials())
			if err != nil {
				t.Fatal(err)
			}
			config := []byte("cli_auth_credentials_store = \"file\"\nmodel_provider = \"openai\"\n[projects.\"/repo\"]\ntrust_level = \"trusted\"\n")
			for name, data := range map[string][]byte{"auth.json": auth, "config.toml": config} {
				if err := os.WriteFile(filepath.Join(codexDir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			if err := SyncCodexAuthConfig(t.TempDir(), &Config{ServerURL: srv.URL, Token: "tok", ProxyPort: 18080}); err != nil {
				t.Fatal(err)
			}
			gotAuth, err := os.ReadFile(filepath.Join(codexDir, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			gotConfig, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotAuth, auth) {
				t.Fatal("fetch failure changed existing credentials")
			}
			if tc.preserve {
				if !bytes.Equal(gotConfig, config) {
					t.Fatal("transient failure changed managed native config")
				}
			} else if !strings.Contains(string(gotConfig), `model_provider = "duckway-openai"`) {
				t.Fatal("authoritative absence preserved native mode instead of API fallback")
			}
		})
	}
}

func TestExistingCodexPhantomOAuthRejectsUnmanagedSetups(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]interface{})
		config string
	}{
		{"real OAuth token", func(c map[string]interface{}) {
			c["tokens"].(map[string]interface{})["id_token"] = "header.payload.signature"
		}, ""},
		{"wrong account", func(c map[string]interface{}) { c["tokens"].(map[string]interface{})["account_id"] = "real-account" }, ""},
		{"missing id", func(c map[string]interface{}) { delete(c["tokens"].(map[string]interface{}), "id_token") }, ""},
		{"wrong refresh", func(c map[string]interface{}) {
			c["tokens"].(map[string]interface{})["refresh_token"] = "rt.duckway.other"
		}, ""},
		{"wrong mode", func(c map[string]interface{}) { c["auth_mode"] = "apikey" }, ""},
		{"keyring", nil, "cli_auth_credentials_store = \"keyring\"\n"},
		{"custom provider", nil, "cli_auth_credentials_store = \"file\"\nmodel_provider = \"custom\"\n"},
		{"nested file store", nil, "[projects.\"/repo\"]\ncli_auth_credentials_store = \"file\"\n"},
		{"file store only in multiline string", nil, "developer_instructions = '''\ncli_auth_credentials_store = \"file\"\n'''\ncli_auth_credentials_store = \"keyring\"\n"},
		{"custom provider after multiline string", nil, "developer_instructions = '''\n[pretend.table]\n'''\ncli_auth_credentials_store = \"file\"\nmodel_provider = \"custom\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			creds := testManagedCodexPhantomCredentials()
			if tc.change != nil {
				tc.change(creds)
			}
			auth, err := json.Marshal(creds)
			if err != nil {
				t.Fatal(err)
			}
			config := tc.config
			if config == "" {
				config = "cli_auth_credentials_store = \"file\"\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			unlock, err := lockCodexConfig()
			if err != nil {
				t.Fatal(err)
			}
			preserve := existingCodexPhantomOAuthConfigured()
			unlock()
			if preserve {
				t.Fatal("unmanaged or incomplete native setup accepted for preservation")
			}
		})
	}
}

func TestCodexConfigWritersPreserveConcurrentMCPAndNativeChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("model = \"gpt-5\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			unlock, err := lockCodexConfig()
			if err == nil {
				err = syncCodexNativeOAuthConfig()
				unlock()
			}
			errors <- err
		}()
		go func() { defer wg.Done(); errors <- writeCodexMCP(configDir, nil) }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`model = "gpt-5"`, `cli_auth_credentials_store = "file"`, `[mcp_servers.duckway-cc]`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("concurrent writer lost %q", want)
		}
	}
}

func TestCodexConfigWritersPublishAtomically(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(string) error
	}{
		{"API provider", func(string) error { return SyncCodexConfig(18080) }},
		{"disable provider", func(string) error { return DisableCodexDuckwayProvider() }},
		{"MCP", func(dir string) error { return writeCodexMCP(dir, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			config := []byte("model_provider = \"duckway-openai\"\n[model_providers.duckway-openai] # managed\nname = \"old\"\n[projects.\"/repo#project\"] # retain\ntrust_level = \"trusted\"\n")
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, config, 0644); err != nil {
				t.Fatal(err)
			}
			reader, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })
			if err := tc.write(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			old, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(old, config) {
				t.Fatal("config writer truncated the file held by an existing reader")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "[projects.\"/repo#project\"] # retain\ntrust_level = \"trusted\"") {
				t.Fatal("config writer lost unrelated table with inline comment")
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("published config permissions=%o", info.Mode().Perm())
			}
		})
	}
}
