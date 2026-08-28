package client

import (
	"context"
	"crypto"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

var githubInstallationTokenPattern = regexp.MustCompile(`ghs_[A-Za-z0-9_]{20,}`)
var githubLiveRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type githubGitLiveDuckwayEnv struct {
	cfg         githubAppLiveConfig
	repos       []string
	phantom     string
	serverURL   string
	clientToken string
	proxyURL    string
	proxyPort   int
	caPEM       []byte
	binPath     string
	home        string
	configDir   string
}

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
	acl := githubGitLivePermissionConfig(strings.TrimSuffix(strings.TrimPrefix(cfg.Repository, "https://github.com/"), ".git"))
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
				Service:         "github",
				DeliveryMode:    "proxy",
				AssignmentKnown: true,
				Assigned:        true,
			},
		},
		httpClient:  &http.Client{Transport: directTransport},
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin,
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
		sanitized := sanitizeGitHubLiveOutput(string(output), phantom, clientToken, cfg.PrivateKey)
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
	env := newGitHubGitLiveDuckwayEnv(t)

	workDir := t.TempDir()
	cloneDir := "repo-clone"
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := liveCommand(ctx, env.binPath, "git", "clone", env.cfg.Repository, cloneDir)
	cmd.Dir = workDir
	cmd.Env = env.duckwayCommandEnv()
	start := time.Now()
	output, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		sanitized := env.sanitizeOutput(string(output))
		t.Fatalf("duckway git clone failed: %v\n%s", err, sanitized)
	}
	t.Logf("duckway git clone live elapsed=%s", elapsed.Round(time.Millisecond))
	if _, err := os.Stat(filepath.Join(workDir, cloneDir, ".git")); err != nil {
		t.Fatalf("clone did not create .git directory: %v\n%s", err, output)
	}
	verifyClone := liveCommand(ctx, "git", "-C", filepath.Join(workDir, cloneDir), "fsck", "--full")
	if verifyOutput, err := verifyClone.CombinedOutput(); err != nil {
		t.Fatalf("cloned repository failed git fsck: %v\n%s", err, verifyOutput)
	}
	assertGitConfigValue(t, filepath.Join(workDir, cloneDir), "remote.origin.url", "https://github.com/"+strings.TrimSuffix(env.cfg.Repository, ".git")+".git")
	assertGitConfigValue(t, filepath.Join(workDir, cloneDir), "http.https://github.com/.proxy", fmt.Sprintf("http://127.0.0.1:%d", env.proxyPort))
	assertGitConfigValue(t, filepath.Join(workDir, cloneDir), "http.https://github.com/.sslCAInfo", filepath.Join(env.configDir, "ca.pem"))
	assertGitConfigValue(t, filepath.Join(workDir, cloneDir), "credential.helper", "store")
	assertGitConfigValue(t, filepath.Join(workDir, cloneDir), "credential.useHttpPath", "true")
	assertGitConfigValue(t, filepath.Join(workDir, cloneDir), "http.emptyAuth", "true")

	native := liveCommand(ctx, "git", "-C", filepath.Join(workDir, cloneDir), "ls-remote", "--exit-code", "origin", "HEAD")
	native.Env = env.gitProxyEnv()
	nativeOutput, err := native.CombinedOutput()
	if err != nil {
		sanitized := env.sanitizeOutput(string(nativeOutput))
		t.Fatalf("native git after duckway git clone failed: %v\n%s", err, sanitized)
	}
}

