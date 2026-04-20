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

// DiscordGateway manages a persistent WSS connection to Discord for receiving
// message events. When a user types !approve or !reject in the channel,
// it calls the approval callback.
type DiscordGateway struct {
	botToken    string
	channelID   string
	onApprove   func(approvalID string) error
	onReject    func(approvalID string) error
	ws          *websocket.Conn
	heartbeatMs int
	sessionID   string
	seq         *int
	mu          sync.Mutex
	stopCh      chan struct{}
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

func NewDiscordGateway(botToken, channelID string, onApprove, onReject func(string) error) *DiscordGateway {
	return &DiscordGateway{
		botToken:  botToken,
		channelID: channelID,
		onApprove: onApprove,
		onReject:  onReject,
		stopCh:    make(chan struct{}),
	}
}

func (g *DiscordGateway) Start() {
	go g.connectLoop()
}

func (g *DiscordGateway) Stop() {
	close(g.stopCh)
	g.mu.Lock()
	if g.ws != nil {
		g.ws.Close()
	}
	g.mu.Unlock()
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
	// Get gateway URL
	gwURL, err := g.getGatewayURL()
	if err != nil {
		return fmt.Errorf("get gateway URL: %w", err)
	}

	ws, err := websocket.Dial(gwURL+"/?v=10&encoding=json", "", "https://discord.com")
	if err != nil {
		return fmt.Errorf("dial WSS: %w", err)
	}

	g.mu.Lock()
	g.ws = ws
	g.mu.Unlock()

	defer func() {
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
	json.Unmarshal(hello.D, &hd)
	g.heartbeatMs = hd.HeartbeatInterval

	// Start heartbeat
	go g.heartbeat(ws)

	// Send Identify
	// Intents: GUILDS(1<<0) | GUILD_MESSAGES(1<<9) | MESSAGE_CONTENT(1<<15)
	identify := map[string]interface{}{
		"op": 2,
		"d": map[string]interface{}{
			"token":   g.botToken,
			"intents": (1 << 0) | (1 << 9) | (1 << 15),
			"properties": map[string]string{
				"os":      "linux",
				"browser": "duckway",
				"device":  "duckway",
			},
		},
	}
	if err := websocket.JSON.Send(ws, identify); err != nil {
		return fmt.Errorf("send identify: %w", err)
	}

	log.Printf("[discord-gw] Connected, listening for commands in channel %s", g.channelID)

	// Event loop
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
			g.seq = payload.S
		}

		switch payload.Op {
		case 0: // Dispatch
			g.handleDispatch(payload.T, payload.D, ws)
		case 1: // Heartbeat request
			g.sendHeartbeat(ws)
		case 7: // Reconnect
			return fmt.Errorf("server requested reconnect")
		case 9: // Invalid session
			return fmt.Errorf("invalid session")
		case 11: // Heartbeat ACK
			// OK
		}
	}
}

func (g *DiscordGateway) handleDispatch(eventType string, data json.RawMessage, ws *websocket.Conn) {
	switch eventType {
	case "READY":
		var rd readyData
		json.Unmarshal(data, &rd)
		g.sessionID = rd.SessionID
		log.Printf("[discord-gw] Ready, session: %s", rd.SessionID)

	case "MESSAGE_CREATE":
		var msg messageCreateData
		json.Unmarshal(data, &msg)

		// Ignore bot messages and messages in other channels
		if msg.Author.Bot || msg.ChannelID != g.channelID {
			return
		}

		g.handleCommand(msg)
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
			g.sendMessage(msg.ChannelID, fmt.Sprintf("Approved `%s` for 24h by %s", approvalID, msg.Author.Username))
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
			g.sendMessage(msg.ChannelID, fmt.Sprintf("Rejected `%s` by %s", approvalID, msg.Author.Username))
		}
	}
}

func (g *DiscordGateway) heartbeat(ws *websocket.Conn) {
	ticker := time.NewTicker(time.Duration(g.heartbeatMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.sendHeartbeat(ws)
		}
	}
}

func (g *DiscordGateway) sendHeartbeat(ws *websocket.Conn) {
	hb := map[string]interface{}{"op": 1, "d": g.seq}
	websocket.JSON.Send(ws, hb)
}

func (g *DiscordGateway) react(channelID, messageID, emoji string) {
	url := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages/%s/reactions/%s/@me", channelID, messageID, emoji)
	req, _ := http.NewRequest("PUT", url, nil)
	req.Header.Set("Authorization", "Bot "+g.botToken)
	http.DefaultClient.Do(req)
}

func (g *DiscordGateway) sendMessage(channelID, content string) {
	url := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages", channelID)
	body, _ := json.Marshal(map[string]string{"content": content})
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bot "+g.botToken)
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)
}

func (g *DiscordGateway) getGatewayURL() (string, error) {
	req, _ := http.NewRequest("GET", "https://discord.com/api/v10/gateway/bot", nil)
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
