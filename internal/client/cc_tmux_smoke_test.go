//go:build smoke

package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Smoke tests behind the `smoke` build tag — they shell out to a real
// tmux binary and exercise the runViaTmux flow end-to-end with a stub
// claude. Run with:
//
//	go test -tags=smoke ./internal/client/ -v -run 'Smoke'
//
// Each test skips when `tmux` is missing from PATH so CI doesn't fail
// just because the box lacks tmux.

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}
	// Reset memoized availability so the test reflects the actual env.
	tmuxAvailableMemo = nil
}

// TestSmokeTmuxPasteRoundTrip starts a tmux session running `cat` redirected
// into a file, pastes a multi-line string via tmuxPastePrompt, and verifies
// the file contains the pasted text. This is the closest we can get to
// proving paste-buffer + bracketed paste works end-to-end without claude.
func TestSmokeTmuxPasteRoundTrip(t *testing.T) {
	requireTmux(t)

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "captured.txt")
	sess := "duckway-test-paste-" + uniqueSuffix()
	defer exec.Command("tmux", "kill-session", "-t", sess).Run()

	// Start a tmux session that reads stdin (the pane) and appends to a
	// file. We use `sh -c` so we can redirect inside tmux.
	// `exec` replaces the shell with cat so pane_current_command becomes
	// "cat" (without exec, it stays "sh" and paste fires into the wrong
	// process).
	startCmd := []string{
		"new-session", "-d", "-s", sess,
		"sh", "-c", "exec cat >> " + shellSingleQuote(outFile),
	}
	if out, err := exec.Command("tmux", startCmd...).CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v (%s)", err, out)
	}

	// Wait for cat to actually exec. Without this, paste-buffer fires
	// into a pane that hasn't taken over the PTY yet and the input is
	// lost.
	waitFor(t, func() bool {
		out, err := exec.Command("tmux", "display-message", "-p", "-t", sess, "#{pane_current_command}").Output()
		if err != nil {
			return false
		}
		return strings.TrimSpace(string(out)) == "cat"
	}, 3*time.Second, "cat to start in tmux pane")

	// Pass a multi-line string so we exercise the "\"-Enter soft-newline
	// split (each \n becomes a backslash + named Enter pair). cat sees
	// both line tokens; what arrives via cooked-mode ICRNL is
	// "hello tmux\\\nsecond line\n" (the trailing CR gets translated
	// to LF too). Assert both substrings appear.
	want := "hello tmux\nsecond line"
	if err := tmuxPastePrompt(sess, want); err != nil {
		t.Fatalf("tmuxPastePrompt: %v", err)
	}

	waitFor(t, func() bool {
		b, err := os.ReadFile(outFile)
		if err != nil {
			return false
		}
		got := string(b)
		return strings.Contains(got, "hello tmux") && strings.Contains(got, "second line")
	}, 3*time.Second, "pasted text to appear in capture file")

	// Sanity-check helper APIs while we're connected.
	if !tmuxHasSession(sess) {
		t.Errorf("tmuxHasSession(%q) = false, want true", sess)
	}
}

