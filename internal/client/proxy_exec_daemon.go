package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const daemonOwnerEnv = "DUCKWAY_CODEX_DAEMON_OWNER"
const daemonFingerprintEnv = "DUCKWAY_CODEX_DAEMON_FINGERPRINT"

type daemonOperation struct {
	needed, mutate, start, stopped, after bool
}

// CodexDaemonPreparation verifies the backend before launch and, for lifecycle
// operations, after successful completion. It never stops or restarts a daemon.
type CodexDaemonPreparation struct {
	socket, owner, fingerprint string
	env                        []string
	op                         daemonOperation
}

func codexDaemonOperation(args []string) (daemonOperation, error) {
	var op daemonOperation
	if len(args) == 0 || !isCodexCommand(args[0]) {
		return op, nil
	}
	options := codexOptions(args[1:])
	for _, option := range options {
		if option == "--help" || option == "-h" || option == "--version" || option == "-V" {
			return op, nil
		}
	}
	command, rest := codexSubcommand(args[1:])
	// Explicit targets cannot be proved by inspecting the native home socket.
	if command == "" || command == "resume" || command == "fork" || command == "agents" || command == "remote-control" {
		for _, option := range codexOptionTokens(args[1:]) {
			name, _, _ := strings.Cut(option, "=")
			if name == "--remote" || name == "--remote-session" {
				return op, fmt.Errorf("duckway proxy exec cannot verify an explicit Codex remote target; use native daemon discovery")
			}
		}
	}
	foreground := false
	for _, option := range options {
		if option == "--no-daemon" {
			foreground = true
		}
	}
	switch command {
	case "", "resume", "fork", "agents":
		if foreground {
			return op, nil
		}
		op.needed, op.start = true, true
	case "remote-control":
		sub, _ := codexScopedSubcommand(rest, "start stop pair")
		if sub == "" {
			if foreground {
				return op, nil
			}
			for _, option := range options {
				if option == "--json" {
					return op, nil
				}
			}
		}
		op.needed, op.start, op.after = true, true, true
		// Enabling remote control can replace a daemon to apply saved settings.
		op.mutate = sub == "start" || sub == "stop" || sub == ""
		op.stopped = sub == "stop"
		op.start = !op.stopped
	case "app-server", "daemon":
		if command == "app-server" {
			nested, remaining := codexScopedSubcommand(rest, "daemon proxy")
			if nested == "" {
				return op, nil
			} // e.g. app-server --listen stdio://
			if nested == "proxy" {
				return op, fmt.Errorf("duckway proxy exec cannot verify a Codex app-server proxy target")
			}
			rest = remaining
		}
		sub, _ := codexScopedSubcommand(rest, "start stop restart update bootstrap enable-remote-control disable-remote-control version status")
		if sub == "version" || sub == "status" {
			return op, nil
		}
		if sub == "" {
			return op, fmt.Errorf("duckway proxy exec cannot verify this Codex daemon operation; use an explicit supported lifecycle command")
		}
		op.needed, op.mutate, op.after = true, sub != "start", true
		op.stopped = sub == "stop"
		// Let the explicit native command perform startup, bootstrap, or updates.
	case "help":
		return op, nil
	default:
		return op, nil
	}
	if command == "" || command == "resume" || command == "fork" || command == "agents" {
		if command != "agents" && codexConfigSelectsEmbedded(args[1:]) {
			return daemonOperation{}, nil
		}
		for _, option := range codexOptionTokens(args[1:]) {
			name, _, _ := strings.Cut(option, "=")
			switch name {
			case "--oss", "--workload-identity":
				return daemonOperation{}, nil
			case "--profile", "-p", "--strict-config", "--dangerously-bypass-hook-trust":
				if command == "agents" {
					continue
				}
				// Upstream daemon_startup::exclusion selects an embedded server.
				return daemonOperation{}, nil
			case "--enable", "--disable", "--search":
				// Upstream may ask to restart for explicitly requested features.
				op.mutate, op.after = true, true
			case "--remote-session":
				return daemonOperation{}, fmt.Errorf("duckway proxy exec cannot verify an explicit Codex --remote-session target")
			case "--config", "-c":
				// Native TUI applies overrides per client, selecting embedded
				// execution for most settings. Boolean feature overrides may
				// replace the shared server, so require ownership before launch.
				op.mutate, op.after = true, true
			}
		}
	}

	// Explicit endpoints bypass native discovery. Never bless an unverified server.
	for _, arg := range codexOptionTokens(args[1:]) {
		if strings.HasPrefix(arg, "--remote=") || arg == "--remote" {
			return daemonOperation{}, fmt.Errorf("duckway proxy exec cannot verify an explicit Codex --remote endpoint; use native daemon discovery or explicit --no-daemon")
		}
	}
	return op, nil
}

