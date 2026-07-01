package client

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CCStateFile is the on-disk shape `duckway sync` writes for the MCP server
// to read. It's the authoritative source for what CCs this client is
// assigned to and how the agent should reach them.
type CCStateFile struct {
	ServerURL string              `json:"server_url"`
	Token     string              `json:"-"` // never persisted to disk
	Generated string              `json:"generated_at"`
	CCs       []CCStateAssignment `json:"ccs"`
}

// CCStateAssignment is the (single) CC the client is bound to.
type CCStateAssignment struct {
	CCID             string `json:"cc_id"`
	CCName           string `json:"cc_name"`
	AgentType        string `json:"agent_type"`
	ManagementHandle string `json:"management_handle"`
}

func ccStateFilePath(configDir string) string {
	return filepath.Join(configDir, "cc.json")
}

// LoadCCState returns the CCs this client has been assigned (as last written
// by `duckway sync`). Returns an empty struct (not an error) when cc.json is
// missing — same shape the MCP server uses, so callers like `duckway status`
// see exactly what the agent sees.
func LoadCCState(configDir string) (*CCStateFile, error) {
	return readCCState(configDir)
}

// SyncCC fetches the assigned CCs from the server, writes the state file
// (~/.duckway/cc.json), then drops per-agent config files for each
// agent_type seen. Returns (count_assigned, error).
//
// Per-agent writers:
//   - claude_code → ~/.claude/mcp.json entry pointing at `duckway mcp serve`
//   - codex       → ~/.codex/config.toml mcp_servers.duckway-cc entry
//   - openclaw / harmes / cursor / copilot_cli — TODO stubs (logged, no file)
//
// Idempotent: re-running overwrites the state file and merges the MCP
// entry without disturbing any user-added entries.
func SyncCC(configDir string, cfg *Config) (int, error) {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	assignments, err := api.FetchCC()
	if err != nil {
		return 0, fmt.Errorf("fetch cc: %w", err)
	}

	// Always (re)write the state file — even when empty, so a previously
	// assigned-then-unassigned client clears its old state.
	state := CCStateFile{
		ServerURL: cfg.ServerURL,
		Generated: nowISO(),
		CCs:       []CCStateAssignment{},
	}
	for _, a := range assignments {
		state.CCs = append(state.CCs, CCStateAssignment{
			CCID:             a.CCID,
			CCName:           a.CCName,
			AgentType:        a.AgentType,
			ManagementHandle: a.ManagementHandle,
		})
	}

	if err := os.MkdirAll(configDir, 0700); err != nil {
		return 0, fmt.Errorf("mkdir config: %w", err)
	}
	stateBytes, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(ccStateFilePath(configDir), stateBytes, 0600); err != nil {
		return 0, fmt.Errorf("write state file: %w", err)
	}

	if len(assignments) == 0 {
		// Nothing to wire into agent configs. Still report success so the
		// caller can show "0 CCs synced".
		return 0, nil
	}

	// Group assignments by agent_type and let each writer decide what to do.
	byType := map[string][]CCStateAssignment{}
	for _, a := range state.CCs {
		byType[a.AgentType] = append(byType[a.AgentType], a)
	}
	for agent, list := range byType {
		switch agent {
		case "claude_code":
			if err := writeClaudeCodeMCP(configDir, list); err != nil {
				log.Printf("Warning: claude_code mcp write failed: %v", err)
			}
		case "codex":
			if err := writeCodexMCP(configDir, list); err != nil {
				log.Printf("Warning: codex mcp write failed: %v", err)
			}
		case "openclaw", "harmes", "cursor", "copilot_cli":
			log.Printf("agent_type %q: writer not implemented yet (%d CCs skipped)", agent, len(list))
		default:
			log.Printf("agent_type %q: unknown — skipped", agent)
		}
	}

	return len(assignments), nil
}

