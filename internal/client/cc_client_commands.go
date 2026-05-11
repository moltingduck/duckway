package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"strings"
)

// clientCommandPayload is what the server packs into a `client_command`
// SSE event. The server itself doesn't dispatch these — it just forwards
// them so we can act on the agent's filesystem.
type clientCommandPayload struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// handleClientCommand is the cc-watch entry point for `!sessions` /
// `!bind` (and any future filesystem-only commands). The reply is always
// posted back into the same channel via PostCC.
func (w *CCWatch) handleClientCommand(data []byte) {
	var env sseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		log.Printf("[cc-watch] bad client_command envelope: %v", err)
		return
	}
	var payload clientCommandPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		log.Printf("[cc-watch] bad client_command payload: %v", err)
		return
	}
	if env.Handle == "" {
		return
	}

	switch payload.Command {
	case "!sessions":
		w.cmdSessions(env.Handle, payload.Args)
	case "!bind":
		w.cmdBind(env.Handle, payload.Args)
	default:
		_ = w.api.PostCC(context.Background(), env.Handle,
			"❌ daemon doesn't know how to handle `"+payload.Command+"` — update your `duckway` binary on the agent.")
	}
}

// cmdSessions lists local claude sessions that aren't already bound to a
// CC channel. Optional positional arg = cwd substring filter.
func (w *CCWatch) cmdSessions(replyHandle string, args []string) {
	root, err := claudeProjectsRoot()
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ couldn't locate ~/.claude/projects: "+err.Error())
		return
	}
	bound := w.sessions.Snapshot()
	all, err := ListLocalSessions(root, bound)
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ scan failed: "+err.Error())
		return
	}

	cwdFilter := strings.TrimSpace(strings.Join(args, " "))
	var unbound []LocalSession
	for _, s := range all {
		if s.BoundTo != "" {
			continue
		}
		if cwdFilter != "" && !strings.Contains(s.Cwd, cwdFilter) {
			continue
		}
		unbound = append(unbound, s)
	}

	if len(unbound) == 0 {
		msg := "_(no unbound local claude sessions found"
		if cwdFilter != "" {
			msg += " matching `" + cwdFilter + "`"
		}
		msg += ")_"
		_ = w.api.PostCC(context.Background(), replyHandle, msg)
		return
	}

	const maxRows = 20
	rows := unbound
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}

	var b strings.Builder
	if cwdFilter != "" {
		fmt.Fprintf(&b, "**Local claude sessions (unbound, cwd matches `%s`):**\n", cwdFilter)
	} else {
		b.WriteString("**Local claude sessions (unbound):**\n")
	}
	for i, s := range rows {
		fmt.Fprintf(&b, "%d. `%s`  — `%s`  (%d turns, %s)\n",
			i+1, s.SessionID, s.Cwd, s.MessageCount, s.LastActive.Format("2006-01-02 15:04"))
		preview := s.FirstMessage
		if len(preview) > 100 {
			preview = preview[:100] + "…"
		}
		fmt.Fprintf(&b, "   > %s\n", preview)
	}
	if len(unbound) > maxRows {
		fmt.Fprintf(&b, "\n_(showing first %d of %d — use `!sessions <cwd-filter>` to narrow)_\n", maxRows, len(unbound))
	}
	b.WriteString("\nPick one or more with `!bind <session_id> [<session_id> …]` — each binding creates a new task channel.")
	_ = w.api.PostCC(context.Background(), replyHandle, b.String())
}

// cmdBind creates a task channel for each session_id and writes the
// channel_handle → session_id binding into cc-sessions.json so the
// daemon's NEXT spawn for that channel uses --resume.
//
// Naming: channel name is derived from the cwd's basename (Discord-sanitized).
// On collision the server returns 400 and we just report it back.
func (w *CCWatch) cmdBind(replyHandle string, args []string) {
	if len(args) == 0 {
		_ = w.api.PostCC(context.Background(), replyHandle,
			"❌ usage: `!bind <session_id> [<session_id> …]`  — run `!sessions` first to find ids.")
		return
	}

	root, err := claudeProjectsRoot()
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ couldn't locate ~/.claude/projects: "+err.Error())
		return
	}
	all, err := ListLocalSessions(root, w.sessions.Snapshot())
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ scan failed: "+err.Error())
		return
	}
	byID := map[string]LocalSession{}
	for _, s := range all {
		byID[s.SessionID] = s
	}

	results := BindLocalSessionsFromMap(context.Background(), w.api, w.sessions, args, byID)
	_ = w.api.PostCC(context.Background(), replyHandle, formatBindReport(results))
}

// BindResult is one line of the !bind / `duckway cc bind` summary.
type BindResult struct {
	SessionID    string
	Channel      string // dwch_ handle on success
	Name         string // discord channel name on success
	Cwd          string
	Error        string // empty on success
	AlreadyBound string // existing handle if session was already in cc-sessions.json
}