// TestSmokeTmuxRunViaTmuxEndToEnd exercises runViaTmux with a stub claude
// shell script. The stub:
//   - parses --settings <path>
//   - finds hook.sh next to the settings file
//   - sleeps briefly to mimic generation latency
//   - fires the Stop hook with a fixed JSON payload
//   - then sleeps so the tmux pane stays alive (mimics interactive TUI)
//
// Verifies runViaTmux returns the expected sessionID + assistant message.
func TestSmokeTmuxRunViaTmuxEndToEnd(t *testing.T) {
	requireTmux(t)

	// Isolate the per-channel state under a temp HOME so tests don't
	// touch the user's real ~/.duckway.
	t.Setenv("HOME", t.TempDir())

	handle := "smoke-" + uniqueSuffix()
	defer tmuxKillSession(handle)

	// Stub claude: a shell script masquerading as the claude binary.
	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "claude")
	stubBody := `#!/bin/sh
settings_path=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --settings) settings_path="$2"; shift 2;;
    *) shift;;
  esac
done
hook_dir=$(dirname "$settings_path")
hook="$hook_dir/hook.sh"
sleep 0.3
payload='{"session_id":"sid-smoke","transcript_path":"/tmp/t","last_assistant_message":"hello from stub claude"}'
printf '%s' "$payload" | "$hook" stop
# Stay alive so the tmux pane doesn't immediately die — mimics interactive claude.
sleep 30
`
	if err := os.WriteFile(stub, []byte(stubBody), 0700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	extraEnv := []string{
		"DUCKWAY_CC_CHANNEL_HANDLE=" + handle,
		"DUCKWAY_CC_MESSAGE_ID=msg-smoke-1",
	}
	sid, result, isErr, err := runViaTmux(ctx, stub, stubDir, "ignored prompt", "", extraEnv)
	if err != nil {
		t.Fatalf("runViaTmux: %v", err)
	}
	if isErr {
		t.Errorf("isError=true, want false")
	}
	if sid != "sid-smoke" {
		t.Errorf("sessionID = %q, want %q", sid, "sid-smoke")
	}
	if result != "hello from stub claude" {
		t.Errorf("result = %q, want %q", result, "hello from stub claude")
	}

	// In-flight marker should have been cleared on successful return.
	chDir, _ := tmuxChannelDir(handle)
	if _, statErr := os.Stat(filepath.Join(chDir, "in-flight.json")); !os.IsNotExist(statErr) {
		t.Errorf("in-flight.json was not cleared after success: stat err = %v", statErr)
	}
}

