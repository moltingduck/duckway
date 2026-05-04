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
//	!new <slug> [--cwd <path>] [--topic <text>]   in management channel
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
func LooksLikeCommand(content string) bool {
	t := strings.TrimSpace(content)
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

	case "!end":
		if ch.Kind == "management" {
			h.reply(ctx, botToken, ch.ChannelID, "❌ `!end` ends the *current* channel's session — run it inside a task channel.")
			return
		}
		h.handleEnd(ctx, botToken, cc, ch)

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

	default:
		h.reply(ctx, botToken, ch.ChannelID, "❓ Unknown command `"+cmd+"`. Type `!help`.")
	}
}

func (h *CCCommandHandler) handleNew(ctx context.Context, botToken string, cc *models.ControlChannel, mgmt *models.CCChannel, args []string) {
	slug, flags, err := splitSlugAndFlags(args)
	if err != nil {
		h.reply(ctx, botToken, mgmt.ChannelID, "❌ "+err.Error()+"\nUsage: `!new <slug> [--cwd <path>] [--topic <text>]`")
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

func (h *CCCommandHandler) handleEnd(ctx context.Context, botToken string, cc *models.ControlChannel, ch *models.CCChannel) {
	// Post farewell, then archive Discord channel + delete cache row.
	_, _ = h.bot.PostMessage(ctx, botToken, ch.ChannelID,
		"🔚 Session ended by `!end`. Archiving this channel.")
	if err := h.bot.ArchiveChannel(ctx, botToken, ch.ChannelID, ch.Name); err != nil {
		// Still drop the row — local state is the source of truth for the daemon.
	}
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
	"`!new <slug> [--cwd <path>] [--topic <text>]` — create a task channel\n" +
	"`!end` — end the *current* task channel's session and archive it\n" +
	"`!list` — list active task channels\n" +
	"`!status` — daemon + session counts\n" +
	"`!help` — this message"

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
