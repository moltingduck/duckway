package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Per-channel claude lives inside a long-lived tmux session named
// "duckway-<handle>". Discord messages are pasted into the same pane so
// context carries over inside the running TUI; the Stop hook signals
// completion by writing a per-event file under the channel's events/
// directory. The user attaches with
//
//	tmux attach -t duckway-<handle>
//
// to watch claude work live.
//
// Files under ~/.duckway/cc-watch/<handle>/ persist across daemon restarts
// so the running claude (started before the restart) keeps writing to
// the same events/ directory the new daemon reads. The in-flight.json
// marker lets the daemon recover a turn whose Stop event arrived while
// the daemon was down — see RecoverPendingTurns.

const (
	tmuxSessionPrefix = "duckway-"
	tmuxRunTimeout    = 5 * time.Minute
	// claudeStartupDelay is how long we wait after launching claude in a
	// fresh tmux pane before sending the first prompt. Claude's Ink TUI
	// takes a couple of seconds to render and start consuming stdin;
	// keystrokes sent before that are dropped.
	claudeStartupDelay = 5 * time.Second
	// claudeSubmitDelay sits between typing the prompt and pressing
	// Enter. Without it the submit can race the TUI render loop.
	claudeSubmitDelay = 400 * time.Millisecond
	// eventPollInterval is how often runViaTmux re-scans the events
	// directory for new Stop events. The hook fires at most once per turn
	// so this isn't on a hot path — we trade a bit of latency for cheap
	// CPU.
	eventPollInterval = 100 * time.Millisecond
	// slashPollInterval is how often the slash-command flow re-captures
	// the pane to check for stability or a Stop event.
	slashPollInterval = 300 * time.Millisecond
	// slashStableWindow: once the pane content hasn't changed for this
	// long, we assume the slash-command panel is fully rendered.
	slashStableWindow = 1500 * time.Millisecond
	// slashMaxWait bounds the slash-command flow so a stuck TUI never
	// blocks forever. Comfortably longer than /compact's typical run
	// time on big contexts.
	slashMaxWait = 90 * time.Second
)

// tmuxAvailable reports whether `tmux` is on PATH. Memoized so repeated
// runner construction doesn't keep stat'ing $PATH.
var tmuxAvailableMemo *bool

func tmuxAvailable() bool {
	if tmuxAvailableMemo != nil {
		return *tmuxAvailableMemo
	}
	_, err := exec.LookPath("tmux")
	v := err == nil
	tmuxAvailableMemo = &v
	return v
}

// tmuxSessionName turns a channel handle into a tmux-safe session name.
// tmux disallows `:` and `.` (used for window/pane targeting); spaces are
// allowed but cumbersome on the CLI. Replace all three with `-`.
func tmuxSessionName(handle string) string {
	safe := strings.NewReplacer(":", "-", ".", "-", " ", "-").Replace(handle)
	return tmuxSessionPrefix + safe
}

// ccWatchRoot is the parent directory that holds per-channel state. Stable
// across daemon restarts so we can find pending turns on relaunch.
func ccWatchRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, ".duckway", "cc-watch")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", root, err)
	}
	return root, nil
}

// tmuxChannelDir returns the per-channel scratch directory.
func tmuxChannelDir(handle string) (string, error) {
	root, err := ccWatchRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, handle)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// runViaTmux is a ccRunFn that drives claude through a long-lived
