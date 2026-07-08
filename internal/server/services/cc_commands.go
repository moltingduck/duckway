package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

// CCCommandHandler processes !-prefix commands posted in a CC's
// management or task channel. Runs server-side from the gateway thread.
//
// Supported commands (v1):
//
//	!new <slug> [--cwd <path>|--project <ref>] [--topic <text>]   in management channel
//	!end                                          in a task channel
//	!list                                         management
//	!status                                       management
//	!help                                         either
//
// Replies are posted as bot messages in the same channel.
type CCCommandHandler struct {
	cc      *queries.ControlChannelQueries
	apiKeys *queries.APIKeyQueries
	crypto  *Crypto
	bot     *DiscordBot
	hub     *CCEventHub
}

func NewCCCommandHandler(cc *queries.ControlChannelQueries, apiKeys *queries.APIKeyQueries, crypto *Crypto, bot *DiscordBot, hub *CCEventHub) *CCCommandHandler {
	return &CCCommandHandler{cc: cc, apiKeys: apiKeys, crypto: crypto, bot: bot, hub: hub}
}

// LooksLikeCommand returns true for messages we should hand to Handle.
// Used by the gateway to decide whether to also publish the message to
// the daemon's SSE stream.
//
// Exceptions, both escapes for claude TUI modes whose trigger character
// would otherwise be eaten by Discord or by us:
//
//	"!/..."  → claude slash command (`/usage`, `/help`, `/compact`, ...)
//	"!!..."  → claude bash shell    (`! ls`, `! cargo test`, ...)
//
// The daemon strips one leading `!` before pasting into claude, so the
// user types `!/usage` to send `/usage` and `!! ls` to send `! ls`.
func LooksLikeCommand(content string) bool {
	t := strings.TrimSpace(content)
	if strings.HasPrefix(t, "!/") || strings.HasPrefix(t, "!!") {
		return false
	}
	return strings.HasPrefix(t, "!")
}

// Handle dispatches the command. Reply is best-effort — Discord errors
// are logged but don't propagate. ctx is the gateway's ws read context;
// callers should not block on Handle (the bot REST calls inside take
// seconds in worst-case rate-limited scenarios).
func (h *CCCommandHandler) Handle(ctx context.Context, ccID string, ch *models.CCChannel, content string) {
	cc, err := h.cc.GetByID(ccID)
	if err != nil {
		return
	}
	botToken, err := h.decryptBotToken(cc.APIKeyID)
	if err != nil {
		return
	}

	args := parseArgs(content)
	if len(args) == 0 {
		return
	}
	cmd := args[0]

	switch cmd {
	case "!help":
		_, _ = h.bot.PostMessage(ctx, botToken, ch.ChannelID, helpText)

	case "!new":
		if ch.Kind != "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `!new` only works in the management channel.")
			return
		}
		h.handleNew(ctx, botToken, cc, ch, args[1:])

	case "!new-confirm":
		if ch.Kind != "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `!new-confirm` only works in the management channel.")
			return
		}
		h.forwardToDaemon(ctx, botToken, cc, ch, cmd, args[1:])

	case "!end":
		if ch.Kind == "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `!end` ends the *current* channel's session — run it inside a task channel.")
			return
		}
		h.handleEnd(ctx, botToken, cc, ch)

	case "!destroy":
		if ch.Kind == "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `!destroy` deletes the *current* task channel — run it inside one. (The management channel can't be destroyed; delete the CC from /admin/cc instead.)")
			return
		}
		h.handleDestroy(ctx, botToken, cc, ch)

	case "!reset":
		if ch.Kind == "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `!reset` clears the *current* channel's session — run it inside a task channel.")
			return
		}
		h.handleReset(ctx, botToken, cc, ch)

	case "!list":
		if ch.Kind != "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `!list` only works in the management channel.")
			return
		}
		h.handleList(ctx, botToken, cc, ch.ChannelID)

	case "!status":
		if ch.Kind != "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `!status` only works in the management channel.")
			return
		}
		h.handleStatus(ctx, botToken, cc, ch.ChannelID)

	case "!sessions", "!bind", "!projects":
		// These are client-handled — the agent machine owns the filesystem
		// state (~/.claude/projects/, saved project dirs) and local stores, so
		// the daemon is the only place that can do useful work. We just
		// forward the raw command via SSE and let the daemon reply.
		if ch.Kind != "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `"+cmd+"` only works in the management channel.")
			return
		}
		h.forwardToDaemon(ctx, botToken, cc, ch, cmd, args[1:])

	case "!log":
		if ch.Kind == "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `!log` shows the current task channel's recent agent conversation — run it inside a task channel.")
			return
		}
		h.forwardToDaemon(ctx, botToken, cc, ch, cmd, args[1:])

	default:
		h.reply(ctx, botToken, ch.ChannelID, unknownCommandReply(cmd))
	}
}

