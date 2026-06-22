package services

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// DiscordGateway manages a persistent WSS connection to Discord.
// Approval messages are sent with ✅/❌ reactions — when a user clicks
// a reaction, the Gateway receives MESSAGE_REACTION_ADD and processes it.
// Also supports !approve / !reject text commands as fallback.
type DiscordGateway struct {
	botToken  string
	channelID string
	botUserID string // filled on READY
	onApprove func(approvalID string) error
	onReject  func(approvalID string) error

	// Maps message ID → approval ID (for reaction-based approval)
	pendingApprovals sync.Map // string → string

	ws          *websocket.Conn
	heartbeatMs int
	sessionID   string
	seq         *int
	mu          sync.Mutex
	stopCh      chan struct{}
	stopOnce    sync.Once
}

type gatewayPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int            `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type helloData struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

type readyData struct {
	SessionID string `json:"session_id"`
	User      struct {
		ID string `json:"id"`
	} `json:"user"`
}

type messageCreateData struct {
	Content   string `json:"content"`
	ChannelID string `json:"channel_id"`
	Author    struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
	ID string `json:"id"`
}

type reactionAddData struct {
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	Emoji     struct {
		Name string `json:"name"`
	} `json:"emoji"`
	Member struct {
		User struct {
			Username string `json:"username"`
			Bot      bool   `json:"bot"`
		} `json:"user"`
	} `json:"member"`
}

func NewDiscordGateway(botToken, channelID string, onApprove, onReject func(string) error) *DiscordGateway {
	return &DiscordGateway{
		botToken:  botToken,
		channelID: channelID,
		onApprove: onApprove,
		onReject:  onReject,
		stopCh:    make(chan struct{}),
	}
}

// RegisterApprovalMessage maps a Discord message ID to an approval ID.
// Called after sending an approval notification so reactions can be tracked.
func (g *DiscordGateway) RegisterApprovalMessage(messageID, approvalID string) {
	g.pendingApprovals.Store(messageID, approvalID)
}

// SendApprovalMessage sends an approval notification with pre-added reactions.
func (g *DiscordGateway) SendApprovalMessage(notif ApprovalNotification) {
	embed := map[string]interface{}{
		"title": "🔑 Duckway Approval Required",
		"color": 16750848,
		"description": fmt.Sprintf(
			"**Client:** `%s`\n**Service:** `%s`\n**Request:** `%s %s`\n**Approval ID:** `%s`\n\n"+
				"React ✅ to approve (24h) or ❌ to reject",
			notif.ClientName, notif.ServiceName, notif.Method, notif.Path, notif.ApprovalID),
	}

	body, _ := json.Marshal(map[string]interface{}{
		"embeds": []interface{}{embed},
	})

	apiURL := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages", g.channelID)
	req, _ := http.NewRequest("POST", apiURL, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bot "+g.botToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[discord-gw] Failed to send approval message: %v", err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var msg struct{ ID string `json:"id"` }
	json.Unmarshal(respBody, &msg)

	if msg.ID == "" {
		log.Printf("[discord-gw] No message ID in response: %s", string(respBody))
		return
	}

	// Register for reaction tracking
	g.RegisterApprovalMessage(msg.ID, notif.ApprovalID)

	// Add ✅ and ❌ reactions to the message
	g.react(g.channelID, msg.ID, "✅")
	time.Sleep(300 * time.Millisecond) // Rate limit
	g.react(g.channelID, msg.ID, "❌")
}

func (g *DiscordGateway) Start() {
	go g.connectLoop()
}

func (g *DiscordGateway) Stop() {
	g.stopOnce.Do(func() {
		close(g.stopCh)
		g.mu.Lock()
		if g.ws != nil {
			g.ws.Close()
		}
		g.mu.Unlock()
	})
}

func (g *DiscordGateway) connectLoop() {
	for {
		select {
		case <-g.stopCh:
			return
		default:
		}

		if err := g.connect(); err != nil {
			log.Printf("[discord-gw] Connection error: %v", err)
		}

		select {
		case <-g.stopCh:
			return
		case <-time.After(5 * time.Second):
			log.Printf("[discord-gw] Reconnecting...")
		}
	}
}

func (g *DiscordGateway) connect() error {
	// Snapshot session state under lock before dialing.
	g.mu.Lock()
	sessionID := g.sessionID
	seq := g.seq
	g.mu.Unlock()

	gwURL, err := g.getGatewayURL()
	if err != nil {
		return fmt.Errorf("get gateway URL: %w", err)
	}

	ws, err := websocket.Dial(gwURL+"/?v=10&encoding=json", "", "https://discord.com")
	if err != nil {
		return fmt.Errorf("dial WSS: %w", err)
	}

	hbDone := make(chan struct{})
	g.mu.Lock()
	g.ws = ws
	g.mu.Unlock()

	defer func() {
		close(hbDone)
		g.mu.Lock()
		g.ws = nil
		g.mu.Unlock()
		ws.Close()
	}()

	// Read Hello
	var hello gatewayPayload
	if err := websocket.JSON.Receive(ws, &hello); err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if hello.Op != 10 {
		return fmt.Errorf("expected Hello (op 10), got op %d", hello.Op)
	}

	var hd helloData
	if err := json.Unmarshal(hello.D, &hd); err != nil || hd.HeartbeatInterval <= 0 {
		return fmt.Errorf("invalid Hello heartbeat_interval: %d", hd.HeartbeatInterval)
	}

	g.mu.Lock()
	g.heartbeatMs = hd.HeartbeatInterval
	g.mu.Unlock()

	// Send RESUME if we have a prior session; otherwise IDENTIFY.
	// Heartbeat goroutine is launched after the handshake frame so it
	// cannot race the websocket.JSON.Send below.
	if sessionID != "" && seq != nil {
		resume := map[string]interface{}{
			"op": 6,
			"d": map[string]interface{}{
				"token":      g.botToken,
				"session_id": sessionID,
				"seq":        seq,
			},
		}
		if err := websocket.JSON.Send(ws, resume); err != nil {
			return fmt.Errorf("send resume: %w", err)
		}
		log.Printf("[discord-gw] Resuming session %s", sessionID)
	} else {
		identify := map[string]interface{}{
			"op": 2,
			"d": map[string]interface{}{
				"token":   g.botToken,
				"intents": (1 << 0) | (1 << 9) | (1 << 10) | (1 << 15),
				"properties": map[string]string{
					"os": "linux", "browser": "duckway", "device": "duckway",
				},
			},
		}
		if err := websocket.JSON.Send(ws, identify); err != nil {
			return fmt.Errorf("send identify: %w", err)
		}
		log.Printf("[discord-gw] Identifying for channel %s", g.channelID)
	}

	go g.heartbeat(ws, hbDone)

	for {
		select {
		case <-g.stopCh:
			return nil
		default:
		}

		var payload gatewayPayload
		if err := websocket.JSON.Receive(ws, &payload); err != nil {
			return fmt.Errorf("read event: %w", err)
		}

		if payload.S != nil {
			g.mu.Lock()
			g.seq = payload.S
			g.mu.Unlock()
		}

		switch payload.Op {
		case 0:
			g.handleDispatch(payload.T, payload.D, ws)
		case 1:
			g.sendHeartbeat(ws)
		case 7:
			return fmt.Errorf("server requested reconnect")
		case 9:
			// Invalid session: clear state so next connect() does IDENTIFY.
			g.mu.Lock()
			g.sessionID = ""
			g.seq = nil
			g.mu.Unlock()
			return fmt.Errorf("invalid session")
		case 11:
			// Heartbeat ACK
		}
	}
}

func (g *DiscordGateway) handleDispatch(eventType string, data json.RawMessage, ws *websocket.Conn) {
	switch eventType {
	case "READY":
		var rd readyData
		json.Unmarshal(data, &rd)
		g.mu.Lock()
		g.sessionID = rd.SessionID
		g.botUserID = rd.User.ID
		g.mu.Unlock()
		log.Printf("[discord-gw] Ready, session: %s, bot user: %s", rd.SessionID, rd.User.ID)

	case "RESUMED":
		log.Printf("[discord-gw] Resumed session")

	case "MESSAGE_CREATE":
		var msg messageCreateData
		json.Unmarshal(data, &msg)
		g.mu.Lock()
		botID := g.botUserID
		g.mu.Unlock()
		if msg.Author.Bot || msg.ChannelID != g.channelID {
			return
		}
		_ = botID
		g.handleCommand(msg)

	case "MESSAGE_REACTION_ADD":
		var reaction reactionAddData
		json.Unmarshal(data, &reaction)
		if reaction.ChannelID != g.channelID {
			return
		}
		g.mu.Lock()
		botID := g.botUserID
		g.mu.Unlock()
		// Ignore bot's own reactions
		if reaction.UserID == botID {
			return
		}
		g.handleReaction(reaction)
	}
}

func (g *DiscordGateway) handleReaction(reaction reactionAddData) {
	// Look up if this message has a pending approval
	val, ok := g.pendingApprovals.Load(reaction.MessageID)
	if !ok {
		return
	}
	approvalID := val.(string)
	username := reaction.Member.User.Username

	switch reaction.Emoji.Name {
	case "✅":
		if err := g.onApprove(approvalID); err != nil {
			g.sendMessage(g.channelID, fmt.Sprintf("❌ Failed to approve `%s`: %s", approvalID, err.Error()))
		} else {
			g.pendingApprovals.Delete(reaction.MessageID)
			g.editMessage(g.channelID, reaction.MessageID,
				fmt.Sprintf("✅ **Approved** `%s` for 24h by %s", approvalID, username))
		}
	case "❌":
		if err := g.onReject(approvalID); err != nil {
			g.sendMessage(g.channelID, fmt.Sprintf("❌ Failed to reject `%s`: %s", approvalID, err.Error()))
		} else {
			g.pendingApprovals.Delete(reaction.MessageID)
			g.editMessage(g.channelID, reaction.MessageID,
				fmt.Sprintf("🚫 **Rejected** `%s` by %s", approvalID, username))
		}
	}
}

func (g *DiscordGateway) handleCommand(msg messageCreateData) {
	content := strings.TrimSpace(msg.Content)

	if strings.HasPrefix(content, "!approve ") {
		approvalID := strings.TrimSpace(strings.TrimPrefix(content, "!approve "))
		if approvalID == "" {
			return
		}
		if err := g.onApprove(approvalID); err != nil {
			g.react(msg.ChannelID, msg.ID, "❌")
			g.sendMessage(msg.ChannelID, fmt.Sprintf("Failed to approve `%s`: %s", approvalID, err.Error()))
		} else {
			g.react(msg.ChannelID, msg.ID, "✅")
			g.sendMessage(msg.ChannelID, fmt.Sprintf("✅ Approved `%s` for 24h by %s", approvalID, msg.Author.Username))
		}
	} else if strings.HasPrefix(content, "!reject ") {
		approvalID := strings.TrimSpace(strings.TrimPrefix(content, "!reject "))
		if approvalID == "" {
			return
		}
		if err := g.onReject(approvalID); err != nil {
			g.react(msg.ChannelID, msg.ID, "❌")
		} else {
			g.react(msg.ChannelID, msg.ID, "🚫")
			g.sendMessage(msg.ChannelID, fmt.Sprintf("🚫 Rejected `%s` by %s", approvalID, msg.Author.Username))
		}
	}
}

func (g *DiscordGateway) heartbeat(ws *websocket.Conn, done <-chan struct{}) {
	g.mu.Lock()
	hbMs := g.heartbeatMs
	g.mu.Unlock()
	ticker := time.NewTicker(time.Duration(hbMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-g.stopCh:
			return
		case <-done:
			return
		case <-ticker.C:
			g.sendHeartbeat(ws)
		}
	}
}

func (g *DiscordGateway) sendHeartbeat(ws *websocket.Conn) {
	g.mu.Lock()
	seq := g.seq
	g.mu.Unlock()
	hb := map[string]interface{}{"op": 1, "d": seq}
	websocket.JSON.Send(ws, hb)
}

// discordDo is a fire-and-forget Discord REST call. It properly closes
// the response body so the connection is returned to the pool.
func (g *DiscordGateway) discordDo(method, url string, body io.Reader, contentType string) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		log.Printf("[discord-gw] build request %s %s: %v", method, url, err)
		return
	}
	req.Header.Set("Authorization", "Bot "+g.botToken)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func (g *DiscordGateway) react(channelID, messageID, emoji string) {
	url := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages/%s/reactions/%s/@me", channelID, messageID, emoji)
	g.discordDo("PUT", url, nil, "")
}

func (g *DiscordGateway) sendMessage(channelID, content string) {
	url := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages", channelID)
	body, _ := json.Marshal(map[string]string{"content": content})
	g.discordDo("POST", url, strings.NewReader(string(body)), "application/json")
}

func (g *DiscordGateway) editMessage(channelID, messageID, content string) {
	url := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages/%s", channelID, messageID)
	body, _ := json.Marshal(map[string]interface{}{
		"content": content,
		"embeds":  []interface{}{}, // Clear embeds after approval
	})
	g.discordDo("PATCH", url, strings.NewReader(string(body)), "application/json")
}

func (g *DiscordGateway) getGatewayURL() (string, error) {
	req, err := http.NewRequest("GET", "https://discord.com/api/v10/gateway/bot", nil)
	if err != nil {
		return "", fmt.Errorf("build gateway request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+g.botToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		URL string `json:"url"`
	}
	json.Unmarshal(body, &result)
	if result.URL == "" {
		return "", fmt.Errorf("no gateway URL in response: %s", string(body))
	}
	return result.URL, nil
}