// claudeCodeMCPPath returns the path to Claude Code's main config file —
// `~/.claude.json`. The user-scope MCP servers live under the top-level
// `mcpServers` key in this file. Override with $CLAUDE_CONFIG_DIR (where
// the file becomes `<dir>/.claude.json`) for tests or non-default
// installs.
func claudeCodeMCPPath() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// writeClaudeCodeMCP merges a `duckway-cc` entry into the user's MCP
// server config (~/.claude.json, top-level `mcpServers` key — the
// "user scope" per Claude Code's docs).
//
// Existing entries (mcpServers + every other top-level key like
// `oauthAccount`, `projects`, `hasCompletedOnboarding` …) are preserved
// verbatim — we only own the "duckway-cc" name.
//
// Shape we write:
//
//	{
//	  "mcpServers": {
//	    "duckway-cc": {
//	      "type":    "stdio",
//	      "command": "duckway",
//	      "args":    ["mcp", "serve"]
//	    }
//	  }
//	}
//
// The `duckway mcp serve` subcommand reads cc.json from the same
// configDir, so we don't need to pass IDs on the command line — the
// agent sees all CCs the user is assigned to as MCP tools.
func writeClaudeCodeMCP(configDir string, ccs []CCStateAssignment) error {
	mcpPath, err := claudeCodeMCPPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(mcpPath), 0700); err != nil {
		return err
	}

	// Read existing file (may not exist) and preserve user keys.
	root := map[string]interface{}{}
	if existing, err := os.ReadFile(mcpPath); err == nil && len(existing) > 0 {
		if err := json.Unmarshal(existing, &root); err != nil {
			// Corrupt or non-JSON — back it up rather than crash.
			backup := mcpPath + ".duckway-backup"
			_ = os.WriteFile(backup, existing, 0600)
			log.Printf("Existing %s was not valid JSON — backed up to %s and replaced", mcpPath, backup)
			root = map[string]interface{}{}
		}
	}

	servers, _ := root["mcpServers"].(map[string]interface{})
	if servers == nil {
		servers = map[string]interface{}{}
	}

	args := []interface{}{"mcp", "serve"}
	if configDir != DefaultConfigDir() {
		args = append(args, "--config-dir", configDir)
	}
	servers["duckway-cc"] = map[string]interface{}{
		"type":    "stdio",
		"command": "duckway",
		"args":    args,
	}
	root["mcpServers"] = servers

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(mcpPath, out, 0600); err != nil {
		return err
	}

	// Best-effort cleanup of the legacy ~/.claude/mcp.json that earlier
	// duckway versions wrote — Claude Code never read it but it's
	// confusing to leave behind. Silent if missing.
	if home, hErr := os.UserHomeDir(); hErr == nil {
		legacy := filepath.Join(home, ".claude", "mcp.json")
		if data, rErr := os.ReadFile(legacy); rErr == nil {
			var legacyRoot map[string]interface{}
			if json.Unmarshal(data, &legacyRoot) == nil {
				if servers, ok := legacyRoot["mcpServers"].(map[string]interface{}); ok {
					// Only delete if the legacy file's only mcpServers entry
					// was ours (or empty) — don't touch user data.
					onlyOurs := true
					for name := range servers {
						if name != "duckway-cc" {
							onlyOurs = false
							break
						}
					}
					if onlyOurs {
						_ = os.Remove(legacy)
					}
				}
			}
		}
	}

	names := make([]string, 0, len(ccs))
	for _, c := range ccs {
		names = append(names, c.CCName)
	}
	log.Printf("Claude Code MCP entry written to %s (CCs: %s)", mcpPath, strings.Join(names, ", "))
	return nil
}

func codexConfigTOMLPath() (string, error) {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

func codexAuthJSONPath() (string, error) {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "auth.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

func writeCodexMCP(configDir string, ccs []CCStateAssignment) error {
	configPath, err := codexConfigTOMLPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return err
	}

	existing, _ := os.ReadFile(configPath)
	next := replaceTOMLSection(string(existing), "mcp_servers.duckway-cc", codexMCPSection(configDir))
	if err := os.WriteFile(configPath, []byte(next), 0600); err != nil {
		return err
	}

	names := make([]string, 0, len(ccs))
	for _, c := range ccs {
		names = append(names, c.CCName)
	}
	log.Printf("Codex MCP entry written to %s (CCs: %s)", configPath, strings.Join(names, ", "))
	return nil
}

func codexMCPSection(configDir string) string {
	args := []string{"mcp", "serve"}
	if configDir != DefaultConfigDir() {
		args = append(args, "--config-dir", configDir)
	}
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, tomlQuote(a))
	}
	return "[mcp_servers.duckway-cc]\n" +
		"command = \"duckway\"\n" +
		"args = [" + strings.Join(quoted, ", ") + "]\n"
}

func replaceTOMLSection(input, section, replacement string) string {
	lines := strings.Split(input, "\n")
	header := "[" + section + "]"
	out := make([]string, 0, len(lines)+4)
	replaced := false
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if trimmed == header {
				if !replaced {
					if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
						out = append(out, "")
					}
					out = append(out, strings.TrimRight(replacement, "\n"))
					replaced = true
				}
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
	if !replaced {
		if result != "" {
			result += "\n\n"
		}
		result += strings.TrimRight(replacement, "\n")
	}
	return result + "\n"
}

func tomlQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// nowISO returns the current UTC timestamp in RFC3339.
func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