// knownCommands is the canonical list used for `!help` discovery + the
// fuzzy "did you mean" suggestion. Order is the user-facing display
// order in !help.
var knownCommands = []string{"!help", "!new", "!new-confirm", "!end", "!destroy", "!reset", "!list", "!status", "!sessions", "!bind", "!projects", "!log"}

// unknownCommandReply formats the friendly response for an unrecognised
// !-prefix command. Suggests close matches (Levenshtein distance ≤ 2)
// instead of just saying "type !help".
func unknownCommandReply(typed string) string {
	suggestions := suggestCommands(typed, 2)
	base := "❓ Unknown command `" + typed + "`."
	if len(suggestions) == 0 {
		return base + " Type `!help` to see the full list."
	}
	if len(suggestions) == 1 {
		return base + " Did you mean `" + suggestions[0] + "`?"
	}
	return base + " Did you mean " + joinTicked(suggestions) + "?"
}

// suggestCommands returns the known commands within `maxDist` Levenshtein
// edits of `typed`, ordered by distance ascending then alphabetically.
// Empty when nothing is close enough.
func suggestCommands(typed string, maxDist int) []string {
	type scored struct {
		cmd  string
		dist int
	}
	var hits []scored
	for _, c := range knownCommands {
		d := levenshtein(typed, c)
		if d <= maxDist {
			hits = append(hits, scored{c, d})
		}
	}
	// Stable sort by distance, then by command name.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0; j-- {
			a, b := hits[j-1], hits[j]
			if a.dist > b.dist || (a.dist == b.dist && a.cmd > b.cmd) {
				hits[j-1], hits[j] = hits[j], hits[j-1]
			} else {
				break
			}
		}
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.cmd)
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// levenshtein returns the edit distance between a and b. Standard
// DP table, byte-level (good enough for ASCII command names).
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func joinTicked(xs []string) string {
	var b strings.Builder
	for i, x := range xs {
		if i > 0 {
			if i == len(xs)-1 {
				b.WriteString(" or ")
			} else {
				b.WriteString(", ")
			}
		}
		b.WriteByte('`')
		b.WriteString(x)
		b.WriteByte('`')
	}
	return b.String()
}

func (h *CCCommandHandler) handleNew(ctx context.Context, botToken string, cc *models.ControlChannel, mgmt *models.CCChannel, args []string) {
	slug, flags, err := splitSlugAndFlags(args)
	if err != nil {
		h.reply(ctx, botToken, mgmt.ChannelID, "❌ "+err.Error()+"\nUsage: `!new <slug> [--cwd <path>|--project <name|number>] [--topic <text>]`")
		return
	}
	if flags["project"] != "" || flags["cwd"] != "" {
		h.forwardToDaemon(ctx, botToken, cc, mgmt, "!new", args)
		return
	}

	var cfg struct {
		GuildID    string `json:"guild_id"`
		CategoryID string `json:"category_id"`
	}
	_ = json.Unmarshal([]byte(cc.Config), &cfg)
	if cfg.GuildID == "" || cfg.CategoryID == "" {
		h.reply(ctx, botToken, mgmt.ChannelID, "❌ CC config missing guild_id/category_id — fix in /admin/cc.")
		return
	}

	created, err := h.bot.CreateChannel(ctx, botToken, CreateChannelOpts{
		GuildID:  cfg.GuildID,
		ParentID: cfg.CategoryID,
		Name:     slug,
		Topic:    flags["topic"],
	})
	if err != nil {
		h.reply(ctx, botToken, mgmt.ChannelID, "❌ Discord refused to create channel: "+err.Error())
		return
	}

	handle, _ := GenerateToken(12)
	handle = "dwch_" + handle
	clientID := cc.ClientID
	row := &models.CCChannel{
		Handle: handle, CCID: cc.ID,
		ClientID:  &clientID,
		ChannelID: created.ID,
		Name:      created.Name,
		Topic:     flags["topic"],
		Kind:      "task",
		Cwd:       flags["cwd"],
	}
	if err := h.cc.CreateChannel(row); err != nil {
		_ = h.bot.ArchiveChannel(ctx, botToken, created.ID, created.Name)
		h.reply(ctx, botToken, mgmt.ChannelID, "❌ persist channel row: "+err.Error())
		return
	}

	cwdNote := flags["cwd"]
	if cwdNote == "" {
		cwdNote = "(default ~/.duckway/cc-workspace/" + handle + ")"
	}
	h.reply(ctx, botToken, mgmt.ChannelID,
		"✅ Created **#"+created.Name+"** — `"+handle+"`\n"+
			"   cwd: `"+cwdNote+"`\n"+
			"   Send a message in that channel to start a claude session.")
}