func TestDuckwayGitCloneMultipleReposLive(t *testing.T) {
	if os.Getenv("DUCKWAY_TEST_GITHUB_GIT_LIVE") != "1" {
		t.Skip("set DUCKWAY_TEST_GITHUB_GIT_LIVE=1 to run")
	}
	cfg := loadGitHubAppLiveConfig(t)
	repos := firstTwoGitHubLiveInstallationRepos(t, cfg)
	env := newGitHubGitLiveDuckwayEnvForRepos(t, cfg, repos)
	if len(repos) < 2 {
		t.Fatalf("GitHub App installation lists %d repositories; configure at least two repositories for the live test installation", len(repos))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(repos))*90*time.Second)
	defer cancel()
	for i, repo := range repos {
		t.Run(fmt.Sprintf("repo_index_%d", i), func(t *testing.T) {
			workDir := t.TempDir()
			cloneDir := fmt.Sprintf("repo-clone-%d", i)
			cmd := liveCommand(ctx, env.binPath, "git", "clone", repo, cloneDir)
			cmd.Dir = workDir
			cmd.Env = env.duckwayCommandEnv()
			start := time.Now()
			output, err := cmd.CombinedOutput()
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("duckway git clone repo index %d failed: %v\n%s", i, err, env.sanitizeOutput(string(output)))
			}
			t.Logf("duckway git clone repo index %d elapsed=%s", i, elapsed.Round(time.Millisecond))
			if _, err := os.Stat(filepath.Join(workDir, cloneDir, ".git")); err != nil {
				t.Fatalf("clone did not create .git directory: %v\n%s", err, env.sanitizeOutput(string(output)))
			}
		})
	}
}

func TestDuckwayGitCloneLiveSeedRepository(t *testing.T) {
	if os.Getenv("DUCKWAY_TEST_GITHUB_GIT_SEED_LIVE") != "1" {
		t.Skip("set DUCKWAY_TEST_GITHUB_GIT_SEED_LIVE=1 to seed the live GitHub repository")
	}
	env := newGitHubGitLiveDuckwayEnv(t)
	seed := githubGitLiveSeedSpecFromEnv(t)
	path, changed := seedGitHubLiveBenchmarkData(t, env, seed)
	if changed {
		t.Logf("seeded live GitHub repo %s with %s", env.cfg.Repository, path)
	} else {
		t.Logf("live GitHub repo %s already has benchmark seed %s", env.cfg.Repository, path)
	}
}

func BenchmarkDuckwayGitCloneLive(b *testing.B) {
	if os.Getenv("DUCKWAY_TEST_GITHUB_GIT_BENCH_LIVE") != "1" {
		b.Skip("set DUCKWAY_TEST_GITHUB_GIT_BENCH_LIVE=1 to run")
	}
	env := newGitHubGitLiveDuckwayEnv(b)
	seed := githubGitLiveSeedSpecFromEnv(b)
	if os.Getenv("DUCKWAY_TEST_GITHUB_GIT_SEED_LIVE") == "1" {
		seedGitHubLiveBenchmarkData(b, env, seed)
	} else {
		b.Log("benchmarking current live repository contents; set DUCKWAY_TEST_GITHUB_GIT_SEED_LIVE=1 to seed random benchmark data first")
	}
	b.ReportAllocs()
	b.ResetTimer()
	var totalCloneBytes int64
	var totalCloneDuration time.Duration
	for i := 0; i < b.N; i++ {
		workDir := b.TempDir()
		cloneDir := fmt.Sprintf("repo-clone-%d", i)
		clonePath := filepath.Join(workDir, cloneDir)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		cmd := liveCommand(ctx, env.binPath, "git", "clone", env.cfg.Repository, cloneDir)
		cmd.Dir = workDir
		cmd.Env = env.duckwayCommandEnv()
		start := time.Now()
		output, err := cmd.CombinedOutput()
		elapsed := time.Since(start)
		cancel()
		if err != nil {
			b.Fatalf("duckway git clone benchmark failed: %v\n%s", err, env.sanitizeOutput(string(output)))
		}
		cloneBytes, err := dirSize(clonePath)
		if err != nil {
			b.Fatalf("measure clone size: %v", err)
		}
		totalCloneBytes += cloneBytes
		totalCloneDuration += elapsed
	}
	if b.N > 0 {
		b.ReportMetric(float64(totalCloneDuration.Milliseconds())/float64(b.N), "clone_ms")
		b.ReportMetric(float64(totalCloneBytes/int64(b.N))/(1024*1024), "clone_MiB")
		if totalCloneDuration > 0 {
			b.ReportMetric(float64(totalCloneBytes)/(1024*1024)/totalCloneDuration.Seconds(), "clone_MiBps")
		}
	}
}

