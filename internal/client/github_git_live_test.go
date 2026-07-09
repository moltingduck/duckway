package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestGitHubAppPhantomGitPullLive(t *testing.T) {
	if os.Getenv("DUCKWAY_TEST_GITHUB_GIT_LIVE") != "1" {
		t.Skip("set DUCKWAY_TEST_GITHUB_GIT_LIVE=1 to run")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git binary is required for live GitHub git test: %v", err)
	}

	cfg := loadGitHubAppLiveConfig(t)
	credentialJSON := buildGitHubAppCredentialJSON(t, cfg)

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encryptedCredential, err := crypto.Encrypt(credentialJSON)
	if err != nil {
		t.Fatalf("encrypt github app credential: %v", err)
	}

	svcQ := queries.NewServiceQueries(db)
	clientQ := queries.NewClientQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	placeholderQ := queries.NewPlaceholderQueries(db)
	groupQ := queries.NewGroupQueries(db)
	approvalQ := queries.NewApprovalQueries(db)
	settingsQ := queries.NewSettingsQueries(db)

	ghSvc := &models.Service{
		ID:           "svc-github-git-live",
		Name:         "github",
		DisplayName:  "GitHub API + Git",
		UpstreamURL:  "https://github.com",
		HostPattern:  "api.github.com,github.com",
		AuthType:     "bearer",
		AuthHeader:   "Authorization",
		AuthPrefix:   "Bearer ",
		KeyPrefix:    "github_pat_",
		KeyLength:    93,
		DeliveryMode: "proxy",
		IsActive:     true,
	}
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatalf("create github service: %v", err)
	}

	const clientToken = "github-git-live-client-token"
	client := &models.Client{
		ID:        "client-github-git-live",
		ShortID:   "ghgitlv",
		Name:      "github-git-live",
		TokenHash: services.HashToken(clientToken),
		IsActive:  true,
	}
	if err := clientQ.Create(client); err != nil {
		t.Fatalf("create client: %v", err)
	}

	apiKey := &models.APIKey{
		ID:           "key-github-app-live",
		ServiceID:    ghSvc.ID,
		Name:         "github app live",
		KeyEncrypted: encryptedCredential,
		IsActive:     true,
	}
	if err := apiKeyQ.Create(apiKey); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	phantom, err := services.GeneratePlaceholderForRealKey(credentialJSON, ghSvc.KeyPrefix, ghSvc.KeyLength)
	if err != nil {
		t.Fatalf("generate github app phantom: %v", err)
	}
	apiKeyID := apiKey.ID
	placeholder := &models.PlaceholderKey{
		ID:          "ph-github-git-live",
		EnvName:     "GITHUB_TOKEN",
		Placeholder: phantom,
		ServiceID:   ghSvc.ID,
		APIKeyID:    &apiKeyID,
		ClientID:    client.ID,
		IsActive:    true,
	}
	if err := placeholderQ.Create(placeholder); err != nil {
		t.Fatalf("create placeholder: %v", err)
	}

	resolver := services.NewKeyResolver(crypto, apiKeyQ, placeholderQ, groupQ, approvalQ)
	proxyH := handlers.NewProxyHandler(svcQ, apiKeyQ, resolver, nil, approvalQ, settingsQ, nil)
	proxyMux := http.NewServeMux()
	proxyMux.HandleFunc("/", proxyH.Handle)
	serverMux := http.NewServeMux()
	serverMux.Handle("/proxy/", middleware.NewClientAuth(clientQ).Middleware(proxyMux))
	duckwayServer := httptest.NewServer(serverMux)
	t.Cleanup(duckwayServer.Close)

	caDir := t.TempDir()
	ca, err := services.LoadOrCreateCA(caDir)
	if err != nil {
		t.Fatalf("create mitm CA: %v", err)
	}
	caPath := filepath.Join(caDir, "ca.pem")
	if err := os.WriteFile(caPath, ca.CertPEM, 0600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}

	localProxy := &httpsProxy{
		serverURL: duckwayServer.URL,
		token:     clientToken,
		ca:        ca,
		hostMap: map[string]hostEntry{
			"github.com": {
				Service:      "github",
				DeliveryMode: "proxy",
			},
		},
		httpClient:  &http.Client{Timeout: 30 * time.Second, Transport: directTransport},
		loanCache:   make(map[string]*loanedToken),
		auditClient: &http.Client{Timeout: time.Second},
	}
	proxyServer := httptest.NewServer(localProxy)
	t.Cleanup(proxyServer.Close)

	home := t.TempDir()
	if err := DeployGitHubCredentialForGit(home, phantom); err != nil {
		t.Fatalf("deploy github phantom credential: %v", err)
	}

	repoURL := "https://github.com/" + strings.TrimSuffix(cfg.Repository, ".git") + ".git"
	cmd := exec.Command(gitBin,
		"-c", "credential.helper=store",
		"-c", "credential.useHttpPath=false",
		"ls-remote", "--exit-code", repoURL, "HEAD",
	)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSL_CAINFO="+caPath,
		"HTTPS_PROXY="+proxyServer.URL,
		"HTTP_PROXY="+proxyServer.URL,
		"https_proxy="+proxyServer.URL,
		"http_proxy="+proxyServer.URL,
		"NO_PROXY=",
		"no_proxy=",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		sanitized := strings.ReplaceAll(string(output), phantom, "[phantom]")
		t.Fatalf("git ls-remote through duckway phantom proxy failed: %v\n%s", err, sanitized)
	}
	if !strings.Contains(string(output), "HEAD") {
		t.Fatalf("git ls-remote output did not include HEAD: %q", string(output))
	}
}

