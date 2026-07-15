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

func TestParseCodexJSONLResultDetectsTerminalEvents(t *testing.T) {
	out := []byte(`{"type":"thread.started","thread_id":"sid-terminal"}
{"type":"item.completed","item":{"type":"agent_message","text":"finished before transport error"}}
{"type":"task_complete"}
failed to connect to websocket: Attack attempt detected
`)
	got := parseCodexJSONLResult(out, "")
	if got.SessionID != "sid-terminal" {
		t.Fatalf("SessionID = %q", got.SessionID)
	}
	if got.Result != "finished before transport error" {
		t.Fatalf("Result = %q", got.Result)
	}
	if got.IsError {
		t.Fatal("IsError = true")
	}
	if !got.Complete {
		t.Fatal("Complete = false")
	}
}

func TestParseCodexJSONLResultFinalEvent(t *testing.T) {
	out := []byte(`{"type":"thread.started","thread_id":"sid-final"}
{"type":"final","text":"final event answer","debug":{"token":"do-not-include"}}
{"type":"task_complete"}
`)
	got := parseCodexJSONLResult(out, "")
	if got.Result != "final event answer" || !got.Complete || got.IsError {
		t.Fatalf("parsed = %+v", got)
	}
	if strings.Contains(got.Result, "do-not-include") {
		t.Fatalf("parser leaked non-public final fields: %q", got.Result)
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

func TestParseCodexJSONL_ErrorMessage(t *testing.T) {
	out := []byte(`{"type":"thread.started","thread_id":"sid-error"}
{"type":"turn.started"}
{"type":"error","message":"unexpected status 401 Unauthorized: Missing scopes: api.responses.write"}
`)
	sid, result, isError := parseCodexJSONL(out, "")
	if sid != "sid-error" {
		t.Fatalf("sid = %q", sid)
	}
	if result != "unexpected status 401 Unauthorized: Missing scopes: api.responses.write" {
		t.Fatalf("result = %q", result)
	}
	if !isError {
		t.Fatal("isError = false")
	}
}

func TestParseCodexJSONL_DoesNotWrapSuccessfulMessageAsError(t *testing.T) {
	out := []byte(`{"type":"thread.started","thread_id":"sid-ok"}
{"type":"item.completed","item":{"type":"agent_message","text":"Hi. What would you like me to work on?"}}
{"type":"turn.failed"}
`)
	sid, result, isError := parseCodexJSONL(out, "")
	if sid != "sid-ok" {
		t.Fatalf("sid = %q", sid)
	}
	if result != "Hi. What would you like me to work on?" {
		t.Fatalf("result = %q", result)
	}
	if isError {
		t.Fatal("isError = true")
	}
}

func TestParseCodexJSONL_KeepsSuccessfulMessageWhenLaterFailureHasText(t *testing.T) {
	want := "Hi，我在。剛剛那張 preview_gang_duck_anims.png 已經成功傳到 quackingduck channel。"
	out := []byte(`{"type":"thread.started","thread_id":"sid-ok"}
{"type":"item.completed","item":{"type":"agent_message","text":"` + want + `"}}
{"type":"turn.failed","message":"` + want + `"}
`)
	sid, result, isError := parseCodexJSONL(out, "")
	if sid != "sid-ok" {
		t.Fatalf("sid = %q", sid)
	}
	if result != want {
		t.Fatalf("result = %q", result)
	}
	if isError {
		t.Fatal("isError = true")
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

func TestRunViaCodexExecReturnsAfterTerminalOutputWhenProcessHangs(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/bin/sh
printf '%s\n' '{"type":"thread.started","thread_id":"sid-hang"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done before hang"}}'
printf '%s\n' '{"type":"task_complete"}'
sleep 30
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()

	sid, result, isErr, err := runViaCodexExec(ctx, stub, dir, "hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("runViaCodexExec waited for hung process; elapsed=%v", elapsed)
	}
	if sid != "sid-hang" || result != "done before hang" || isErr {
		t.Fatalf("sid=%q result=%q isErr=%v", sid, result, isErr)
	}
}

func TestRunViaCodexExecFinalAnswerWinsOverTransportExitError(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/bin/sh
printf '%s\n' '{"type":"thread.started","thread_id":"sid-exit-error"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done before websocket error"}}'
printf '%s\n' '{"type":"turn.completed"}'
printf '%s\n' 'failed to connect to websocket: Attack attempt detected' >&2
exit 7
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
	if sid != "sid-exit-error" || result != "done before websocket error" || isErr {
		t.Fatalf("sid=%q result=%q isErr=%v", sid, result, isErr)
	}
}

func TestRunViaCodexExecDetectsTransportFailureBeforeCompletion(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/bin/sh
printf '%s\n' 'failed to connect to websocket: Attack attempt detected' >&2
printf '%s\n' 'Falling back from WebSockets to HTTPS transport. stream disconnected before completion: Attack attempt detected' >&2
exit 7
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, result, isErr, err := runViaCodexExec(ctx, stub, dir, "hello", "", nil)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if result != "" || isErr {
		t.Fatalf("result=%q isErr=%v, want empty false", result, isErr)
	}
	msg := err.Error()
	for _, want := range []string{
		"transport failed before completion",
		"failed to connect to websocket: Attack attempt detected",
		"stream disconnected before completion: Attack attempt detected",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q:\n%s", want, msg)
		}
	}
}

func TestCodexTransportFailureSummaryIsGeneric(t *testing.T) {
	cases := []string{
		"proxyconnect tcp: connection refused",
		"TLS handshake timeout",
		"stream disconnected before completion",
		"unexpected EOF",
	}
	for _, input := range cases {
		if got, ok := codexTransportFailureSummary([]byte(input), nil); !ok || got == "" {
			t.Fatalf("summary(%q) = %q, %v; want match", input, got, ok)
		}
	}
	if got, ok := codexTransportFailureSummary([]byte("model returned a policy answer"), nil); ok {
		t.Fatalf("non-transport text matched: %q", got)
	}
}

func TestRunViaCodexExecResumeUsesConfigSandbox(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/bin/sh
case "$*" in
  *"--sandbox"* ) printf 'unexpected --sandbox on resume: %s\n' "$*" >&2; exit 2;;
  *"exec resume --json --skip-git-repo-check -c sandbox_mode=\"danger-full-access\" sid-123 hello"* ) ;;
  *) printf 'unexpected argv: %s\n' "$*" >&2; exit 12;;
esac
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"resumed"}}'
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sid, result, isErr, err := runViaCodexExec(ctx, stub, dir, "hello", "sid-123", []string{"DUCKWAY_CC_CODEX_SANDBOX=danger-full-access"})
	if err != nil {
		t.Fatal(err)
	}
	if sid != "sid-123" || result != "resumed" || isErr {
		t.Fatalf("sid=%q result=%q isErr=%v", sid, result, isErr)
	}
}