func newGitHubGitLiveDuckwayEnv(tb testing.TB) *githubGitLiveDuckwayEnv {
	tb.Helper()
	cfg := loadGitHubAppLiveConfig(tb)
	return newGitHubGitLiveDuckwayEnvForRepos(tb, cfg, []string{cfg.Repository})
}

func newGitHubGitLiveDuckwayEnvForRepos(tb testing.TB, cfg githubAppLiveConfig, repos []string) *githubGitLiveDuckwayEnv {
	tb.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		tb.Fatalf("git binary is required for live GitHub git test: %v", err)
	}
	credentialJSON := buildGitHubAppCredentialJSON(tb, cfg)
	phantom, serverURL, clientToken, ca, caPEM := startGitHubGitLiveDuckwayServerForRepos(tb, credentialJSON, repos)

	localProxy := &httpsProxy{
		serverURL: serverURL,
		token:     clientToken,
		ca:        ca,
		hostMap: map[string]hostEntry{
			"github.com": {
				Service:      "github",
				DeliveryMode: "proxy",
				UpstreamURL:  "https://github.com",
			},
		},
		httpClient:  &http.Client{Transport: directTransport},
		loanCache:   make(map[string]*loanedToken),
		auditClient: &http.Client{Timeout: time.Second},
	}
	proxyServer := httptest.NewServer(localProxy)
	tb.Cleanup(proxyServer.Close)
	proxyPort := portFromTestServerURL(tb, proxyServer.URL)

	root := findRepoRoot(tb)
	binDir := tb.TempDir()
	binPath := filepath.Join(binDir, "duckway")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/client")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		tb.Fatalf("build duckway client: %v\n%s", err, out)
	}

	home := tb.TempDir()
	configDir := filepath.Join(home, ".duckway")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		tb.Fatal(err)
	}
	if err := SaveConfig(configDir, &Config{
		ServerURL:  serverURL,
		ClientName: "github-git-clone-live",
		Token:      clientToken,
		ProxyPort:  proxyPort,
	}); err != nil {
		tb.Fatalf("save config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "ca.pem"), caPEM, 0600); err != nil {
		tb.Fatalf("write config CA: %v", err)
	}
	if err := DeployGitHubCredentialForGit(home, phantom); err != nil {
		tb.Fatalf("deploy github phantom credential: %v", err)
	}

	return &githubGitLiveDuckwayEnv{
		cfg:         cfg,
		repos:       repos,
		phantom:     phantom,
		serverURL:   serverURL,
		clientToken: clientToken,
		proxyURL:    proxyServer.URL,
		proxyPort:   proxyPort,
		caPEM:       caPEM,
		binPath:     binPath,
		home:        home,
		configDir:   configDir,
	}
}

type githubGitLiveSeedSpec struct {
	megabytes int
	files     int
}

func (s githubGitLiveSeedSpec) totalBytes() int {
	return s.megabytes * 1024 * 1024
}

func (s githubGitLiveSeedSpec) path() string {
	return fmt.Sprintf(".duckway-live-benchmark/seed-%dmb-%dfiles", s.megabytes, s.files)
}

func githubGitLiveSeedSpecFromEnv(tb testing.TB) githubGitLiveSeedSpec {
	tb.Helper()
	return githubGitLiveSeedSpec{
		megabytes: positiveEnvInt(tb, "DUCKWAY_GITHUB_GIT_LIVE_SEED_MB", 32),
		files:     positiveEnvInt(tb, "DUCKWAY_GITHUB_GIT_LIVE_SEED_FILES", 256),
	}
}

func positiveEnvInt(tb testing.TB, name string, fallback int) int {
	tb.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		tb.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return n
}