// Only conclusively embedded config keys bypass daemon preparation. Native
// TOML parsing remains authoritative; accepted feature/table values are left
// untouched and guarded as potentially shared-server mutations.
func codexConfigSelectsEmbedded(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		var value string
		if arg == "-c" || arg == "--config" {
			if i+1 >= len(args) {
				continue
			}
			i++
			value = args[i]
		} else if strings.HasPrefix(arg, "--config=") {
			value = strings.TrimPrefix(arg, "--config=")
		} else if strings.HasPrefix(arg, "-c") && len(arg) > 2 {
			value = strings.TrimPrefix(arg[2:], "=")
		} else {
			if codexValueOptions[arg] {
				i++
			}
			continue
		}
		key, _, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "use_legacy_landlock" || key == "features" || key == "tui" || key == "suppress_unstable_features_warning" || key == "tui.fullscreen_transcript" {
			continue
		}
		if strings.HasPrefix(key, "features.") && !strings.Contains(strings.TrimPrefix(key, "features."), ".") {
			continue
		}
		return true
	}
	return false
}

// PrepareCodexDaemon preserves CODEX_HOME and native CLI semantics. A daemon
// whose startup trust/proxy environment cannot be proven is a conflict, not a
// reason to interrupt someone else's work or silently fall back to foreground.
func PrepareCodexDaemon(args, env []string, configDir string) (*CodexDaemonPreparation, []string, error) {
	op, err := codexDaemonOperation(args)
	if err != nil || !op.needed {
		return nil, env, err
	}
	if !codexDaemonVerificationSupported() {
		return nil, env, fmt.Errorf("duckway proxy exec cannot verify Codex daemon startup environment on this platform; explicit --no-daemon remains available")
	}
	values := daemonEnvMap(env)
	if values["CODEX_EXEC_SERVER_URL"] != "" {
		command, _ := codexSubcommand(args[1:])
		if command == "" || command == "resume" || command == "fork" || command == "agents" {
			return nil, env, nil // upstream explicitly selects embedded execution
		}
	}
	home := values["CODEX_HOME"]
	if home == "" {
		home = filepath.Join(values["HOME"], ".codex")
	}
	if !filepath.IsAbs(home) {
		return nil, env, fmt.Errorf("duckway proxy exec requires an absolute Codex home for daemon verification")
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return nil, env, fmt.Errorf("resolve Codex home: %w", err)
	}
	owner, err := daemonOwner(configDir)
	if err != nil {
		return nil, env, err
	}
	fingerprint, err := daemonEnvFingerprint(env)
	if err != nil {
		return nil, env, err
	}
	prepared := append([]string(nil), env...)
	prepared = setDaemonEnv(prepared, daemonOwnerEnv, owner)
	prepared = setDaemonEnv(prepared, daemonFingerprintEnv, fingerprint)
	p := &CodexDaemonPreparation{socket: filepath.Join(home, "app-server-control", "app-server-control.sock"), owner: owner, fingerprint: fingerprint, env: prepared, op: op}
	exists, err := p.verify(op.mutate)
	if err != nil {
		return nil, env, err
	}
	if !exists && op.start {
		if err := p.start(args[0]); err != nil {
			return nil, env, err
		}
		if exists, err = p.verify(op.mutate); err != nil || !exists {
			if err == nil {
				err = fmt.Errorf("codex daemon did not expose its native control socket")
			}
			return nil, env, err
		}
	}
	return p, prepared, nil
}