func TestDuckwayGitCloneLive(t *testing.T) {
	if os.Getenv("DUCKWAY_TEST_GITHUB_GIT_LIVE") != "1" {
		t.Skip("set DUCKWAY_TEST_GITHUB_GIT_LIVE=1 to run")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git binary is required for live duckway git clone test: %v", err)
	}

	cfg := loadGitHubAppLiveConfig(t)
	credentialJSON := buildGitHubAppCredentialJSON(t, cfg)
	phantom, serverURL, clientToken, ca, caPEM := startGitHubGitLiveDuckwayServer(t, credentialJSON, cfg.Repository)

	localProxy := &httpsProxy{
		serverURL: serverURL,
		token:     clientToken,
		ca:        ca,
		hostMap: map[string]hostEntry{
			"github.com": {
				Service:      "github",
				DeliveryMode: "proxy",
			},
		},
		httpClient:  &http.Client{Timeout: 30 * time.Second, Transport: directTransport},
		loanCache:   make(map[string]*loanedToken),
		auditClient: &http.Client{Timeout: time.Second},
	}
	proxyServer := httptest.NewServer(localProxy)
	t.Cleanup(proxyServer.Close)
	proxyPort := portFromTestServerURL(t, proxyServer.URL)

	root := findRepoRoot(t)
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "duckway")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/client")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build duckway client: %v\n%s", err, out)
	}

	home := t.TempDir()
	configDir := filepath.Join(home, ".duckway")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(configDir, &Config{
		ServerURL:  serverURL,
		ClientName: "github-git-clone-live",
		Token:      clientToken,
		ProxyPort:  proxyPort,
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "ca.pem"), caPEM, 0600); err != nil {
		t.Fatalf("write config CA: %v", err)
	}

	workDir := t.TempDir()
	cloneDir := "repo-clone"
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "git", "clone", cfg.Repository, cloneDir)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"DUCKWAY_CONFIG_DIR="+configDir,
		"GIT_TERMINAL_PROMPT=0",
		"NO_PROXY=",
		"no_proxy=",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		sanitized := strings.ReplaceAll(string(output), phantom, "[phantom]")
		t.Fatalf("duckway git clone failed: %v\n%s", err, sanitized)
	}
	if _, err := os.Stat(filepath.Join(workDir, cloneDir, ".git")); err != nil {
		t.Fatalf("clone did not create .git directory: %v\n%s", err, output)
	}
}

