package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/client"
	"github.com/hackerduck/duckway/internal/version"
)

var isTTY = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}()

var sessionAttachExec = func(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing attach command")
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("%s is not installed or not on PATH", args[0])
	}
	return syscall.Exec(path, args, os.Environ())
}

func fileIsTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

var canPrompt = func() bool {
	return fileIsTTY(os.Stdin) && fileIsTTY(os.Stderr)
}

// cyan wraps s in cyan ANSI color when stdout is a terminal, for commands the user should run.
func cyan(s string) string {
	if !isTTY {
		return s
	}
	return "\033[36m" + s + "\033[0m"
}

// shellQuote returns s quoted so it can be pasted into a POSIX shell verbatim.
// Plain tokens (paths, URLs) pass through unquoted; anything with shell-special
// characters is single-quoted with embedded single quotes escaped.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`&|;<>(){}[]*?!#~=") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func runDucklionCompat(args []string) error {
	if os.Getenv("DUCKWAY_DUCKLION_WRAPPER") == "1" {
		return fmt.Errorf("duckway ducklion reached itself again; install the standalone ducklion binary instead of the old ducklion shim")
	}
	path, err := exec.LookPath("ducklion")
	if err != nil {
		return fmt.Errorf("standalone ducklion not found in PATH; install or update Duckway so ducklion is installed beside duckway")
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "DUCKWAY_DUCKLION_WRAPPER=1")
	return cmd.Run()
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	configDir := client.DefaultConfigDir()

	switch os.Args[1] {
	case "init":
		cmdInit(configDir)
	case "sync":
		cmdSync(configDir)
	case "env":
		cmdEnv(configDir)
	case "proxy":
		cmdProxy(configDir)
	case "status":
		cmdStatus(configDir)
	case "logs":
		cmdLogs(configDir, os.Args[2:])
	case "install-ca":
		// Manually re-install the Duckway CA cert to the system trust store.
		// Useful when the original `duckway init` ran on a system that was
		// later upgraded (e.g. ca-certificates package added) or to recover
		// after the cert was removed.
		if err := client.InstallCACert(configDir); err != nil {
			log.Fatalf("CA install failed: %v", err)
		}
		fmt.Println("CA certificate installed to system trust store")
	case "update":
		cmdUpdate(configDir)
	case "mcp":
		cmdMCP(configDir)
	case "cc":
		cmdCC(configDir)
	case "session":
		cmdSession(configDir, os.Args[2:])
	case "ducklion":
		if err := runDucklionCompat(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "projects":
		cmdProjects(configDir, os.Args[2:])
	case "git":
		cmdGit(configDir, os.Args[2:])
	case "start":
		cmdStart(configDir)
	case "stop":
		cmdStop(configDir)
	case "restart":
		cmdRestart(configDir)
	case "version", "--version", "-v":
		fmt.Println("duckway", version.Get())
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`duckway — API proxy client for AI agents

Usage:
  duckway init           Register this machine with a Duckway server
  duckway sync           Fetch placeholder keys + statusline from server
  duckway start          Start both daemons (proxy + cc watch) — daemon mode
  duckway stop           Stop both daemons
  duckway restart        Restart both daemons
  duckway logs [-f]      Show daemon logs (proxy + cc watch)
  duckway logs proxy     Show only proxy logs
  duckway logs cc -f     Follow only cc-watch logs
  duckway env            Print keys + HTTP(S)_PROXY exports as shell statements
                         (eval "$(duckway env)" or append to ~/.bashrc)
  duckway env --proxy    Print only the HTTP(S)_PROXY exports for the local proxy
  duckway proxy          Start local proxy (foreground)
  duckway proxy -d       Start local proxy as background daemon
  duckway proxy stop     Stop the running daemon
  duckway proxy restart  Stop the running daemon and start a fresh one
  duckway proxy status   Show daemon status
  duckway proxy exec -- CMD [ARGS...]
                         Run one command with HTTP(S)_PROXY set to the local proxy
  duckway proxy hosts        List services the proxy intercepts (queries server)
  duckway proxy hosts reload Signal the running proxy daemon to refresh its host list now
  duckway status             Show connection status, CA cert expiry
  duckway install-ca     Re-install the Duckway CA into the system trust store
  duckway update         Compare local version with server, download + replace if drifted
                         (uses saved config; override with --server <url>
                          or DUCKWAY_SERVER_URL — works without init)
  duckway update --restart
                         After an update, restart any duckway daemons that were running
  duckway mcp serve      Run the Control-Channel MCP server over stdio
                         (launched by Claude Code from ~/.claude/mcp.json)
  duckway cc watch       Connect to the server's SSE feed and run a
                         local agent session per Discord task channel
                         (default runner uses Duckway's PTY supervisor)
  duckway cc watch -d    Same, but run in background as a daemon
  duckway cc watch --tmux     Use legacy tmux runner instead of PTY
  duckway cc watch --no-tmux  Ignore DUCKWAY_CC_USE_TMUX and never use tmux
                         (also: DUCKWAY_CC_NO_TMUX=1)
  duckway cc watch --debug    Log agent runner settings and sanitized CLI argv
  duckway cc watch stop  Stop the running daemon
  duckway cc watch restart  Stop and start a fresh daemon
  duckway cc watch status  Show daemon status
  duckway cc bind        Interactive picker: pick existing local claude
                         sessions and create a CC channel + binding for
                         each. Use --session <id> for headless mode.
  duckway session list   List local terminal agent sessions
  duckway session start --name <name> [--agent <agent>] [--cwd <dir>] [--tmux] -- CMD [ARGS...]
                         Start a local PTY-backed terminal agent session
  duckway session attach <name>
                         Attach to a local terminal agent session
  duckway session send <name> <text>
                         Send text + Enter to a local terminal agent session
  duckway session read <name> [--lines N]
                         Capture recent output from a local terminal agent session
  duckway session stop <name>
                         Stop a local terminal agent session
  duckway ducklion ...   Compatibility wrapper for standalone ducklion
                         (install ducklion beside duckway)
  duckway projects       Manage project folders usable from Discord
  duckway git list       List GitHub repos configured for this client
  duckway git setup      Sync GitHub phantom credential for native git
  duckway git clone OWNER/REPO [DIR]
                         Clone a configured repo and write local git proxy config
  duckway version        Print the duckway version

Proxy flags:
  --port N               Override proxy port
  --daemon, -d           Run in background
  --debug, -D            Log every request/response (stdout in foreground,
                         ~/.duckway/proxy.log in daemon mode)

Config directory: ~/.duckway/
Daemon files:
  ~/.duckway/proxy.pid   PID of the running daemon
  ~/.duckway/proxy.log   Proxy daemon logs (stdout + stderr)
  ~/.duckway/cc-watch.log  Control-channel daemon logs (stdout + stderr)`)
}

func cmdInit(configDir string) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Duckway server URL (e.g., http://192.168.1.100:8080): ")
	serverURL, _ := reader.ReadString('\n')
	serverURL = strings.TrimSpace(serverURL)

	fmt.Print("Client name (e.g., my-laptop): ")
	clientName, _ := reader.ReadString('\n')
	clientName = strings.TrimSpace(clientName)

	fmt.Println("\nChoose authentication method:")
	fmt.Println("  1. Enter a pre-shared token (admin already created this client)")
	fmt.Println("  2. Login as admin to register this client")
	fmt.Print("Choice [1/2]: ")
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)

	var token string

	switch choice {
	case "2":
		fmt.Print("Admin username: ")
		username, _ := reader.ReadString('\n')
		username = strings.TrimSpace(username)

		fmt.Print("Admin password: ")
		password, _ := reader.ReadString('\n')
		password = strings.TrimSpace(password)

		session, err := client.AdminLogin(serverURL, username, password)
		if err != nil {
			log.Fatalf("Login failed: %v", err)
		}

		_, tok, err := client.RegisterClient(serverURL, session, clientName)
		if err != nil {
			log.Fatalf("Registration failed: %v", err)
		}
		token = tok
		fmt.Printf("Client registered successfully!\n")

	default:
		fmt.Print("Client token: ")
		token, _ = reader.ReadString('\n')
		token = strings.TrimSpace(token)
	}

	cfg := &client.Config{
		ServerURL:  serverURL,
		ClientName: clientName,
		Token:      token,
		ProxyPort:  18080,
	}

	// Verify connection
	api := client.NewAPIClient(serverURL, token)
	if err := api.Ping(); err != nil {
		log.Fatalf("Server connection failed: %v", err)
	}

	if err := client.SaveConfig(configDir, cfg); err != nil {
		log.Fatalf("Failed to save config: %v", err)
	}

	// Write proxy env script
	client.WriteProxyEnvScript(configDir, cfg.ProxyPort)

	// Download CA cert for HTTPS proxy
	if err := api.DownloadCA(configDir); err != nil {
		log.Printf("Warning: CA cert download failed: %v", err)
		log.Printf("HTTPS proxy MITM will not work — only HTTP proxy mode available")
	} else {
		fmt.Println("CA certificate downloaded for HTTPS proxy")
		// Try to install to system trust store
		if err := client.InstallCACert(configDir); err != nil {
			log.Printf("Warning: could not install CA to system trust store: %v", err)
			fmt.Printf("Manually install: %s\n", cyan(fmt.Sprintf("sudo cp %s/ca.pem /usr/local/share/ca-certificates/duckway.crt && sudo update-ca-certificates", configDir)))
		} else {
			fmt.Println("CA certificate installed to system trust store")
		}
	}

	// Initial sync
	count, err := client.SyncKeys(configDir, cfg)
	if err != nil {
		log.Printf("Warning: initial sync failed: %v", err)
	} else {
		fmt.Printf("Synced %d placeholder keys\n", count)
	}

	// Apply supply-chain hardening to package-manager rc files.
	if lines := client.FormatSupplyChainChanges(client.SyncSupplyChainRC(cfg)); len(lines) > 0 {
		fmt.Println("Supply-chain hardening applied:")
		for _, l := range lines {
			fmt.Println(l)
		}
	}

	fmt.Printf("\nConfig saved to %s/config.yaml\n", configDir)
	fmt.Println("\nNext steps:")
	fmt.Printf("  %s           — start HTTPS proxy (background daemon)\n", cyan("duckway proxy -d"))
	fmt.Printf("  %s\n", cyan(fmt.Sprintf("export HTTPS_PROXY=%s", client.LocalProxyURL(cfg.ProxyPort))))
	fmt.Printf("  %s\n", cyan(fmt.Sprintf("export HTTP_PROXY=%s", client.LocalProxyURL(cfg.ProxyPort))))
	fmt.Printf("\nTo run in foreground for debugging: %s\n", cyan("duckway proxy --debug"))
}

