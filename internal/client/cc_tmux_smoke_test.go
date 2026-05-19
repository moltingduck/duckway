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

	// Pass a multi-line string so we also exercise the newline-to-space
	// rewrite that keeps Ink from submitting partial prompts.
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
		// The embedded "\n" should have been replaced with a space, and a
		// trailing "\r" appended for submit. cat translates CR→LF on
		// output via the line discipline so we see the full pasted line
		// terminated by a newline.
		if strings.Contains(got, "tmux\nsecond") {
			return false
		}
		return strings.Contains(got, "hello tmux second line")
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
