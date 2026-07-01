package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseCodexJSONL(t *testing.T) {
	out := []byte(`Reading additional input from stdin...
{"type":"thread.started","thread_id":"019f02a8-0abe-71c1-bbf6-54b1c4a41dc7"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"first"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"final answer"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}
`)
	sid, result, isError := parseCodexJSONL(out, "")
	if sid != "019f02a8-0abe-71c1-bbf6-54b1c4a41dc7" {
		t.Fatalf("sid = %q", sid)
	}
	if result != "final answer" {
		t.Fatalf("result = %q", result)
	}
	if isError {
		t.Fatal("isError = true")
	}
}

func TestParseCodexJSONL_KeepsFallbackSessionID(t *testing.T) {
	out := []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`)
	sid, result, _ := parseCodexJSONL(out, "existing-thread")
	if sid != "existing-thread" {
		t.Fatalf("sid = %q", sid)
	}
	if result != "ok" {
		t.Fatalf("result = %q", result)
	}
}

func TestParseCodexTmuxEventPayload(t *testing.T) {
	payload := `{"output_path":"/tmp/codex.jsonl","fallback_session_id":"sid-old","exit_code":0}`
	ev, ok := parseCodexTmuxEventPayload(payload)
	if !ok {
		t.Fatal("parseCodexTmuxEventPayload ok = false")
	}
	if ev.OutputPath != "/tmp/codex.jsonl" || ev.FallbackSessionID != "sid-old" || ev.ExitCode != 0 {
		t.Fatalf("event = %+v", ev)
	}

	if _, ok := parseCodexTmuxEventPayload(`{"exit_code":0}`); ok {
		t.Fatal("parseCodexTmuxEventPayload accepted event without output_path")
	}
}

func TestRunViaCodexExecSkipsGitRepoCheck(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/bin/sh
case "$*" in
  *"exec --json --skip-git-repo-check --sandbox workspace-write -C"* ) ;;
  *) printf 'unexpected argv: %s\n' "$*" >&2; exit 12;;
esac
printf '%s\n' '{"type":"thread.started","thread_id":"sid-codex-exec"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}'
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sid, result, isErr, err := runViaCodexExec(ctx, stub, dir, "hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "sid-codex-exec" || result != "ok" || isErr {
		t.Fatalf("sid=%q result=%q isErr=%v", sid, result, isErr)
	}
}

func TestWriteCodexTmuxLaunchScript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-launch.sh")
	if err := writeCodexTmuxLaunchScript(path, "/usr/local/bin/codex", "/repo", "/tmp/prompt.txt", "/tmp/out.jsonl", "/tmp/events", ""); err != nil {
		t.Fatalf("writeCodexTmuxLaunchScript: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"set -- '/usr/local/bin/codex' 'exec' '--json' '--skip-git-repo-check' '--sandbox' 'workspace-write' '-C' '/repo' '-'",
		"\"$@\" < \"$prompt\" > \"$out\" 2>&1",
		`"output_path":%s`,
		"exec ${SHELL:-/bin/sh} -i",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("launch script missing %q:\n%s", want, text)
		}
	}
}

func TestWriteCodexTmuxLaunchScriptResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-launch.sh")
	if err := writeCodexTmuxLaunchScript(path, "codex", "/repo", "/tmp/prompt.txt", "/tmp/out.jsonl", "/tmp/events", "sid-123"); err != nil {
		t.Fatalf("writeCodexTmuxLaunchScript: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "set -- 'codex' 'exec' 'resume' '--json' '--skip-git-repo-check' 'sid-123' '-'") {
		t.Fatalf("resume launch script has wrong argv:\n%s", text)
	}
}

func TestRecoverPendingTurnsCodexTmuxEvent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	handle := "codex-recover"
	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		t.Fatal(err)
	}
	eventsDir := filepath.Join(chDir, "events")
	if err := os.MkdirAll(eventsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeInFlight(filepath.Join(chDir, "in-flight.json"), handle, "msg-codex-1", 100); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(chDir, "codex.jsonl")
	out := []byte(`{"type":"thread.started","thread_id":"sid-codex-new"}
{"type":"item.completed","item":{"type":"agent_message","text":"recovered codex reply"}}
`)
	if err := os.WriteFile(outputPath, out, 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(codexTmuxEvent{OutputPath: outputPath, FallbackSessionID: "sid-old", ExitCode: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eventsDir, "200.stop.json"), payload, 0600); err != nil {
		t.Fatal(err)
	}

	results, err := RecoverPendingTurns()
	if err != nil {
		t.Fatalf("RecoverPendingTurns: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1: %+v", len(results), results)
	}
	got := results[0]
	if !got.HadResult || got.Handle != handle || got.MessageID != "msg-codex-1" {
		t.Fatalf("result metadata = %+v", got)
	}
	if got.SessionID != "sid-codex-new" {
		t.Fatalf("SessionID = %q", got.SessionID)
	}
	if got.LastAssistantMessage != "recovered codex reply" {
		t.Fatalf("LastAssistantMessage = %q", got.LastAssistantMessage)
	}
	if _, err := os.Stat(filepath.Join(chDir, "in-flight.json")); !os.IsNotExist(err) {
		t.Fatalf("in-flight still exists: %v", err)
	}
}