// per-channel tmux session. Prompts are pasted via the tmux paste buffer
// (bracketed paste, so embedded newlines don't submit early). The Stop
// hook writes an event file under events/; this function polls for a
// file with a timestamp newer than turn-start and returns its payload.
func runViaTmux(ctx context.Context, bin, cwd, prompt, sid string, extraEnv []string) (sessionID, result string, isError bool, err error) {
	handle := envValue(extraEnv, "DUCKWAY_CC_CHANNEL_HANDLE")
	if handle == "" {
		return "", "", false, fmt.Errorf("tmux runner: missing DUCKWAY_CC_CHANNEL_HANDLE in extraEnv")
	}
	sess := tmuxSessionName(handle)

	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		return "", "", false, err
	}
	eventsDir := filepath.Join(chDir, "events")
	if err := os.MkdirAll(eventsDir, 0700); err != nil {
		return "", "", false, fmt.Errorf("mkdir events: %w", err)
	}

	hookPath := filepath.Join(chDir, "hook.sh")
	settingsPath := filepath.Join(chDir, "settings.json")
	launchPath := filepath.Join(chDir, "launch.sh")
	inFlightPath := filepath.Join(chDir, "in-flight.json")

	if err := writeHookScript(hookPath, eventsDir); err != nil {
		return "", "", false, err
	}

	// Turn start timestamp: anything older than this in events/ is from
	// a prior turn (or a different daemon instance) and ignored. We use
	// the same nanosecond resolution as the hook script's `date +%s%N`.
	turnTS := time.Now().UnixNano()

	// Persist the in-flight marker BEFORE pasting the prompt so a daemon
	// crash between paste and stop is recoverable on next launch.
	messageID := envValue(extraEnv, "DUCKWAY_CC_MESSAGE_ID")
	if err := writeInFlight(inFlightPath, handle, messageID, turnTS); err != nil {
		return "", "", false, err
	}

	// Pre-mark the cwd as trusted in claude's per-user state file. The
	// first time claude opens a directory it shows a modal trust dialog
	// ("Is this a project you created or one you trust?") that swallows
	// our typed prompt as an option select. Without this our prompt
	// never reaches the real input field.
	if err := markCwdTrustedInClaude(cwd); err != nil {
		log.Printf("[cc-watch] could not pre-trust cwd %s: %v (claude may show its trust modal and lose the first prompt)", cwd, err)
	}

	launched, err := ensureClaudeInTmux(sess, cwd, bin, sid, hookPath, settingsPath, launchPath, extraEnv)
	if err != nil {
		return "", "", false, err
	}
	if launched {
		select {
		case <-time.After(claudeStartupDelay):
		case <-ctx.Done():
			return "", "", false, ctx.Err()
		}
	}

	if err := tmuxPastePrompt(sess, prompt); err != nil {
		return "", "", false, err
	}

	// claude TUI mode commands (slash + bash) get a different wait
	// strategy. Local-only ones render output to the pane but never fire
	// the Stop hook, so the regular pollForStop would block for the full
	// tmuxRunTimeout. Race pane-stabilization against the Stop event and
	// return whichever fires first. Slash panels need Esc to close;
	// bash output is inline and shouldn't be Esc'd.
	trimmedPrompt := strings.TrimSpace(prompt)
	isSlash := strings.HasPrefix(trimmedPrompt, "/")
	isShell := strings.HasPrefix(trimmedPrompt, "!")
	if isSlash || isShell {
		res, sp, perr := runTUICommand(ctx, sess, eventsDir, turnTS, prompt, isSlash, slashStableWindow, slashMaxWait)
		if perr != nil {
			return "", "", false, perr
		}
		_ = os.Remove(inFlightPath)
		if sp != nil {
			// Model-invoking command (rare for `!` shell; possible for
			// `/compact`) — Stop fired; use the real result.
			return sp.SessionID, resolveAssistantMessage(*sp), false, nil
		}
		return "", res, false, nil
	}

	sid2, res2, isErr2, perr := pollForStop(ctx, eventsDir, turnTS, tmuxRunTimeout)
	if perr != nil {
		return "", "", false, perr
	}
	// Successful Stop — clear the in-flight marker so daemon restart
	// doesn't try to re-post the result.
	_ = os.Remove(inFlightPath)
	return sid2, res2, isErr2, nil
}