// TestSmokeRecoverPendingTurns simulates a daemon crash mid-turn: writes
// an in-flight marker and an unconsumed Stop event under the channel's
// events dir, then calls RecoverPendingTurns and checks it returns the
// reconstructed result and cleans up both files.
func TestSmokeRecoverPendingTurns(t *testing.T) {
	requireTmux(t) // not strictly needed but keeps this with the other smokes

	t.Setenv("HOME", t.TempDir())

	handle := "recover-" + uniqueSuffix()
	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		t.Fatal(err)
	}
	eventsDir := filepath.Join(chDir, "events")
	if err := os.MkdirAll(eventsDir, 0700); err != nil {
		t.Fatal(err)
	}

	turnTS := time.Now().UnixNano() - int64(time.Second) // turn started 1s ago
	if err := writeInFlight(filepath.Join(chDir, "in-flight.json"), handle, "msg-recover-1", turnTS); err != nil {
		t.Fatal(err)
	}

	// Stop event with ts > turnTS — this is the reply that should be
	// recovered.
	stopTS := turnTS + 100
	stopName := filepath.Join(eventsDir, intToStr(stopTS)+".stop.json")
	stopPayload := `{"session_id":"sid-recover","transcript_path":"/tmp/t","last_assistant_message":"recovered reply"}`
	if err := os.WriteFile(stopName, []byte(stopPayload), 0600); err != nil {
		t.Fatal(err)
	}

	results, err := RecoverPendingTurns()
	if err != nil {
		t.Fatalf("RecoverPendingTurns: %v", err)
	}
	// May contain entries from other test channels; find ours.
	var got *RecoverPendingTurnsResult
	for i := range results {
		if results[i].Handle == handle {
			got = &results[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no result for handle %q; got %+v", handle, results)
	}
	if !got.HadResult {
		t.Error("HadResult = false, want true")
	}
	if got.SessionID != "sid-recover" {
		t.Errorf("SessionID = %q, want sid-recover", got.SessionID)
	}
	if got.LastAssistantMessage != "recovered reply" {
		t.Errorf("LastAssistantMessage = %q, want recovered reply", got.LastAssistantMessage)
	}
	if got.MessageID != "msg-recover-1" {
		t.Errorf("MessageID = %q, want msg-recover-1", got.MessageID)
	}

	// in-flight.json should be gone; the event file should be removed too.
	if _, statErr := os.Stat(filepath.Join(chDir, "in-flight.json")); !os.IsNotExist(statErr) {
		t.Error("in-flight.json was not cleaned up")
	}
	if _, statErr := os.Stat(stopName); !os.IsNotExist(statErr) {
		t.Error("stop event file was not cleaned up")
	}
}

func TestSmokeCodexTmuxEndToEnd(t *testing.T) {
	requireTmux(t)
	t.Setenv("HOME", t.TempDir())

	handle := "smoke-codex-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})

	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "codex")
	stubBody := `#!/bin/sh
prompt=$(cat)
case "$*" in
  *"exec --json --skip-git-repo-check --sandbox workspace-write -C"*"-") ;;
  *) printf 'unexpected argv: %s\n' "$*" >&2; exit 12;;
esac
if [ "$prompt" != "hello codex" ]; then
  printf 'unexpected prompt: %s\n' "$prompt" >&2
  exit 13
fi
printf '%s\n' '{"type":"thread.started","thread_id":"sid-codex-smoke"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"hello from stub codex"}}'
`
	if err := os.WriteFile(stub, []byte(stubBody), 0700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sid, result, isErr, err := runViaCodexTmux(ctx, stub, stubDir, "hello codex", "", []string{
		"DUCKWAY_CC_CHANNEL_HANDLE=" + handle,
		"DUCKWAY_CC_MESSAGE_ID=msg-codex-smoke-1",
	})
	if err != nil {
		t.Fatalf("runViaCodexTmux: %v", err)
	}
	if isErr {
		t.Fatal("isError=true, want false")
	}
	if sid != "sid-codex-smoke" {
		t.Fatalf("sessionID = %q, want sid-codex-smoke", sid)
	}
	if result != "hello from stub codex" {
		t.Fatalf("result = %q", result)
	}

	chDir, _ := tmuxChannelDir(handle)
	if _, statErr := os.Stat(filepath.Join(chDir, "in-flight.json")); !os.IsNotExist(statErr) {
		t.Fatalf("in-flight.json was not cleared: %v", statErr)
	}
}

func TestSmokeCodexTmuxHonorsContextTimeout(t *testing.T) {
	requireTmux(t)
	t.Setenv("HOME", t.TempDir())

	handle := "smoke-codex-timeout-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})

	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "codex")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, _, err := runViaCodexTmux(ctx, stub, stubDir, "will timeout", "", []string{
		"DUCKWAY_CC_CHANNEL_HANDLE=" + handle,
		"DUCKWAY_CC_MESSAGE_ID=msg-codex-timeout-1",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("runViaCodexTmux returned nil error, want context timeout")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runViaCodexTmux took %v after context timeout", elapsed)
	}
}

// TestSmokeRealClaudeSlashCommand verifies the full slash-command flow
// end-to-end against real claude: runViaTmux pastes `/usage` into the
// TUI, waits for the panel to stabilize (Stop hook does NOT fire for
// local-only commands so this is what unblocks us), captures the pane,
// sends Esc to dismiss the panel, and returns the captured output.
//
// Gated by DUCKWAY_TEST_REAL_CLAUDE=1. Doesn't hit the model — /usage
// is a TUI-local command — so this is cheap to run.
func TestSmokeRealClaudeSlashCommand(t *testing.T) {
	requireTmux(t)
	if os.Getenv("DUCKWAY_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set DUCKWAY_TEST_REAL_CLAUDE=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	handle := "smoke-slash-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})

	cwd := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	_, result, isErr, runErr := runViaTmux(ctx, bin, cwd, "/usage",
		"", []string{"DUCKWAY_CC_CHANNEL_HANDLE=" + handle, "DUCKWAY_CC_MESSAGE_ID=msg-slash-1"})
	elapsed := time.Since(start)

	if runErr != nil {
		t.Fatalf("runViaTmux(/usage): %v", runErr)
	}
	if isErr {
		t.Errorf("isError=true, want false")
	}
	t.Logf("elapsed=%v isError=%v\nresult:\n----\n%s\n----", elapsed, isErr, result)

	// runSlashCommand should return WELL within slashMaxWait (90s).
	// Anything close to 90s means stable-detection never fired and
	// we hit the bailout — that's a regression of the "don't block"
	// guarantee.
	if elapsed > 30*time.Second {
		t.Errorf("slash command took %v — too close to the %v bailout, stability detection likely broken", elapsed, slashMaxWait)
	}

	// Result should contain the /usage panel content (mentions usage,
	// session, or token info) and be wrapped in a code block.
	if !strings.Contains(result, "```") {
		t.Errorf("result should be wrapped in a code block; got: %q", result)
	}
	lower := strings.ToLower(result)
	if !(strings.Contains(lower, "usage") || strings.Contains(lower, "session") || strings.Contains(lower, "token")) {
		t.Errorf("result doesn't look like /usage panel output: %q", result)
	}

	// Welcome banner / pre-existing TUI chrome must NOT leak into the
	// reply — that's the regression the diff against the pre-pane
	// snapshot is meant to fix.
	for _, leak := range []string{"Welcome back", "Try \"", "Tips for getting started"} {
		if strings.Contains(result, leak) {
			t.Errorf("welcome banner leaked into reply (substring %q present)\nresult:\n%s", leak, result)
		}
	}

	// After runViaTmux returns, the Esc should have dismissed the
	// panel. dismissTUIModal polls until the pane actually changes, so
	// by now the panel should be gone. Strongest signal: "Esc to
	// cancel" (the panel's own footer hint) should no longer appear.
	sess := tmuxSessionName(handle)
	pane, _ := exec.Command("tmux", "capture-pane", "-p", "-t", sess).Output()
	t.Logf("post-dismiss pane:\n----\n%s\n----", string(pane))
	if strings.Contains(string(pane), "Esc to cancel") {
		t.Errorf("panel still visible after runViaTmux returned — dismiss didn't take effect")
	}
}