func startGitHubGitLiveDuckwayServer(t *testing.T, credentialJSON, repo string) (phantom, serverURL, clientToken string, ca *services.CAManager, caPEM []byte) {
	t.Helper()
	normalizedRepo := strings.TrimSuffix(strings.TrimPrefix(repo, "https://github.com/"), ".git")
	parts := strings.Split(normalizedRepo, "/")
	if len(parts) != 2 {
		t.Fatalf("live repository must be OWNER/REPO, got %q", repo)
	}

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encryptedCredential, err := crypto.Encrypt(credentialJSON)
	if err != nil {
		t.Fatalf("encrypt github app credential: %v", err)
	}

	svcQ := queries.NewServiceQueries(db)
	clientQ := queries.NewClientQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	placeholderQ := queries.NewPlaceholderQueries(db)
	groupQ := queries.NewGroupQueries(db)
	approvalQ := queries.NewApprovalQueries(db)
	settingsQ := queries.NewSettingsQueries(db)

	ghSvc := &models.Service{
		ID:           "svc-github-git-live",
		Name:         "github",
		DisplayName:  "GitHub API + Git",
		UpstreamURL:  "https://api.github.com",
		HostPattern:  "api.github.com,github.com",
		AuthType:     "bearer",
		AuthHeader:   "Authorization",
		AuthPrefix:   "Bearer ",
		KeyPrefix:    "github_pat_",
		KeyLength:    93,
		DeliveryMode: "proxy",
		IsActive:     true,
	}
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatalf("create github service: %v", err)
	}

	clientToken = "github-git-live-client-token"
	client := &models.Client{
		ID:        "client-github-git-live",
		ShortID:   "ghgitlv",
		Name:      "github-git-live",
		TokenHash: services.HashToken(clientToken),
		IsActive:  true,
	}
	if err := clientQ.Create(client); err != nil {
		t.Fatalf("create client: %v", err)
	}

	apiKey := &models.APIKey{
		ID:           "key-github-app-live",
		ServiceID:    ghSvc.ID,
		Name:         "github app live",
		KeyEncrypted: encryptedCredential,
		IsActive:     true,
	}
	if err := apiKeyQ.Create(apiKey); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	phantom, err = services.GeneratePlaceholderForRealKey(credentialJSON, ghSvc.KeyPrefix, ghSvc.KeyLength)
	if err != nil {
		t.Fatalf("generate github app phantom: %v", err)
	}
	acl := githubGitLivePermissionConfig(normalizedRepo)
	apiKeyID := apiKey.ID
	placeholder := &models.PlaceholderKey{
		ID:               "ph-github-git-live",
		EnvName:          "GITHUB_TOKEN",
		Placeholder:      phantom,
		ServiceID:        ghSvc.ID,
		APIKeyID:         &apiKeyID,
		ClientID:         client.ID,
		PermissionConfig: &acl,
		IsActive:         true,
	}
	if err := placeholderQ.Create(placeholder); err != nil {
		t.Fatalf("create placeholder: %v", err)
	}

	resolver := services.NewKeyResolver(crypto, apiKeyQ, placeholderQ, groupQ, approvalQ)
	proxyH := handlers.NewProxyHandler(svcQ, apiKeyQ, resolver, nil, approvalQ, settingsQ, nil)
	proxyMux := http.NewServeMux()
	proxyMux.HandleFunc("/", proxyH.Handle)
	serverMux := http.NewServeMux()
	serverMux.Handle("/proxy/", middleware.NewClientAuth(clientQ).Middleware(proxyMux))
	serverMux.HandleFunc("/client/keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Duckway-Token") != clientToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode([]PlaceholderKeyInfo{{
			EnvName:          "GITHUB_TOKEN",
			Placeholder:      phantom,
			ServiceName:      "github",
			PermissionConfig: acl,
		}})
	})
	serverMux.HandleFunc("/client/canaries", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	})
	serverMux.HandleFunc("/client/cc", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	duckwayServer := httptest.NewServer(serverMux)
	t.Cleanup(duckwayServer.Close)

	caDir := t.TempDir()
	ca, err = services.LoadOrCreateCA(caDir)
	if err != nil {
		t.Fatalf("create mitm CA: %v", err)
	}
	return phantom, duckwayServer.URL, clientToken, ca, ca.CertPEM
}

func githubGitLivePermissionConfig(repo string) string {
	return `{"version":"1","provider":"github","rules":[{"name":"live-read","endpoints":[` +
		`{"method":"GET","path":"/` + repo + `.git/info/refs","allow":true},` +
		`{"method":"POST","path":"/` + repo + `.git/git-upload-pack","allow":true},` +
		`{"method":"GET","path":"/repos/` + repo + `","allow":true}` +
		`],"deny_all_other":true}]}`
}

func portFromTestServerURL(t *testing.T, rawURL string) int {
	t.Helper()
	idx := strings.LastIndex(rawURL, ":")
	if idx < 0 {
		t.Fatalf("test server URL has no port: %s", rawURL)
	}
	port, err := strconv.Atoi(rawURL[idx+1:])
	if err != nil {
		t.Fatalf("parse proxy port from %s: %v", rawURL, err)
	}
	return port
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", wd)
		}
	}
}

type githubAppLiveConfig struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKey     string `json:"private_key"`
	Repository     string `json:"repository"`
	BaseURL        string `json:"base_url,omitempty"`
}

func loadGitHubAppLiveConfig(t *testing.T) githubAppLiveConfig {
	t.Helper()
	configPath := os.Getenv("DUCKWAY_GITHUB_APP_LIVE_CONFIG")
	if configPath == "" {
		configPath = findGitHubAppLiveConfigFrom(t, "secrets/github-app-live.json")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read live config %s: %v", configPath, err)
	}
	var cfg githubAppLiveConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse live config %s: %v", configPath, err)
	}
	if cfg.AppID <= 0 || cfg.InstallationID <= 0 || strings.TrimSpace(cfg.PrivateKey) == "" || strings.TrimSpace(cfg.Repository) == "" {
		t.Fatalf("live config %s requires app_id, installation_id, private_key, and repository", configPath)
	}
	return cfg
}

func findGitHubAppLiveConfigFrom(t *testing.T, rel string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return rel
}

func buildGitHubAppCredentialJSON(t *testing.T, cfg githubAppLiveConfig) string {
	t.Helper()
	credential := map[string]interface{}{
		"type":            "github_app",
		"app_id":          cfg.AppID,
		"installation_id": cfg.InstallationID,
		"private_key":     cfg.PrivateKey,
	}
	if cfg.BaseURL != "" {
		credential["base_url"] = cfg.BaseURL
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatalf("marshal github app credential: %v", err)
	}
	return string(raw)
}
