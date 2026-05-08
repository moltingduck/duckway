package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"golang.org/x/net/websocket"
)

// CCGatewayManager owns one WSS connection per unique Discord bot token used
// by any active Control Channel. It listens for MESSAGE_CREATE events,
// resolves the channel back to a CC via the cc_channels cache, and writes
// the (filtered) event into discord_inbox so clients can long-poll for it.
//
// One connection per bot is ideal — Discord's gateway is a per-bot resource,
// and putting all CCs that share a bot behind a single connection avoids
// hitting session-count limits.
type CCGatewayManager struct {
	cc        *queries.ControlChannelQueries
	apiKeys   *queries.APIKeyQueries
	crypto    *Crypto
	hub       *CCEventHub
	commands  *CCCommandHandler
	approvals *CCApprovalRegistry

	mu      sync.Mutex
	conns   map[string]*ccBotConn // keyed by api_key_id
	stopCh  chan struct{}
	stopped bool
}

func NewCCGatewayManager(cc *queries.ControlChannelQueries, apiKeys *queries.APIKeyQueries, crypto *Crypto, hub *CCEventHub, approvals *CCApprovalRegistry) *CCGatewayManager {
	return &CCGatewayManager{
		cc:        cc,
		apiKeys:   apiKeys,
		crypto:    crypto,
		hub:       hub,
		commands:  NewCCCommandHandler(cc, apiKeys, crypto, NewDiscordBot(), hub),
		approvals: approvals,
		conns:     map[string]*ccBotConn{},
		stopCh:    make(chan struct{}),
	}
}

// Start scans existing CCs once at boot and opens one connection per bot.
// Future CCs added by the admin will only join an existing connection if
// one is already up for that bot — otherwise the next manual restart will
// pick them up. (Phase B keeps this simple; a Phase B+ tweak can add a
// poll loop if dynamic-add becomes important.)
func (m *CCGatewayManager) Start() {
	ccs, err := m.cc.List()
	if err != nil {
		log.Printf("[cc-gw] cannot list CCs at start: %v", err)
		return
	}
	seen := map[string]bool{}
	for _, c := range ccs {
		if !c.IsActive || seen[c.APIKeyID] {
			continue
		}
		seen[c.APIKeyID] = true
		m.startConn(c.APIKeyID)
	}
	log.Printf("[cc-gw] started %d bot connection(s)", len(m.conns))
}

// Stop closes every connection.
func (m *CCGatewayManager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	close(m.stopCh)
	conns := make([]*ccBotConn, 0, len(m.conns))
	for _, c := range m.conns {
		conns = append(conns, c)
	}
	m.mu.Unlock()
	for _, c := range conns {
		c.stop()
	}
}

// startConn launches a bot connection if not already running.
func (m *CCGatewayManager) startConn(apiKeyID string) {
	m.mu.Lock()
	if _, ok := m.conns[apiKeyID]; ok {
		m.mu.Unlock()
		return
	}
	key, err := m.apiKeys.GetByID(apiKeyID)
	if err != nil {
		log.Printf("[cc-gw] api_key %s lookup failed: %v", apiKeyID, err)
		m.mu.Unlock()
		return
	}
	tok, err := m.crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		log.Printf("[cc-gw] decrypt bot token %s failed: %v", apiKeyID, err)
		m.mu.Unlock()
		return
	}
	conn := &ccBotConn{
		apiKeyID:  apiKeyID,
		botToken:  tok,
		cc:        m.cc,
		hub:       m.hub,
		commands:  m.commands,
		approvals: m.approvals,
		stopCh:    make(chan struct{}),
	}
	m.conns[apiKeyID] = conn
	m.mu.Unlock()

	go conn.connectLoop()
}

// ccBotConn is one bot's WSS gateway connection.
type ccBotConn struct {
	apiKeyID  string
	botToken  string
	cc        *queries.ControlChannelQueries
	hub       *CCEventHub
	commands  *CCCommandHandler
	approvals *CCApprovalRegistry

	mu      sync.Mutex
	ws      *websocket.Conn
	hbMs    int
	seq     *int
	stopCh  chan struct{}
	stopped bool
}

func (c *ccBotConn) stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	close(c.stopCh)
	if c.ws != nil {
		c.ws.Close()
	}
	c.mu.Unlock()
}