// TestSmokeRealClaudeSlashCommandVariants exercises several local-only
// slash commands through runViaTmux to prove the flow is generic, not
// just hard-coded for /usage. Each subtest asserts:
//   - elapsed time is bounded (no Stop-hook hang)
//   - reply is non-empty and wrapped in a code block
//   - reply contains a command-specific marker
//   - welcome banner doesn't leak through
//
// Gated by DUCKWAY_TEST_REAL_CLAUDE=1. No API tokens consumed — all
// these commands render locally.
func TestSmokeRealClaudeSlashCommandVariants(t *testing.T) {
	requireTmux(t)
	if os.Getenv("DUCKWAY_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set DUCKWAY_TEST_REAL_CLAUDE=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	// Marker: a lowercase substring that should appear somewhere in
	// the captured panel for that command. Chosen to be distinctive
	// (not a word that's in the welcome banner).
	cases := []struct {
		cmd     string
		markers []string // at least one must appear (case-insensitive)
	}{
		{cmd: "/help", markers: []string{"command", "shortcut", "slash"}},
		{cmd: "/context", markers: []string{"context", "tokens", "system prompt"}},
		{cmd: "/release-notes", markers: []string{"release", "version", "added", "fixed"}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(strings.TrimPrefix(tc.cmd, "/"), func(t *testing.T) {
			handle := "smoke-var-" + strings.TrimPrefix(tc.cmd, "/") + "-" + uniqueSuffix()
			t.Cleanup(func() {
				tmuxKillSession(handle)
				if chDir, err := tmuxChannelDir(handle); err == nil {
					_ = os.RemoveAll(chDir)
				}
			})
			cwd := t.TempDir()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			start := time.Now()
			_, result, isErr, runErr := runViaTmux(ctx, bin, cwd, tc.cmd, "",
				[]string{"DUCKWAY_CC_CHANNEL_HANDLE=" + handle, "DUCKWAY_CC_MESSAGE_ID=msg-var"})
			elapsed := time.Since(start)
			if runErr != nil {
				t.Fatalf("runViaTmux(%s): %v", tc.cmd, runErr)
			}
			if isErr {
				t.Errorf("%s: isError=true", tc.cmd)
			}
			t.Logf("%s elapsed=%v\nresult:\n----\n%s\n----", tc.cmd, elapsed, result)

			if elapsed > 30*time.Second {
				t.Errorf("%s took %v — too close to bailout, stability detection broken", tc.cmd, elapsed)
			}
			if !strings.Contains(result, "```") {
				t.Errorf("%s: reply should be wrapped in a code block: %q", tc.cmd, result)
			}
			if strings.Contains(result, "no panel output captured") {
				t.Errorf("%s: panel capture failed", tc.cmd)
			}

			low := strings.ToLower(result)
			hit := false
			for _, m := range tc.markers {
				if strings.Contains(low, strings.ToLower(m)) {
					hit = true
					break
				}
			}
			if !hit {
				t.Errorf("%s: reply contained none of %v\nresult: %s", tc.cmd, tc.markers, result)
			}

			for _, leak := range []string{"Welcome back", "Tips for getting started"} {
				if strings.Contains(result, leak) {
					t.Errorf("%s: banner leak %q in reply", tc.cmd, leak)
				}
			}
		})
	}
}

// TestSmokeRealClaudeFileReference exercises claude's `@<path>` syntax
// through runViaTmux. The daemon types `@<file> question` into the TUI,
// claude expands the @-reference to read the file's contents into the
// prompt, the model answers, and Stop fires. Without proper handling
// the @-reference would arrive as literal text and the model wouldn't
// have the file content to reason about.
//
// Gated by DUCKWAY_TEST_REAL_CLAUDE=1 (consumes one model call).
func TestSmokeRealClaudeFileReference(t *testing.T) {
	requireTmux(t)
	if os.Getenv("DUCKWAY_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set DUCKWAY_TEST_REAL_CLAUDE=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	handle := "smoke-at-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})

	cwd := t.TempDir()
	// Seed a file containing a distinctive token the model can only
	// know about by actually reading the file.
	secretToken := "DUCKWAY_AT_REF_MARKER_47291"
	if err := os.WriteFile(filepath.Join(cwd, "notes.txt"), []byte("The secret token is "+secretToken+".\n"), 0600); err != nil {
		t.Fatal(err)
	}

	prompt := "@notes.txt — repeat back only the secret token value from this file, exactly as written, with no extra text."

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, result, isErr, runErr := runViaTmux(ctx, bin, cwd, prompt, "",
		[]string{"DUCKWAY_CC_CHANNEL_HANDLE=" + handle, "DUCKWAY_CC_MESSAGE_ID=msg-at-1"})
	if runErr != nil {
		t.Fatalf("runViaTmux: %v", runErr)
	}
	if isErr {
		t.Errorf("isError=true, want false")
	}
	t.Logf("result:\n----\n%s\n----", result)

	if !strings.Contains(result, secretToken) {
		t.Errorf("model didn't repeat the secret token %q — @-reference likely didn't expand\nresult: %s", secretToken, result)
	}
}

// TestSmokeRealClaudePickerPassthrough drives a two-turn picker
// interaction through runViaTmux against real claude:
//
//	turn 1: "/release-notes" — opens a numbered picker of versions
//	turn 2: "2"               — picks the second option (latest version)
//
// Verifies:
//   - turn 1 reply is the "picker open" message containing the picker
//     options + the reply hint
//   - turn 2 reply contains the release-notes content for the chosen
//     version (i.e. selection actually navigated + confirmed)
//   - both turns return promptly (no 90s bailout)
//
// Gated by DUCKWAY_TEST_REAL_CLAUDE=1.
func TestSmokeRealClaudePickerPassthrough(t *testing.T) {
	requireTmux(t)
	if os.Getenv("DUCKWAY_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set DUCKWAY_TEST_REAL_CLAUDE=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	handle := "smoke-pick-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})
	cwd := t.TempDir()
	extraEnv := []string{
		"DUCKWAY_CC_CHANNEL_HANDLE=" + handle,
		"DUCKWAY_CC_MESSAGE_ID=msg-pick",
	}

	// Turn 1: open the picker.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel1()
	start := time.Now()
	_, r1, _, err := runViaTmux(ctx1, bin, cwd, "/release-notes", "", extraEnv)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	elapsed1 := time.Since(start)
	t.Logf("turn 1 elapsed=%v\nreply:\n----\n%s\n----", elapsed1, r1)
	if elapsed1 > 30*time.Second {
		t.Errorf("turn 1 took %v — picker detection likely missed", elapsed1)
	}
	if !strings.Contains(r1, "picker open") {
		t.Errorf("turn 1 reply should announce the picker; got: %s", r1)
	}
	if !strings.Contains(r1, "Version") {
		t.Errorf("turn 1 reply should list version options; got: %s", r1)
	}

	// Turn 2: pick option 2.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	start = time.Now()
	_, r2, _, err := runViaTmux(ctx2, bin, cwd, "2", "", extraEnv)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	elapsed2 := time.Since(start)
	t.Logf("turn 2 elapsed=%v\nreply:\n----\n%s\n----", elapsed2, r2)
	if elapsed2 > 30*time.Second {
		t.Errorf("turn 2 took %v — selection didn't resolve cleanly", elapsed2)
	}
	// The reply should be the release-notes content. Look for any
	// release-notes-flavored markers; we don't pin a specific version
	// number because the available versions change over time.
	low := strings.ToLower(r2)
	hit := strings.Contains(low, "release") || strings.Contains(low, "version") ||
		strings.Contains(low, "added") || strings.Contains(low, "fixed")
	if !hit {
		t.Errorf("turn 2 reply doesn't look like release-notes content: %s", r2)
	}
	if strings.Contains(r2, "picker open") {
		t.Errorf("turn 2 still shows the picker — selection didn't go through: %s", r2)
	}
}