func seedGitHubLiveBenchmarkData(tb testing.TB, env *githubGitLiveDuckwayEnv, seed githubGitLiveSeedSpec) (string, bool) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	workDir := tb.TempDir()
	repoDir := filepath.Join(workDir, "seed-repo")
	repoURL := "https://github.com/" + strings.TrimSuffix(env.cfg.Repository, ".git") + ".git"
	clone := liveCommand(ctx, "git",
		"-c", "credential.helper=store",
		"-c", "credential.useHttpPath=false",
		"clone", repoURL, repoDir,
	)
	clone.Env = env.gitProxyEnv()
	if output, err := clone.CombinedOutput(); err != nil {
		tb.Fatalf("clone live repo for seeding failed: %v\n%s", err, env.sanitizeOutput(string(output)))
	}

	seedPath := seed.path()
	manifest := filepath.Join(repoDir, seedPath, "manifest.json")
	if _, err := os.Stat(manifest); err == nil {
		return seedPath, false
	} else if !os.IsNotExist(err) {
		tb.Fatalf("stat seed manifest: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(repoDir, seedPath), 0755); err != nil {
		tb.Fatalf("create seed dir: %v", err)
	}
	if err := writeDeterministicSeedFiles(filepath.Join(repoDir, seedPath), seed); err != nil {
		tb.Fatalf("write seed files: %v", err)
	}
	manifestBody := fmt.Sprintf("{\n  \"generated_by\": \"duckway live git benchmark\",\n  \"megabytes\": %d,\n  \"files\": %d,\n  \"bytes\": %d\n}\n", seed.megabytes, seed.files, seed.totalBytes())
	if err := os.WriteFile(manifest, []byte(manifestBody), 0644); err != nil {
		tb.Fatalf("write seed manifest: %v", err)
	}

	for _, args := range [][]string{
		{"-C", repoDir, "config", "user.name", "Duckway Live Benchmark"},
		{"-C", repoDir, "config", "user.email", "duckway-live-benchmark@example.invalid"},
		{"-C", repoDir, "config", "credential.helper", "store"},
		{"-C", repoDir, "config", "credential.useHttpPath", "false"},
		{"-C", repoDir, "add", seedPath},
		{"-C", repoDir, "commit", "-m", fmt.Sprintf("Add Duckway live benchmark seed %dMiB", seed.megabytes)},
		{"-C", repoDir, "push", "origin", "HEAD"},
	} {
		cmd := liveCommand(ctx, "git", args...)
		cmd.Env = env.gitProxyEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			if strings.Contains(string(output), "Write access to repository not granted") {
				tb.Fatalf("git push failed because the GitHub App installation does not grant Contents write access; enable Contents: read/write for %s, reinstall/update the app permissions, then rerun with DUCKWAY_TEST_GITHUB_GIT_SEED_LIVE=1\n%s", env.cfg.Repository, env.sanitizeOutput(string(output)))
			}
			tb.Fatalf("git %s failed while seeding live repo: %v\n%s", strings.Join(args, " "), err, env.sanitizeOutput(string(output)))
		}
	}
	return seedPath, true
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func writeDeterministicSeedFiles(dir string, seed githubGitLiveSeedSpec) error {
	total := seed.totalBytes()
	perFile := total / seed.files
	remainder := total % seed.files
	buf := make([]byte, 32*1024)
	for i := 0; i < seed.files; i++ {
		size := perFile
		if i < remainder {
			size++
		}
		path := filepath.Join(dir, fmt.Sprintf("blob-%04d.bin", i))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		rng := rand.New(rand.NewSource(int64(seed.megabytes*100000 + seed.files*1000 + i)))
		remaining := size
		for remaining > 0 {
			n := len(buf)
			if remaining < n {
				n = remaining
			}
			if _, err := rng.Read(buf[:n]); err != nil {
				f.Close()
				return err
			}
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			remaining -= n
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (e *githubGitLiveDuckwayEnv) duckwayCommandEnv() []string {
	return append(os.Environ(),
		"HOME="+e.home,
		"DUCKWAY_CONFIG_DIR="+e.configDir,
		"GIT_TERMINAL_PROMPT=0",
		"NO_PROXY=",
		"no_proxy=",
	)
}

func (e *githubGitLiveDuckwayEnv) gitProxyEnv() []string {
	return append(os.Environ(),
		"HOME="+e.home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSL_CAINFO="+filepath.Join(e.configDir, "ca.pem"),
		"HTTPS_PROXY="+e.proxyURL,
		"HTTP_PROXY="+e.proxyURL,
		"https_proxy="+e.proxyURL,
		"http_proxy="+e.proxyURL,
		"NO_PROXY=",
		"no_proxy=",
	)
}

func (e *githubGitLiveDuckwayEnv) sanitizeOutput(output string) string {
	secrets := []string{e.phantom, e.clientToken, e.cfg.PrivateKey, e.cfg.Repository}
	secrets = append(secrets, e.repos...)
	return sanitizeGitHubLiveOutput(output, secrets...)
}

func liveCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func assertGitConfigValue(t *testing.T, repoDir, key, want string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repoDir, "config", "--local", "--get", key)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("read git config %s: %v", key, err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("git config %s = %q, want %q", key, got, want)
	}
}

func sanitizeGitHubLiveOutput(output string, secrets ...string) string {
	sanitized := output
	for _, secret := range secrets {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		sanitized = strings.ReplaceAll(sanitized, secret, "[secret]")
	}
	return githubInstallationTokenPattern.ReplaceAllString(sanitized, "ghs_[redacted]")
}

func startGitHubGitLiveDuckwayServerForRepos(t testing.TB, credentialJSON string, repos []string) (phantom, serverURL, clientToken string, ca *services.CAManager, caPEM []byte) {
	t.Helper()
	if len(repos) == 0 {
		t.Fatal("at least one live repository is required")
	}
	normalizedRepos := make([]string, 0, len(repos))
	seenRepos := map[string]bool{}
	for _, repo := range repos {
		normalizedRepo := normalizeGitHubLiveRepo(t, repo)
		key := strings.ToLower(normalizedRepo)
		if seenRepos[key] {
			continue
		}
		seenRepos[key] = true
		normalizedRepos = append(normalizedRepos, normalizedRepo)
	}
	parts := strings.Split(normalizedRepos[0], "/")
	if len(parts) != 2 {
		t.Fatalf("live repository must be OWNER/REPO, got %q", normalizedRepos[0])
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
	acl := githubGitLivePermissionConfig(normalizedRepos...)
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
	serverMux.HandleFunc("/client/services", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Duckway-Token") != clientToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		assigned := true
		json.NewEncoder(w).Encode([]ServiceInfo{{
			Name: "github", HostPattern: "api.github.com,github.com",
			UpstreamURL: "https://api.github.com", DeliveryMode: "proxy", Assigned: &assigned,
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

func githubGitLivePermissionConfig(repos ...string) string {
	type endpoint struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Allow  bool   `json:"allow"`
	}
	config := struct {
		Version  string `json:"version"`
		Provider string `json:"provider"`
		Rules    []struct {
			Name         string     `json:"name"`
			Endpoints    []endpoint `json:"endpoints"`
			DenyAllOther bool       `json:"deny_all_other"`
		} `json:"rules"`
	}{
		Version:  "1",
		Provider: "github",
	}
	var endpoints []endpoint
	for _, repo := range repos {
		endpoints = append(endpoints,
			endpoint{Method: "GET", Path: "/" + repo + ".git/info/refs", Allow: true},
			endpoint{Method: "POST", Path: "/" + repo + ".git/git-upload-pack", Allow: true},
			endpoint{Method: "POST", Path: "/" + repo + ".git/git-receive-pack", Allow: true},
			endpoint{Method: "GET", Path: "/repos/" + repo, Allow: true},
		)
	}
	config.Rules = append(config.Rules, struct {
		Name         string     `json:"name"`
		Endpoints    []endpoint `json:"endpoints"`
		DenyAllOther bool       `json:"deny_all_other"`
	}{
		Name:         "live-read-write",
		Endpoints:    endpoints,
		DenyAllOther: true,
	})
	raw, _ := json.Marshal(config)
	return string(raw)
}

func portFromTestServerURL(t testing.TB, rawURL string) int {
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

func findRepoRoot(t testing.TB) string {
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

func loadGitHubAppLiveConfig(t testing.TB) githubAppLiveConfig {
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
	cfg.Repository = normalizeGitHubLiveRepo(t, cfg.Repository)
	return cfg
}

func firstTwoGitHubLiveInstallationRepos(tb testing.TB, cfg githubAppLiveConfig) []string {
	tb.Helper()
	repos := listGitHubLiveInstallationRepos(tb, cfg)
	if len(repos) < 2 {
		tb.Fatalf("GitHub App installation %d lists %d repositories; configure at least two repositories for live multi-repo tests", cfg.InstallationID, len(repos))
	}
	return repos[:2]
}

func listGitHubLiveInstallationRepos(tb testing.TB, cfg githubAppLiveConfig) []string {
	tb.Helper()
	token := mintGitHubLiveInstallationToken(tb, cfg)
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/installation/repositories?per_page=100", nil)
	if err != nil {
		tb.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tb.Fatalf("list GitHub App installation repositories: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tb.Fatalf("list GitHub App installation repositories returned %d", resp.StatusCode)
	}
	var out struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		tb.Fatalf("decode GitHub App installation repositories: %v", err)
	}
	seen := map[string]bool{}
	var repos []string
	for _, repo := range out.Repositories {
		normalized := normalizeGitHubLiveRepo(tb, repo.FullName)
		key := strings.ToLower(normalized)
		if seen[key] {
			continue
		}
		seen[key] = true
		repos = append(repos, normalized)
	}
	return repos
}

func mintGitHubLiveInstallationToken(tb testing.TB, cfg githubAppLiveConfig) string {
	tb.Helper()
	jwt := githubLiveAppJWT(tb, cfg)
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", baseURL, cfg.InstallationID), strings.NewReader(`{}`))
	if err != nil {
		tb.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tb.Fatalf("mint GitHub App installation token for repo listing: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		tb.Fatalf("mint GitHub App installation token for repo listing returned %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		tb.Fatalf("decode GitHub App installation token: %v", err)
	}
	if out.Token == "" {
		tb.Fatal("GitHub App installation token response missing token")
	}
	return out.Token
}

func githubLiveAppJWT(tb testing.TB, cfg githubAppLiveConfig) string {
	tb.Helper()
	block, _ := pem.Decode([]byte(cfg.PrivateKey))
	if block == nil {
		tb.Fatal("decode GitHub App private key PEM")
		return ""
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if parseErr != nil {
			tb.Fatalf("parse GitHub App private key: %v", err)
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			tb.Fatal("GitHub App private key is not RSA")
		}
	}
	headerJSON, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	now := time.Now()
	claimsJSON, _ := json.Marshal(map[string]int64{
		"iat": now.Add(-1 * time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": cfg.AppID,
	})
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(cryptorand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		tb.Fatalf("sign GitHub App JWT: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func findGitHubAppLiveConfigFrom(t testing.TB, rel string) string {
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

func buildGitHubAppCredentialJSON(t testing.TB, cfg githubAppLiveConfig) string {
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

func normalizeGitHubLiveRepo(t testing.TB, raw string) string {
	t.Helper()
	repo := strings.TrimSpace(raw)
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimPrefix(repo, "git@github.com:")
	repo = strings.Trim(repo, "/")
	repo = strings.TrimSuffix(repo, ".git")
	repo = strings.Trim(repo, "/")
	if !githubLiveRepoPattern.MatchString(repo) {
		t.Fatalf("live repository must be OWNER/REPO, got %q", raw)
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." || strings.HasSuffix(part, ".git") {
			t.Fatalf("live repository must be OWNER/REPO, got %q", raw)
		}
	}
	return repo
}
