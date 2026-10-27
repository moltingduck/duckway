package client

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ProxyExecArgs keeps Codex's backend in this process tree. A shared daemon
// retains its launch environment, so changing the TUI environment is insufficient.
// Probe the installed CLI's help instead of assuming a version supports the flag.
func ProxyExecArgs(args, env []string) ([]string, error) {
	if len(args) == 0 || !isCodexCommand(args[0]) {
		return args, nil
	}
	command, rest := codexSubcommand(args[1:])
	nested, nestedRest := codexScopedSubcommand(rest, "daemon proxy start stop pair status restart")
	if command == "app-server" && (nested == "daemon" || nested == "proxy") {
		status, _ := codexScopedSubcommand(nestedRest, "status")
		if nested == "daemon" && status == "status" {
			return args, nil
		}
		return nil, fmt.Errorf("duckway proxy exec cannot change a shared Codex daemon; use codex app-server directly or foreground codex --no-daemon")
	}
	switch command {
	case "daemon", "agents":
		if command == "daemon" && len(rest) > 0 && rest[0] == "status" {
			return args, nil
		}
		return nil, fmt.Errorf("duckway proxy exec cannot change or attach to a shared Codex daemon: use codex --no-daemon or codex remote-control --no-daemon through proxy exec")
	case "remote-control":
		subcommand, _ := codexScopedSubcommand(rest, "start stop pair")
		if subcommand == "start" || subcommand == "stop" || subcommand == "pair" {
			return nil, fmt.Errorf("duckway proxy exec requires foreground remote control; use codex remote-control --no-daemon (start/stop/pair operate on the shared daemon)")
		}
	case "", "resume", "fork":
	default:
		return args, nil
	}
	for _, arg := range codexOptions(args[1:]) {
		if arg == "--help" || arg == "-h" || arg == "--version" || arg == "-V" {
			return args, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	helpArgs := []string{"--help"}
	if command == "remote-control" {
		helpArgs = []string{"remote-control", "--help"}
	}
	probe := exec.CommandContext(ctx, args[0], helpArgs...)
	probe.Env = env
	probe.WaitDelay = 100 * time.Millisecond
	probe.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	probe.Cancel = func() error { return syscall.Kill(-probe.Process.Pid, syscall.SIGKILL) }
	defer func() {
		if probe.Process != nil {
			_ = syscall.Kill(-probe.Process.Pid, syscall.SIGKILL)
		}
	}()
	help, err := probe.Output()
	if err != nil || !strings.Contains(string(help), "--no-daemon") {
		return nil, fmt.Errorf("installed Codex does not advertise --no-daemon; upgrade Codex before using its interactive backend through duckway proxy exec")
	}
	for _, arg := range codexOptions(args[1:]) {
		if arg == "--no-daemon" {
			return args, nil
		}
	}
	// This root option is forwarded by Codex to resume/fork and remote-control.
	out := make([]string, 0, len(args)+1)
	out = append(out, args[0], "--no-daemon")
	out = append(out, args[1:]...)
	return out, nil
}

// Only option tokens count: values and everything following -- are user data.
func codexOptions(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if codexValueOptions[arg] {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			out = append(out, arg)
		}
	}
	return out
}

var codexValueOptions = map[string]bool{"-c": true, "--config": true, "-m": true, "--model": true, "-p": true, "--profile": true, "-s": true, "--sandbox": true, "-a": true, "--ask-for-approval": true, "-C": true, "--cd": true, "--add-dir": true, "--enable": true, "--disable": true, "-i": true, "--image": true, "--remote": true, "--remote-session": true, "--oss-provider": true, "--local-provider": true}

func isCodexCommand(command string) bool {
	name := filepath.Base(command)
	return name == "codex" || name == "codex.exe"
}

// Skip values of root options before looking for a clap subcommand. Positional
// prompts which aren't subcommands remain interactive TUI invocations.
func codexSubcommand(args []string) (string, []string) {
	return codexScopedSubcommand(args, "exec e review login logout mcp mcp-server app-server app completion sandbox debug apply a resume fork cloud features remote-control daemon agents update sync help")
}

func codexScopedSubcommand(args []string, names string) (string, []string) {
	values := codexValueOptions
	commands := map[string]bool{}
	for _, s := range strings.Fields(names) {
		commands[s] = true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if values[arg] {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if commands[arg] {
			return arg, args[i+1:]
		}
		break
	}
	return "", nil
}

// ProxyExecKeyEnv applies the synced assignments without evaluating shell code.
// Native Codex OAuth must not inherit an API key that selects API-key login.
// A missing keys.env preserves the caller's environment, as proxy exec also
// supports commands that use their own credentials.
func ProxyExecKeyEnv(base []string, configDir, command string) []string {
	keys := loadKeysEnv(configDir)
	nativeCodex := isCodexCommand(command) && codexNativeOAuthConfigured(configDir)
	overrides := make(map[string]string, len(keys))
	var order []string
	for _, kv := range keys {
		key, value, _ := strings.Cut(kv, "=")
		if _, exists := overrides[key]; !exists {
			order = append(order, key)
		}
		overrides[key] = value
	}
	out := make([]string, 0, len(base)+len(keys))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, replace := overrides[key]; replace || (nativeCodex && key == "OPENAI_API_KEY") {
			continue
		}
		out = append(out, kv)
	}
	for _, key := range order {
		if nativeCodex && key == "OPENAI_API_KEY" {
			continue
		}
		out = append(out, key+"="+overrides[key])
	}
	return out
}