// TestSmokeRealClaudeCompact drives runViaTmux with `/compact`, the
// canonical model-invoking slash command (asks the model to summarize
// the conversation context). The interesting property under test: the
// Stop hook fires for /compact (unlike /usage), so the runTUICommand
// race-stability-vs-Stop loop must pick Stop, not return a mid-flight
// pane snapshot. Verifies we get an actual model reply (resolved from
// the transcript by resolveAssistantMessage), not the picker/spinner
// pane.
//
// Gated by DUCKWAY_TEST_REAL_CLAUDE=1; consumes one model call against
// an essentially-empty session so the cost is tiny.
func TestSmokeRealClaudeCompact(t *testing.T) {
	requireTmux(t)
	if os.Getenv("DUCKWAY_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set DUCKWAY_TEST_REAL_CLAUDE=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	handle := "smoke-compact-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})
	cwd := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	start := time.Now()
	sid, result, isErr, runErr := runViaTmux(ctx, bin, cwd, "/compact", "",
		[]string{"DUCKWAY_CC_CHANNEL_HANDLE=" + handle, "DUCKWAY_CC_MESSAGE_ID=msg-compact"})
	elapsed := time.Since(start)
	if runErr != nil {
		t.Fatalf("runViaTmux: %v", runErr)
	}
	if isErr {
		t.Errorf("isError=true, want false")
	}
	t.Logf("sid=%q elapsed=%v\nresult:\n----\n%s\n----", sid, elapsed, result)

	// We don't pin specific text — the model is free to phrase the
	// compaction summary however it likes, including "no context to
	// compact" for a fresh session. But the reply must be non-empty,
	// non-error, and NOT the "picker open" hint.
	if strings.TrimSpace(result) == "" || strings.Contains(result, "no panel output captured") {
		t.Errorf("got empty / no-output result for /compact: %q", result)
	}
	if strings.Contains(result, "picker open") {
		t.Errorf("/compact mistakenly routed through picker passthrough: %q", result)
	}
}

