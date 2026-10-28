package client

import (
	"path/filepath"
	"strings"
)

// ProxyExecArgs preserves the user's native Codex arguments. Backend preparation
// is separate because the environment belongs to the daemon, not its TUI client.
func ProxyExecArgs(args, env []string) ([]string, error) {
	return args, nil
}

// codexOptionTokens retains option names while skipping their argument values.
func codexOptionTokens(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "-") {
			token := arg
			// clap accepts attached short option values (-cfoo, -pname).
			if len(arg) > 2 && (strings.HasPrefix(arg, "-c") || strings.HasPrefix(arg, "-p")) {
				token = arg[:2] + "=" + arg[2:]
			}
			out = append(out, token)
		}
		if codexValueOptions[arg] {
			i++
		}
	}
	return out
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