func (c *ccBotConn) connectLoop() {
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		if err := c.connect(); err != nil {
			log.Printf("[cc-gw] %s connection error: %v", c.apiKeyID, err)
		}

		select {
		case <-c.stopCh:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *ccBotConn) connect() error {
	gwURL, err := getGatewayURL(c.botToken)
	if err != nil {
		return fmt.Errorf("gateway lookup: %w", err)
	}

	ws, err := websocket.Dial(gwURL+"/?v=10&encoding=json", "", "https://discord.com")
	if err != nil {
		return fmt.Errorf("dial WSS: %w", err)
	}
	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.ws = nil
		c.mu.Unlock()
		ws.Close()
	}()

	var hello gatewayPayload
	if err := websocket.JSON.Receive(ws, &hello); err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if hello.Op != 10 {
		return fmt.Errorf("expected Hello, got op %d", hello.Op)
	}
	var hd helloData
	json.Unmarshal(hello.D, &hd)
	c.hbMs = hd.HeartbeatInterval
	go c.heartbeat(ws)

	// Intents: GUILDS(0) | GUILD_MESSAGES(9) | GUILD_MESSAGE_REACTIONS(10) | MESSAGE_CONTENT(15).
	// MESSAGE_CONTENT is privileged — bot owner must enable it in the
	// developer portal. GUILD_MESSAGE_REACTIONS is needed for the
	// discord_request_approval reaction-vote flow.
	identify := map[string]interface{}{
		"op": 2,
		"d": map[string]interface{}{
			"token":   c.botToken,
			"intents": (1 << 0) | (1 << 9) | (1 << 10) | (1 << 15),
			"properties": map[string]string{
				"os": "linux", "browser": "duckway-cc", "device": "duckway-cc",
			},
		},
	}
	if err := websocket.JSON.Send(ws, identify); err != nil {
		return fmt.Errorf("send identify: %w", err)
	}

	log.Printf("[cc-gw] %s connected", c.apiKeyID)

	for {
		select {
		case <-c.stopCh:
			return nil
		default:
		}

		var payload gatewayPayload
		if err := websocket.JSON.Receive(ws, &payload); err != nil {
			return fmt.Errorf("read event: %w", err)
		}
		if payload.S != nil {
			c.seq = payload.S
		}

		switch payload.Op {
		case 0:
			c.handleDispatch(payload.T, payload.D)
		case 1:
			c.sendHeartbeat(ws)
		case 7:
			return fmt.Errorf("reconnect requested")
		case 9:
			return fmt.Errorf("invalid session")
		}
	}
}

func (c *ccBotConn) handleDispatch(eventType string, data json.RawMessage) {
	switch eventType {
	case "MESSAGE_CREATE":
		var msg messageCreateData
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		// Bot's own messages: skip — they were initiated by an agent and the
		// agent already has the message_id via PostMessage's return value.
		if msg.Author.Bot {
			return
		}
		c.routeMessageEvent(eventType, msg.ChannelID, data)
	case "MESSAGE_UPDATE", "MESSAGE_DELETE":
		var generic struct {
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(data, &generic); err != nil {
			return
		}
		c.routeMessageEvent(eventType, generic.ChannelID, data)
	case "CHANNEL_DELETE":
		// User deleted a channel directly in Discord. Resolve, drop the
		// cc_channels row so the daemon clears its session map, and notify
		// the daemon via the hub.
		var generic struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &generic); err != nil {
			return
		}
		c.handleChannelDelete(generic.ID, data)
	case "MESSAGE_REACTION_ADD":
		// Used by discord_request_approval. Look the message_id up in
		// the approval registry; if it's a tracked vote, resolve.
		var data2 struct {
			MessageID string `json:"message_id"`
			UserID    string `json:"user_id"`
			Emoji     struct {
				Name string `json:"name"`
			} `json:"emoji"`
		}
		if err := json.Unmarshal(data, &data2); err != nil {
			return
		}
		if c.approvals != nil {
			c.approvals.Resolve(data2.MessageID, data2.Emoji.Name, data2.UserID)
		}
	case "CHANNEL_UPDATE":
		// Channel rename / topic change — refresh our cached metadata.
		var ch struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Topic string `json:"topic"`
		}
		if err := json.Unmarshal(data, &ch); err != nil {
			return
		}
		c.handleChannelUpdate(ch.ID, ch.Name, ch.Topic, data)
	}
}

// routeMessageEvent looks the channel up in our cache, runs any !command
// inline, then either skips forwarding (commands aren't messages for the
// daemon) or appends to discord_inbox and publishes a live event.
func (c *ccBotConn) routeMessageEvent(eventType, realChannelID string, payload json.RawMessage) {
	all, err := c.cc.List()
	if err != nil {
		return
	}
	for _, cc := range all {
		if cc.APIKeyID != c.apiKeyID {
			continue
		}
		ch, err := c.cc.GetChannelByRealID(cc.ID, realChannelID)
		if err != nil || ch == nil {
			continue
		}

		// !-prefix messages on MESSAGE_CREATE are commands. Run them
		// server-side and don't forward — daemons should never see
		// human commands as agent input.
		if eventType == "MESSAGE_CREATE" && c.commands != nil {
			var msg struct {
				Content string `json:"content"`
			}
			_ = json.Unmarshal(payload, &msg)
			if LooksLikeCommand(msg.Content) {
				go c.commands.Handle(context.Background(), cc.ID, ch, msg.Content)
				return
			}
		}

		_ = c.cc.AppendInbox(cc.ID, &ch.Handle, eventType, string(payload))
		if c.hub != nil && cc.ClientID != "" {
			c.hub.Publish(cc.ClientID, CCEvent{
				Type:    sseTypeFromGateway(eventType),
				CCID:    cc.ID,
				Handle:  ch.Handle,
				Payload: payload,
			})
		}
		return
	}
}