func cmdSync(configDir string) {
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}

	count, err := client.SyncKeys(configDir, cfg)
	if err != nil {
		log.Fatalf("Sync failed: %v", err)
	}

	fmt.Printf("Synced %d placeholder keys to %s\n", count, client.KeysEnvPath(configDir))

	// Apply supply-chain hardening to package-manager rc files and tell the
	// user exactly which files changed and what was written.
	changes := client.SyncSupplyChainRC(cfg)
	if lines := client.FormatSupplyChainChanges(changes); len(lines) > 0 {
		fmt.Println("Supply-chain hardening applied:")
		for _, l := range lines {
			fmt.Println(l)
		}
	} else if changes != nil {
		fmt.Println("Supply-chain hardening: rc files already up to date")
	}
}

// cmdGit manages native git setup for GitHub repos assigned to this client.
func cmdGit(configDir string, args []string) {
	if len(args) < 1 {
		printGitUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "list", "ls":
		cmdGitList(configDir)
	case "setup":
		cmdGitSetup(configDir)
	case "clone":
		cmdGitClone(configDir, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown git subcommand: %s\n", args[0])
		printGitUsage()
		os.Exit(1)
	}
}

func printGitUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  duckway git list")
	fmt.Fprintln(os.Stderr, "  duckway git setup")
	fmt.Fprintln(os.Stderr, "  duckway git clone OWNER/REPO [DIR]")
}

type configuredGitRepo struct {
	Repo        string
	Mode        string
	Placeholder string
}

func cmdGitList(configDir string) {
	cfg, keys := loadGitConfigAndKeys(configDir)
	_ = cfg
	repos := configuredGitRepos(keys)
	if len(repos) == 0 {
		fmt.Println("No GitHub repositories are configured for this client.")
		return
	}
	fmt.Println("Configured GitHub repositories:")
	for _, repo := range repos {
		fmt.Printf("  %-40s %s\n", repo.Repo, gitRepoAccessLabel(repo.Mode))
	}
}

func cmdGitSetup(configDir string) {
	cfg, keys := loadGitConfigAndKeys(configDir)
	if !hasUsableGitHubCredential(keys) {
		log.Fatal("no usable GitHub phantom token is configured for this client; assign a GitHub key with at least one repository first")
	}
	if _, err := client.SyncKeys(configDir, cfg); err != nil {
		log.Fatalf("git setup sync failed: %v", err)
	}
	if err := ensureGitHubCredentialStored(keys, ""); err != nil {
		log.Fatalf("git setup failed: %v", err)
	}
	fmt.Println("Equivalent git commands:")
	printGitCommand("", "config", "--global", "credential.helper", "store")
	printGitCommand("", "config", "--global", "credential.useHttpPath", "true")
	if err := configureGlobalGitCredential(); err != nil {
		log.Fatalf("git setup failed: %v", err)
	}
	fmt.Println("GitHub phantom credential synced for native git.")
	fmt.Printf("Local proxy expected at %s. Run `duckway proxy -d` if it is not already running.\n", client.LocalProxyURL(cfg.ProxyPort))
}

func cmdGitClone(configDir string, args []string) {
	if len(args) < 1 || len(args) > 2 {
		printGitUsage()
		os.Exit(1)
	}
	repo, err := normalizeGitRepoArg(args[0])
	if err != nil {
		log.Fatal(err)
	}
	cfg, keys := loadGitConfigAndKeys(configDir)
	repos := configuredGitRepos(keys)
	repoInfo, ok := configuredGitRepoFor(repos, repo)
	if !ok {
		fmt.Fprintf(os.Stderr, "Repository %s is not configured for this client.\n", repo)
		fmt.Fprintln(os.Stderr, "Configured repos:")
		for _, r := range repos {
			fmt.Fprintf(os.Stderr, "  %s (%s)\n", r.Repo, r.Mode)
		}
		os.Exit(1)
	}
	repo = repoInfo.Repo
	if err := ensureGitHubCredentialStored([]client.PlaceholderKeyInfo{{
		ServiceName: "github",
		Placeholder: repoInfo.Placeholder,
	}}, repo); err != nil {
		log.Fatalf("git clone setup failed: %v", err)
	}

	dir := repo[strings.LastIndex(repo, "/")+1:]
	if len(args) == 2 {
		dir = args[1]
	}
	fmt.Println("Equivalent git commands:")
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if !os.IsNotExist(err) {
			log.Fatal(err)
		}
		repoURL := "https://github.com/" + repo + ".git"
		cloneArgs := []string{"-c", "credential.helper=store", "-c", "credential.useHttpPath=true", "-c", fmt.Sprintf("http.https://github.com/.proxy=%s", client.LocalProxyURL(cfg.ProxyPort)), "-c", "http.https://github.com/.sslCAInfo=" + filepath.Join(configDir, "ca.pem"), "clone", repoURL, dir}
		printGitCommand("", cloneArgs...)
		if err := runGit("", cloneArgs...); err != nil {
			log.Fatalf("git clone failed: %v", err)
		}
	} else {
		if err := ensureExistingGitRepoMatchesRemote(dir, repo); err != nil {
			log.Fatalf("configure existing repo failed: %v", err)
		}
		fmt.Printf("# %s already exists; skipping clone\n", filepath.Join(dir, ".git"))
	}
	printGitCommand(dir, "remote", "set-url", "origin", "https://github.com/"+repo+".git")
	printGitCommand(dir, "config", "--local", "http.https://github.com/.proxy", client.LocalProxyURL(cfg.ProxyPort))
	printGitCommand(dir, "config", "--local", "http.https://github.com/.sslCAInfo", filepath.Join(configDir, "ca.pem"))
	printGitCommand(dir, "config", "--local", "credential.helper", "store")
	printGitCommand(dir, "config", "--local", "credential.useHttpPath", "true")
	if err := configureRepoGit(dir, configDir, cfg.ProxyPort, repo); err != nil {
		log.Fatalf("configure repo failed: %v", err)
	}
	fmt.Printf("Repository ready: %s\n", dir)
	fmt.Printf("Access: %s\n", gitRepoAccessLabel(repoInfo.Mode))
	fmt.Println("Next native git commands:")
	fmt.Printf("  cd %s\n", shellQuote(dir))
	fmt.Println("  git pull --ff-only")
	printGitNextPushHint(repoInfo.Mode)
}

func printGitNextPushHint(mode string) {
	if mode == "dev" {
		fmt.Println("  git push")
		return
	}
	fmt.Println("  # push is not available for read-only assignments")
}

func gitRepoAccessLabel(mode string) string {
	if mode == "dev" {
		return "read-write"
	}
	return "read"
}

func loadGitConfigAndKeys(configDir string) (*client.Config, []client.PlaceholderKeyInfo) {
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}
	keys, err := client.NewAPIClient(cfg.ServerURL, cfg.Token).FetchKeys()
	if err != nil {
		log.Fatalf("fetch configured keys: %v", err)
	}
	return cfg, keys
}

func configureGlobalGitCredential() error {
	if err := runGit("", "config", "--global", "credential.helper", "store"); err != nil {
		return err
	}
	return runGit("", "config", "--global", "credential.useHttpPath", "true")
}

func hasUsableGitHubCredential(keys []client.PlaceholderKeyInfo) bool {
	for _, key := range keys {
		if key.ServiceName == "github" && strings.TrimSpace(key.Placeholder) != "" {
			return true
		}
	}
	return false
}

func ensureGitHubCredentialStored(keys []client.PlaceholderKeyInfo, repo string) error {
	home, _ := os.UserHomeDir()
	if home == "" {
		return fmt.Errorf("cannot locate home directory for git credential store")
	}
	if repo != "" {
		for _, key := range keys {
			if key.ServiceName != "github" || strings.TrimSpace(key.Placeholder) == "" {
				continue
			}
			if err := client.DeployGitHubCredentialForGitRepo(home, repo, key.Placeholder); err != nil {
				return err
			}
			if gitHubCredentialStoreContains(home, key.Placeholder, repo) {
				return nil
			}
		}
		return fmt.Errorf("no GitHub phantom credential was written for %s; reassign the GitHub key for this client", repo)
	}
	wrote := false
	for _, key := range keys {
		if key.ServiceName != "github" || strings.TrimSpace(key.Placeholder) == "" {
			continue
		}
		if err := client.DeployGitHubCredentialsForKey(home, key.Placeholder, key.PermissionConfig); err != nil {
			return err
		}
		wrote = true
	}
	if wrote {
		return nil
	}
	return fmt.Errorf("no GitHub phantom credential was written; reassign the GitHub key for this client")
}

