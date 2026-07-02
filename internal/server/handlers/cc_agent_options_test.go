package handlers

import (
	"encoding/json"
	"testing"
)

func TestNormalizeCCConfigForCodexSandbox(t *testing.T) {
	got, err := normalizeCCConfigForAgent("codex", `{"guild_id":"g","agent_options":{"sandbox":"danger-full-access"}}`)
	if err != nil {
		t.Fatalf("normalizeCCConfigForAgent: %v", err)
	}
	var cfg struct {
		GuildID      string            `json:"guild_id"`
		AgentOptions map[string]string `json:"agent_options"`
	}
	if err := json.Unmarshal([]byte(got), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GuildID != "g" || cfg.AgentOptions["sandbox"] != "danger-full-access" {
		t.Fatalf("normalized config = %+v", cfg)
	}
}

func TestNormalizeCCConfigRejectsInjectedCodexSandbox(t *testing.T) {
	_, err := normalizeCCConfigForAgent("codex", `{"agent_options":{"sandbox":"workspace-write --dangerously-bypass"}}`)
	if err == nil {
		t.Fatal("normalizeCCConfigForAgent accepted injected sandbox")
	}
}

func TestNormalizeCCConfigStripsOptionsForOtherAgents(t *testing.T) {
	got, err := normalizeCCConfigForAgent("openclaw", `{"guild_id":"g","agent_options":{"sandbox":"danger-full-access"}}`)
	if err != nil {
		t.Fatalf("normalizeCCConfigForAgent: %v", err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(got), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["agent_options"]; ok {
		t.Fatalf("agent_options was not stripped: %s", got)
	}
	if cfg["guild_id"] != "g" {
		t.Fatalf("guild_id not preserved: %s", got)
	}
}