// handleReset wipes the channel's session_id without archiving the channel.
// The next message starts a fresh claude session in the same cwd. Useful
// when the agent wedges itself or you want a clean context but want to
// keep the channel + history.
func (h *CCCommandHandler) handleReset(ctx context.Context, botToken string, cc *models.ControlChannel, ch *models.CCChannel) {
	prev := ch.SessionID
	if err := h.cc.SetChannelSession(ch.Handle, "", ch.Cwd); err != nil {
		h.reply(ctx, botToken, ch.ChannelID, "❌ reset failed: "+err.Error())
		return
	}
	if h.hub != nil && cc.ClientID != "" {
		// Daemons watching this channel should drop their cached
		// session_id too. Reuse the channel_delete event shape — the
		// daemon's runner will drop the session map entry. The Discord
		// channel itself is NOT deleted; only the daemon's local
		// session_id binding is.
		h.hub.Publish(cc.ClientID, CCEvent{
			Type:   "session_reset",
			CCID:   cc.ID,
			Handle: ch.Handle,
		})
	}
	if prev == "" {
		h.reply(ctx, botToken, ch.ChannelID, "♻️ No session was active. Next message starts a fresh one.")
	} else {
		h.reply(ctx, botToken, ch.ChannelID, "♻️ Session `"+short(prev, 8)+"` cleared. Next message starts fresh.")
	}
}

// handleDestroy is the heavier sibling of !end. Both close the session,
// but !end *archives* the Discord channel (rename + remove from category,
// history preserved); !destroy hard-deletes via DELETE /channels/{id}
// (history gone, channel id reused-after-delete is fine because we drop
// our cache row too). Use !end when you might want to look back; use
// !destroy when the channel was a one-shot experiment.
func (h *CCCommandHandler) handleDestroy(ctx context.Context, botToken string, cc *models.ControlChannel, ch *models.CCChannel) {
	// No farewell post — the channel is about to vanish so a message
	// would only appear for a millisecond. Discord will broadcast a
	// CHANNEL_DELETE event when the delete succeeds, which the gateway
	// also handles (drops the cc_channels row + fires channel_delete
	// to the daemon). Belt-and-braces: do the local cleanup here too in
	// case the Discord call fails or we don't hear the event back fast
	// enough.
	if err := h.bot.DeleteChannel(ctx, botToken, ch.ChannelID); err != nil {
		// Channel might already be gone (someone deleted in Discord UI
		// before us) — still clean local state and let the user know.
		_ = h.cc.DeleteChannel(ch.Handle)
		if h.hub != nil && cc.ClientID != "" {
			h.hub.Publish(cc.ClientID, CCEvent{Type: "channel_delete", CCID: cc.ID, Handle: ch.Handle})
		}
		// Can't reply to a destroyed channel; if the destroy itself failed,
		// the channel still exists so we CAN reply with the error. Try.
		h.reply(ctx, botToken, ch.ChannelID, "⚠️ discord delete failed: "+err.Error()+" (local state cleared regardless)")
		return
	}
	_ = h.cc.DeleteChannel(ch.Handle)
	if h.hub != nil && cc.ClientID != "" {
		h.hub.Publish(cc.ClientID, CCEvent{Type: "channel_delete", CCID: cc.ID, Handle: ch.Handle})
	}
}

func (h *CCCommandHandler) handleEnd(ctx context.Context, botToken string, cc *models.ControlChannel, ch *models.CCChannel) {
	// Post farewell, then archive Discord channel + delete cache row.
	_, _ = h.bot.PostMessage(ctx, botToken, ch.ChannelID,
		"🔚 Session ended by `!end`. Archiving this channel.")
	// Best-effort — local state (DeleteChannel below) is the source of truth.
	_ = h.bot.ArchiveChannel(ctx, botToken, ch.ChannelID, ch.Name)
	_ = h.cc.DeleteChannel(ch.Handle)
	if h.hub != nil && cc.ClientID != "" {
		h.hub.Publish(cc.ClientID, CCEvent{
			Type: "channel_delete", CCID: cc.ID, Handle: ch.Handle,
		})
	}
}