// TestSmokeRealClaudePickerAutoDismiss verifies: when a picker is left
// open from a previous turn and the user sends a fresh slash command
// (not a number / not "cancel"), the daemon dismisses the picker and
// processes the new command normally — instead of stalling with a
// "reply with the option number or cancel" hint.
//
// Gated by DUCKWAY_TEST_REAL_CLAUDE=1.
func TestSmokeRealClaudePickerAutoDismiss(t *testing.T) {
	requireTmux(t)
	if os.Getenv("DUCKWAY_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set DUCKWAY_TEST_REAL_CLAUDE=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	handle := "smoke-autodismiss-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})
	cwd := t.TempDir()
	extraEnv := []string{
		"DUCKWAY_CC_CHANNEL_HANDLE=" + handle,
		"DUCKWAY_CC_MESSAGE_ID=msg-ad",
	}

	// Turn 1: open the picker.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel1()
	_, r1, _, err := runViaTmux(ctx1, bin, cwd, "/release-notes", "", extraEnv)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if !strings.Contains(r1, "picker open") {
		t.Fatalf("turn 1 didn't open picker: %s", r1)
	}

	// Turn 2: send a fresh slash command instead of a picker selection.
	// The daemon should auto-dismiss the picker and run /help normally.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	start := time.Now()
	_, r2, _, err := runViaTmux(ctx2, bin, cwd, "/help", "", extraEnv)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("turn 2 elapsed=%v\nreply:\n----\n%s\n----", elapsed, r2)
	if elapsed > 30*time.Second {
		t.Errorf("turn 2 took %v — auto-dismiss didn't unblock", elapsed)
	}
	if strings.Contains(r2, "picker open") {
		t.Errorf("turn 2 still says picker is open — auto-dismiss didn't fire: %s", r2)
	}
	if strings.Contains(r2, "picker is still open") {
		t.Errorf("turn 2 stalled on picker-stall hint: %s", r2)
	}
	low := strings.ToLower(r2)
	if !(strings.Contains(low, "command") || strings.Contains(low, "shortcut") || strings.Contains(low, "slash")) {
		t.Errorf("turn 2 doesn't look like /help output: %s", r2)
	}
}

