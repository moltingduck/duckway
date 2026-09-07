package ducklioncli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunAgentsReportsOnlyHostAvailableTypesForProject(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"codex", "claude"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("SHELL", "/bin/zsh")
	project := t.TempDir()
	var output bytes.Buffer
	if err := runAgents([]string{"--cwd", project, "--json"}, &output); err != nil {
		t.Fatal(err)
	}
	var agents []AgentOutput
	if err := json.Unmarshal(output.Bytes(), &agents); err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 || agents[0].Type != "shell" || agents[0].Command[0] != "/bin/zsh" || agents[1].Type != "codex" || agents[2].Type != "claude_code" {
		t.Fatalf("agents=%+v", agents)
	}
}

func TestRunAgentsFailsClosedForMissingProject(t *testing.T) {
	if err := runAgents([]string{"--cwd", filepath.Join(t.TempDir(), "missing"), "--json"}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing project accepted")
	}
}
