package client

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// fallbackOnboardingVersion is the lastOnboardingVersion written to
// ~/.claude.json when the locally installed Claude Code version can't be
// detected. Claude Code re-runs onboarding (and "What's New" prompts) when
// this value is OLDER than the running binary, so it must track a recent
// release. Detection via detectClaudeVersion() supersedes it whenever the
// `claude` binary is on PATH — keeping the agent's config in lockstep with
// whatever Claude Code is actually installed.
const fallbackOnboardingVersion = "2.1.165"

// detectClaudeVersion runs `claude --version` and returns the bare semver
// (e.g. "2.1.165" from "2.1.165 (Claude Code)"). Returns "" if the binary is
// not on PATH or the output doesn't start with a version-looking token.
func detectClaudeVersion() string {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return ""
	}
	tok := strings.TrimSpace(string(out))
	if i := strings.IndexByte(tok, ' '); i >= 0 {
		tok = tok[:i]
	}
	// Sanity check: must look like a dotted numeric version (e.g. 2.1.165).
	if tok == "" || strings.TrimLeft(tok, "0123456789.") != "" || !strings.Contains(tok, ".") {
		return ""
	}
	return tok
}

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

	// Sync admin-configured Claude Code statusline script.
	if err := SyncStatusline(configDir, cfg); err != nil {
		log.Printf("Warning: statusline sync failed: %v", err)
	}

	// Codex supports two distinct credential modes:
	//   - Codex OAuth: write ~/.codex/auth.json and let Codex use native auth.
	//   - OpenAI Platform key: route Codex through Duckway's local OpenAI proxy.
	if synced := SyncCodexOAuthCredentials(configDir, cfg); synced {
		if err := DisableCodexDuckwayProvider(); err != nil {
			log.Printf("Warning: codex provider cleanup failed: %v", err)
		}
	} else {
		ClearCodexOAuthMode(configDir)
		if err := SyncCodexConfig(cfg.ProxyPort); err != nil {
			log.Printf("Warning: codex config sync failed: %v", err)
		}
	}

	return len(keys), nil
}

// SyncStatusline pulls the admin-configured statusline script body from
// the server, writes it to ~/.duckway/statusline.sh (executable), and
// wires ~/.claude/settings.json's statusLine.command to point at it.
//
// When ~/.claude/settings.json already exists with non-statusLine
// content, it's backed up to settings.json.duckway-backup before we
// merge in the statusLine entry — we never blow away user-edited
// settings, only the statusLine key.
//
// Empty script body from the server means "no statusline configured"
// — we leave the local script alone (so agents who had a previously
// installed statusline keep it) and don't touch settings.json.
func SyncStatusline(configDir string, cfg *Config) error {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	script, err := api.FetchStatusline()
	if err != nil {
		return err
	}
	if strings.TrimSpace(script) == "" {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dwDir := filepath.Join(home, ".duckway")
	if err := os.MkdirAll(dwDir, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dwDir, err)
	}
	scriptPath := filepath.Join(dwDir, "statusline.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0700); err != nil {
		return fmt.Errorf("write %s: %w", scriptPath, err)
	}
	log.Printf("Statusline script written to %s", scriptPath)

	if err := installStatuslineIntoClaudeSettings(home, scriptPath); err != nil {
		return fmt.Errorf("install statusline into ~/.claude/settings.json: %w", err)
	}
	return nil
}

