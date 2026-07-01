package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"
)

// urlPathEscape returns a URL-path-encoded form of s. Unicode emoji
// pass through net/url's PathEscape.
func urlPathEscape(s string) string { return url.PathEscape(s) }

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

const (
	discordPermManageChannels     int64 = 1 << 4
	discordPermAddReactions       int64 = 1 << 6
	discordPermViewChannel        int64 = 1 << 10
	discordPermSendMessages       int64 = 1 << 11
	discordPermReadMessageHistory int64 = 1 << 16
	discordPermManageRoles        int64 = 1 << 28

	discordDuckwayCategoryPerms = discordPermManageChannels |
		discordPermAddReactions |
		discordPermViewChannel |
		discordPermSendMessages |
		discordPermReadMessageHistory
	discordDuckwaySetupInvitePerms = discordDuckwayCategoryPerms | discordPermManageRoles
)

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
//
// On 400 "Invalid Form Body", Discord embeds the field-level reason under
// `errors.<field>._errors[].message`. FieldErrors flattens that to a
// readable per-field list so the operator sees "name: must match regex"
// instead of just "Invalid Form Body".
type DiscordError struct {
	Status      int
	Code        int                        `json:"code"`
	Message     string                     `json:"message"`
	FieldErrors map[string]json.RawMessage `json:"errors,omitempty"`
	Raw         string                     `json:"-"`
}

func (e *DiscordError) Error() string {
	if e.Code != 0 || e.Message != "" {
		base := fmt.Sprintf("discord %d (code %d): %s", e.Status, e.Code, e.Message)
		if detail := e.fieldDetail(); detail != "" {
			return base + " — " + detail
		}
		return base
	}
	return fmt.Sprintf("discord %d: %s", e.Status, e.Raw)
}

