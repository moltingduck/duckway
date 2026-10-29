package client

import (
	"path/filepath"
	"strings"
)

func isCodexCommand(command string) bool {
	name := filepath.Base(command)
	return name == "codex" || name == "codex.exe"
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