func (c *ccBotConn) handleChannelDelete(realChannelID string, payload json.RawMessage) {
	all, err := c.cc.List()
	if err != nil {
		return
	}
	for _, cc := range all {
		if cc.APIKeyID != c.apiKeyID {
			continue
		}
		ch, err := c.cc.GetChannelByRealID(cc.ID, realChannelID)
		if err != nil || ch == nil {
			continue
		}
		// Drop the cache row entirely so daemon's "clear session" hook
		// fires from a single signal.
		_ = c.cc.DeleteChannelByRealID(cc.ID, realChannelID)
		if c.hub != nil && cc.ClientID != "" {
			c.hub.Publish(cc.ClientID, CCEvent{
				Type:    "channel_delete",
				CCID:    cc.ID,
				Handle:  ch.Handle,
				Payload: payload,
			})
		}
		return
	}
}

func (c *ccBotConn) handleChannelUpdate(realChannelID, name, topic string, payload json.RawMessage) {
	all, err := c.cc.List()
	if err != nil {
		return
	}
	for _, cc := range all {
		if cc.APIKeyID != c.apiKeyID {
			continue
		}
		ch, err := c.cc.GetChannelByRealID(cc.ID, realChannelID)
		if err != nil || ch == nil {
			continue
		}
		_ = c.cc.UpdateChannelMeta(ch.Handle, name, topic)
		if c.hub != nil && cc.ClientID != "" {
			c.hub.Publish(cc.ClientID, CCEvent{
				Type:    "channel_update",
				CCID:    cc.ID,
				Handle:  ch.Handle,
				Payload: payload,
			})
		}
		return
	}
}

// sseTypeFromGateway lowercases Discord's SCREAMING_CASE event names into
// the snake_case used in our SSE stream.
func sseTypeFromGateway(s string) string {
	switch s {
	case "MESSAGE_CREATE":
		return "message_create"
	case "MESSAGE_UPDATE":
		return "message_update"
	case "MESSAGE_DELETE":
		return "message_delete"
	}
	return s
}

func (c *ccBotConn) heartbeat(ws *websocket.Conn) {
	ticker := time.NewTicker(time.Duration(c.hbMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.sendHeartbeat(ws)
		}
	}
}

func (c *ccBotConn) sendHeartbeat(ws *websocket.Conn) {
	hb := gatewayPayload{Op: 1}
	if c.seq != nil {
		hb.D = json.RawMessage(strconv.Itoa(*c.seq))
	} else {
		hb.D = json.RawMessage("null")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ws != nil {
		websocket.JSON.Send(ws, hb)
	}
}

// getGatewayURL is a lightweight helper that calls /gateway/bot once to learn
// the WSS URL for the connect call. Used by ccBotConn so it doesn't depend
// on DiscordGateway.
func getGatewayURL(botToken string) (string, error) {
	bot := NewDiscordBot()
	raw, err := bot.do(context.Background(), botToken, "GET", "/gateway/bot", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse gateway/bot: %w", err)
	}
	if out.URL == "" {
		return "", fmt.Errorf("empty gateway URL")
	}
	return out.URL, nil
}

// StartInboxCleanup launches a goroutine that periodically prunes
// discord_inbox per the configured retention. Settings keys:
//
//	cc_inbox_retention_hours  (default 24)
//	cc_inbox_max_per_channel  (default 1000)
//	cc_inbox_cleanup_interval_minutes (default 10)
//
// All optional. The goroutine stops when stopCh is closed.
func StartInboxCleanup(cc *queries.ControlChannelQueries, settings *queries.SettingsQueries, stopCh <-chan struct{}) {
	go func() {
		readInt := func(key string, def int) int {
			v := settings.Get(key)
			if v == "" {
				return def
			}
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return def
			}
			return n
		}
		runOnce := func() {
			retention := readInt("cc_inbox_retention_hours", 24)
			perChannel := readInt("cc_inbox_max_per_channel", 1000)
			if err := cc.CleanupInbox(retention, perChannel); err != nil {
				log.Printf("[cc-inbox] cleanup error: %v", err)
			}
		}
		runOnce()

		intervalMin := readInt("cc_inbox_cleanup_interval_minutes", 10)
		ticker := time.NewTicker(time.Duration(intervalMin) * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()
	log.Printf("[cc-inbox] cleanup loop started")
}