// fieldDetail walks `errors.<field>._errors[].message` and returns a
// "name: too short, parent_id: must be a string" style summary.
func (e *DiscordError) fieldDetail() string {
	if len(e.FieldErrors) == 0 {
		return ""
	}
	type inner struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"_errors"`
	}
	var parts []string
	for field, raw := range e.FieldErrors {
		var in inner
		if err := json.Unmarshal(raw, &in); err == nil {
			for _, msg := range in.Errors {
				parts = append(parts, fmt.Sprintf("%s: %s", field, msg.Message))
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
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

// User is the bot identity returned by /users/@me.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Guild is the subset of Discord guild metadata needed by the CC setup wizard.
type Guild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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

type DiscordFile struct {
	Filename    string
	ContentType string
	Data        []byte
	ReplyTo     string
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

func (b *DiscordBot) CreateCategory(ctx context.Context, botToken, guildID, name string) (*Channel, error) {
	body := map[string]interface{}{
		"name": sanitizeChannelName(name),
		"type": 4, // GUILD_CATEGORY
	}
	raw, err := b.do(ctx, botToken, "POST", "/guilds/"+guildID+"/channels", body)
	if err != nil {
		return nil, err
	}
	var ch Channel
	if err := json.Unmarshal(raw, &ch); err != nil {
		return nil, fmt.Errorf("parse category response: %w", err)
	}
	return &ch, nil
}

// GrantCategoryAccess gives the bot user the category-level permissions that
// Duckway needs. New task channels under the category inherit this overwrite.
func (b *DiscordBot) GrantCategoryAccess(ctx context.Context, botToken, categoryID, botUserID string) error {
	if categoryID == "" || botUserID == "" {
		return fmt.Errorf("category_id and bot_user_id are required")
	}
	body := map[string]interface{}{
		"type":  1, // member overwrite
		"allow": fmt.Sprintf("%d", discordDuckwayCategoryPerms),
		"deny":  "0",
	}
	_, err := b.do(ctx, botToken, "PUT", fmt.Sprintf("/channels/%s/permissions/%s", categoryID, botUserID), body)
	return err
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

// DeleteChannel removes a channel entirely (DELETE /channels/{id}). Used by
// the CC test flow — for normal teardown we prefer ArchiveChannel which
// preserves message history.
func (b *DiscordBot) DeleteChannel(ctx context.Context, botToken, channelID string) error {
	if channelID == "" {
		return nil
	}
	_, err := b.do(ctx, botToken, "DELETE", "/channels/"+channelID, nil)
	if derr, ok := err.(*DiscordError); ok && derr.IsNotFound() {
		return nil
	}
	return err
}

// AddReaction makes the bot react to a message with the given emoji.
// emoji must be the unicode codepoint (e.g. "✅") — custom emoji use
// "name:id" form. Discord requires the emoji URL-encoded in the path.
func (b *DiscordBot) AddReaction(ctx context.Context, botToken, channelID, messageID, emoji string) error {
	path := fmt.Sprintf("/channels/%s/messages/%s/reactions/%s/@me",
		channelID, messageID, urlEscapeEmoji(emoji))
	_, err := b.do(ctx, botToken, "PUT", path, nil)
	return err
}

// urlEscapeEmoji URL-encodes a Discord emoji for use in a request path.
// Unicode emoji like "✅" need percent-encoding because Discord routes
// on the raw bytes.
func urlEscapeEmoji(emoji string) string {
	return urlPathEscape(emoji)
}

// PostMessage sends content to a channel and returns the new message id.
func (b *DiscordBot) PostMessage(ctx context.Context, botToken, channelID, content string) (string, error) {
	return b.PostMessageReply(ctx, botToken, channelID, content, "")
}

func (b *DiscordBot) PostMessageReply(ctx context.Context, botToken, channelID, content, replyToMessageID string) (string, error) {
	body := map[string]interface{}{
		"content": content,
	}
	if replyToMessageID != "" {
		body["message_reference"] = map[string]interface{}{
			"message_id":         replyToMessageID,
			"channel_id":         channelID,
			"fail_if_not_exists": false,
		}
	}
	raw, err := b.do(ctx, botToken, "POST", "/channels/"+channelID+"/messages", body)
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

// PostMessageWithFile sends one Discord message containing content and a
// single attachment. Discord requires multipart/form-data for attachments:
// payload_json contains the message metadata and files[0] contains bytes.
func (b *DiscordBot) PostMessageWithFile(ctx context.Context, botToken, channelID, content string, file DiscordFile) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	payload := map[string]interface{}{
		"content": content,
		"attachments": []map[string]interface{}{
			{"id": 0, "filename": file.Filename},
		},
	}
	if file.ReplyTo != "" {
		payload["message_reference"] = map[string]interface{}{
			"message_id":         file.ReplyTo,
			"channel_id":         channelID,
			"fail_if_not_exists": false,
		}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload_json: %w", err)
	}
	if err := mw.WriteField("payload_json", string(payloadJSON)); err != nil {
		return "", fmt.Errorf("write payload_json: %w", err)
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files[0]"; filename="%s"`, escapeMultipartFilename(file.Filename)))
	if file.ContentType != "" {
		h.Set("Content-Type", file.ContentType)
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := part.Write(file.Data); err != nil {
		return "", fmt.Errorf("write file part: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", b.BaseURL+"/channels/"+channelID+"/messages", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+botToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", "DuckwayCC/1.0")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("discord request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		derr := &DiscordError{Status: resp.StatusCode, Raw: string(respBody)}
		if len(respBody) > 0 && respBody[0] == '{' {
			_ = json.Unmarshal(respBody, derr)
		}
		return "", derr
	}
	var msg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &msg); err != nil {
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

func (b *DiscordBot) CurrentUser(ctx context.Context, botToken string) (*User, error) {
	raw, err := b.do(ctx, botToken, "GET", "/users/@me", nil)
	if err != nil {
		return nil, err
	}
	var u User
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("parse current user: %w", err)
	}
	return &u, nil
}

func (b *DiscordBot) ListGuilds(ctx context.Context, botToken string) ([]Guild, error) {
	raw, err := b.do(ctx, botToken, "GET", "/users/@me/guilds", nil)
	if err != nil {
		return nil, err
	}
	var guilds []Guild
	if err := json.Unmarshal(raw, &guilds); err != nil {
		return nil, fmt.Errorf("parse guilds: %w", err)
	}
	return guilds, nil
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

func DiscordInviteURL(clientID string) string {
	return discordInviteURL(clientID, discordDuckwayCategoryPerms)
}

func DiscordSetupInviteURL(clientID string) string {
	return discordInviteURL(clientID, discordDuckwaySetupInvitePerms)
}

func escapeMultipartFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "\\\\")
	name = strings.ReplaceAll(name, `"`, `\"`)
	name = strings.ReplaceAll(name, "\r", "_")
	name = strings.ReplaceAll(name, "\n", "_")
	return name
}

func discordInviteURL(clientID string, perms int64) string {
	v := url.Values{}
	v.Set("client_id", clientID)
	v.Set("permissions", fmt.Sprintf("%d", perms))
	v.Set("scope", "bot applications.commands")
	return "https://discord.com/oauth2/authorize?" + v.Encode()
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
