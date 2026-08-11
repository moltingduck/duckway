package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunViaClaudePTYParsesJSON(t *testing.T) {
	binDir := installDucklionTestHelper(t)
	claude := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nprintf '%s\\n' '{\"session_id\":\"sid-1\",\"result\":\"hello pty\",\"is_error\":false}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	configDir := shortTempDir(t)
	sid, result, isErr, err := runViaClaudePTY(context.Background(), claude, t.TempDir(), "hi", "", []string{
		"DUCKWAY_CONFIG_DIR=" + configDir,
		"DUCKWAY_CC_CHANNEL_HANDLE=dwch_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sid != "sid-1" || result != "hello pty" || isErr {
		t.Fatalf("sid=%q result=%q isErr=%v", sid, result, isErr)
	}
}

func TestRunViaCodexPTYParsesJSONL(t *testing.T) {
	binDir := installDucklionTestHelper(t)
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\ncat <<'EOF'\n{\"type\":\"thread.started\",\"thread_id\":\"codex-sid\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"codex pty ok\"}}\n{\"type\":\"task_complete\"}\nEOF\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	sid, result, isErr, err := runViaCodexPTY(context.Background(), codex, t.TempDir(), "hi", "", []string{
		"DUCKWAY_CONFIG_DIR=" + shortTempDir(t),
		"DUCKWAY_CC_CHANNEL_HANDLE=dwch_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sid != "codex-sid" || !strings.Contains(result, "codex pty ok") || isErr {
		t.Fatalf("sid=%q result=%q isErr=%v", sid, result, isErr)
	}
}

func TestRunViaCodexPTYTreatsIncompleteOutputAsError(t *testing.T) {
	binDir := installDucklionTestHelper(t)
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\nprintf '%s\\n' 'Network connection interrupted; retrying (5/5).'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, _, _, err := runViaCodexPTY(context.Background(), codex, t.TempDir(), "hi", "", []string{
		"DUCKWAY_CONFIG_DIR=" + shortTempDir(t),
		"DUCKWAY_CC_CHANNEL_HANDLE=dwch_test",
	})
	if err == nil {
		t.Fatal("expected incomplete codex output to return an error")
	}
	if !strings.Contains(err.Error(), "transport failed before completion") {
		t.Fatalf("error = %v, want transport failure summary", err)
	}
}

func TestRunViaCodexPTYTreatsTaskCompleteWithoutResultAsError(t *testing.T) {
	binDir := installDucklionTestHelper(t)
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\ncat <<'EOF'\n{\"type\":\"thread.started\",\"thread_id\":\"codex-sid\"}\n{\"type\":\"task_complete\"}\nEOF\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	sid, result, isErr, err := runViaCodexPTY(context.Background(), codex, t.TempDir(), "hi", "", []string{
		"DUCKWAY_CONFIG_DIR=" + shortTempDir(t),
		"DUCKWAY_CC_CHANNEL_HANDLE=dwch_test",
	})
	if err == nil {
		t.Fatalf("expected task_complete without result to fail, got sid=%q result=%q isErr=%v", sid, result, isErr)
	}
	if !strings.Contains(err.Error(), "completed without result") {
		t.Fatalf("error = %v, want incomplete completion error", err)
	}
}

func installDucklionTestHelper(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	link := filepath.Join(binDir, "ducklion")
	if err := os.Symlink(os.Args[0], link); err != nil {
		t.Fatal(err)
	}
	return binDir
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
