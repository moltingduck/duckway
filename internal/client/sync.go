package client

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// SyncKeys fetches placeholder keys from the server and writes them to keys.env.
func SyncKeys(configDir string, cfg *Config) (int, error) {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	keys, err := api.FetchKeys()
	if err != nil {
		return 0, err
	}

	var lines []string
	lines = append(lines, "# Duckway placeholder keys — auto-generated, do not edit")
	lines = append(lines, fmt.Sprintf("# Server: %s | Client: %s", cfg.ServerURL, cfg.ClientName))
	lines = append(lines, "")

	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("# Service: %s", k.ServiceName))
		lines = append(lines, fmt.Sprintf("%s=%s", k.EnvName, k.Placeholder))
		lines = append(lines, "")
	}

	envPath := KeysEnvPath(configDir)
	if err := os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		return 0, fmt.Errorf("write keys.env: %w", err)
	}

	// Deploy keys to their configured paths (e.g., ~/.config/openai/credentials)
	home, _ := os.UserHomeDir()
	for _, k := range keys {
		if k.KeyPath == "" {
			continue
		}
		keyFilePath := filepath.Join(home, k.KeyPath)
		if err := os.MkdirAll(filepath.Dir(keyFilePath), 0700); err != nil {
			log.Printf("Warning: cannot create dir for %s: %v", k.KeyPath, err)
			continue
		}
		content := fmt.Sprintf("%s=%s\n", k.EnvName, k.Placeholder)
		if err := os.WriteFile(keyFilePath, []byte(content), 0600); err != nil {
			log.Printf("Warning: cannot write key to %s: %v", k.KeyPath, err)
		}
	}

	// Sync canary tokens
	canaryCount, err := SyncCanaries(configDir, cfg)
	if err != nil {
		log.Printf("Warning: canary sync failed: %v", err)
	} else if canaryCount > 0 {
		log.Printf("Deployed %d canary tokens", canaryCount)
	}

	// Sync Claude OAuth credentials (phantom tokens → ~/.claude/.credentials.json)
	SyncClaudeCredentials(cfg)

	// Sync Control Channel assignments → state file + per-agent MCP config.
	if ccCount, err := SyncCC(configDir, cfg); err != nil {
		log.Printf("Warning: cc sync failed: %v", err)
	} else if ccCount > 0 {
		log.Printf("Synced %d Control Channel assignment(s)", ccCount)
		printCCDaemonHint()
	}

	return len(keys), nil
}

// printCCDaemonHint nudges the user toward starting the watcher daemon
// when they've just synced a CC for the first time. Idempotent on
// repeat syncs — the message is short enough to not be noisy.
func printCCDaemonHint() {
	log.Printf("To receive Discord messages in this client, start the watcher:")
	log.Printf("  duckway cc watch -d                   # background daemon (recommended)")
	log.Printf("  duckway cc watch status / stop        # check / stop")
	log.Printf("Or for boot-on-login (Linux): examples/duckway-cc-watch.service")
}