// installStatuslineIntoClaudeSettings sets the statusLine entry in
// ~/.claude/settings.json so Claude Code runs scriptPath for its
// status bar.
//
//   - If ~/.claude/settings.json does NOT exist: create it with just
//     the statusLine entry.
//   - If it exists: copy the current bytes to settings.json.duckway-backup,
//     then merge our statusLine into the parsed JSON (preserving every
//     other key — theme, env, hooks, anything the user has set).
//
// Merging keeps other settings intact across syncs; the per-sync backup
// is a one-step undo in case the merge ever does something surprising.
func installStatuslineIntoClaudeSettings(homeDir, scriptPath string) error {
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
		return err
	}

	entry := map[string]interface{}{
		"type":    "command",
		"command": scriptPath,
	}

	existing, err := os.ReadFile(settingsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// Fresh file — no backup needed, write just our entry.
		body, _ := json.MarshalIndent(map[string]interface{}{"statusLine": entry}, "", "  ")
		return os.WriteFile(settingsPath, body, 0644)
	}

	backupPath := settingsPath + ".duckway-backup"
	if werr := os.WriteFile(backupPath, existing, 0644); werr != nil {
		return fmt.Errorf("backup %s: %w", settingsPath, werr)
	}
	log.Printf("Existing %s backed up to %s", settingsPath, backupPath)

	root := map[string]interface{}{}
	if jerr := json.Unmarshal(existing, &root); jerr != nil {
		// Existing file isn't valid JSON. The backup preserves it
		// verbatim; replace the file with a fresh one carrying just
		// the statusLine entry.
		log.Printf("Existing %s was not valid JSON (backed up); writing fresh statusLine-only file", settingsPath)
		root = map[string]interface{}{}
	}
	root["statusLine"] = entry
	body, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath, body, 0644)
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
	// Pin lastOnboardingVersion to whatever Claude Code is actually installed
	// so the agent's config follows the latest binary. A stale value (older
	// than the running version) makes Claude Code re-trigger onboarding /
	// "What's New" flows, which intermittently breaks headless agents.
	onboardingVersion := detectClaudeVersion()
	if onboardingVersion == "" {
		onboardingVersion = fallbackOnboardingVersion
	}

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
		// The locally installed version is authoritative over the server's
		// (potentially stale) lastOnboardingVersion — follow the real binary.
		root["lastOnboardingVersion"] = onboardingVersion
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
		fallback := []byte(fmt.Sprintf("{\n  \"hasCompletedOnboarding\": true,\n  \"lastOnboardingVersion\": %q\n}\n", onboardingVersion))
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

// PrintEnv outputs keys in shell-eval format, followed by the local proxy
// exports (HTTP_PROXY/HTTPS_PROXY/NO_PROXY). Emitting both means a single
// `eval "$(duckway env)"` configures the agent's keys AND routes its traffic
// through the local duckway proxy — without a separate `duckway env --proxy`
// step. `duckway env --proxy` remains available to print the proxy exports
// alone.
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

	// Append the proxy exports. Fall back to the default port if the config
	// can't be read — the proxy exports are still useful and the default
	// matches LoadConfig's own ProxyPort default.
	port := 18080
	if cfg, err := LoadConfig(configDir); err == nil {
		port = cfg.ProxyPort
	}
	PrintProxyEnv(port)
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

// SyncCodexConfig writes a Duckway OpenAI provider into ~/.codex/config.toml
// so Codex CLI routes through the local duckway proxy and reads the phantom
// OPENAI_API_KEY from the environment. All other settings already in the file
// are preserved; Duckway only owns the model_provider top-level key and the
// [model_providers.duckway-openai] section.
//
// The resulting provider base_url format is:
//
//	http://localhost:{port}/proxy/openai/v1
//
// The OpenAI SDK (used by Codex CLI) appends its own method paths
// (e.g. /chat/completions) to this base, so the final request looks like:
//
//	http://localhost:{port}/proxy/openai/v1/chat/completions
//
// which the duckway local proxy forwards to the server as-is.
func SyncCodexConfig(proxyPort int) error {
	configPath, err := codexConfigTOMLPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(configPath), err)
	}

	existing, _ := os.ReadFile(configPath)
	next := replaceTopLevelTOMLString(string(existing), "model_provider", "duckway-openai")
	next = replaceTOMLSection(next, "model_providers.duckway-openai", codexDuckwayProviderSection(proxyPort))
	if err := os.WriteFile(configPath, []byte(next), 0600); err != nil {
		return err
	}
	log.Printf("Codex config synced to %s (provider=duckway-openai)", configPath)
	return nil
}

