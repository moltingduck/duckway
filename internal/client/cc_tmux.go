package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	// Enter. Without it the submit can race the TUI input handler.
	claudeSubmitDelay = 250 * time.Millisecond
	// eventPollInterval is how often runViaTmux re-scans the events
	// directory for new Stop events. The hook fires at most once per turn
	// so this isn't on a hot path — we trade a bit of latency for cheap
	// CPU.
	eventPollInterval = 100 * time.Millisecond
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

	sid2, res2, isErr2, perr := pollForStop(ctx, eventsDir, turnTS, tmuxRunTimeout)
	if perr != nil {
		return "", "", false, perr
	}
	// Successful Stop — clear the in-flight marker so daemon restart
	// doesn't try to re-post the result.
	_ = os.Remove(inFlightPath)
	return sid2, res2, isErr2, nil
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

// tmuxPastePrompt types `prompt` into `sess`'s active pane as raw
// keystrokes, then sends Enter to submit.
//
// Why not bracketed paste? Claude's Ink TUI doesn't reliably opt in to
// bracketed paste mode, so tmux's `paste-buffer -p` leaks its ESC[200~
// / ESC[201~ markers into the input field as literal characters. We
// also can't send newlines as part of the typed text — Ink would treat
// each `\n` as Enter and submit a partial prompt; replace them with
// spaces (matching what runViaPTY already does in buildPTYInput).
//
// A short pause sits between the typing pass and Enter so the TUI input
// handler sees the text before the submit fires.
func tmuxPastePrompt(sess, prompt string) error {
	safe := strings.ReplaceAll(prompt, "\n", " ")
	// `send-keys -l` sends the argument literally — no special-key
	// interpretation of e.g. "Enter".
	if out, err := exec.Command("tmux", "send-keys", "-t", sess, "-l", safe).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys -l: %w (%s)", err, string(out))
	}
	time.Sleep(claudeSubmitDelay)
	if out, err := exec.Command("tmux", "send-keys", "-t", sess, "Enter").CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys Enter: %w (%s)", err, string(out))
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
