package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// clientCommandPayload is what the server packs into a `client_command`
// SSE event. The server itself doesn't dispatch these — it just forwards
// them so we can act on the agent's filesystem.
type clientCommandPayload struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type pendingNewProject struct {
	Slug      string
	Topic     string
	Cwd       string
	CreatedAt time.Time
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
	case "!projects":
		w.cmdProjects(env.Handle, payload.Args)
	case "!new":
		w.cmdNewProject(env.Handle, payload.Args)
	case "!new-confirm":
		w.cmdNewProjectConfirm(env.Handle, payload.Args)
	case "!log":
		w.cmdLog(env.Handle, payload.Args)
	default:
		_ = w.api.PostCC(context.Background(), env.Handle,
			"❌ daemon doesn't know how to handle `"+payload.Command+"` — update your `duckway` binary on the agent.")
	}
}

func (w *CCWatch) cmdLog(replyHandle string, args []string) {
	n := 3
	if len(args) > 0 {
		joined := strings.TrimSpace(strings.Join(args, " "))
		switch joined {
		case "":
		case "last 3":
			n = 3
		default:
			_ = w.api.PostCC(context.Background(), replyHandle, "❌ usage: `!log`")
			return
		}
	}
	w.mu.Lock()
	runner := w.runners[replyHandle]
	w.mu.Unlock()
	if runner == nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "_(no agent runner has started for this channel yet)_")
		return
	}
	_ = w.api.PostCC(context.Background(), replyHandle, runner.formatRecentHistory(n))
}

func (w *CCWatch) cmdProjects(replyHandle string, args []string) {
	filter := strings.TrimSpace(strings.Join(args, " "))
	projects, err := NewCCProjectStore(w.configDir).List()
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ read projects failed: "+err.Error())
		return
	}
	if filter != "" {
		var filtered []CCProject
		for _, p := range projects {
			if strings.Contains(p.Name, filter) || strings.Contains(p.Path, filter) {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}
	_ = w.api.PostCC(context.Background(), replyHandle, formatProjectsReport(projects, filter))
}

func (w *CCWatch) cmdNewProject(replyHandle string, args []string) {
	slug, flags, err := splitClientSlugAndFlags(args)
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ "+err.Error()+"\nUsage: `!new <slug> --project <name|number> [--topic <text>]` or `!new <slug> --cwd <path> [--topic <text>]`")
		return
	}
	projectRef := strings.TrimSpace(flags["project"])
	cwdRef := strings.TrimSpace(flags["cwd"])
	if projectRef != "" && cwdRef != "" {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ choose either `--project` or `--cwd`, not both.")
		return
	}
	if projectRef == "" && cwdRef == "" {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ daemon only handles `!new` when `--project <name|number>` or `--cwd <path>` is set.")
		return
	}

	if cwdRef != "" {
		w.cmdNewWithCwd(replyHandle, slug, flags["topic"], cwdRef)
		return
	}

	project, err := NewCCProjectStore(w.configDir).Resolve(projectRef)
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ "+err.Error()+" — run `!projects` to see saved projects.")
		return
	}
	created, err := w.createProjectChannel(slug, flags["topic"], project.Path)
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ create channel: "+err.Error())
		return
	}
	_ = w.api.PostCC(context.Background(), replyHandle,
		"✅ Created **#"+created.Name+"** — `"+created.Handle+"`\n"+
			"   project: `"+project.Name+"`\n"+
			"   cwd: `"+project.Path+"`\n"+
			"   Send a message in that channel to start the agent.")
}

func (w *CCWatch) cmdNewWithCwd(replyHandle, slug, topic, cwdRef string) {
	cwd, err := normalizeProjectPattern(cwdRef)
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ invalid cwd: "+err.Error())
		return
	}
	info, err := os.Stat(cwd)
	if err == nil {
		if !info.IsDir() {
			_ = w.api.PostCC(context.Background(), replyHandle, "❌ cwd exists but is not a directory: `"+cwd+"`")
			return
		}
		created, err := w.createProjectChannel(slug, topic, cwd)
		if err != nil {
			_ = w.api.PostCC(context.Background(), replyHandle, "❌ create channel: "+err.Error())
			return
		}
		_ = w.api.PostCC(context.Background(), replyHandle,
			"✅ Created **#"+created.Name+"** — `"+created.Handle+"`\n"+
				"   cwd: `"+cwd+"`\n"+
				"   Send a message in that channel to start the agent.")
		return
	}
	if !os.IsNotExist(err) {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ inspect cwd failed: "+err.Error())
		return
	}

	token := randomConfirmToken()
	w.mu.Lock()
	if w.pendingNew == nil {
		w.pendingNew = map[string]pendingNewProject{}
	}
	w.prunePendingNewLocked(time.Now())
	w.pendingNew[token] = pendingNewProject{Slug: slug, Topic: topic, Cwd: cwd, CreatedAt: time.Now()}
	w.mu.Unlock()
	_ = w.api.PostCC(context.Background(), replyHandle,
		"⚠️ Project folder does not exist:\n`"+cwd+"`\n\n"+
			"Create it, add it to saved projects, and open the task channel?\n"+
			"Reply with `!new-confirm "+token+"` within 30 minutes.")
}