func (h *CCCommandHandler) handleList(ctx context.Context, botToken string, cc *models.ControlChannel, replyChannelID string) {
	chans, err := h.cc.ListChannels(cc.ID)
	if err != nil {
		h.reply(ctx, botToken, replyChannelID, "❌ list failed: "+err.Error())
		return
	}
	type row struct {
		name, handle, session, cwd string
		archived                   bool
	}
	var tasks []row
	for _, c := range chans {
		if c.Kind != "task" {
			continue
		}
		tasks = append(tasks, row{
			name: c.Name, handle: c.Handle, session: c.SessionID, cwd: c.Cwd, archived: c.Archived,
		})
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].name < tasks[j].name })

	if len(tasks) == 0 {
		h.reply(ctx, botToken, replyChannelID, "_(no task channels)_  Type `!new <slug>` to create one.")
		return
	}
	var b strings.Builder
	b.WriteString("**Task channels:**\n")
	for _, t := range tasks {
		status := "○"
		if t.session != "" {
			status = "●"
		}
		if t.archived {
			status = "✗"
		}
		fmt.Fprintf(&b, "%s `%s`  *(handle: `%s`)*", status, t.name, t.handle)
		if t.cwd != "" {
			fmt.Fprintf(&b, "  cwd: `%s`", t.cwd)
		}
		if t.session != "" {
			fmt.Fprintf(&b, "  session: `%s`", short(t.session, 8))
		}
		b.WriteString("\n")
	}
	b.WriteString("_● = active session  ○ = no session yet  ✗ = archived_")
	h.reply(ctx, botToken, replyChannelID, b.String())
}

func (h *CCCommandHandler) handleStatus(ctx context.Context, botToken string, cc *models.ControlChannel, replyChannelID string) {
	chans, _ := h.cc.ListChannels(cc.ID)
	taskCount := 0
	activeSessions := 0
	for _, c := range chans {
		if c.Kind == "task" && !c.Archived {
			taskCount++
			if c.SessionID != "" {
				activeSessions++
			}
		}
	}

	daemonStatus := "❌ offline"
	if h.hub != nil && h.hub.SubscriberCount(cc.ClientID) > 0 {
		daemonStatus = "✅ connected"
	}

	msg := fmt.Sprintf(
		"**CC:** `%s`\n**Agent:** `%s`\n**Daemon (`duckway cc watch`):** %s\n**Task channels:** %d (%d active sessions)",
		cc.Name, cc.AgentType, daemonStatus, taskCount, activeSessions,
	)
	h.reply(ctx, botToken, replyChannelID, msg)
}

// forwardToDaemon hands a !-prefix command off to the cc-watch daemon via
// SSE. Used for commands that need filesystem access on the agent box
// (!sessions, !bind). If no daemon is connected we fall back to a polite
// error in the channel so the user knows to start one.
func (h *CCCommandHandler) forwardToDaemon(ctx context.Context, botToken string, cc *models.ControlChannel, ch *models.CCChannel, cmd string, args []string) {
	if h.hub == nil || cc.ClientID == "" {
		h.reply(ctx, botToken, ch.ChannelID, "❌ `"+cmd+"` needs the cc-watch daemon on the agent — start one with `duckway cc watch -d`.")
		return
	}
	if h.hub.SubscriberCount(cc.ClientID) == 0 {
		h.reply(ctx, botToken, ch.ChannelID, "❌ daemon offline — start `duckway cc watch -d` on the agent box, then retry.")
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"command": cmd,
		"args":    args,
	})
	h.hub.Publish(cc.ClientID, CCEvent{
		Type:    "client_command",
		CCID:    cc.ID,
		Handle:  ch.Handle,
		Kind:    ch.Kind,
		Payload: payload,
	})
}

func (h *CCCommandHandler) reply(ctx context.Context, botToken, channelID, content string) {
	_, _ = h.bot.PostMessage(ctx, botToken, channelID, content)
}

func (h *CCCommandHandler) decryptBotToken(apiKeyID string) (string, error) {
	key, err := h.apiKeys.GetByID(apiKeyID)
	if err != nil {
		return "", err
	}
	return h.crypto.Decrypt(key.KeyEncrypted)
}