func TestWriteCodexTmuxLaunchScript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-launch.sh")
	if err := writeCodexTmuxLaunchScript(path, "/usr/local/bin/codex", "/repo", "/tmp/prompt.txt", "/tmp/out.jsonl", "/tmp/events", "", nil); err != nil {
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
	if err := writeCodexTmuxLaunchScript(path, "codex", "/repo", "/tmp/prompt.txt", "/tmp/out.jsonl", "/tmp/events", "sid-123", []string{"DUCKWAY_CC_CODEX_SANDBOX=danger-full-access"}); err != nil {
		t.Fatalf("writeCodexTmuxLaunchScript: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "'--sandbox'") {
		t.Fatalf("resume launch script must not pass --sandbox:\n%s", text)
	}
	if !strings.Contains(text, "set -- 'codex' 'exec' 'resume' '--json' '--skip-git-repo-check' '-c' 'sandbox_mode=\"danger-full-access\"' 'sid-123' '-'") {
		t.Fatalf("resume launch script has wrong argv:\n%s", text)
	}
}

func TestCodexExecSandboxArgs(t *testing.T) {
	cases := []struct {
		env  []string
		want string
	}{
		{nil, "--sandbox workspace-write"},
		{[]string{"DUCKWAY_CC_CODEX_SANDBOX=read-only"}, "--sandbox read-only"},
		{[]string{"DUCKWAY_CC_CODEX_SANDBOX=danger-full-access"}, "--sandbox danger-full-access"},
		{[]string{"DUCKWAY_CC_CODEX_SANDBOX=none"}, ""},
		{[]string{"DUCKWAY_CC_CODEX_SANDBOX=$(rm -rf /)"}, "--sandbox workspace-write"},
	}
	for _, tc := range cases {
		got := strings.Join(codexExecSandboxArgs(tc.env), " ")
		if got != tc.want {
			t.Errorf("codexExecSandboxArgs(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestCodexResumeSandboxArgs(t *testing.T) {
	cases := []struct {
		env  []string
		want string
	}{
		{nil, "-c sandbox_mode=\"workspace-write\""},
		{[]string{"DUCKWAY_CC_CODEX_SANDBOX=read-only"}, "-c sandbox_mode=\"read-only\""},
		{[]string{"DUCKWAY_CC_CODEX_SANDBOX=danger-full-access"}, "-c sandbox_mode=\"danger-full-access\""},
		{[]string{"DUCKWAY_CC_CODEX_SANDBOX=none"}, ""},
		{[]string{"DUCKWAY_CC_CODEX_SANDBOX=$(rm -rf /)"}, "-c sandbox_mode=\"workspace-write\""},
	}
	for _, tc := range cases {
		got := strings.Join(codexResumeSandboxArgs(tc.env), " ")
		if got != tc.want {
			t.Errorf("codexResumeSandboxArgs(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestSanitizeAgentOptions(t *testing.T) {
	cases := []struct {
		name      string
		agentType string
		opts      map[string]string
		want      map[string]string
	}{
		{
			name:      "codex danger full access",
			agentType: "codex",
			opts:      map[string]string{"sandbox": "danger-full-access"},
			want:      map[string]string{"sandbox": "danger-full-access"},
		},
		{
			name:      "codex injected sandbox",
			agentType: "codex",
			opts:      map[string]string{"sandbox": "workspace-write --profile unsafe"},
			want:      map[string]string{"sandbox": "workspace-write"},
		},
		{
			name:      "claude strips options",
			agentType: "claude_code",
			opts:      map[string]string{"sandbox": "danger-full-access"},
			want:      map[string]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeAgentOptions(tc.agentType, tc.opts)
			if len(got) != len(tc.want) {
				t.Fatalf("sanitizeAgentOptions = %+v, want %+v", got, tc.want)
			}
			for k, want := range tc.want {
				if got[k] != want {
					t.Fatalf("sanitizeAgentOptions[%s] = %q, want %q", k, got[k], want)
				}
			}
		})
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

func TestRecoverPendingTurnsCodexCompletedOutputWithoutStopEvent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	handle := "codex-recover-complete"
	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(chDir, "events"), 0700); err != nil {
		t.Fatal(err)
	}
	turnTS := int64(123456789)
	if err := writeInFlight(filepath.Join(chDir, "in-flight.json"), handle, "msg-codex-complete", turnTS); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(chDir, "codex-123456789.jsonl")
	out := []byte(`{"type":"thread.started","thread_id":"sid-recovered-complete"}
{"type":"item.completed","item":{"type":"agent_message","text":"recovered without stop event"}}
{"type":"task_complete"}
failed to connect to websocket: Attack attempt detected
`)
	if err := os.WriteFile(outputPath, out, 0600); err != nil {
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
	if !got.HadResult || got.Handle != handle || got.MessageID != "msg-codex-complete" {
		t.Fatalf("result metadata = %+v", got)
	}
	if got.SessionID != "sid-recovered-complete" {
		t.Fatalf("SessionID = %q", got.SessionID)
	}
	if got.LastAssistantMessage != "recovered without stop event" {
		t.Fatalf("LastAssistantMessage = %q", got.LastAssistantMessage)
	}
	if _, err := os.Stat(filepath.Join(chDir, "in-flight.json")); !os.IsNotExist(err) {
		t.Fatalf("in-flight still exists: %v", err)
	}
}

func TestRecoverPendingTurnsCodexIncompleteOutputStillRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	handle := "codex-recover-incomplete"
	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(chDir, "events"), 0700); err != nil {
		t.Fatal(err)
	}
	turnTS := int64(987654321)
	if err := writeInFlight(filepath.Join(chDir, "in-flight.json"), handle, "msg-codex-incomplete", turnTS); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(chDir, "codex-987654321.jsonl")
	out := []byte(`{"type":"thread.started","thread_id":"sid-incomplete"}
{"type":"item.completed","item":{"type":"agent_message","text":"partial answer"}}
`)
	if err := os.WriteFile(outputPath, out, 0600); err != nil {
		t.Fatal(err)
	}

	results, err := RecoverPendingTurns()
	if err != nil {
		t.Fatalf("RecoverPendingTurns: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1: %+v", len(results), results)
	}
	if results[0].HadResult {
		t.Fatalf("incomplete output should not be recovered as final: %+v", results[0])
	}
	if _, err := os.Stat(filepath.Join(chDir, "in-flight.json")); err != nil {
		t.Fatalf("in-flight should remain: %v", err)
	}
}

func TestRecoverPendingTurnsCodexRejectsOutputPathOutsideChannelDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	handle := "codex-recover-path"
	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		t.Fatal(err)
	}
	eventsDir := filepath.Join(chDir, "events")
	if err := os.MkdirAll(eventsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeInFlight(filepath.Join(chDir, "in-flight.json"), handle, "msg-codex-2", 100); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"do not post"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(codexTmuxEvent{OutputPath: outside, FallbackSessionID: "sid-old", ExitCode: 0})
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
	if len(results) != 0 {
		t.Fatalf("results len = %d, want 0: %+v", len(results), results)
	}
	if _, err := os.Stat(filepath.Join(chDir, "in-flight.json")); err != nil {
		t.Fatalf("in-flight should remain quarantined for inspection: %v", err)
	}
}

func TestRecoverPendingTurnsSkipsMismatchedInFlightHandle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dirHandle := "codex-dir-handle"
	chDir, err := tmuxChannelDir(dirHandle)
	if err != nil {
		t.Fatal(err)
	}
	eventsDir := filepath.Join(chDir, "events")
	if err := os.MkdirAll(eventsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeInFlight(filepath.Join(chDir, "in-flight.json"), "different-handle", "msg-mismatch", 100); err != nil {
		t.Fatal(err)
	}
	stop := `{"session_id":"sid-mismatch","last_assistant_message":"should not post"}`
	if err := os.WriteFile(filepath.Join(eventsDir, "200.stop.json"), []byte(stop), 0600); err != nil {
		t.Fatal(err)
	}

	results, err := RecoverPendingTurns()
	if err != nil {
		t.Fatalf("RecoverPendingTurns: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results len = %d, want 0: %+v", len(results), results)
	}
}