// TestSmokeRealClaudeMultiLinePrompt verifies that prompts containing
// embedded "\n" are preserved as actual multi-line input in claude's
// TUI (via the "\"-Enter soft-newline syntax). Without the multi-line
// support, all newlines would collapse to spaces and the model would
// see "Reply line1 line2 line3" instead of three distinct lines.
//
// Gated by DUCKWAY_TEST_REAL_CLAUDE=1.
func TestSmokeRealClaudeMultiLinePrompt(t *testing.T) {
	requireTmux(t)
	if os.Getenv("DUCKWAY_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set DUCKWAY_TEST_REAL_CLAUDE=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	handle := "smoke-ml-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})

	cwd := t.TempDir()
	// Use a prompt that makes the model echo distinct words for each
	// line — if newlines were collapsed to spaces the answer would
	// show all three words on one line; if they were preserved as
	// separate inputs the answer reflects that structure.
	prompt := "Repeat back exactly the THREE distinct words I list below, one per line, no other text:\nAlphaWord\nBetaWord\nGammaWord"

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	_, result, isErr, runErr := runViaTmux(ctx, bin, cwd, prompt, "",
		[]string{"DUCKWAY_CC_CHANNEL_HANDLE=" + handle, "DUCKWAY_CC_MESSAGE_ID=msg-ml-1"})
	if runErr != nil {
		t.Fatalf("runViaTmux: %v", runErr)
	}
	if isErr {
		t.Errorf("isError=true, want false")
	}
	t.Logf("result:\n----\n%s\n----", result)

	// All three distinct words must be present in the reply.
	for _, w := range []string{"AlphaWord", "BetaWord", "GammaWord"} {
		if !strings.Contains(result, w) {
			t.Errorf("missing word %q in reply", w)
		}
	}
}