// VerifyAfter checks native lifecycle replacements before reporting success.
// Long-running interactive clients already use the backend verified at startup.
func (p *CodexDaemonPreparation) VerifyAfter() error {
	if p == nil || !p.op.after {
		return nil
	}
	exists, err := p.verify(p.op.mutate)
	if err != nil {
		return err
	}
	if p.op.stopped && exists {
		return fmt.Errorf("codex daemon remains running after stop")
	}
	if !p.op.stopped && !exists {
		return fmt.Errorf("codex daemon lifecycle completed without a verifiable backend")
	}
	return nil
}

func (p *CodexDaemonPreparation) start(binary string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server", "daemon", "start")
	cmd.Env = p.env
	cmd.WaitDelay = 200 * time.Millisecond
	configureDaemonProbe(cmd)
	defer cleanupDaemonProbe(cmd)
	// Never forward lifecycle stderr or malformed output: it may include inherited
	// network settings, remote-control capabilities, or credential-bearing URLs.
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("codex native daemon startup failed; verify the installed CLI supports app-server daemon start (no existing daemon was stopped)")
	}
	var response struct{ Status, SocketPath string }
	if json.Unmarshal(output, &response) != nil || (response.Status != "started" && response.Status != "alreadyRunning") || filepath.Clean(response.SocketPath) != filepath.Clean(p.socket) {
		return fmt.Errorf("codex native daemon returned an unsupported startup response")
	}
	return nil
}

func daemonOwner(configDir string) (string, error) {
	// The token identifies wrapper ownership; never emit it in diagnostics.
	info, err := os.Lstat(configDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("codex daemon ownership requires a private Duckway configuration directory")
	}
	f, err := openDaemonOwner(filepath.Join(configDir, "codex-daemon-owner"))
	if err != nil {
		return "", fmt.Errorf("open private Codex daemon ownership token")
	}
	defer closeDaemonOwner(f)
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("codex daemon ownership token is not a private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return "", fmt.Errorf("read Codex daemon ownership token")
	}
	if len(data) == 0 {
		data = make([]byte, 32)
		if _, err := rand.Read(data); err != nil {
			return "", fmt.Errorf("create Codex daemon ownership token")
		}
		token := hex.EncodeToString(data)
		if _, err := f.WriteString(token); err != nil {
			return "", fmt.Errorf("write Codex daemon ownership token")
		}
		if err := f.Sync(); err != nil {
			return "", fmt.Errorf("persist Codex daemon ownership token")
		}
		return token, nil
	}
	if len(data) != 64 {
		return "", fmt.Errorf("invalid Codex daemon ownership token")
	}
	if _, err := hex.DecodeString(string(data)); err != nil {
		return "", fmt.Errorf("invalid Codex daemon ownership token")
	}
	return string(data), nil
}

func daemonEnvMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if key, value, ok := strings.Cut(kv, "="); ok {
			m[key] = value
		}
	}
	return m
}

func setDaemonEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

func daemonRelevantEnv(key string) bool {
	switch key {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy", "CODEX_CA_CERTIFICATE", "SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "CODEX_HOME", "HOME", "OPENAI_BASE_URL", "CHATGPT_BASE_URL", "CODEX_EXEC_SERVER_URL":
		return true
	}
	return strings.HasSuffix(key, "_API_KEY") || strings.HasSuffix(key, "_ACCESS_TOKEN") || strings.HasSuffix(key, "_AUTH_TOKEN")
}