// runTUICommand handles claude TUI mode commands — both slash (`/usage`,
// `/help`, `/compact`) and bash (`! ls`, `! cargo test`). Two outcomes:
//
//  1. The Stop hook fires (model-invoking command) → return
//     (nil, stopPayload, nil); caller uses the Stop result.
//  2. The pane content stops changing for stableWindow (local panel or
//     shell output settled) → grab scrollback, anchor on the user's
//     prompt rendering, take everything after, optionally send Esc,
//     return (formattedOutput, nil, nil).
//
// dismissModal is true for slash commands (the panel needs Esc to close
// so the next message lands in an empty input box) and false for shell
// commands (output is inline; Esc would do nothing useful).
//
// Bounded by maxWait so a runaway TUI never blocks forever.
func runTUICommand(ctx context.Context, sess, eventsDir string, turnTS int64, prompt string, dismissModal bool, stableWindow, maxWait time.Duration) (paneCapture string, stop *stopPayload, err error) {
	deadline := time.Now().Add(maxWait)
	tick := time.NewTicker(slashPollInterval)
	defer tick.Stop()

	var lastSnap string
	var lastChange time.Time = time.Now()

	for {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-tick.C:
		}

		// Stop hook fired? Use that.
		if evt, found, ferr := findStopEvent(eventsDir, turnTS); ferr == nil && found {
			var sp stopPayload
			if jerr := json.Unmarshal([]byte(evt.payload), &sp); jerr == nil {
				_ = os.Remove(evt.path)
				return "", &sp, nil
			}
		}

		snap := capturePane(sess)
		if snap != lastSnap {
			lastSnap = snap
			lastChange = time.Now()
		} else if time.Since(lastChange) >= stableWindow && lastSnap != "" {
			body := extractSlashOutput(sess, prompt)
			if dismissModal {
				dismissTUIModal(sess)
			}
			return formatSlashPaneForDiscord(body), nil, nil
		}

		if time.Now().After(deadline) {
			body := extractSlashOutput(sess, prompt)
			if dismissModal {
				dismissTUIModal(sess)
			}
			if body == "" && lastSnap == "" {
				return "", nil, fmt.Errorf("TUI command produced no pane output within %v", maxWait)
			}
			if body == "" {
				body = lastSnap
			}
			return formatSlashPaneForDiscord(body), nil, nil
		}
	}
}

// extractSlashOutput grabs the pane scrollback and returns everything
// after the line where claude echoed the user's prompt. Falls back to
// the full pane if the anchor isn't found.
func extractSlashOutput(sess, prompt string) string {
	out, err := exec.Command("tmux", "capture-pane", "-p", "-t", sess, "-S", "-2000").Output()
	if err != nil {
		return ""
	}
	return extractAfterPromptAnchor(string(out), prompt)
}

// extractAfterPromptAnchor isolates the pane bytes after the user's
// echoed prompt. Two cases:
//
//	prompt = "/usage"   → claude renders "❯ /usage"   → anchor "❯ /usage"
//	prompt = "! ls"     → claude renders "!  ls"      → anchor "ls"
//	                       (indicator "!" + padding + command — search
//	                        on the command portion because the spacing
//	                        between indicator and content isn't stable)
//
// LastIndex returns the most recent occurrence which, after the user
// submits a TUI command, is the just-echoed input line.
//
// Pure function so it's easy to unit test.
func extractAfterPromptAnchor(text, prompt string) string {
	p := strings.TrimSpace(prompt)
	var anchor string
	switch {
	case strings.HasPrefix(p, "/"):
		anchor = "❯ " + p
	case strings.HasPrefix(p, "!"):
		// Shell mode: the indicator "!" is rendered with variable
		// padding before the command. Anchor on the command portion
		// (everything after "!") so spacing differences don't matter.
		anchor = strings.TrimSpace(strings.TrimPrefix(p, "!"))
	default:
		anchor = "❯ " + p
	}
	if anchor == "" {
		return text
	}
	idx := strings.LastIndex(text, anchor)
	if idx < 0 {
		return text
	}
	after := text[idx+len(anchor):]
	if i := strings.Index(after, "\n"); i >= 0 {
		return after[i+1:]
	}
	return ""
}