func gitHubCredentialStoreContains(home, placeholder, repo string) bool {
	if home == "" || placeholder == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(home, ".git-credentials"))
	if err != nil {
		return false
	}
	if repo == "" {
		return strings.Contains(string(raw), placeholder)
	}
	credentialPath := "github.com/" + strings.TrimSuffix(strings.Trim(repo, "/"), ".git") + ".git"
	return strings.Contains(string(raw), placeholder) && strings.Contains(strings.ToLower(string(raw)), strings.ToLower(credentialPath))
}

func configureRepoGit(dir, configDir string, port int, repo string) error {
	if err := runGit(dir, "remote", "set-url", "origin", "https://github.com/"+repo+".git"); err != nil {
		return err
	}
	if err := runGit(dir, "config", "--local", "http.https://github.com/.proxy", client.LocalProxyURL(port)); err != nil {
		return err
	}
	if err := runGit(dir, "config", "--local", "http.https://github.com/.sslCAInfo", filepath.Join(configDir, "ca.pem")); err != nil {
		return err
	}
	if err := runGit(dir, "config", "--local", "credential.helper", "store"); err != nil {
		return err
	}
	return runGit(dir, "config", "--local", "credential.useHttpPath", "true")
}

func ensureExistingGitRepoMatchesRemote(dir, repo string) error {
	want := "https://github.com/" + repo + ".git"
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s already contains a git repo but origin is missing; refusing to reconfigure it", dir)
	}
	got := strings.TrimSpace(string(out))
	if strings.EqualFold(strings.TrimSuffix(got, ".git"), strings.TrimSuffix(want, ".git")) || strings.EqualFold(got, want) {
		return nil
	}
	return fmt.Errorf("%s already contains a git repo with origin %q; refusing to replace it with %q", dir, got, want)
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func printGitCommand(dir string, args ...string) {
	if dir != "" {
		fmt.Printf("+ cd %s && %s\n", shellQuote(dir), formatGitCommand(args...))
		return
	}
	fmt.Printf("+ %s\n", formatGitCommand(args...))
}

func formatGitCommand(args ...string) string {
	parts := []string{"git"}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

var gitRepoArgPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func normalizeGitRepoArg(raw string) (string, error) {
	repo := strings.TrimSpace(raw)
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimPrefix(repo, "git@github.com:")
	repo = strings.Trim(repo, "/")
	repo = strings.TrimSuffix(repo, ".git")
	repo = strings.Trim(repo, "/")
	if !gitRepoArgPattern.MatchString(repo) {
		return "", fmt.Errorf("repository must be OWNER/REPO")
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." || strings.HasSuffix(part, ".git") {
			return "", fmt.Errorf("repository must be OWNER/REPO")
		}
	}
	return repo, nil
}

func configuredGitRepoFor(repos []configuredGitRepo, repo string) (configuredGitRepo, bool) {
	for _, r := range repos {
		if strings.EqualFold(r.Repo, repo) {
			return r, true
		}
	}
	return configuredGitRepo{}, false
}

func configuredGitRepos(keys []client.PlaceholderKeyInfo) []configuredGitRepo {
	reposByName := map[string]configuredGitRepo{}
	for _, key := range keys {
		if key.ServiceName != "github" || strings.TrimSpace(key.PermissionConfig) == "" || strings.TrimSpace(key.Placeholder) == "" {
			continue
		}
		for _, repo := range gitReposFromPermissionConfig(key.PermissionConfig) {
			current := reposByName[repo.Repo]
			if current.Mode != "dev" {
				repo.Placeholder = key.Placeholder
				reposByName[repo.Repo] = repo
			}
		}
	}
	repos := make([]configuredGitRepo, 0, len(reposByName))
	for _, repo := range reposByName {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool { return strings.ToLower(repos[i].Repo) < strings.ToLower(repos[j].Repo) })
	return repos
}

func gitReposFromPermissionConfig(configJSON string) []configuredGitRepo {
	var config struct {
		Provider string `json:"provider"`
		Rules    []struct {
			Endpoints []struct {
				Method string `json:"method"`
				Path   string `json:"path"`
				Allow  bool   `json:"allow"`
			} `json:"endpoints"`
		} `json:"rules"`
	}
	if json.Unmarshal([]byte(configJSON), &config) != nil || config.Provider != "github" {
		return nil
	}
	modes := map[string]string{}
	for _, rule := range config.Rules {
		for _, ep := range rule.Endpoints {
			repo, ok := repoFromGitHubACLEndpoint(ep.Method, ep.Path, ep.Allow)
			if !ok {
				continue
			}
			if strings.EqualFold(ep.Method, "POST") && strings.HasSuffix(ep.Path, ".git/git-receive-pack") {
				modes[repo] = "dev"
			} else if modes[repo] == "" {
				modes[repo] = "deploy"
			}
		}
	}
	repos := make([]configuredGitRepo, 0, len(modes))
	for repo, mode := range modes {
		repos = append(repos, configuredGitRepo{Repo: repo, Mode: mode})
	}
	sort.Slice(repos, func(i, j int) bool { return strings.ToLower(repos[i].Repo) < strings.ToLower(repos[j].Repo) })
	return repos
}