// SyncClaudeCredentials fetches phantom Claude OAuth credentials and writes
// the three files Claude needs to skip onboarding and use the subscription:
//   - ~/.claude/.credentials.json  (phantom OAuth tokens — always overwritten)
//   - ~/.claude/settings.json      (default settings — only if not exists)
//   - ~/.claude.json               (onboarding flag — only if not exists)
//
// If no OAuth credentials are configured on the server, this is a no-op.
func SyncClaudeCredentials(cfg *Config) {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	creds, err := api.FetchClaudeCredentials()
	if err != nil || creds == nil {
		return // no OAuth credentials configured
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		log.Printf("Warning: cannot create ~/.claude: %v", err)
		return
	}

	// Extract claudeAiOauth for .credentials.json
	oauthData := map[string]interface{}{}
	if v, ok := creds["claudeAiOauth"]; ok {
		oauthData["claudeAiOauth"] = v
	}

	// 1. Always write .credentials.json (phantom tokens from server)
	credPath := filepath.Join(claudeDir, ".credentials.json")
	data, err := json.MarshalIndent(oauthData, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(credPath, data, 0600); err != nil {
		log.Printf("Warning: cannot write Claude credentials: %v", err)
		return
	}
	log.Printf("Claude credentials synced to %s", credPath)

	// 2. Write settings.json — preserve any existing user prefs but always
	//    refresh the proxy env vars so Claude Code routes through the local
	//    duckway proxy without the user having to export HTTPS_PROXY manually.
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := mergeProxySettings(settingsPath, cfg.ProxyPort); err != nil {
		log.Printf("Warning: cannot update Claude settings: %v", err)
	} else {
		log.Printf("Claude settings synced to %s (proxy env)", settingsPath)
	}

	// 3. Merge server's claudeConfig into ~/.claude.json. The server owns
	// the oauthAccount/onboarding fields; everything else (mcpServers,
	// projects, user prefs added by Claude itself, etc.) MUST be preserved.
	// Earlier versions blindly overwrote the whole file, which wiped the
	// duckway-cc MCP entry on every sync.
	onboardingPath := filepath.Join(home, ".claude.json")
	if claudeConfig, ok := creds["claudeConfig"].(map[string]interface{}); ok {
		root := map[string]interface{}{}
		if existing, err := os.ReadFile(onboardingPath); err == nil && len(existing) > 0 {
			if err := json.Unmarshal(existing, &root); err != nil {
				backup := onboardingPath + ".duckway-backup"
				_ = os.WriteFile(backup, existing, 0600)
				log.Printf("Existing %s was not valid JSON — backed up to %s", onboardingPath, backup)
				root = map[string]interface{}{}
			}
		}
		// Server-controlled fields overwrite. Non-server keys stay.
		for k, v := range claudeConfig {
			root[k] = v
		}
		configData, err := json.MarshalIndent(root, "", "  ")
		if err == nil {
			if werr := os.WriteFile(onboardingPath, configData, 0600); werr != nil {
				log.Printf("Warning: cannot write Claude onboarding config: %v", werr)
			} else {
				log.Printf("Claude config synced to %s", onboardingPath)
			}
		}
	} else if _, err := os.Stat(onboardingPath); os.IsNotExist(err) {
		// Fallback if server doesn't send claudeConfig
		fallback := []byte("{\n  \"hasCompletedOnboarding\": true,\n  \"lastOnboardingVersion\": \"2.1.119\"\n}\n")
		if werr := os.WriteFile(onboardingPath, fallback, 0600); werr != nil {
			log.Printf("Warning: cannot write Claude onboarding config: %v", werr)
		} else {
			log.Printf("Claude config written to %s (defaults)", onboardingPath)
		}
	}
}

// SyncCanaries fetches canary tokens and deploys them as decoy files.
// Canary tokens are placed in realistic paths under $HOME to look like
// real credentials, but NOT in keys.env so agents use the real placeholders.
func SyncCanaries(configDir string, cfg *Config) (int, error) {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	canaries, err := api.FetchCanaries()
	if err != nil {
		return 0, err
	}
	if len(canaries) == 0 {
		return 0, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return 0, fmt.Errorf("get home dir: %w", err)
	}

	deployed := 0
	for _, c := range canaries {
		deployPath := filepath.Join(home, c.DeployPath)

		if err := os.MkdirAll(filepath.Dir(deployPath), 0700); err != nil {
			log.Printf("Warning: cannot create dir for canary %s: %v", c.DeployPath, err)
			continue
		}

		if c.DeployMode == "append" {
			// Append to existing file (e.g., .bash_history, .bashrc)
			// Check if already injected by looking for a snippet of the content
			existing, _ := os.ReadFile(deployPath)
			snippet := c.DeployContent
			if len(snippet) > 60 {
				// Use chars 10-60 as the dedup check (avoids matching on common prefixes)
				snippet = snippet[10:60]
			}
			if len(existing) > 0 && strings.Contains(string(existing), snippet) {
				continue // Already deployed
			}
			f, err := os.OpenFile(deployPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				log.Printf("Warning: cannot append canary to %s: %v", c.DeployPath, err)
				continue
			}
			f.WriteString(c.DeployContent)
			f.Close()
		} else {
			// Create mode — don't overwrite existing real files
			if _, err := os.Stat(deployPath); err == nil {
				continue
			}
			if err := os.WriteFile(deployPath, []byte(c.DeployContent), 0600); err != nil {
				log.Printf("Warning: cannot deploy canary %s: %v", c.DeployPath, err)
				continue
			}
		}

		deployed++
	}

	// Write a manifest so we know what we deployed (for cleanup)
	var manifest []string
	for _, c := range canaries {
		manifest = append(manifest, c.DeployPath)
	}
	manifestPath := filepath.Join(configDir, "canaries.manifest")
	os.WriteFile(manifestPath, []byte(strings.Join(manifest, "\n")), 0600)

	return deployed, nil
}

// PrintEnv outputs keys in shell-eval format.
func PrintEnv(configDir string) error {
	data, err := os.ReadFile(KeysEnvPath(configDir))
	if err != nil {
		return fmt.Errorf("no keys found — run 'duckway sync' first: %w", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fmt.Printf("export %s\n", line)
	}
	return nil
}

// mergeProxySettings reads ~/.claude/settings.json (if any), refreshes the
// HTTPS_PROXY / HTTP_PROXY / NO_PROXY entries under settings.env so Claude
// Code's process picks up the local duckway proxy automatically, then writes
// the file back. All other user-defined settings keys are preserved.
//
// Idempotent: re-running just refreshes the proxy values to match the current
// proxy_port (useful if the admin changes the port via Settings).
func mergeProxySettings(path string, proxyPort int) error {
	settings := map[string]interface{}{}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		// Best-effort parse — if the existing file is malformed JSON we
		// start from a blank slate rather than crash.
		_ = json.Unmarshal(data, &settings)
	}

	// Default visible theme on first write — won't clobber if user customised.
	if _, ok := settings["theme"]; !ok {
		settings["theme"] = "dark"
	}

	env, _ := settings["env"].(map[string]interface{})
	if env == nil {
		env = map[string]interface{}{}
	}
	proxyURL := fmt.Sprintf("http://localhost:%d", proxyPort)
	env["HTTPS_PROXY"] = proxyURL
	env["HTTP_PROXY"] = proxyURL
	// Always include localhost loopback. Preserve any user-added NO_PROXY
	// entries, just guarantee the loopback set is present.
	if existing, ok := env["NO_PROXY"].(string); ok && existing != "" {
		env["NO_PROXY"] = ensureLoopbackInNoProxy(existing)
	} else {
		env["NO_PROXY"] = "localhost,127.0.0.1"
	}
	settings["env"] = env

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0600)
}

// ensureLoopbackInNoProxy returns the NO_PROXY string with localhost and
// 127.0.0.1 guaranteed present (added if missing, comma-separated).
func ensureLoopbackInNoProxy(existing string) string {
	hosts := strings.Split(existing, ",")
	have := map[string]bool{}
	for _, h := range hosts {
		have[strings.TrimSpace(h)] = true
	}
	for _, must := range []string{"localhost", "127.0.0.1"} {
		if !have[must] {
			hosts = append(hosts, must)
		}
	}
	// Trim spaces on each entry for tidy output
	for i := range hosts {
		hosts[i] = strings.TrimSpace(hosts[i])
	}
	return strings.Join(hosts, ",")
}
