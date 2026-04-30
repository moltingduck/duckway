package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DiscordBot is a thin REST client for the bits of the Discord HTTP API the
// Control Channel feature needs. It deliberately exposes a small surface —
// channel CRUD inside a category, message read/write, list channels — so
// agents and admin code don't need to learn Discord's full API.
//
// The bot token is passed in per-call rather than stored on the struct so a
// single client can serve many CCs that share or own different bots.
type DiscordBot struct {
	BaseURL string
	HTTP    *http.Client
}

func NewDiscordBot() *DiscordBot {
	base := os.Getenv("DUCKWAY_DISCORD_BASE_URL")
	if base == "" {
		base = "https://discord.com/api/v10"
	}
	return &DiscordBot{
		BaseURL: base,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// DiscordError carries Discord's structured error response so callers can
// distinguish "you have no MANAGE_CHANNELS" from "channel not found".
type DiscordError struct {
	Status  int
	Code    int    `json:"code"`
	Message string `json:"message"`
	Raw     string `json:"-"`
}

func (e *DiscordError) Error() string {
	if e.Code != 0 || e.Message != "" {
		return fmt.Sprintf("discord %d (code %d): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("discord %d: %s", e.Status, e.Raw)
}

// IsNotFound reports the upstream said the resource doesn't exist.
func (e *DiscordError) IsNotFound() bool { return e.Status == 404 }

// IsForbidden reports the bot lacks permission.
func (e *DiscordError) IsForbidden() bool { return e.Status == 403 }

// CreateChannelOpts is the subset of fields we expose. type=0 (text) by default.
type CreateChannelOpts struct {
	GuildID  string
	ParentID string // category id
	Name     string
	Topic    string
}

// Channel is the Discord channel shape we care about.
type Channel struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     int     `json:"type"`
	Topic    string  `json:"topic"`
	ParentID *string `json:"parent_id"`
	GuildID  string  `json:"guild_id"`
}

// Message is the dispatched message shape we care about.
type Message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
	Author    struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
	Timestamp string `json:"timestamp"`
}

func (b *DiscordBot) do(ctx context.Context, botToken, method, path string, body interface{}) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bot "+botToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "DuckwayCC/1.0")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return respBody, nil
	}

	derr := &DiscordError{Status: resp.StatusCode, Raw: string(respBody)}
	if len(respBody) > 0 && respBody[0] == '{' {
		_ = json.Unmarshal(respBody, derr)
	}
	return nil, derr
}

func (b *DiscordBot) CreateChannel(ctx context.Context, botToken string, opts CreateChannelOpts) (*Channel, error) {
	body := map[string]interface{}{
		"name":      sanitizeChannelName(opts.Name),
		"type":      0, // GUILD_TEXT
		"parent_id": opts.ParentID,
	}
	if opts.Topic != "" {
		body["topic"] = opts.Topic
	}
	raw, err := b.do(ctx, botToken, "POST", "/guilds/"+opts.GuildID+"/channels", body)
	if err != nil {
		return nil, err
	}
	var ch Channel
	if err := json.Unmarshal(raw, &ch); err != nil {
		return nil, fmt.Errorf("parse channel response: %w", err)
	}
	return &ch, nil
}

// ArchiveChannel moves the channel out of its category and prefixes the name
// with "archived-". This is non-destructive — the channel + its messages
// stay readable to anyone with access. Idempotent: archiving a missing
// channel returns nil.
func (b *DiscordBot) ArchiveChannel(ctx context.Context, botToken, channelID, currentName string) error {
	if channelID == "" {
		return nil // Phase A stub or already cleaned up
	}
	newName := "archived-" + sanitizeChannelName(currentName)
	if len(newName) > 100 {
		newName = newName[:100]
	}
	body := map[string]interface{}{
		"name":      newName,
		"parent_id": nil,
	}
	_, err := b.do(ctx, botToken, "PATCH", "/channels/"+channelID, body)
	if derr, ok := err.(*DiscordError); ok && derr.IsNotFound() {
		return nil
	}
	return err
}

// PostMessage sends content to a channel and returns the new message id.
func (b *DiscordBot) PostMessage(ctx context.Context, botToken, channelID, content string) (string, error) {
	raw, err := b.do(ctx, botToken, "POST", "/channels/"+channelID+"/messages", map[string]interface{}{
		"content": content,
	})
	if err != nil {
		return "", err
	}
	var msg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return "", fmt.Errorf("parse message response: %w", err)
	}
	return msg.ID, nil
}

// EditMessage replaces a message's content.
func (b *DiscordBot) EditMessage(ctx context.Context, botToken, channelID, messageID, content string) error {
	_, err := b.do(ctx, botToken, "PATCH", "/channels/"+channelID+"/messages/"+messageID, map[string]interface{}{
		"content": content,
	})
	return err
}

// DeleteMessage removes a message.
func (b *DiscordBot) DeleteMessage(ctx context.Context, botToken, channelID, messageID string) error {
	_, err := b.do(ctx, botToken, "DELETE", "/channels/"+channelID+"/messages/"+messageID, nil)
	return err
}

// GetMessages reads recent messages, newest first. limit ≤ 100.
func (b *DiscordBot) GetMessages(ctx context.Context, botToken, channelID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	raw, err := b.do(ctx, botToken, "GET",
		fmt.Sprintf("/channels/%s/messages?limit=%d", channelID, limit), nil)
	if err != nil {
		return nil, err
	}
	var msgs []Message
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, fmt.Errorf("parse messages: %w", err)
	}
	return msgs, nil
}

// ListGuildChannels returns all channels in a guild. The caller can filter
// by parent_id to scope to a single category.
func (b *DiscordBot) ListGuildChannels(ctx context.Context, botToken, guildID string) ([]Channel, error) {
	raw, err := b.do(ctx, botToken, "GET", "/guilds/"+guildID+"/channels", nil)
	if err != nil {
		return nil, err
	}
	var chans []Channel
	if err := json.Unmarshal(raw, &chans); err != nil {
		return nil, fmt.Errorf("parse channels: %w", err)
	}
	return chans, nil
}

// sanitizeChannelName conforms a string to Discord's channel-name rules:
// lowercase, ASCII letters/digits/dashes/underscores, max 100 chars, no
// leading/trailing dash. We don't enforce the "no consecutive dashes" rule —
// Discord accepts those; the UI just collapses them visually.
func sanitizeChannelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "channel"
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}
