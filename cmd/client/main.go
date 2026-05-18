package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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

// cyan wraps s in cyan ANSI color when stdout is a terminal, for commands the user should run.
func cyan(s string) string {
	if !isTTY {
		return s
	}
	return "\033[36m" + s + "\033[0m"
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
  duckway sync           Fetch placeholder keys from server
  duckway env            Print keys as shell export statements
  duckway proxy          Start local proxy (foreground)
  duckway proxy -d       Start local proxy as background daemon
  duckway proxy stop     Stop the running daemon
  duckway proxy status   Show daemon status
  duckway status         Show connection status, CA cert expiry
  duckway install-ca     Re-install the Duckway CA into the system trust store
  duckway update         Compare local version with server, download + replace if drifted
                         (uses saved config; override with --server <url>
                          or DUCKWAY_SERVER_URL — works without init)
  duckway mcp serve      Run the Control-Channel MCP server over stdio
                         (launched by Claude Code from ~/.claude/mcp.json)
  duckway cc watch       Connect to the server's SSE feed and run a
                         claude session per Discord task channel
                         (when tmux is installed claude runs inside
                         a session named duckway-<handle>; attach with
                         "tmux attach -t duckway-<handle>")
  duckway cc watch -d    Same, but run in background as a daemon
  duckway cc watch --no-tmux  Force the headless --print runner
                         (also: DUCKWAY_CC_NO_TMUX=1)
  duckway cc watch stop  Stop the running daemon
  duckway cc watch status  Show daemon status
  duckway cc bind        Interactive picker: pick existing local claude
                         sessions and create a CC channel + binding for
                         each. Use --session <id> for headless mode.
  duckway version        Print the duckway version

Proxy flags:
  --port N               Override proxy port
  --daemon, -d           Run in background
  --debug, -D            Log every request/response (stdout in foreground,
                         ~/.duckway/proxy.log in daemon mode)

Config directory: ~/.duckway/
Daemon files:
  ~/.duckway/proxy.pid   PID of the running daemon
  ~/.duckway/proxy.log   Daemon logs (stdout + stderr)`)
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

	fmt.Printf("\nConfig saved to %s/config.yaml\n", configDir)
	fmt.Println("\nNext steps:")
	fmt.Printf("  %s           — start HTTPS proxy (background daemon)\n", cyan("duckway proxy -d"))
	fmt.Printf("  %s\n", cyan(fmt.Sprintf("export HTTPS_PROXY=http://localhost:%d", cfg.ProxyPort)))
	fmt.Printf("  %s\n", cyan(fmt.Sprintf("export HTTP_PROXY=http://localhost:%d", cfg.ProxyPort)))
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
}

// cmdCC dispatches `duckway cc <subcommand>`.
func cmdCC(configDir string) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  duckway cc watch [-d|--daemon|stop|status]")
		fmt.Fprintln(os.Stderr, "  duckway cc bind [--session <id>...] [--cwd <substr>]")
		os.Exit(1)
	}
	switch os.Args[2] {
	case "watch":
		cmdCCWatch(configDir)
	case "bind":
		cmdCCBind(configDir)
	default:
		fmt.Fprintf(os.Stderr, "Unknown cc subcommand: %s\n", os.Args[2])
		os.Exit(1)
	}
}

func cmdCCWatch(configDir string) {
	pidFile := filepath.Join(configDir, "cc-watch.pid")
	logFile := filepath.Join(configDir, "cc-watch.log")

	daemon := false
	stop := false
	status := false
	// --no-tmux forces the headless --print runner even when tmux is
	// installed. Also honored via DUCKWAY_CC_NO_TMUX=1 in the environment
	// so it's settable from a systemd unit without rewriting argv.
	noTmux := os.Getenv("DUCKWAY_CC_NO_TMUX") == "1"
	for i := 3; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch arg {
		case "--daemon", "-d":
			daemon = true
		case "stop":
			stop = true
		case "status":
			status = true
		case "--no-tmux":
			noTmux = true
		case "--config-dir":
			if i+1 < len(os.Args) {
				configDir = os.Args[i+1]
				pidFile = filepath.Join(configDir, "cc-watch.pid")
				logFile = filepath.Join(configDir, "cc-watch.log")
				i++
			}
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

	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		log.Fatal(err)
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

	w, err := client.NewCCWatchWithOptions(configDir, cfg, client.CCWatchOptions{NoTmux: noTmux})
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

func cmdUpdate(configDir string) {
	// `duckway update` only needs a server URL — not auth, not keys.
	// Don't require `duckway init` to have run; pick the URL from (in
	// order): --server flag, $DUCKWAY_SERVER_URL, the saved config if
	// one exists. This lets a user upgrade a freshly downloaded binary
	// before they've configured it, or upgrade a binary on a host that
	// never had a client config.
	serverURL := os.Getenv("DUCKWAY_SERVER_URL")
	for i := 2; i < len(os.Args); i++ {
		if os.Args[i] == "--server" && i+1 < len(os.Args) {
			serverURL = os.Args[i+1]
			i++
		}
	}
	if serverURL == "" {
		if cfg, err := client.LoadConfig(configDir); err == nil {
			serverURL = cfg.ServerURL
		}
	}
	if serverURL == "" {
		log.Fatalf("no server URL available — pass %s or set DUCKWAY_SERVER_URL, or run %s first",
			cyan("--server <url>"), cyan("duckway init"))
	}

	current := version.Get()
	fmt.Printf("Current: %s\n", current)

	serverVer, err := client.CheckServerVersion(serverURL)
	if err != nil {
		log.Fatalf("Could not reach server: %v", err)
	}
	fmt.Printf("Server:  %s\n", serverVer)

	if serverVer == current {
		fmt.Println("Already up to date.")
		return
	}

	fmt.Println("New version available — downloading...")
	if err := client.DownloadAndReplaceClient(serverURL); err != nil {
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

	if pid, alive := readPID(filepath.Join(configDir, "proxy.pid")); alive {
		fmt.Printf("\nNote: a proxy daemon is running (PID %d) using the OLD binary.\n", pid)
		fmt.Println("      Restart it to pick up the new code:")
		fmt.Printf("        %s\n", cyan("duckway proxy stop && duckway proxy -d"))
	}
}

func cmdEnv(configDir string) {
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
		if os.Args[i] == "--daemon" || os.Args[i] == "-d" {
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
	fmt.Printf("  Logs:  %s\n", logFilePath)
	fmt.Printf("  Stop:  %s\n", cyan("duckway "+stopVerb+" stop"))
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
	fmt.Printf("Proxy port:  %d\n", cfg.ProxyPort)

	api := client.NewAPIClient(cfg.ServerURL, cfg.Token)
	if err := api.Ping(); err != nil {
		fmt.Printf("Connection:  FAILED (%v)\n", err)
		return
	}
	fmt.Println("Connection:  OK")

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
	proxyURL := fmt.Sprintf("http://localhost:%d", cfg.ProxyPort)
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