// BindLocalSessions is the public entry point used by `duckway cc bind`.
// It scans ~/.claude/projects/ itself (the daemon already has metadata
// cached, but the CLI doesn't), then runs the create-channel + write-store
// flow per session_id.
func BindLocalSessions(ctx context.Context, api *APIClient, store *CCSessionStore, sessionIDs []string) []BindResult {
	root, err := ClaudeProjectsRoot()
	if err != nil {
		// One result per id, all failing — keeps the caller's loop dumb.
		out := make([]BindResult, 0, len(sessionIDs))
		for _, sid := range sessionIDs {
			out = append(out, BindResult{SessionID: sid, Error: err.Error()})
		}
		return out
	}
	all, err := ListLocalSessions(root, store.Snapshot())
	if err != nil {
		out := make([]BindResult, 0, len(sessionIDs))
		for _, sid := range sessionIDs {
			out = append(out, BindResult{SessionID: sid, Error: err.Error()})
		}
		return out
	}
	byID := map[string]LocalSession{}
	for _, s := range all {
		byID[s.SessionID] = s
	}
	return BindLocalSessionsFromMap(ctx, api, store, sessionIDs, byID)
}

// BindLocalSessionsFromMap is the same as BindLocalSessions but accepts a
// pre-built lookup so the daemon can reuse its scan.
func BindLocalSessionsFromMap(ctx context.Context, api *APIClient, store *CCSessionStore, sessionIDs []string, byID map[string]LocalSession) []BindResult {
	out := make([]BindResult, 0, len(sessionIDs))
	reverse := map[string]string{}
	for h, s := range store.Snapshot() {
		reverse[s] = h
	}
	for _, sid := range sessionIDs {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			continue
		}
		r := BindResult{SessionID: sid}
		sess, ok := byID[sid]
		if !ok {
			r.Error = "session_id not found under ~/.claude/projects (run `!sessions` first)"
			out = append(out, r)
			continue
		}
		r.Cwd = sess.Cwd
		if existing := reverse[sid]; existing != "" {
			r.AlreadyBound = existing
			out = append(out, r)
			continue
		}

		name := discordChannelNameFromCwd(sess.Cwd)
		created, err := api.CreateCCChannel(ctx, name, "", sess.Cwd)
		if err != nil {
			r.Error = "create channel: " + err.Error()
			out = append(out, r)
			continue
		}
		if err := store.Set(created.Handle, sid); err != nil {
			r.Error = "channel created (" + created.Handle + ") but writing cc-sessions.json failed: " + err.Error()
			out = append(out, r)
			continue
		}
		r.Channel = created.Handle
		r.Name = created.Name
		out = append(out, r)
	}
	return out
}

var nonDiscordName = regexp.MustCompile(`[^a-z0-9-]+`)

// discordChannelNameFromCwd produces a Discord-legal channel name from a
// filesystem cwd. Discord lowercases and substitutes dashes anyway, but
// doing it locally lets the name match what the server posts back.
func discordChannelNameFromCwd(cwd string) string {
	base := filepath.Base(strings.TrimRight(cwd, "/"))
	if base == "" || base == "." || base == "/" {
		base = "session"
	}
	base = strings.ToLower(base)
	base = nonDiscordName.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "session"
	}
	if len(base) > 90 {
		base = base[:90]
	}
	return base
}

// formatBindReport collapses per-session results into a single Discord
// message. Successes first, then "already bound" notes, then errors.
func formatBindReport(rs []BindResult) string {
	var ok, dup, fail []BindResult
	for _, r := range rs {
		switch {
		case r.Error != "":
			fail = append(fail, r)
		case r.AlreadyBound != "":
			dup = append(dup, r)
		default:
			ok = append(ok, r)
		}
	}
	if len(rs) == 0 {
		return "❌ nothing to bind."
	}
	var b strings.Builder
	if len(ok) > 0 {
		b.WriteString("✅ **Bound:**\n")
		for _, r := range ok {
			fmt.Fprintf(&b, "• `%s` → **#%s** (`%s`)  cwd: `%s`\n", r.SessionID, r.Name, r.Channel, r.Cwd)
		}
		b.WriteString("Send a message in the new channel — claude will resume with the existing history.\n")
	}
	if len(dup) > 0 {
		b.WriteString("\nℹ️ **Already bound:**\n")
		for _, r := range dup {
			fmt.Fprintf(&b, "• `%s` → `%s` (use that channel directly)\n", r.SessionID, r.AlreadyBound)
		}
	}
	if len(fail) > 0 {
		b.WriteString("\n❌ **Failed:**\n")
		for _, r := range fail {
			fmt.Fprintf(&b, "• `%s` — %s\n", r.SessionID, r.Error)
		}
	}
	return b.String()
}