const helpText = "**Duckway CC commands**\n" +
	"`!new <slug> [--cwd <path>|--project <name|number>] [--topic <text>]` — create a task channel\n" +
	"`!new-confirm <token>` — confirm creating a missing `--cwd` folder and saving it as a project\n" +
	"`!end` — end the *current* task channel's session and **archive** it (history kept)\n" +
	"`!destroy` — end and **hard-delete** the *current* task channel (history gone)\n" +
	"`!reset` — wipe the *current* task channel's session_id; next message starts fresh\n" +
	"`!list` — list active task channels\n" +
	"`!status` — daemon + session counts\n" +
	"`!sessions [<cwd-filter>]` — list local claude sessions on the agent that aren't yet bound to a CC channel\n" +
	"`!bind <session_id> [<session_id> …]` — create a task channel for each session_id and attach it (run `!sessions` first to find IDs)\n" +
	"`!projects [<filter>]` — list saved project folders from the agent machine\n" +
	"`!log` — show the current task channel's latest 3 agent conversation entries\n" +
	"`!help` — this message\n" +
	"\n" +
	"**Sending claude slash & shell commands**\n" +
	"Discord/the daemon eat the `/` and `!` trigger chars, so prefix them with an extra `!`:\n" +
	"  • `!/usage`, `!/compact`, `!/help` → claude slash command\n" +
	"  • `!! ls`, `!! cargo test` → claude bash shell (the `!` mode)\n" +
	"The daemon strips one `!` before pasting into claude and snapshots the panel/output back to the channel.\n" +
	"\n" +
	"**Picker commands** (`!/release-notes`, `!/effort`, `!/model`, `!/agents`, …)\n" +
	"When the slash command opens a numbered picker, the daemon posts the options here and leaves the picker open. Reply with the **option number** (e.g. `2`) to pick it, or `cancel` to dismiss the picker without choosing."

// BuildWelcomeMessage returns the message the server posts in a freshly
// provisioned management channel. Different from helpText (the !help
// response): the welcome explains the model + what the user does next,
// not just the command list.
func BuildWelcomeMessage(clientName string) string {
	return "**👋 Duckway Control Channel for `" + clientName + "`**\n" +
		"\n" +
		"**Where to type what**\n" +
		"• **This channel** is for control: `!`-prefix commands manage the agent.\n" +
		"• **Task channels** (created with `!new`) are for the actual conversation — every message there goes to a claude session and the response comes back here.\n" +
		"_(Plain text here also gets forwarded to claude — but task channels keep each conversation focused on one topic, which works better.)_\n" +
		"\n" +
		"**Start a task**\n" +
		"`!new fix-login`                          — opens `#fix-login` with default cwd\n" +
		"`!new deploy --cwd /home/me/myapp`        — opens `#deploy` rooted at that project\n" +
		"`!new deploy --cwd /home/me/newapp`       — asks before creating a missing folder\n" +
		"`!new deploy --project duckway`           — opens `#deploy` rooted at a saved project\n" +
		"`!new analyze --topic \"Q2 metrics\"`        — channel topic appears in Discord's UI\n" +
		"\n" +
		"**Inside a task channel**\n" +
		"`!reset`   — clear the session id, next message starts a fresh claude\n" +
		"`!end`     — close session and **archive** the channel (history kept)\n" +
		"`!destroy` — close session and **hard-delete** the channel (history gone)\n" +
		"\n" +
		"**Here in this channel**\n" +
		"`!list`     — active task channels + which have running sessions\n" +
		"`!status`   — daemon connection + session counts\n" +
		"`!sessions` — list local claude sessions on the agent that aren't bound yet\n" +
		"`!bind <session_id>` — create a task channel and resume that session (one channel per id, repeat for more)\n" +
		"`!help`     — full command list\n" +
		"\n" +
		"_Make sure the daemon is running on the agent machine: `duckway cc watch -d`._"
}

// parseArgs is a minimal shell-like splitter: whitespace separates tokens,
// double-quotes group multi-word values. No backslash escapes — Discord
// content is short and admin-typed.
func parseArgs(s string) []string {
	s = strings.TrimSpace(s)
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t' || r == '\n') && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// splitSlugAndFlags pulls the first positional and any --key value pairs
// out of an args list. Returns (slug, flags, err).
func splitSlugAndFlags(args []string) (string, map[string]string, error) {
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
			return "", nil, fmt.Errorf("unexpected positional arg %q (already have slug %q)", a, slug)
		}
	}
	if slug == "" {
		return "", nil, fmt.Errorf("missing <slug>")
	}
	return slug, flags, nil
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