func repoFromGitHubACLEndpoint(method, path string, allow bool) (string, bool) {
	if !allow {
		return "", false
	}
	method = strings.ToUpper(method)
	path = strings.TrimSpace(path)
	var repo string
	switch {
	case method == "GET" && strings.HasSuffix(path, ".git/info/refs"):
		repo = strings.TrimPrefix(strings.TrimSuffix(path, ".git/info/refs"), "/")
	case method == "POST" && strings.HasSuffix(path, ".git/git-upload-pack"):
		repo = strings.TrimPrefix(strings.TrimSuffix(path, ".git/git-upload-pack"), "/")
	case method == "POST" && strings.HasSuffix(path, ".git/git-receive-pack"):
		repo = strings.TrimPrefix(strings.TrimSuffix(path, ".git/git-receive-pack"), "/")
	case method == "GET" && strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/*"):
		repo = strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/*")
	case method == "GET" && strings.HasPrefix(path, "/repos/"):
		repo = strings.TrimPrefix(path, "/repos/")
	default:
		return "", false
	}
	if !gitRepoArgPattern.MatchString(repo) {
		return "", false
	}
	return repo, true
}

// cmdCC dispatches `duckway cc <subcommand>`.
func cmdCC(configDir string) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  duckway cc watch [-d|--daemon|stop|restart|status]")
		fmt.Fprintln(os.Stderr, "  duckway cc bind [--session <id>...] [--cwd <substr>]")
		os.Exit(1)
	}
	switch os.Args[2] {
	case "watch":
		cmdCCWatch(configDir)
	case "bind":
		cmdCCBind(configDir)
	case "projects":
		cmdProjects(configDir, os.Args[3:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown cc subcommand: %s\n", os.Args[2])
		os.Exit(1)
	}
}

type sessionManagerAPI interface {
	List() ([]client.SessionRecord, error)
	Start(client.SessionStartOptions) (*client.SessionRecord, error)
	Send(name, text string) error
	Read(name string, lines int) (string, error)
	Stop(name string) error
	AttachArgs(name string) ([]string, error)
}

func cmdSession(configDir string, args []string) {
	manager := client.NewSessionManager(configDir, nil)
	if err := runSessionCommand(manager, args, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func runSessionCommand(manager sessionManagerAPI, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: duckway session <list|start|attach|send|read|stop>")
	}
	switch args[0] {
	case "list":
		sessions, err := manager.List()
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			fmt.Fprintln(out, "No local terminal sessions.")
			return nil
		}
		fmt.Fprintf(out, "%-18s %-10s %-12s %-8s %-24s %s\n", "NAME", "STATUS", "AGENT", "BACKEND", "TARGET", "CWD")
		for _, s := range sessions {
			fmt.Fprintf(out, "%-18s %-10s %-12s %-8s %-24s %s\n", s.Name, s.Status, s.AgentType, s.DisplayBackend(), s.TargetSession(), s.Cwd)
		}
		return nil
	case "start":
		opts, err := parseSessionStartOptions(args[1:])
		if err != nil {
			return err
		}
		rec, err := manager.Start(opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Started %s (%s, %s)\nAttach: duckway session attach %s\n", rec.Name, rec.AgentType, rec.DisplayBackend(), rec.Name)
		return nil
	case "attach":
		if len(args) != 2 {
			return fmt.Errorf("usage: duckway session attach <name>")
		}
		attachArgs, err := manager.AttachArgs(args[1])
		if err != nil {
			return err
		}
		return sessionAttachExec(attachArgs)
	case "send":
		if len(args) < 3 {
			return fmt.Errorf("usage: duckway session send <name> <text>")
		}
		return manager.Send(args[1], strings.Join(args[2:], " "))
	case "read":
		name, lines, err := parseSessionReadArgs(args[1:])
		if err != nil {
			return err
		}
		text, err := manager.Read(name, lines)
		if err != nil {
			return err
		}
		fmt.Fprint(out, text)
		return nil
	case "stop":
		if len(args) != 2 {
			return fmt.Errorf("usage: duckway session stop <name>")
		}
		if err := manager.Stop(args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "Stopped %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown session subcommand: %s", args[0])
	}
}

func parseSessionStartOptions(args []string) (client.SessionStartOptions, error) {
	opts := client.SessionStartOptions{Kind: "terminal", AgentType: "shell"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			opts.Command = append([]string(nil), args[i+1:]...)
			i = len(args)
		case "--name", "-n":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--name requires a value")
			}
			opts.Name = args[i+1]
			i++
		case "--kind":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--kind requires a value")
			}
			opts.Kind = args[i+1]
			i++
		case "--agent", "--agent-type":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("%s requires a value", args[i])
			}
			opts.AgentType = args[i+1]
			i++
		case "--tmux":
			opts.Backend = client.SessionBackendTmux
		case "--pty":
			opts.Backend = client.SessionBackendPTY
		case "--cwd", "-C":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--cwd requires a value")
			}
			opts.Cwd = args[i+1]
			i++
		default:
			return opts, fmt.Errorf("unknown session start option: %s", args[i])
		}
	}
	if opts.Name == "" {
		return opts, fmt.Errorf("--name is required")
	}
	if len(opts.Command) == 0 {
		return opts, fmt.Errorf("command is required after --")
	}
	return opts, nil
}

func parseSessionReadArgs(args []string) (name string, lines int, err error) {
	lines = 120
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--lines", "-n":
			if i+1 >= len(args) {
				return "", 0, fmt.Errorf("--lines requires a value")
			}
			lines, err = strconv.Atoi(args[i+1])
			if err != nil || lines <= 0 {
				return "", 0, fmt.Errorf("invalid --lines value")
			}
			i++
		default:
			if name != "" {
				return "", 0, fmt.Errorf("usage: duckway session read <name> [--lines N]")
			}
			name = args[i]
		}
	}
	if name == "" {
		return "", 0, fmt.Errorf("usage: duckway session read <name> [--lines N]")
	}
	return name, lines, nil
}

func cmdProjects(configDir string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  duckway projects add [--name <name>] <path-or-glob>...")
		fmt.Fprintln(os.Stderr, "  duckway projects list")
		fmt.Fprintln(os.Stderr, "  duckway projects remove <name|number|path>")
		fmt.Fprintln(os.Stderr, "  duckway projects clear")
		os.Exit(1)
	}
	store := client.NewCCProjectStore(configDir)
	switch args[0] {
	case "add":
		var name string
		var paths []string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--name", "-n":
				if i+1 >= len(args) {
					log.Fatal("--name needs a value")
				}
				name = args[i+1]
				i++
			case "--config-dir":
				if i+1 >= len(args) {
					log.Fatal("--config-dir needs a value")
				}
				configDir = args[i+1]
				store = client.NewCCProjectStore(configDir)
				i++
			case "-h", "--help":
				fmt.Println("Usage: duckway projects add [--name <name>] <path-or-glob>...")
				return
			default:
				paths = append(paths, args[i])
			}
		}
		added, err := store.Add(paths, name)
		if err != nil {
			log.Fatal(err)
		}
		for _, p := range added {
			fmt.Printf("Added %s -> %s\n", p.Name, p.Path)
		}
	case "list":
		projects, err := store.List()
		if err != nil {
			log.Fatal(err)
		}
		if len(projects) == 0 {
			fmt.Println("No projects saved. Add one with: duckway projects add ~/duckway")
			return
		}
		for i, p := range projects {
			fmt.Printf("%2d. %-20s %s\n", i+1, p.Name, p.Path)
		}
	case "remove", "rm":
		if len(args) < 2 {
			log.Fatal("missing project name, number, or path")
		}
		removed, err := store.Remove(args[1])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Removed %s -> %s\n", removed.Name, removed.Path)
	case "clear":
		n, err := store.Clear()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Cleared %d saved project(s). Project folders were not deleted.\n", n)
	default:
		fmt.Fprintf(os.Stderr, "Unknown projects subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func cmdCCWatch(configDir string) {
	pidFile := filepath.Join(configDir, "cc-watch.pid")
	logFile := filepath.Join(configDir, "cc-watch.log")

	daemon := false
	stop := false
	status := false
	restart := false
	// PTY is the default runner. --tmux opts into the legacy tmux runner.
	// --no-tmux is kept as an override for DUCKWAY_CC_USE_TMUX=1.
	noTmux := os.Getenv("DUCKWAY_CC_NO_TMUX") == "1"
	useTmux := os.Getenv("DUCKWAY_CC_USE_TMUX") == "1"
	debug := os.Getenv("DUCKWAY_CC_DEBUG") == "1"
	for i := 3; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch arg {
		case "--daemon", "-d":
			daemon = true
		case "stop":
			stop = true
		case "status":
			status = true
		case "restart":
			restart = true
		case "--no-tmux":
			noTmux = true
			useTmux = false
		case "--tmux":
			useTmux = true
			noTmux = false
		case "--debug", "-D":
			debug = true
		case "--help", "-h":
			printUsage()
			return
		case "--config-dir":
			if i+1 >= len(os.Args) {
				log.Fatal("--config-dir requires a value")
			}
			configDir = os.Args[i+1]
			pidFile = filepath.Join(configDir, "cc-watch.pid")
			logFile = filepath.Join(configDir, "cc-watch.log")
			i++
		default:
			log.Fatalf("unknown cc watch argument %q", arg)
		}
	}

	if stop {
		stopBackgroundDaemon("duckway cc watch", pidFile)
		return
	}
	if status {
		statusBackgroundDaemon(pidFile)
		return
	}
	if restart {
		// stopBackgroundDaemon waits for the SIGTERMed process to exit
		// (up to 2s) and removes the PID file. After it returns the next
		// startBackgroundDaemon won't see a live PID and will spawn cleanly.
		stopBackgroundDaemon("duckway cc watch", pidFile)
		if useTmux {
			warnIfTmuxRequestedButUnavailable()
		}
		daemon = true
	}

	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}

	if useTmux {
		os.Setenv("DUCKWAY_CC_USE_TMUX", "1")
	} else {
		os.Unsetenv("DUCKWAY_CC_USE_TMUX")
	}

	if daemon {
		startCCWatchDaemon(pidFile, logFile)
		return
	}

	if pid, alive := readPID(pidFile); alive && pid != os.Getpid() {
		log.Fatalf("duckway cc watch is already running (PID %d). Run 'duckway cc watch stop' first.", pid)
	}

	// Write our PID so `cc watch stop` works even when started in foreground.
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0600)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		if pid, _ := readPID(pidFile); pid == os.Getpid() {
			os.Remove(pidFile)
		}
		os.Exit(0)
	}()

	w, err := client.NewCCWatchWithOptions(configDir, cfg, client.CCWatchOptions{NoTmux: noTmux, Debug: debug})
	if err != nil {
		log.Fatalf("cc watch: %v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		log.Fatalf("cc watch: %v", err)
	}
}

// cmdCCBind lists local claude sessions that aren't bound to a CC channel
// and creates a task channel + binding for each one the user picks.
//
// Two modes:
//   - Interactive (no args): prints a numbered table, reads selections
//     from stdin (formats: "1,3,5" or "1-3" or "all").
//   - Headless: `--session <id>` (repeatable) skips the prompt entirely.
//     `--cwd <substr>` filters the listing in both modes.
func cmdCCBind(configDir string) {
	var (
		sessionIDs []string
		cwdFilter  string
	)
	for i := 3; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--session", "-s":
			if i+1 < len(os.Args) {
				sessionIDs = append(sessionIDs, os.Args[i+1])
				i++
			}
		case "--cwd":
			if i+1 < len(os.Args) {
				cwdFilter = os.Args[i+1]
				i++
			}
		case "--config-dir":
			if i+1 < len(os.Args) {
				configDir = os.Args[i+1]
				i++
			}
		case "-h", "--help":
			fmt.Println("Usage: duckway cc bind [--session <id>...] [--cwd <substr>]")
			fmt.Println("       (no args) opens an interactive picker with multi-select")
			return
		}
	}

	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}
	api := client.NewAPIClient(cfg.ServerURL, cfg.Token)
	store := client.NewCCSessionStore(configDir)

	root, err := client.ClaudeProjectsRoot()
	if err != nil {
		log.Fatalf("locate ~/.claude/projects: %v", err)
	}
	all, err := client.ListLocalSessions(root, store.Snapshot())
	if err != nil {
		log.Fatalf("scan sessions: %v", err)
	}
	var unbound []client.LocalSession
	for _, s := range all {
		if s.BoundTo != "" {
			continue
		}
		if cwdFilter != "" && !strings.Contains(s.Cwd, cwdFilter) {
			continue
		}
		unbound = append(unbound, s)
	}
	if len(unbound) == 0 {
		if cwdFilter != "" {
			fmt.Printf("No unbound local claude sessions matching %q.\n", cwdFilter)
		} else {
			fmt.Println("No unbound local claude sessions found under ~/.claude/projects/.")
		}
		return
	}

	// Headless: skip the picker.
	if len(sessionIDs) == 0 {
		printSessionTable(unbound)
		picks, err := promptSessionPicks(os.Stdin, len(unbound))
		if err != nil {
			log.Fatalf("read selection: %v", err)
		}
		if len(picks) == 0 {
			fmt.Println("Nothing selected.")
			return
		}
		for _, i := range picks {
			sessionIDs = append(sessionIDs, unbound[i-1].SessionID)
		}
	}

	results := client.BindLocalSessions(context.Background(), api, store, sessionIDs)
	printBindResults(results)
}

func printSessionTable(rows []client.LocalSession) {
	fmt.Printf("\nLocal claude sessions not yet bound to a CC channel:\n\n")
	fmt.Printf("  %-3s  %-36s  %-19s  %s\n", "#", "session_id", "last_active", "cwd")
	fmt.Printf("  %-3s  %-36s  %-19s  %s\n", "---", "------------------------------------",
		"-------------------", "----------------------------------")
	for i, s := range rows {
		fmt.Printf("  %-3d  %-36s  %-19s  %s\n",
			i+1, s.SessionID, s.LastActive.Format("2006-01-02 15:04:05"), s.Cwd)
		if s.FirstMessage != "" && s.FirstMessage != "(no user message recorded)" {
			preview := s.FirstMessage
			if len(preview) > 80 {
				preview = preview[:80] + "…"
			}
			fmt.Printf("       └─ %s\n", preview)
		}
	}
	fmt.Println()
	fmt.Println("Select one or more (examples: '1', '1,3,5', '1-3', 'all', empty to cancel):")
	fmt.Print("> ")
}

// promptSessionPicks reads a single line off `in` and parses it into a set
// of 1-based indices. Accepts: "1,3,5", "1-3", "all", or empty.
func promptSessionPicks(in *os.File, max int) ([]int, error) {
	var line string
	if _, err := fmt.Fscanln(in, &line); err != nil {
		// Fscanln returns "unexpected newline" for an empty line — treat
		// as cancel rather than failure.
		if err.Error() == "unexpected newline" {
			return nil, nil
		}
		return nil, err
	}
	return parseSelectionLine(line, max)
}

func parseSelectionLine(line string, max int) ([]int, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	if line == "all" {
		out := make([]int, max)
		for i := range out {
			out[i] = i + 1
		}
		return out, nil
	}
	seen := map[int]bool{}
	var out []int
	for _, tok := range strings.Split(line, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "-") {
			parts := strings.SplitN(tok, "-", 2)
			a, errA := strconv.Atoi(strings.TrimSpace(parts[0]))
			b, errB := strconv.Atoi(strings.TrimSpace(parts[1]))
			if errA != nil || errB != nil || a < 1 || b > max || a > b {
				return nil, fmt.Errorf("bad range %q (valid: 1..%d)", tok, max)
			}
			for i := a; i <= b; i++ {
				if !seen[i] {
					seen[i] = true
					out = append(out, i)
				}
			}
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil || n < 1 || n > max {
			return nil, fmt.Errorf("bad index %q (valid: 1..%d)", tok, max)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

func printBindResults(rs []client.BindResult) {
	if len(rs) == 0 {
		fmt.Println("Nothing to bind.")
		return
	}
	var ok, dup, fail int
	for _, r := range rs {
		switch {
		case r.Error != "":
			fail++
			fmt.Printf("  ✗ %s — %s\n", r.SessionID, r.Error)
		case r.AlreadyBound != "":
			dup++
			fmt.Printf("  • %s already bound to %s (skipped)\n", r.SessionID, r.AlreadyBound)
		default:
			ok++
			fmt.Printf("  ✓ %s → #%s (%s)  cwd: %s\n", r.SessionID, r.Name, r.Channel, r.Cwd)
		}
	}
	fmt.Printf("\nBound %d, already-bound %d, failed %d.\n", ok, dup, fail)
	if ok > 0 {
		fmt.Println("Send a message in the new channel — claude will resume with the existing history.")
	}
}

// cmdMCP implements the `duckway mcp serve` subcommand. Reads requests
// from stdin and writes responses to stdout per the MCP spec — Claude
// Code's mcp.json launches it for the duckway-cc server entry.
//
// `--config-dir <path>` overrides the default ~/.duckway/ — Phase D's
// SyncCC writes the same flag if the user is on a non-default config dir.
func cmdMCP(configDir string) {
	if len(os.Args) < 3 || os.Args[2] != "serve" {
		fmt.Fprintln(os.Stderr, "Usage: duckway mcp serve [--config-dir <path>]")
		os.Exit(1)
	}
	for i := 3; i < len(os.Args)-1; i++ {
		if os.Args[i] == "--config-dir" {
			configDir = os.Args[i+1]
		}
	}
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "duckway mcp: %v\n", err)
		os.Exit(1)
	}
	srv := client.NewMCPServer(configDir, cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := srv.Run(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "duckway mcp: %v\n", err)
		os.Exit(1)
	}
}

// sudoUpdateCommand builds the copy-pasteable command that re-runs only the
// binary replacement as root. It uses the running binary's absolute path (so it
// works under sudo's restricted PATH) and pins --server explicitly (root's
// environment won't see the user's $DUCKWAY_SERVER_URL or ~/.duckway config).
//
// When --restart was requested, append a user-level restart after sudo returns.
// Do not pass --restart into the sudo command: that would restart daemons as
// root and read root's ~/.duckway instead of the user's daemon PID files.
func sudoUpdateCommand(exe, serverURL string, restartAfter bool) string {
	if exe == "" {
		exe = "duckway"
	}
	cmd := fmt.Sprintf("sudo %s update --server %s", shellQuote(exe), shellQuote(serverURL))
	if restartAfter {
		cmd += fmt.Sprintf(" && %s restart", shellQuote(exe))
	}
	return cmd
}

func confirmSudoUpdate(r io.Reader, w io.Writer) bool {
	fmt.Fprint(w, "Run sudo now to replace the Duckway binary? [y/N] ")
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func runSudoUpdate(exe, serverURL string) error {
	if exe == "" {
		exe = "duckway"
	}
	cmd := exec.Command("sudo", exe, "update", "--server", serverURL)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

type updateOptions struct {
	serverURL              string
	restartAfter           bool
	expectedVersion        string
	expectedBinary         string
	expectedSHA256         string
	expectedSize           int64
	expectedDucklionBinary string
	expectedDucklionSHA256 string
	expectedDucklionSize   int64
}

func parseUpdateOptions(args []string, envServerURL string) updateOptions {
	opts := updateOptions{serverURL: envServerURL}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--server":
			if i+1 < len(args) {
				opts.serverURL = args[i+1]
				i++
			}
		case "--restart":
			opts.restartAfter = true
		case "--expected-version", "--expected-binary", "--expected-sha256", "--expected-size", "--expected-ducklion-binary", "--expected-ducklion-sha256", "--expected-ducklion-size":
			if i+1 < len(args) {
				value := args[i+1]
				i++
				switch args[i-1] {
				case "--expected-version":
					opts.expectedVersion = value
				case "--expected-binary":
					opts.expectedBinary = value
				case "--expected-sha256":
					opts.expectedSHA256 = value
				case "--expected-size":
					opts.expectedSize, _ = strconv.ParseInt(value, 10, 64)
				case "--expected-ducklion-binary":
					opts.expectedDucklionBinary = value
				case "--expected-ducklion-sha256":
					opts.expectedDucklionSHA256 = value
				case "--expected-ducklion-size":
					opts.expectedDucklionSize, _ = strconv.ParseInt(value, 10, 64)
				}
			}
		}
	}
	return opts
}

func cmdUpdate(configDir string) {
	// `duckway update` only needs a server URL — not auth, not keys.
	// Don't require `duckway init` to have run; pick the URL from (in
	// order): --server flag, $DUCKWAY_SERVER_URL, the saved config if
	// one exists. This lets a user upgrade a freshly downloaded binary
	// before they've configured it, or upgrade a binary on a host that
	// never had a client config.
	opts := parseUpdateOptions(os.Args[2:], os.Getenv("DUCKWAY_SERVER_URL"))
	serverURL := opts.serverURL
	if serverURL == "" {
		if cfg, err := client.LoadConfig(configDir); err == nil {
			serverURL = cfg.ServerURL
		}
	}
	updateLock, err := acquireUpdateLock(configDir)
	if err != nil {
		log.Fatalf("cannot start update: %v", err)
	}
	defer releaseUpdateLock(updateLock)
	if os.Getenv("DUCKWAY_MANAGED_UPDATE") == "1" &&
		(opts.expectedVersion == "" || opts.expectedBinary == "" || opts.expectedSHA256 == "" || opts.expectedSize <= 0) {
		log.Fatal("managed update is missing its pinned artifact manifest")
	}
	if serverURL == "" {
		log.Fatalf("no server URL available — pass %s or set DUCKWAY_SERVER_URL, or run %s first",
			cyan("--server <url>"), cyan("duckway init"))
	}

	current := version.Get()
	fmt.Printf("Current: %s\n", current)

	updateInfo, err := client.CheckUpdateInfo(serverURL, current)
	if err != nil {
		log.Fatalf("Could not fetch update info: %v", err)
	}
	if opts.expectedVersion != "" {
		if updateInfo.ClientRecommendedVersion != opts.expectedVersion || updateInfo.Binary != opts.expectedBinary ||
			!strings.EqualFold(updateInfo.SHA256, opts.expectedSHA256) || updateInfo.Size != opts.expectedSize {
			log.Fatalf("rollout artifact changed: got version=%s binary=%s sha256=%s size=%d; expected version=%s binary=%s sha256=%s size=%d",
				updateInfo.ClientRecommendedVersion, updateInfo.Binary, updateInfo.SHA256, updateInfo.Size,
				opts.expectedVersion, opts.expectedBinary, opts.expectedSHA256, opts.expectedSize)
		}
		if opts.expectedDucklionBinary != "" &&
			(updateInfo.DucklionBinary != opts.expectedDucklionBinary ||
				!strings.EqualFold(updateInfo.DucklionSHA256, opts.expectedDucklionSHA256) ||
				updateInfo.DucklionSize != opts.expectedDucklionSize) {
			log.Fatalf("rollout ducklion artifact changed: got binary=%s sha256=%s size=%d; expected binary=%s sha256=%s size=%d",
				updateInfo.DucklionBinary, updateInfo.DucklionSHA256, updateInfo.DucklionSize,
				opts.expectedDucklionBinary, opts.expectedDucklionSHA256, opts.expectedDucklionSize)
		}
	}
	fmt.Printf("Server:  %s\n", updateInfo.ServerVersion)
	fmt.Printf("Target:  %s\n", updateInfo.ClientRecommendedVersion)
	if updateInfo.Reason != "" {
		fmt.Printf("Reason:  %s\n", updateInfo.Reason)
	}

	if !updateInfo.UpdateRequired && !updateInfo.UpdateRecommended {
		fmt.Println("Already up to date.")
		return
	}
	if updateInfo.UpdateRequired {
		fmt.Println("Update required by server policy.")
	}

	fmt.Println("New version available — downloading...")
	if err := client.DownloadAndReplaceClientWithInfo(serverURL, updateInfo); err != nil {
		// Permission-denied usually means the install dir is root-owned
		// (e.g. /usr/local/bin). In an interactive terminal, offer to re-run
		// through sudo and let sudo handle password input. In non-interactive
		// contexts, print the exact command instead of blocking for a password.
		if errors.Is(err, os.ErrPermission) {
			exe, _ := os.Executable()
			sudoCmd := sudoUpdateCommand(exe, serverURL, opts.restartAfter)
			fmt.Fprintf(os.Stderr, "Update failed: %v\n\nThe install location isn't writable by your user.\n", err)
			if canPrompt() && confirmSudoUpdate(os.Stdin, os.Stderr) {
				if sudoErr := runSudoUpdate(exe, serverURL); sudoErr != nil {
					log.Fatalf("sudo update failed: %v\n\nYou can re-run manually:\n\n    %s\n", sudoErr, cyan(sudoCmd))
				}
				if opts.restartAfter {
					fmt.Println("\nUpdated with sudo. Restarting user daemons with the new binary...")
					restartDaemonsRunningBeforeUpdate(configDir)
				}
				return
			}
			log.Fatalf("Re-run with sudo:\n\n    %s\n", cyan(sudoCmd))
		}
		log.Fatalf("Update failed: %v", err)
	}

	// Verify the new binary reports the server's version. We can't import-and-call
	// the new code from this process, so just shell out and run it.
	exe, _ := os.Executable()
	out, err := exec.Command(exe, "version").Output()
	if err == nil {
		fmt.Printf("\nUpdated. New binary: %s", string(out))
	} else {
		fmt.Printf("\nUpdated. Run %s to confirm.\n", cyan("duckway version"))
	}

	if opts.restartAfter {
		restartDaemonsRunningBeforeUpdate(configDir)
		return
	}

	if pid, alive := readPID(filepath.Join(configDir, "proxy.pid")); alive {
		fmt.Printf("\nNote: a proxy daemon is running (PID %d) using the OLD binary.\n", pid)
	}
	if pid, alive := readPID(filepath.Join(configDir, "cc-watch.pid")); alive {
		fmt.Printf("\nNote: a cc-watch daemon is running (PID %d) using the OLD binary.\n", pid)
	}
	if _, proxyAlive := readPID(filepath.Join(configDir, "proxy.pid")); proxyAlive {
		fmt.Println("      Restart daemons to pick up the new code:")
		fmt.Printf("        %s\n", cyan("duckway restart"))
	} else if _, ccAlive := readPID(filepath.Join(configDir, "cc-watch.pid")); ccAlive {
		fmt.Println("      Restart daemons to pick up the new code:")
		fmt.Printf("        %s\n", cyan("duckway restart"))
	}
}

func acquireUpdateLock(configDir string) (*os.File, error) {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(configDir, "update.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another duckway update is already running")
	}
	return f, nil
}

func releaseUpdateLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func restartDaemonsRunningBeforeUpdate(configDir string) {
	proxyPidFile := filepath.Join(configDir, "proxy.pid")
	proxyLogFile := filepath.Join(configDir, "proxy.log")
	ccPidFile := filepath.Join(configDir, "cc-watch.pid")
	ccLogFile := filepath.Join(configDir, "cc-watch.log")

	proxyPID, proxyAlive := readPID(proxyPidFile)
	ccPID, ccAlive := readPID(ccPidFile)
	if !proxyAlive && !ccAlive {
		fmt.Println("\nNo running duckway daemons to restart.")
		return
	}

	fmt.Println("\nRestarting running duckway daemons...")
	if proxyAlive {
		fmt.Printf("duckway proxy: restarting old PID %d\n", proxyPID)
		stopBackgroundDaemon("duckway proxy", proxyPidFile)
		if err := spawnDaemonProcess([]string{"proxy"}, proxyPidFile, proxyLogFile); err != nil {
			log.Fatalf("Failed to restart proxy: %v", err)
		}
		fmt.Printf("duckway proxy: restarted (logs %s)\n", proxyLogFile)
	}
	if ccAlive {
		fmt.Printf("duckway cc watch: restarting old PID %d\n", ccPID)
		oldUseTmux := processEnvValue(ccPID, "DUCKWAY_CC_USE_TMUX") == "1"
		stopBackgroundDaemon("duckway cc watch", ccPidFile)
		if !hasSupportedCCAgent() {
			fmt.Println("duckway cc watch: skipped — neither `claude` nor `codex` is on PATH")
			return
		}
		if oldUseTmux {
			os.Setenv("DUCKWAY_CC_USE_TMUX", "1")
		} else {
			os.Unsetenv("DUCKWAY_CC_USE_TMUX")
		}
		if err := spawnDaemonProcess([]string{"cc", "watch"}, ccPidFile, ccLogFile); err != nil {
			log.Fatalf("Failed to restart cc-watch: %v", err)
		}
		warnIfTmuxRequestedButUnavailable()
		fmt.Printf("duckway cc watch: restarted (logs %s)\n", ccLogFile)
	}
}

func processEnvValue(pid int, key string) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, item := range strings.Split(string(data), "\x00") {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func cmdEnv(configDir string) {
	// `duckway env --proxy` prints the HTTP(S)_PROXY exports for the local
	// proxy instead of the placeholder keys, so a shell can route traffic
	// through duckway via `eval "$(duckway env --proxy)"` or by appending the
	// output to a startup file.
	for _, a := range os.Args[2:] {
		if a == "--proxy" || a == "-p" {
			cfg, err := client.LoadConfig(configDir)
			if err != nil {
				log.Fatal(err)
			}
			client.PrintProxyEnv(cfg.ProxyPort)
			return
		}
	}
	if err := client.PrintEnv(configDir); err != nil {
		log.Fatal(err)
	}
}

func cmdProxy(configDir string) {
	pidFile := filepath.Join(configDir, "proxy.pid")
	logFile := filepath.Join(configDir, "proxy.log")

	// Parse subcommands and flags
	daemon := false
	stop := false
	status := false
	restart := false
	debug := false
	port := 0
	for i := 2; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch arg {
		case "--daemon", "-d":
			daemon = true
		case "stop":
			stop = true
		case "status":
			status = true
		case "restart":
			restart = true
		case "exec":
			cmdProxyExec(configDir, os.Args[i+1:])
			return
		case "hosts":
			cmdHosts(configDir)
			return
		case "--debug", "-D":
			debug = true
		case "--port":
			if i+1 < len(os.Args) {
				port, _ = strconv.Atoi(os.Args[i+1])
				i++
			}
		}
	}

	if stop {
		stopProxyDaemon(pidFile)
		return
	}
	if status {
		statusProxyDaemon(pidFile)
		return
	}
	if restart {
		// Stop waits for the previous process to exit and removes the
		// PID file before returning, so the start path below sees a
		// clean state.
		stopProxyDaemon(pidFile)
		daemon = true
	}

	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}
	if port > 0 {
		cfg.ProxyPort = port
	}

	if daemon {
		startProxyDaemon(pidFile, logFile)
		return
	}

	// Refuse to start if a daemon is already running (unless this IS the daemon child)
	if pid, alive := readPID(pidFile); alive && pid != os.Getpid() {
		log.Fatalf("duckway proxy is already running (PID %d). Run 'duckway proxy stop' first.", pid)
	}

	// On SIGTERM/SIGINT, clean up our PID file (only if it matches our PID)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		if pid, _ := readPID(pidFile); pid == os.Getpid() {
			os.Remove(pidFile)
		}
		os.Exit(0)
	}()

	syncInterval := 5 * time.Minute
	if err := client.RunHTTPSProxy(cfg, syncInterval, debug); err != nil {
		log.Fatal(err)
	}
}

func cmdProxyExec(configDir string, args []string) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: duckway proxy exec -- CMD [ARGS...]")
		os.Exit(2)
	}

	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = proxyExecEnv(os.Environ(), cfg.ProxyPort, configDir)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("duckway proxy exec: %v", err)
	}
}

func proxyExecEnv(base []string, port int, configDir string) []string {
	proxyURL := client.LocalProxyURL(port)
	override := map[string]string{
		"HTTP_PROXY":  proxyURL,
		"HTTPS_PROXY": proxyURL,
		"http_proxy":  proxyURL,
		"https_proxy": proxyURL,
		"ALL_PROXY":   proxyURL,
		"all_proxy":   proxyURL,
		"NO_PROXY":    "localhost,127.0.0.1,::1",
		"no_proxy":    "localhost,127.0.0.1,::1",
	}
	for _, kv := range client.ProxyCAEnv(configDir) {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			override[key] = value
		}
	}
	out := make([]string, 0, len(base)+len(override))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		if _, replace := override[key]; replace {
			continue
		}
		out = append(out, kv)
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"} {
		if value, ok := override[key]; ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

// cmdStart spawns both the proxy and the cc-watch daemons in the
// background — the single command most users want after `duckway init`.
// Defaults to daemon mode (no foreground option), since `duckway proxy`
// and `duckway cc watch` already expose foreground/debug modes
// individually for users who need them.
//
// If a daemon is already running, that subsystem is skipped with a note
// (idempotent — re-running `duckway start` is safe). cc-watch needs at
// least one supported agent binary on PATH; we warn and skip if none are
// present, rather than failing the whole command — most users care about
// the proxy first.
func cmdStart(configDir string) {
	proxyPidFile := filepath.Join(configDir, "proxy.pid")
	proxyLogFile := filepath.Join(configDir, "proxy.log")
	ccPidFile := filepath.Join(configDir, "cc-watch.pid")
	ccLogFile := filepath.Join(configDir, "cc-watch.log")

	// Proxy
	if pid, alive := readPID(proxyPidFile); alive {
		fmt.Printf("duckway proxy: already running (PID %d)\n", pid)
	} else if err := spawnDaemonProcess([]string{"proxy"}, proxyPidFile, proxyLogFile); err != nil {
		log.Fatalf("Failed to start proxy: %v", err)
	} else {
		fmt.Printf("duckway proxy: started (logs %s)\n", proxyLogFile)
	}

	// cc watch — skip cleanly if no supported local agent is installed
	if !hasSupportedCCAgent() {
		fmt.Printf("duckway cc watch: skipped — neither `claude` nor `codex` is on PATH (install a supported agent to enable control channels)\n")
		return
	}
	if pid, alive := readPID(ccPidFile); alive {
		fmt.Printf("duckway cc watch: already running (PID %d)\n", pid)
	} else if err := spawnDaemonProcess([]string{"cc", "watch"}, ccPidFile, ccLogFile); err != nil {
		log.Fatalf("Failed to start cc-watch: %v", err)
	} else {
		warnIfTmuxRequestedButUnavailable()
		fmt.Printf("duckway cc watch: started (logs %s)\n", ccLogFile)
	}
}

func hasSupportedCCAgent() bool {
	for _, bin := range []string{"claude", "codex"} {
		if _, err := exec.LookPath(bin); err == nil {
			return true
		}
	}
	return false
}

func tmuxUnavailableWarning(noTmux bool, lookPath func(string) (string, error)) string {
	if noTmux || os.Getenv("DUCKWAY_CC_USE_TMUX") != "1" {
		return ""
	}
	if _, err := lookPath("tmux"); err == nil {
		return ""
	}
	return "Warning: tmux was requested but is not installed or not on PATH; control channels will use PTY/headless runners."
}

func warnIfTmuxRequestedButUnavailable() {
	if msg := tmuxUnavailableWarning(os.Getenv("DUCKWAY_CC_NO_TMUX") == "1", exec.LookPath); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
}

// cmdStop terminates both daemons. Each side is independent — a missing
// daemon prints a "not running" note but doesn't abort the other stop.
func cmdStop(configDir string) {
	stopBackgroundDaemon("duckway proxy", filepath.Join(configDir, "proxy.pid"))
	stopBackgroundDaemon("duckway cc watch", filepath.Join(configDir, "cc-watch.pid"))
}

// cmdRestart is just stop + start. Sequential (proxy stop → cc stop →
// proxy start → cc start) so the PID files don't race.
func cmdRestart(configDir string) {
	cmdStop(configDir)
	cmdStart(configDir)
}

type logTarget struct {
	Name string
	Path string
}

func cmdLogs(configDir string, args []string) {
	follow := false
	lines := 200
	targetName := "all"

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-f", "--follow":
			follow = true
		case "-n", "--lines":
			if i+1 >= len(args) {
				log.Fatal("duckway logs: --lines requires a number")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				log.Fatalf("duckway logs: invalid --lines value %q", args[i+1])
			}
			lines = n
			i++
		case "proxy", "cc", "cc-watch", "all":
			targetName = args[i]
		default:
			log.Fatalf("duckway logs: unknown argument %q", args[i])
		}
	}

	targets := logTargets(configDir, targetName)
	printLogSnapshot(os.Stdout, targets, lines)
	if follow {
		followLogs(os.Stdout, targets)
	}
}

func logTargets(configDir, targetName string) []logTarget {
	proxy := logTarget{Name: "proxy", Path: filepath.Join(configDir, "proxy.log")}
	cc := logTarget{Name: "cc-watch", Path: filepath.Join(configDir, "cc-watch.log")}
	switch targetName {
	case "proxy":
		return []logTarget{proxy}
	case "cc", "cc-watch":
		return []logTarget{cc}
	default:
		return []logTarget{proxy, cc}
	}
}

func printLogSnapshot(w io.Writer, targets []logTarget, lines int) {
	for i, target := range targets {
		if len(targets) > 1 {
			if i > 0 {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "==> %s (%s) <==\n", target.Name, target.Path)
		}
		if err := printTail(w, target.Path, lines); err != nil {
			fmt.Fprintf(w, "No log file at %s\n", target.Path)
		}
	}
}

func printTail(w io.Writer, path string, lines int) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmt.Fprint(w, lastLines(string(body), lines))
	return nil
}

func lastLines(s string, lines int) string {
	if lines <= 0 || s == "" {
		return s
	}
	trimmed := strings.TrimRight(s, "\n")
	if trimmed == "" {
		return s
	}
	parts := strings.Split(trimmed, "\n")
	if len(parts) <= lines {
		return s
	}
	return strings.Join(parts[len(parts)-lines:], "\n") + "\n"
}

func followLogs(w io.Writer, targets []logTarget) {
	offsets := make(map[string]int64, len(targets))
	for _, target := range targets {
		if st, err := os.Stat(target.Path); err == nil {
			offsets[target.Path] = st.Size()
		}
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for _, target := range targets {
			offset := offsets[target.Path]
			nextOffset, err := printLogAppend(w, target, offset, len(targets) > 1)
			if err == nil {
				offsets[target.Path] = nextOffset
			}
		}
	}
}

func printLogAppend(w io.Writer, target logTarget, offset int64, showHeader bool) (int64, error) {
	f, err := os.Open(target.Path)
	if err != nil {
		return offset, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return offset, err
	}
	size := st.Size()
	if size < offset {
		offset = 0
	}
	if size == offset {
		return offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	if showHeader {
		fmt.Fprintf(w, "\n==> %s (%s) <==\n", target.Name, target.Path)
	}
	if _, err := io.Copy(w, f); err != nil {
		return offset, err
	}
	return size, nil
}

// spawnDaemonProcess re-execs the current binary with an explicit argv
// slice as the child's command-line. Different from startBackgroundDaemon
// which strips flags off os.Args and recurses — for `duckway start` we
// need to spawn TWO children with DIFFERENT argvs (`proxy` vs `cc watch`),
// so the os.Args-based helper can't be reused.
//
// Writes stdio to logFilePath, records the child's PID in pidFile, and
// detaches via Setsid so the daemon outlives the parent process.
func spawnDaemonProcess(childArgs []string, pidFile, logFilePath string) error {
	if pid, alive := readPID(pidFile); alive {
		return fmt.Errorf("already running (PID %d)", pid)
	}
	logF, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log %s: %w", logFilePath, err)
	}
	defer logF.Close()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own executable: %w", err)
	}
	cmd := exec.Command(exe, childArgs...)
	cmd.Stdin = nil
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		log.Printf("Warning: cannot write PID file %s: %v", pidFile, err)
	}
	cmd.Process.Release()
	return nil
}

// startProxyDaemon backgrounds `duckway proxy`.
func startProxyDaemon(pidFile, logFilePath string) {
	startBackgroundDaemon("duckway proxy", "proxy", pidFile, logFilePath)
}

// startCCWatchDaemon backgrounds `duckway cc watch`.
func startCCWatchDaemon(pidFile, logFilePath string) {
	startBackgroundDaemon("duckway cc watch", "cc watch", pidFile, logFilePath)
}

func stopProxyDaemon(pidFile string) {
	stopBackgroundDaemon("duckway proxy", pidFile)
}

func statusProxyDaemon(pidFile string) {
	statusBackgroundDaemon(pidFile)
}

// startBackgroundDaemon re-execs the current binary with --daemon/-d
// stripped from argv, attaches stdio to a log file, writes a PID file,
// and exits the parent. `label` is the user-visible name (e.g. "duckway
// proxy"); `stopVerb` is the subcommand the user types to stop it
// (e.g. "proxy stop" so the printed instruction is `duckway proxy stop`).
func startBackgroundDaemon(label, stopVerb, pidFile, logFilePath string) {
	if pid, alive := readPID(pidFile); alive {
		fmt.Fprintf(os.Stderr, "%s already running (PID %d)\n", label, pid)
		os.Exit(1)
	}

	logF, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.Fatalf("Cannot open log file %s: %v", logFilePath, err)
	}
	defer logF.Close()

	args := []string{}
	for i := 1; i < len(os.Args); i++ {
		// Drop --daemon/-d (the parent acts as launcher; the child runs
		// in foreground inside its own session) and `restart` (we've
		// already stopped the previous instance; the child must just
		// start, not re-enter the stop+start loop).
		switch os.Args[i] {
		case "--daemon", "-d", "restart":
			continue
		}
		args = append(args, os.Args[i])
	}

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("Cannot find own executable: %v", err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start daemon: %v", err)
	}

	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		log.Printf("Warning: cannot write PID file: %v", err)
	}

	fmt.Printf("%s started in background (PID %d)\n", label, cmd.Process.Pid)
	fmt.Printf("  Logs:    %s\n", logFilePath)
	fmt.Printf("  Stop:    %s\n", cyan("duckway "+stopVerb+" stop"))
	fmt.Printf("  Restart: %s\n", cyan("duckway "+stopVerb+" restart"))
	cmd.Process.Release()
}

func stopBackgroundDaemon(label, pidFile string) {
	pid, alive := readPID(pidFile)
	if pid == 0 {
		fmt.Printf("No PID file — %s does not appear to be running.\n", label)
		return
	}
	if !alive {
		fmt.Printf("Stale PID file (PID %d not running). Removing.\n", pid)
		os.Remove(pidFile)
		return
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		log.Fatalf("Failed to send SIGTERM to PID %d: %v", pid, err)
	}
	for i := 0; i < 20; i++ {
		if _, alive := readPID(pidFile); !alive {
			os.Remove(pidFile)
			fmt.Printf("Stopped %s (PID %d)\n", label, pid)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("PID %d did not exit within 2s — sent SIGTERM, check 'ps' if it lingers.\n", pid)
	os.Remove(pidFile)
}

func statusBackgroundDaemon(pidFile string) {
	pid, alive := readPID(pidFile)
	if pid == 0 {
		fmt.Println("Status: not running (no PID file)")
		return
	}
	if alive {
		fmt.Printf("Status: running (PID %d)\n", pid)
	} else {
		fmt.Printf("Status: stale PID file (PID %d not running)\n", pid)
	}
}

// readPID returns the recorded PID and whether that process is alive.
func readPID(pidFile string) (int, bool) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	// Signal 0 = check existence
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

func cmdStatus(configDir string) {
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		fmt.Println("Status: not initialized")
		fmt.Printf("Run %s to set up\n", cyan("duckway init"))
		return
	}

	fmt.Printf("Server:      %s\n", cfg.ServerURL)
	fmt.Printf("Client name: %s\n", cfg.ClientName)
	fmt.Printf("Version:     %s\n", version.Get())
	fmt.Printf("Proxy port:  %d\n", cfg.ProxyPort)

	api := client.NewAPIClient(cfg.ServerURL, cfg.Token)
	if err := api.Ping(); err != nil {
		fmt.Printf("Connection:  FAILED (%v)\n", err)
		return
	}
	fmt.Println("Connection:  OK")

	if updateInfo, err := client.CheckUpdateInfo(cfg.ServerURL, version.Get()); err == nil {
		switch {
		case updateInfo.UpdateRequired:
			fmt.Printf("Update:      REQUIRED -> %s\n", updateInfo.ClientRecommendedVersion)
			if updateInfo.Reason != "" {
				fmt.Printf("  %s\n", updateInfo.Reason)
			}
		case updateInfo.UpdateRecommended:
			fmt.Printf("Update:      available -> %s\n", updateInfo.ClientRecommendedVersion)
			if updateInfo.Reason != "" {
				fmt.Printf("  %s\n", updateInfo.Reason)
			}
		default:
			fmt.Println("Update:      up to date")
		}
	}

	keys, err := api.FetchKeys()
	if err != nil {
		fmt.Printf("Keys:        error (%v)\n", err)
	} else {
		fmt.Printf("Keys:        %d placeholder keys assigned\n", len(keys))
		for _, k := range keys {
			if k.EnvName == "DUCKWAY_HEARTBEAT" {
				continue // don't show heartbeat in key list
			}
			fmt.Printf("  %s (%s) = %s...%s\n", k.EnvName, k.ServiceName, k.Placeholder[:12], k.Placeholder[len(k.Placeholder)-4:])
		}
	}

	// Test heartbeat proxy
	hbResult := api.Heartbeat()
	if hbResult == nil {
		fmt.Println("Heartbeat:   OK (proxy reachable)")
	} else {
		fmt.Printf("Heartbeat:   FAILED (%v)\n", hbResult)
	}

	// Check if local proxy is running
	proxyURL := client.LocalProxyURL(cfg.ProxyPort)
	proxyRunning := false
	resp, err := http.Get(proxyURL + "/proxy/heartbeat/ping")
	if err == nil {
		resp.Body.Close()
		// The proxy doesn't handle plain GET without token, but if we get a response it's alive
		proxyRunning = true
	}

	if proxyRunning {
		fmt.Printf("Local proxy: RUNNING on %s\n", proxyURL)
		fmt.Printf("  %s\n", cyan("export HTTPS_PROXY="+proxyURL))
		fmt.Printf("  %s\n", cyan("export HTTP_PROXY="+proxyURL))
	} else {
		fmt.Printf("Local proxy: NOT RUNNING (start with: %s)\n", cyan("duckway proxy -d"))
		fmt.Printf("  Will listen on %s\n", proxyURL)
	}

	// CC watch daemon — only mention it when a CC is actually assigned to
	// this client, otherwise the hint is just noise.
	if state, err := client.LoadCCState(configDir); err == nil && len(state.CCs) > 0 {
		ccPid := filepath.Join(configDir, "cc-watch.pid")
		if pid, alive := readPID(ccPid); alive {
			fmt.Printf("CC watch:    RUNNING (PID %d, %d CC assigned)\n", pid, len(state.CCs))
		} else {
			fmt.Printf("CC watch:    NOT RUNNING (start with: %s)\n", cyan("duckway cc watch -d"))
			fmt.Printf("  %d CC assigned — the daemon delivers Discord messages to claude\n", len(state.CCs))
		}
	}

	// Check CA cert and report expiry
	caPath := configDir + "/ca.pem"
	caData, err := os.ReadFile(caPath)
	if err != nil {
		fmt.Printf("CA cert:     MISSING (run %s to download)\n", cyan("duckway init"))
		return
	}
	block, _ := pem.Decode(caData)
	if block == nil {
		fmt.Println("CA cert:     installed (could not parse PEM)")
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		fmt.Printf("CA cert:     installed (parse error: %v)\n", err)
		return
	}
	now := time.Now()
	expiresIn := cert.NotAfter.Sub(now)
	exp := cert.NotAfter.Format("2006-01-02")
	switch {
	case now.After(cert.NotAfter):
		fmt.Printf("CA cert:     EXPIRED on %s — re-run %s\n", exp, cyan("duckway init"))
	case expiresIn < 30*24*time.Hour:
		days := int(expiresIn / (24 * time.Hour))
		fmt.Printf("CA cert:     expires %s (%d days — consider re-running %s)\n", exp, days, cyan("duckway init"))
	default:
		days := int(expiresIn / (24 * time.Hour))
		fmt.Printf("CA cert:     expires %s (%d days)\n", exp, days)
	}
}

func cmdHosts(configDir string) {
	if len(os.Args) >= 4 && os.Args[3] == "reload" {
		pidFile := filepath.Join(configDir, "proxy.pid")
		pid, alive := readPID(pidFile)
		if pid == 0 || !alive {
			log.Fatalf("duckway proxy is not running (no live PID at %s)", pidFile)
		}
		if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
			log.Fatalf("signal proxy (PID %d): %v", pid, err)
		}
		fmt.Printf("Sent reload signal to proxy daemon (PID %d)\n", pid)
		return
	}

	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
	}
	svcs, err := client.FetchServices(cfg.ServerURL, cfg.Token)
	if err != nil {
		log.Fatalf("fetch services: %v", err)
	}
	if len(svcs) == 0 {
		fmt.Println("No proxy services configured on server.")
		return
	}

	fmt.Printf("%-20s  %-38s  %s\n", "SERVICE", "HOSTS", "MODE")
	fmt.Printf("%-20s  %-38s  %s\n",
		strings.Repeat("─", 20), strings.Repeat("─", 38), strings.Repeat("─", 25))
	for _, s := range svcs {
		mode := s.DeliveryMode
		if mode == "" {
			mode = "proxy"
		}
		if mode == "loan_proxy" && s.UpstreamURL != "" {
			mode = "loan_proxy → " + s.UpstreamURL
		}
		fmt.Printf("%-20s  %-38s  %s\n", s.Name, s.HostPattern, mode)
	}
}