func daemonEnvFingerprint(env []string) (string, error) {
	values := daemonEnvMap(env)
	var keys []string
	for key := range values {
		if daemonRelevantEnv(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, key := range keys {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", key, values[key])
		switch key {
		case "CODEX_CA_CERTIFICATE", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS":
			if values[key] == "" {
				continue
			}
			data, err := os.ReadFile(values[key])
			if err != nil {
				return "", fmt.Errorf("cannot fingerprint Codex daemon CA material for %s", key)
			}
			_, _ = h.Write(data)
			_, _ = h.Write([]byte{0})
		case "SSL_CERT_DIR":
			if values[key] != "" {
				return "", fmt.Errorf("codex daemon trust verification does not support inherited SSL_CERT_DIR; use explicit CA files")
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (p *CodexDaemonPreparation) verify(requireOwner bool) (bool, error) {
	if fingerprint, err := daemonEnvFingerprint(p.env); err != nil || fingerprint != p.fingerprint {
		return false, fmt.Errorf("codex daemon CA material changed during launch; retry with stable trust files")
	}
	socket := p.socket
	if _, err := os.Lstat(socket); errors.Is(err, os.ErrNotExist) {
		if err := p.verifyPidRecords(false, requireOwner); err != nil {
			return false, err
		}
		return false, nil
	}
	if resolved, err := filepath.EvalSymlinks(socket); err == nil {
		socket = resolved
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || daemonSocketRefused(err) {
			if err := p.verifyPidRecords(false, requireOwner); err != nil {
				return false, err
			}
			return false, nil
		}
		return false, fmt.Errorf("cannot inspect Codex daemon socket; existing backend was left untouched")
	}
	defer conn.Close()
	actual, err := codexDaemonPeerEnvironment(conn)
	if err != nil {
		return true, fmt.Errorf("cannot verify live Codex daemon environment; existing backend was left untouched")
	}
	if err := p.verifyEnvironment(actual, requireOwner); err != nil {
		return true, err
	}
	if err := p.verifyPidRecords(true, requireOwner); err != nil {
		return true, err
	}
	return true, nil
}

func (p *CodexDaemonPreparation) verifyEnvironment(actual []string, requireOwner bool) error {
	want, got := daemonEnvMap(p.env), daemonEnvMap(actual)
	for key, value := range want {
		if daemonRelevantEnv(key) {
			other, ok := got[key]
			if !ok || other != value {
				return daemonConflict()
			}
		}
	}
	for key := range got {
		if daemonRelevantEnv(key) {
			if _, ok := want[key]; !ok {
				return daemonConflict()
			}
		}
	}
	if got[daemonFingerprintEnv] != p.fingerprint {
		return daemonConflict()
	}
	if requireOwner && got[daemonOwnerEnv] != p.owner {
		return fmt.Errorf("codex daemon belongs to another launcher; lifecycle changes refused and existing backend left untouched")
	}
	return nil
}

func (p *CodexDaemonPreparation) verifyPidRecords(socketLive, requireOwner bool) error {
	state := filepath.Join(filepath.Dir(filepath.Dir(p.socket)), "app-server-daemon")
	for _, name := range []string{"app-server.pid", "daemon.pid", "app-server-updater.pid", "daemon-updater.pid"} {
		data, err := os.ReadFile(filepath.Join(state, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot inspect Codex daemon process record; backend left untouched")
		}
		var record struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(data, &record) != nil || record.PID <= 0 {
			return fmt.Errorf("codex daemon process record is unverifiable or starting; retry after it becomes ready")
		}
		actual, alive, err := codexDaemonProcessEnvironment(record.PID)
		if err != nil {
			return fmt.Errorf("cannot verify Codex managed process environment; backend left untouched")
		}
		if !alive {
			continue
		}
		if !socketLive && !strings.Contains(name, "updater") {
			return fmt.Errorf("codex daemon is running without a verifiable control socket; backend left untouched")
		}
		if err := p.verifyEnvironment(actual, requireOwner); err != nil {
			return err
		}
	}
	return nil
}

func daemonConflict() error {
	return fmt.Errorf("existing Codex daemon has unverified or stale proxy, credential, or CA startup settings; it was left untouched; finish its work and stop it explicitly before retrying, or choose explicit --no-daemon")
}