func SyncCodexOAuthCredentials(configDir string, cfg *Config) bool {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	creds, err := api.FetchCodexCredentials()
	if err != nil || creds == nil {
		return false
	}
	if !codexCredentialsHaveIDToken(creds) {
		log.Printf("Warning: Codex OAuth credentials from server are missing tokens.id_token; re-upload the full ~/.codex/auth.json in Refreshable Tokens")
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		log.Printf("Warning: cannot create ~/.codex: %v", err)
		return false
	}
	authPath := filepath.Join(codexDir, "auth.json")
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return false
	}
	if err := os.WriteFile(authPath, data, 0600); err != nil {
		log.Printf("Warning: cannot write Codex OAuth credentials: %v", err)
		return false
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		log.Printf("Warning: cannot create Duckway config dir for Codex OAuth mode marker: %v", err)
	} else if err := os.WriteFile(CodexOAuthModePath(configDir), []byte("oauth\n"), 0600); err != nil {
		log.Printf("Warning: cannot write Codex OAuth mode marker: %v", err)
	}
	log.Printf("Codex OAuth credentials synced to %s", authPath)
	return true
}

func codexCredentialsHaveIDToken(creds map[string]interface{}) bool {
	tokens, ok := creds["tokens"].(map[string]interface{})
	if !ok {
		return false
	}
	idToken, _ := tokens["id_token"].(string)
	return strings.TrimSpace(idToken) != ""
}

func CodexOAuthModePath(configDir string) string {
	return filepath.Join(configDir, "codex-auth-mode")
}

func ClearCodexOAuthMode(configDir string) {
	_ = os.Remove(CodexOAuthModePath(configDir))
}

func CodexOAuthModeActive(configDir string) bool {
	data, err := os.ReadFile(CodexOAuthModePath(configDir))
	return err == nil && strings.TrimSpace(string(data)) == "oauth"
}

func DisableCodexDuckwayProvider() error {
	configPath, err := codexConfigTOMLPath()
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	next := removeTopLevelTOMLStringValue(string(existing), "model_provider", "duckway-openai")
	next = removeTOMLSection(next, "model_providers.duckway-openai")
	if next == string(existing) {
		return nil
	}
	if err := os.WriteFile(configPath, []byte(next), 0600); err != nil {
		return err
	}
	log.Printf("Codex Duckway OpenAI provider disabled in %s (using native Codex OAuth)", configPath)
	return nil
}

func codexDuckwayProviderSection(proxyPort int) string {
	return "[model_providers.duckway-openai]\n" +
		"name = \"Duckway OpenAI\"\n" +
		"base_url = " + tomlQuote(fmt.Sprintf("http://localhost:%d/proxy/openai/v1", proxyPort)) + "\n" +
		"env_key = \"OPENAI_API_KEY\"\n" +
		"wire_api = \"responses\"\n"
}

func removeTopLevelTOMLStringValue(input, key, value string) string {
	lines := strings.Split(input, "\n")
	prefix := key + " = "
	target := prefix + tomlQuote(value)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == target {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func removeTOMLSection(input, section string) string {
	lines := strings.Split(input, "\n")
	header := "[" + section + "]"
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if trimmed == header {
				skipping = true
				continue
			}
			skipping = false
		}
		if !skipping {
			out = append(out, line)
		}
	}
	result := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if result == "" {
		return ""
	}
	return result + "\n"
}

func replaceTopLevelTOMLString(input, key, value string) string {
	lines := strings.Split(input, "\n")
	replacement := key + " = " + tomlQuote(value)
	out := make([]string, 0, len(lines)+1)
	replaced := false
	inserted := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inserted && !replaced && strings.HasPrefix(trimmed, "[") {
			out = append(out, replacement)
			inserted = true
		}
		if !inserted && !replaced && isTOMLKeyLine(trimmed, key) {
			out = append(out, replacement)
			replaced = true
			continue
		}
		out = append(out, line)
	}
	if !inserted && !replaced {
		out = append(out, replacement)
	}
	return strings.Join(out, "\n")
}

func isTOMLKeyLine(line, key string) bool {
	if strings.HasPrefix(line, "#") {
		return false
	}
	if !strings.HasPrefix(line, key) {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, key))
	return strings.HasPrefix(rest, "=")
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