// TestSmokeRealClaudeShellCommand drives runViaTmux with "! ls" (the
// post-strip form of Discord's "!! ls" escape) against real claude.
// Verifies that claude's bash-mode output (an `ls` listing of the cwd)
// makes it back as the Discord reply, the welcome banner is stripped
// by the anchor extraction, and runViaTmux returns promptly without
// blocking on a Stop hook that never fires for shell mode.
//
// Gated by DUCKWAY_TEST_REAL_CLAUDE=1.
func TestSmokeRealClaudeShellCommand(t *testing.T) {
	requireTmux(t)
	if os.Getenv("DUCKWAY_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set DUCKWAY_TEST_REAL_CLAUDE=1 to run")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	handle := "smoke-shell-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})

	// Use a temp cwd with known contents we can grep for in the reply.
	cwd := t.TempDir()
	for _, name := range []string{"alpha.txt", "beta.txt", "gamma.txt"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	_, result, isErr, runErr := runViaTmux(ctx, bin, cwd, "! ls",
		"", []string{"DUCKWAY_CC_CHANNEL_HANDLE=" + handle, "DUCKWAY_CC_MESSAGE_ID=msg-shell-1"})
	elapsed := time.Since(start)
	if runErr != nil {
		t.Fatalf("runViaTmux: %v", runErr)
	}
	if isErr {
		t.Errorf("isError=true, want false")
	}
	t.Logf("elapsed=%v\nresult:\n----\n%s\n----", elapsed, result)

	if elapsed > 30*time.Second {
		t.Errorf("shell command took %v — too close to bailout, stability detection broken", elapsed)
	}
	for _, want := range []string{"alpha.txt", "beta.txt", "gamma.txt"} {
		if !strings.Contains(result, want) {
			t.Errorf("result missing expected file %q\nresult: %s", want, result)
		}
	}
	for _, leak := range []string{"Welcome back", "Tips for getting started"} {
		if strings.Contains(result, leak) {
			t.Errorf("banner leak %q in shell-mode reply", leak)
		}
	}
}

// TestSmokeRealClaudeRoundTrip runs the full runViaTmux flow against
// the real `claude` CLI. Gated by DUCKWAY_TEST_REAL_CLAUDE=1 because it
// consumes API tokens and requires claude to be authenticated. Use
// this to verify end-to-end that the prompt actually reaches claude
// and the Stop hook fires.
//
// Run with:
//
//	DUCKWAY_TEST_REAL_CLAUDE=1 go test -tags=smoke ./internal/client/ \
//	    -run TestSmokeRealClaudeRoundTrip -v -count=1
func TestSmokeRealClaudeRoundTrip(t *testing.T) {
	requireTmux(t)
	if os.Getenv("DUCKWAY_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set DUCKWAY_TEST_REAL_CLAUDE=1 to run (real claude, consumes tokens)")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	// Unique handle so this test doesn't collide with any real cc-watch
	// state. We DON'T override HOME — claude needs its credentials.
	handle := "smoke-real-" + uniqueSuffix()
	t.Cleanup(func() {
		tmuxKillSession(handle)
		if chDir, err := tmuxChannelDir(handle); err == nil {
			_ = os.RemoveAll(chDir)
		}
	})

	dumpDiag := func(stage string) {
		sess := tmuxSessionName(handle)
		pane, _ := exec.Command("tmux", "capture-pane", "-p", "-t", sess).Output()
		t.Logf("[%s] pane contents:\n----\n%s\n----", stage, string(pane))
		chDir, _ := tmuxChannelDir(handle)
		if entries, err := os.ReadDir(filepath.Join(chDir, "events")); err == nil {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Logf("[%s] events/ contents: %v", stage, names)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	extraEnv := []string{
		"DUCKWAY_CC_CHANNEL_HANDLE=" + handle,
		"DUCKWAY_CC_MESSAGE_ID=msg-real-1",
	}

	prompt := "Reply with exactly the word OK and nothing else."
	sid, result, isErr, err := runViaTmux(ctx, bin, t.TempDir(), prompt, "", extraEnv)
	if err != nil {
		dumpDiag("error")
		t.Fatalf("runViaTmux: %v", err)
	}
	dumpDiag("success")
	t.Logf("sessionID=%q isError=%v result=%q", sid, isErr, result)
	if !strings.Contains(strings.ToLower(result), "ok") {
		t.Errorf("result %q doesn't look like an OK reply", result)
	}
}

// waitFor polls cond up to timeout, calling t.Fatalf with desc if it
// never becomes true. Avoids racy sleeps in the smoke tests.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// uniqueSuffix returns a short pid+ns-time suffix so concurrent smoke
// runs don't collide on tmux session names or channel dirs.
func uniqueSuffix() string {
	return intToStr(int64(os.Getpid())) + "-" + intToStr(time.Now().UnixNano())
}

func intToStr(n int64) string {
	// Avoid pulling strconv into a file that doesn't otherwise need it.
	var b []byte
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
