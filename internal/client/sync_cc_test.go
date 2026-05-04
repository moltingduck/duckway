package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// mockServer returns canned /client/cc responses so tests don't need a real
// duckway server.
func mockServer(t *testing.T, payload []CCAssignment, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/cc" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("X-Duckway-Token") != "tok" {
			t.Errorf("missing/wrong token: %q", r.Header.Get("X-Duckway-Token"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
}

func TestSyncCC_WritesStateFile(t *testing.T) {
	srv := mockServer(t, []CCAssignment{
		{CCID: "cc1", CCName: "alpha", AgentType: "claude_code", HomeHandle: "dwch_a", HomeChannelName: "client-a"},
		{CCID: "cc2", CCName: "beta", AgentType: "cursor", HomeHandle: "dwch_b", HomeChannelName: "client-b"},
	}, 200)
	defer srv.Close()

	configDir := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // isolate ~/.claude writes

	cfg := &Config{ServerURL: srv.URL, Token: "tok", ClientName: "test"}
	n, err := SyncCC(configDir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2 assignments, got %d", n)
	}

	raw, err := os.ReadFile(filepath.Join(configDir, "cc.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state CCStateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.ServerURL != srv.URL {
		t.Errorf("server_url = %q", state.ServerURL)
	}
	if len(state.CCs) != 2 {
		t.Fatalf("ccs = %d", len(state.CCs))
	}
	if state.CCs[0].CCID != "cc1" || state.CCs[1].AgentType != "cursor" {
		t.Errorf("contents wrong: %+v", state.CCs)
	}
	// Token must NEVER be persisted.
	if state.Token != "" {
		t.Error("token leaked into state file")
	}
}

func TestSyncCC_EmptyAssignmentsClearsState(t *testing.T) {
	srv := mockServer(t, []CCAssignment{}, 200)
	defer srv.Close()
	configDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	// Pre-write a stale state file with old assignments.
	stale := CCStateFile{ServerURL: "http://old", CCs: []CCStateAssignment{{CCID: "stale"}}}
	b, _ := json.Marshal(stale)
	_ = os.WriteFile(filepath.Join(configDir, "cc.json"), b, 0600)

	n, err := SyncCC(configDir, &Config{ServerURL: srv.URL, Token: "tok"})
	if err != nil || n != 0 {
		t.Fatalf("expected 0 assignments cleanly, got n=%d err=%v", n, err)
	}
	raw, _ := os.ReadFile(filepath.Join(configDir, "cc.json"))
	var state CCStateFile
	_ = json.Unmarshal(raw, &state)
	if len(state.CCs) != 0 {
		t.Errorf("expected stale state cleared, still has %+v", state.CCs)
	}
}

func TestWriteClaudeCodeMCP_PreservesUserKeys(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", tmp)

	// User already has an MCP entry — we must keep it.
	user := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"my-other-mcp": map[string]interface{}{
				"command": "node",
				"args":    []interface{}{"server.js"},
			},
		},
		"someOtherKey": "preserved",
	}
	ub, _ := json.MarshalIndent(user, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "mcp.json"), ub, 0600); err != nil {
		t.Fatal(err)
	}

	if err := writeClaudeCodeMCP(t.TempDir(), []CCStateAssignment{
		{CCID: "cc1", CCName: "alpha", AgentType: "claude_code", HomeHandle: "dwch_a"},
	}); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(filepath.Join(tmp, "mcp.json"))
	var got map[string]interface{}
	_ = json.Unmarshal(raw, &got)

	if got["someOtherKey"] != "preserved" {
		t.Error("non-mcp key wiped")
	}
	servers := got["mcpServers"].(map[string]interface{})
	if _, ok := servers["my-other-mcp"]; !ok {
		t.Error("user mcp entry wiped")
	}
	dw, ok := servers["duckway-cc"].(map[string]interface{})
	if !ok {
		t.Fatal("duckway-cc entry missing")
	}
	if dw["command"] != "duckway" {
		t.Errorf("command = %v", dw["command"])
	}
}

func TestWriteClaudeCodeMCP_NewFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", tmp)

	// No existing file — writer should create one.
	if err := writeClaudeCodeMCP(t.TempDir(), []CCStateAssignment{
		{CCID: "cc1", CCName: "x", AgentType: "claude_code"},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	_ = json.Unmarshal(raw, &got)
	servers := got["mcpServers"].(map[string]interface{})
	if _, ok := servers["duckway-cc"]; !ok {
		t.Error("duckway-cc entry not written")
	}
}

func TestWriteClaudeCodeMCP_BackupOnInvalidJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", tmp)

	// Pre-write garbage.
	garbage := []byte("not json {{{")
	_ = os.WriteFile(filepath.Join(tmp, "mcp.json"), garbage, 0600)

	if err := writeClaudeCodeMCP(t.TempDir(), []CCStateAssignment{
		{CCID: "cc1", CCName: "x", AgentType: "claude_code"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "mcp.json.duckway-backup")); err != nil {
		t.Errorf("expected backup file: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(tmp, "mcp.json"))
	if string(raw) == string(garbage) {
		t.Error("garbage was not replaced with valid JSON")
	}
}