func (w *CCWatch) cmdNewProjectConfirm(replyHandle string, args []string) {
	if len(args) != 1 {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ usage: `!new-confirm <token>`")
		return
	}
	token := strings.TrimSpace(args[0])
	w.mu.Lock()
	if w.pendingNew == nil {
		w.pendingNew = map[string]pendingNewProject{}
	}
	w.prunePendingNewLocked(time.Now())
	pending, ok := w.pendingNew[token]
	if ok {
		delete(w.pendingNew, token)
	}
	w.mu.Unlock()
	if !ok {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ no pending `!new` request for that token. Run `!new ... --cwd ...` again.")
		return
	}
	if err := os.MkdirAll(pending.Cwd, 0700); err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ create folder failed: "+err.Error())
		return
	}
	added, err := NewCCProjectStore(w.configDir).Add([]string{pending.Cwd}, "")
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ add project failed: "+err.Error())
		return
	}
	projectName := filepath.Base(pending.Cwd)
	if len(added) > 0 {
		projectName = added[0].Name
	}
	created, err := w.createProjectChannel(pending.Slug, pending.Topic, pending.Cwd)
	if err != nil {
		_ = w.api.PostCC(context.Background(), replyHandle, "❌ create channel: "+err.Error())
		return
	}
	_ = w.api.PostCC(context.Background(), replyHandle,
		"✅ Created folder, saved project **"+projectName+"**, and opened **#"+created.Name+"** — `"+created.Handle+"`\n"+
			"   cwd: `"+pending.Cwd+"`\n"+
			"   Send a message in that channel to start the agent.")
}

func (w *CCWatch) createProjectChannel(slug, topic, cwd string) (*CreateCCChannelResult, error) {
	return w.api.CreateCCChannel(context.Background(), slug, topic, cwd)
}

func (w *CCWatch) prunePendingNewLocked(now time.Time) {
	for token, p := range w.pendingNew {
		if now.Sub(p.CreatedAt) > 30*time.Minute {
			delete(w.pendingNew, token)
		}
	}
}

func randomConfirmToken() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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

func formatProjectsReport(projects []CCProject, filter string) string {
	if len(projects) == 0 {
		if filter != "" {
			return "_(no saved projects matching `" + filter + "`)_"
		}
		return "No saved projects yet.\n\nAdd projects on the agent machine:\n`duckway projects add ~/duckway`\n`duckway projects add ~/projects/*`"
	}
	const maxRows = 30
	rows := projects
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}
	var b strings.Builder
	if filter != "" {
		fmt.Fprintf(&b, "**Projects matching `%s`:**\n", filter)
	} else {
		b.WriteString("**Saved projects:**\n")
	}
	for i, p := range rows {
		fmt.Fprintf(&b, "%d. `%s`  — `%s`\n", i+1, p.Name, p.Path)
	}
	if len(projects) > maxRows {
		fmt.Fprintf(&b, "\n_(showing first %d of %d — use `!projects <filter>` to narrow)_\n", maxRows, len(projects))
	}
	b.WriteString("\nUse `!new <slug> --project <name|number>`.")
	return b.String()
}

func splitClientSlugAndFlags(args []string) (string, map[string]string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("missing <slug>")
	}
	slug := ""
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			key := strings.TrimPrefix(a, "--")
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("flag --%s needs a value", key)
			}
			flags[key] = args[i+1]
			i++
			continue
		}
		if slug == "" {
			slug = a
		} else {
			return "", nil, fmt.Errorf("unexpected positional arg %q", a)
		}
	}
	if slug == "" {
		return "", nil, fmt.Errorf("missing <slug>")
	}
	return slug, flags, nil
}
