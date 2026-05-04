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
	ServerURL string                  `json:"server_url"`
	Token     string                  `json:"-"` // never persisted to disk
	Generated string                  `json:"generated_at"`
	CCs       []CCStateAssignment     `json:"ccs"`
}

// CCStateAssignment is one CC the client is assigned to.
type CCStateAssignment struct {
	CCID            string `json:"cc_id"`
	CCName          string `json:"cc_name"`
	AgentType       string `json:"agent_type"`
	HomeHandle      string `json:"home_handle"`
	HomeChannelName string `json:"home_channel_name"`
}

func ccStateFilePath(configDir string) string {
	return filepath.Join(configDir, "cc.json")
}

// SyncCC fetches the assigned CCs from the server, writes the state file
// (~/.duckway/cc.json), then drops per-agent config files for each
// agent_type seen. Returns (count_assigned, error).
//
// Per-agent writers:
//   * claude_code → ~/.claude/mcp.json entry pointing at `duckway mcp serve`
//   * openclaw / harmes / cursor / copilot_cli — TODO stubs (logged, no file)
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
			CCID:            a.CCID,
			CCName:          a.CCName,
			AgentType:       a.AgentType,
			HomeHandle:      a.HomeHandle,
			HomeChannelName: a.HomeChannelName,
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
		case "openclaw", "harmes", "cursor", "copilot_cli":
			log.Printf("agent_type %q: writer not implemented yet (%d CCs skipped)", agent, len(list))
		default:
			log.Printf("agent_type %q: unknown — skipped", agent)
		}
	}

	return len(assignments), nil
}

// claudeCodeMCPPath returns the location to write Claude Code's MCP server
// config to. Honours $CLAUDE_CONFIG_DIR if set; otherwise defaults to
// ~/.claude/mcp.json (which Claude Code reads on startup).
func claudeCodeMCPPath() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "mcp.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "mcp.json"), nil
}

// writeClaudeCodeMCP merges a `duckway-cc` entry into the user's MCP server
// config. Existing entries are preserved verbatim — we only own the
// "duckway-cc" key.
//
// Shape we write (Claude Code's "Stdio MCP server" form):
//
//	{
//	  "mcpServers": {
//	    "duckway-cc": {
//	      "command": "duckway",
//	      "args": ["mcp", "serve"]
//	    }
//	  }
//	}
//
// The `duckway mcp serve` subcommand (Phase E) reads cc.json from the same
// configDir, so we don't need to pass IDs on the command line — the agent
// will see all CCs the user is assigned to as MCP tools.
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
	names := make([]string, 0, len(ccs))
	for _, c := range ccs {
		names = append(names, c.CCName)
	}
	log.Printf("Claude Code MCP entry written to %s (CCs: %s)", mcpPath, strings.Join(names, ", "))
	return nil
}

// nowISO returns the current UTC timestamp in RFC3339.
func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