// capturePane returns the current visible pane contents as plain text.
// Empty string on tmux error so the caller can keep polling cleanly.
func capturePane(sess string) string {
	out, err := exec.Command("tmux", "capture-pane", "-p", "-t", sess).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// dismissTUIModal sends Escape to close any open claude panel (`/usage`,
// `/help`, etc.) so the next prompt lands in an empty input box. Polls
// the pane until the panel actually disappears (or a short timeout) so
// the caller can be confident the TUI is back to input-ready state.
func dismissTUIModal(sess string) {
	before := capturePane(sess)
	_ = exec.Command("tmux", "send-keys", "-t", sess, "Escape").Run()
	// Wait up to 1.5s for the pane to change — the panel collapsing
	// back to the input box is a visible content swap.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if capturePane(sess) != before {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// formatSlashPaneForDiscord trims TUI chrome off the extracted output
// and wraps the result in a code block. Three trim passes:
//
//  1. Trim leading whitespace-only lines.
//  2. Walk from the bottom skipping trailing chrome — input-box
//     separator lines (runs of U+2500 "─"), the empty `❯` prompt,
//     and the bypass-permissions / tmux-focus hint footer that show
//     up below inline shell output. Stop at the first real content
//     line.
//  3. Trim any trailing whitespace-only lines that remain.
//
// Modal commands (`/usage`) don't have trailing chrome; this trim is a
// no-op for them. Inline-output commands (`! ls`) leave the input box
// and footer visible — those get cut here.
//
// Caps length at ~1900 chars so it fits Discord's 2000-char message
// limit even with the code-block fences.
func formatSlashPaneForDiscord(content string) string {
	lines := strings.Split(content, "\n")
	end := len(lines)
	for end > 0 && isTrailingTUIChrome(lines[end-1]) {
		end--
	}
	start := 0
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	trimmed := strings.Join(lines[start:end], "\n")
	if strings.TrimSpace(trimmed) == "" {
		return "_(no panel output captured)_"
	}
	const maxLen = 1900
	if len(trimmed) > maxLen {
		trimmed = trimmed[:maxLen] + "\n… (truncated)"
	}
	return "```\n" + trimmed + "\n```"
}

// isTrailingTUIChrome reports whether a line is part of claude's
// always-on TUI chrome shown below the active content:
//
//   - the input-box horizontal separator (a run of "─" U+2500)
//   - the empty `❯` prompt line
//   - the bypass-perms hint starting with "⏵⏵"
//   - the tmux focus-events warning
//   - status indicators starting with "◉" (effort / model badges)
//
// Used to walk back from the bottom of a pane capture until we hit
// real content. Anchor extraction strips any `❯ <prompt>` echo before
// this runs, so a remaining `❯`-prefixed line is always chrome.
func isTrailingTUIChrome(line string) bool {
	t := strings.TrimSpace(line)
	switch {
	case t == "":
		return true
	case strings.HasPrefix(t, "──"):
		return true
	case strings.HasPrefix(t, "❯"):
		return true
	case strings.HasPrefix(t, "⏵⏵"):
		return true
	case strings.HasPrefix(t, "tmux focus-events"):
		return true
	case strings.HasPrefix(t, "◉"):
		return true
	}
	return false
}

// pollForStop watches eventsDir until a Stop event newer than afterTS
// arrives, the context is cancelled, or the timeout fires. On success it
// returns (session_id, last_assistant_message, false, nil) and deletes
// the event file it consumed.
func pollForStop(ctx context.Context, eventsDir string, afterTS int64, timeout time.Duration) (sessionID, result string, isError bool, err error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(eventPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", "", false, ctx.Err()
		case <-deadline.C:
			return "", "", false, fmt.Errorf("tmux runner: timed out waiting for claude after %v", timeout)
		case <-tick.C:
			evt, found, perr := findStopEvent(eventsDir, afterTS)
			if perr != nil {
				return "", "", false, fmt.Errorf("scan events: %w", perr)
			}
			if !found {
				continue
			}
			var sp stopPayload
			if jerr := json.Unmarshal([]byte(evt.payload), &sp); jerr != nil {
				return "", "", false, fmt.Errorf("parse stop payload: %w", jerr)
			}
			result := resolveAssistantMessage(sp)
			_ = os.Remove(evt.path)
			return sp.SessionID, result, false, nil
		}
	}
}

// resolveAssistantMessage returns the text we should post to Discord for
// a Stop event. Claude Code's Stop hook payload usually only carries
// session_id + transcript_path, so the LastAssistantMessage field is
// empty — we fall back to reading the transcript jsonl and pulling the
// latest assistant text out of it.
func resolveAssistantMessage(sp stopPayload) string {
	if sp.LastAssistantMessage != "" {
		return sp.LastAssistantMessage
	}
	if sp.TranscriptPath == "" {
		return ""
	}
	msg, err := readLastAssistantMessage(sp.TranscriptPath)
	if err != nil {
		log.Printf("[cc-watch] could not read transcript %s: %v", sp.TranscriptPath, err)
		return ""
	}
	return msg
}

// readLastAssistantMessage walks a Claude Code transcript (one JSON
// object per line) and returns the concatenated text of the latest
// assistant turn. Non-text blocks (thinking, tool_use, tool_result)
// are ignored; if the latest assistant entry has no text content we
// keep walking and use the previous one.
func readLastAssistantMessage(transcriptPath string) (string, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	type entry struct {
		Type    string `json:"type"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}

	scanner := bufio.NewScanner(f)
	// Transcripts can carry large pasted documents; raise from the
	// default 64 KB cap so giant lines don't truncate.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var latest string
	for scanner.Scan() {
		var e entry
		if jerr := json.Unmarshal(scanner.Bytes(), &e); jerr != nil {
			continue
		}
		if e.Type != "assistant" {
			continue
		}
		if text := extractAssistantText(e.Message.Content); text != "" {
			latest = text
		}
	}
	return latest, scanner.Err()
}

// extractAssistantText handles both transcript schemas:
//   - content is an array of typed blocks (current) — concatenate all
//     "text" blocks
//   - content is a plain string (legacy) — return as-is
func extractAssistantText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '[' {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return ""
		}
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return ""
}

// pendingEvent is one Stop file we found in events/.
type pendingEvent struct {
	ts      int64
	payload string
	path    string
}

// findStopEvent returns the oldest "stop" event file in eventsDir whose
// timestamp is strictly greater than afterTS. Filename format is
// "<unix_nanos>.<event>.json" (see writeHookScript).
func findStopEvent(eventsDir string, afterTS int64) (*pendingEvent, bool, error) {
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	type candidate struct {
		ts   int64
		name string
	}
	var stops []candidate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip tmp files (".<ts>.<event>.json.tmp" or hidden) and
		// anything that doesn't look like an event file.
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		ts, eventName, ok := parseEventFilename(name)
		if !ok || eventName != "stop" {
			continue
		}
		if ts <= afterTS {
			continue
		}
		stops = append(stops, candidate{ts: ts, name: name})
	}
	if len(stops) == 0 {
		return nil, false, nil
	}
	sort.Slice(stops, func(i, j int) bool { return stops[i].ts < stops[j].ts })
	chosen := stops[0]
	path := filepath.Join(eventsDir, chosen.name)
	payload, rerr := os.ReadFile(path)
	if rerr != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, rerr)
	}
	return &pendingEvent{ts: chosen.ts, payload: string(payload), path: path}, true, nil
}

// parseEventFilename splits "<ts>.<event>.json" into (ts, event).
func parseEventFilename(name string) (int64, string, bool) {
	// Strip ".json"
	if !strings.HasSuffix(name, ".json") {
		return 0, "", false
	}
	base := strings.TrimSuffix(name, ".json")
	dot := strings.Index(base, ".")
	if dot <= 0 || dot == len(base)-1 {
		return 0, "", false
	}
	tsStr := base[:dot]
	eventName := base[dot+1:]
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return ts, eventName, true
}

// inFlight tracks a turn whose prompt was pasted but whose Stop event we
// haven't seen yet. Persisted to disk so a daemon restart can recover.
type inFlight struct {
	Handle    string `json:"handle"`
	MessageID string `json:"message_id,omitempty"`
	TurnTS    int64  `json:"turn_ts"`
}

func writeInFlight(path, handle, messageID string, ts int64) error {
	body, _ := json.Marshal(inFlight{Handle: handle, MessageID: messageID, TurnTS: ts})
	// Write via tmp + rename so a half-written file never appears.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0600); err != nil {
		return fmt.Errorf("write in-flight: %w", err)
	}
	return os.Rename(tmp, path)
}

func readInFlight(path string) (*inFlight, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f inFlight
	if jerr := json.Unmarshal(body, &f); jerr != nil {
		return nil, jerr
	}
	return &f, nil
}

// RecoverPendingTurnsResult describes one pending turn we recovered.
type RecoverPendingTurnsResult struct {
	Handle               string
	MessageID            string
	SessionID            string
	LastAssistantMessage string
	// HadResult is true when we found a Stop event for this turn. False
	// means the in-flight marker exists but no Stop has fired yet (claude
	// might still be generating); the marker is preserved for next time.
	HadResult bool
}

// RecoverPendingTurns scans every per-channel workspace for an in-flight
// marker. For each, it returns the matching Stop event if one is already
// queued in events/ — the caller (CCWatch on startup) is expected to
// post those results to Discord and update its session map. If no Stop
// has arrived yet, HadResult=false and the in-flight marker is left in
// place so the NEXT call (after the user's next message, or another
// restart) can pick it up.
func RecoverPendingTurns() ([]RecoverPendingTurnsResult, error) {
	root, err := ccWatchRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []RecoverPendingTurnsResult
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		chDir := filepath.Join(root, e.Name())
		inFlightPath := filepath.Join(chDir, "in-flight.json")
		f, rerr := readInFlight(inFlightPath)
		if rerr != nil {
			continue // no in-flight for this channel
		}
		eventsDir := filepath.Join(chDir, "events")
		evt, found, ferr := findStopEvent(eventsDir, f.TurnTS)
		if ferr != nil {
			// Surface as a non-fatal warning by including a result without
			// HadResult; caller can log + skip.
			continue
		}
		if !found {
			out = append(out, RecoverPendingTurnsResult{
				Handle:    f.Handle,
				MessageID: f.MessageID,
				HadResult: false,
			})
			continue
		}
		var sp stopPayload
		if jerr := json.Unmarshal([]byte(evt.payload), &sp); jerr != nil {
			continue
		}
		_ = os.Remove(evt.path)
		_ = os.Remove(inFlightPath)
		out = append(out, RecoverPendingTurnsResult{
			Handle:               f.Handle,
			MessageID:            f.MessageID,
			SessionID:            sp.SessionID,
			LastAssistantMessage: resolveAssistantMessage(sp),
			HadResult:            true,
		})
	}
	return out, nil
}

// ensureClaudeInTmux makes sure a tmux session `sess` exists with claude
// running in its first pane. Returns launched=true when we just created
// or respawned the pane (caller should wait for the TUI to render).
func ensureClaudeInTmux(sess, cwd, bin, sid, hookPath, settingsPath, launchPath string, extraEnv []string) (launched bool, err error) {
	if err := writeSettingsJSON(settingsPath, hookPath); err != nil {
		return false, err
	}
	if err := writeLaunchScript(launchPath, bin, sid, settingsPath); err != nil {
		return false, err
	}

	if !tmuxHasSession(sess) {
		if err := tmuxNewSession(sess, cwd, launchPath, extraEnv); err != nil {
			return false, err
		}
		return true, nil
	}
	if isClaudeProcess(tmuxPaneCommand(sess)) {
		return false, nil
	}
	if err := tmuxRespawnPane(sess, cwd, launchPath, extraEnv); err != nil {
		return false, err
	}
	return true, nil
}

// isClaudeProcess heuristically tells whether the pane's current command
// is the claude TUI. claude is a Node.js binary so `pane_current_command`
// usually shows "node"; fall back to a `claude*` literal in case future
// releases ship a native binary.
func isClaudeProcess(cmd string) bool {
	switch cmd {
	case "node", "claude":
		return true
	}
	return strings.HasPrefix(cmd, "claude")
}

func tmuxHasSession(sess string) bool {
	return exec.Command("tmux", "has-session", "-t", sess).Run() == nil
}

func tmuxPaneCommand(sess string) string {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", sess, "#{pane_current_command}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func tmuxNewSession(sess, cwd, launchPath string, extraEnv []string) error {
	args := []string{"new-session", "-d", "-s", sess, "-c", cwd}
	for _, e := range extraEnv {
		args = append(args, "-e", e)
	}
	args = append(args, launchPath)
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %w (%s)", err, string(out))
	}
	return nil
}

func tmuxRespawnPane(sess, cwd, launchPath string, extraEnv []string) error {
	args := []string{"respawn-pane", "-k", "-t", sess, "-c", cwd}
	for _, e := range extraEnv {
		args = append(args, "-e", e)
	}
	args = append(args, launchPath)
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux respawn-pane: %w (%s)", err, string(out))
	}
	return nil
}

// tmuxPastePrompt types `prompt` into `sess`'s active pane and submits
// it with a named Enter key.
//
// Why not bracketed paste? Claude's Ink TUI doesn't reliably opt in to
// bracketed paste mode, so tmux's `paste-buffer -p` leaks its ESC[200~
// / ESC[201~ markers into the input field as literal characters.
//
// Why two send-keys calls instead of "<text>\r" in one? Manual probes
// against the real claude TUI showed that submitting via tmux's named
// `Enter` key is more reliable than embedding a CR byte in the literal
// text — Ink's input handler interprets the named key event directly,
// where a raw CR can be missed if Ink hasn't finished rendering the
// pasted text. A small pause between the typing pass and Enter gives
// Ink one extra render frame to commit the input buffer before the
// submit fires.
//
// Embedded newlines in the prompt are replaced with spaces so Ink
// doesn't treat each `\n` as Enter and submit a partial prompt.
func tmuxPastePrompt(sess, prompt string) error {
	safe := strings.ReplaceAll(prompt, "\n", " ")
	if out, err := exec.Command("tmux", "send-keys", "-t", sess, "-l", safe).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys -l: %w (%s)", err, string(out))
	}
	time.Sleep(claudeSubmitDelay)
	if out, err := exec.Command("tmux", "send-keys", "-t", sess, "Enter").CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys Enter: %w (%s)", err, string(out))
	}
	return nil
}

// markCwdTrustedInClaude writes hasTrustDialogAccepted=true under
// projects[cwd] in ~/.claude.json so claude doesn't show its
// "Is this a project you created or one you trust?" modal when it
// opens a directory for the first time. The modal blocks all real
// input — without this, the first prompt we paste into a fresh cwd
// gets interpreted as a trust-dialog option select and lost.
//
// Read-modify-write under an exclusive file lock. claude writes to
// the same file at startup so the lock matters.
func markCwdTrustedInClaude(cwd string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude.json")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	body, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	var data map[string]interface{}
	if len(body) > 0 {
		if jerr := json.Unmarshal(body, &data); jerr != nil {
			return fmt.Errorf("parse: %w", jerr)
		}
	}
	if data == nil {
		data = map[string]interface{}{}
	}
	projects, _ := data["projects"].(map[string]interface{})
	if projects == nil {
		projects = map[string]interface{}{}
		data["projects"] = projects
	}
	project, _ := projects[cwd].(map[string]interface{})
	if project == nil {
		project = map[string]interface{}{}
		projects[cwd] = project
	}
	if v, _ := project["hasTrustDialogAccepted"].(bool); v {
		return nil // already trusted; no rewrite needed
	}
	project["hasTrustDialogAccepted"] = true

	out, jerr := json.MarshalIndent(data, "", "  ")
	if jerr != nil {
		return jerr
	}

	// Truncate + write back. Same fd that holds the lock — we don't
	// rename here because losing the lock between rename and reopen
	// would re-introduce the race claude could lose updates against.
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(out); err != nil {
		return err
	}
	return nil
}

// tmuxKillSession terminates the per-channel session. Called when a
// Discord channel is deleted. Safe when the session doesn't exist.
func tmuxKillSession(handle string) {
	_ = exec.Command("tmux", "kill-session", "-t", tmuxSessionName(handle)).Run()
}

// writeHookScript emits a sh script that claude invokes from the Stop
// hook. The script reads the hook payload (JSON) from stdin, then writes
// it to a uniquely-named file under eventsDir via tmp + rename so the
// daemon never reads a half-written file. Filename format:
// "<unix_nanos>.<event>.json" so a lexical sort gives chronological order.
//
// Requires GNU date (Linux). On macOS, install coreutils + alias `gdate`.
func writeHookScript(path, eventsDir string) error {
	q := shellSingleQuote
	body := "" +
		"#!/bin/sh\n" +
		"payload=$(cat)\n" +
		"ts=$(date +%s%N)\n" +
		"final=" + q(eventsDir) + "/${ts}.${1}.json\n" +
		"tmp=\"${final}.tmp\"\n" +
		"printf '%s' \"$payload\" > \"$tmp\"\n" +
		"mv \"$tmp\" \"$final\"\n"
	return os.WriteFile(path, []byte(body), 0700)
}

func writeSettingsJSON(path, hookPath string) error {
	s, err := buildHooksSettings(hookPath)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(s), 0600)
}

// writeLaunchScript emits a sh script that exec's claude with the right
// flags. The script is what we hand to `tmux new-session` / `respawn-pane`
// so tmux's shell-command parsing can't mangle our argv. Rewritten on
// every turn since `--resume <sid>` can change.
func writeLaunchScript(path, bin, sid, settingsPath string) error {
	args := []string{bin, "--dangerously-skip-permissions", "--settings", settingsPath}
	if sid != "" {
		args = append(args, "--resume", sid)
	}
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\nexec")
	for _, a := range args {
		sb.WriteByte(' ')
		sb.WriteString(shellSingleQuote(a))
	}
	sb.WriteByte('\n')
	return os.WriteFile(path, []byte(sb.String()), 0700)
}

// shellSingleQuote wraps s in '...' for safe sh interpolation. Embedded
// single quotes are escaped via the standard '\'' dance.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// envValue extracts the value of a "KEY=VAL" entry from a slice of env
// strings as used by os/exec.Cmd.Env.
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}
