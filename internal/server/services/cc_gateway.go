package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"golang.org/x/net/websocket"
)

// Reconnect backoff bounds for a bot gateway connection. Discord rate-limits
// IDENTIFY (a fresh IDENTIFY is sent on every reconnect), so a flapping
// connection must NOT re-identify on a fixed short interval — doing so trips
// the limit and Discord answers op 9 (invalid session), which on a fixed loop
// becomes a self-sustaining storm. We back off exponentially with jitter and
// reset once a connection has proven stable.
const (
	ccReconnectMin    = 1 * time.Second
	ccReconnectMax    = 60 * time.Second
	ccStableThreshold = 60 * time.Second // a connection up this long is "healthy"
)

// ccReconnectDelay decides how long to wait before the next connect attempt and
// what the backoff base becomes afterwards. A connection that stayed up at
// least stableAfter is treated as healthy (e.g. a routine op-7 reconnect
// request) and resets the wait to min; otherwise the wait grows exponentially,
// capped at max. Jitter is added by the caller.
func ccReconnectDelay(prevBase, uptime, min, max, stableAfter time.Duration) (wait, nextBase time.Duration) {
	wait = prevBase
	if uptime >= stableAfter {
		wait = min
	}
	nextBase = wait * 2
	if nextBase > max {
		nextBase = max
	}
	return wait, nextBase
}

// parseHello validates Discord's Hello (op 10) frame and returns the heartbeat
// interval. It rejects a non-positive interval explicitly: feeding 0 into
// time.NewTicker panics, and an unrecovered panic in the heartbeat goroutine
// would crash the whole gateway process — taking the :80 listener down with it.
func parseHello(p gatewayPayload) (int, error) {
	if p.Op != 10 {
		return 0, fmt.Errorf("expected Hello, got op %d", p.Op)
	}
	var hd helloData
	if err := json.Unmarshal(p.D, &hd); err != nil {
		return 0, fmt.Errorf("parse hello: %w", err)
	}
	if hd.HeartbeatInterval <= 0 {
		return 0, fmt.Errorf("invalid heartbeat_interval %d", hd.HeartbeatInterval)
	}
	return hd.HeartbeatInterval, nil
}

// recoverCC swallows and logs a panic in a cc-gw goroutine. These goroutines
// parse untrusted Discord frames; without recovery a single malformed event
// would crash the entire gateway process and stop the :80 listener. Defer it
// at the top of every cc-gw goroutine.
func recoverCC(apiKeyID, where string) {
	if r := recover(); r != nil {
		log.Printf("[cc-gw] %s panic in %s recovered: %v", apiKeyID, where, r)
	}
}

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

	mu        sync.Mutex
	ws        *websocket.Conn
	hbMs      int
	seq       *int
	sessionID string // Discord session_id from last READY; empty = must IDENTIFY
	resumeURL string // resume_gateway_url from READY; falls back to /gateway/bot
	stopCh    chan struct{}
	stopped   bool
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
	base := ccReconnectMin
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		start := time.Now()
		// Recover here so a panic anywhere in the connect/read/dispatch path
		// (e.g. a malformed Discord frame) becomes one logged reconnect, not a
		// process crash that drops the :80 listener.
		func() {
			defer recoverCC(c.apiKeyID, "connect")
			if err := c.connect(); err != nil {
				log.Printf("[cc-gw] %s connection error: %v", c.apiKeyID, err)
			}
		}()

		var wait time.Duration
		wait, base = ccReconnectDelay(base, time.Since(start), ccReconnectMin, ccReconnectMax, ccStableThreshold)
		// Add up to 1s of jitter so a flapping connection doesn't re-identify
		// in lockstep and re-trip Discord's IDENTIFY rate limit.
		wait += time.Duration(rand.Int63n(int64(time.Second)))

		select {
		case <-c.stopCh:
			return
		case <-time.After(wait):
		}
	}
}

func (c *ccBotConn) connect() error {
	// Snapshot session state. canResume requires both sessionID and resumeURL —
	// resumeURL is always present in Discord API v10 READY, and Discord requires
	// RESUME to use that URL specifically. seq may be null (READY has S=null);
	// Discord accepts a null seq in RESUME and replays from the session start.
	c.mu.Lock()
	sessionID := c.sessionID
	resumeURL := c.resumeURL
	seqSnap := c.seq // nil is valid; we send "null" in the RESUME payload
	c.mu.Unlock()

	canResume := sessionID != "" && resumeURL != ""

	var gwURL string
	if canResume {
		gwURL = resumeURL
	} else {
		var err error
		gwURL, err = getGatewayURL(c.botToken)
		if err != nil {
			return fmt.Errorf("gateway lookup: %w", err)
		}
	}

	ws, err := websocket.Dial(gwURL+"/?v=10&encoding=json", "", "https://discord.com")
	if err != nil {
		return fmt.Errorf("dial WSS: %w", err)
	}
	// hbDone stops this connection's heartbeat goroutine when connect()
	// returns. Without it every reconnect leaks a heartbeat goroutine that
	// lives until the whole bot stops, accumulating over a day of reconnects
	// and beating on stale sockets.
	hbDone := make(chan struct{})
	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()
	defer func() {
		close(hbDone)
		c.mu.Lock()
		c.ws = nil
		c.mu.Unlock()
		ws.Close()
	}()

	var hello gatewayPayload
	if err := websocket.JSON.Receive(ws, &hello); err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	hbMs, err := parseHello(hello)
	if err != nil {
		return err
	}
	c.hbMs = hbMs

	if canResume {
		// RESUME: re-attach to the existing session. Discord replays any
		// dispatches with S > seq. A null seq is valid — Discord replays from
		// the session start. seq is NOT reset here.
		var seqJSON json.RawMessage
		if seqSnap != nil {
			seqJSON = json.RawMessage(strconv.Itoa(*seqSnap))
		} else {
			seqJSON = json.RawMessage("null")
		}
		resume := map[string]interface{}{
			"op": 6,
			"d": map[string]interface{}{
				"token":      c.botToken,
				"session_id": sessionID,
				"seq":        seqJSON,
			},
		}
		if err := websocket.JSON.Send(ws, resume); err != nil {
			return fmt.Errorf("send resume: %w", err)
		}
	} else {
		// Fresh IDENTIFY — start a new session. Reset seq so the next reconnect
		// doesn't try to resume a session that never fully started.
		// Intents: GUILDS(0) | GUILD_MESSAGES(9) | GUILD_MESSAGE_REACTIONS(10) | MESSAGE_CONTENT(15).
		// MESSAGE_CONTENT is privileged — bot owner must enable it in the
		// developer portal. GUILD_MESSAGE_REACTIONS is needed for the
		// discord_request_approval reaction-vote flow.
		c.mu.Lock()
		c.seq = nil
		c.mu.Unlock()
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
	}

	// Launch heartbeat after the handshake frame (RESUME or IDENTIFY) so
	// the goroutine cannot interleave with the handshake write above.
	go c.heartbeat(ws, hbDone)

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
		c.mu.Lock()
		if payload.S != nil {
			c.seq = payload.S
		}
		c.mu.Unlock()

		switch payload.Op {
		case 0:
			c.handleDispatch(payload.T, payload.D)
		case 1:
			c.sendHeartbeat(ws)
		case 7:
			return fmt.Errorf("reconnect requested")
		case 9:
			// Invalid session. Clear session state so the next connect()
			// sends a fresh IDENTIFY rather than looping on failed RESUMEs.
			c.mu.Lock()
			c.sessionID = ""
			c.resumeURL = ""
			c.seq = nil
			c.mu.Unlock()
			return fmt.Errorf("invalid session")
		}
	}
}

func (c *ccBotConn) handleDispatch(eventType string, data json.RawMessage) {
	switch eventType {
	case "READY":
		var rd struct {
			SessionID string `json:"session_id"`
			ResumeURL string `json:"resume_gateway_url"`
		}
		if err := json.Unmarshal(data, &rd); err == nil && rd.SessionID != "" {
			c.mu.Lock()
			c.sessionID = rd.SessionID
			if rd.ResumeURL != "" {
				c.resumeURL = rd.ResumeURL
			}
			c.mu.Unlock()
			log.Printf("[cc-gw] %s connected", c.apiKeyID)
		}
	case "RESUMED":
		log.Printf("[cc-gw] %s resumed", c.apiKeyID)
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
	all, err := c.cc.ListByAPIKeyID(c.apiKeyID)
	if err != nil {
		return
	}
	for _, cc := range all {
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
				ccID, chCopy, content := cc.ID, ch, msg.Content
				go func() {
					defer recoverCC(c.apiKeyID, "command")
					c.commands.Handle(context.Background(), ccID, chCopy, content)
				}()
				return
			}
		}

		inboxID, _ := c.cc.AppendInbox(cc.ID, &ch.Handle, eventType, string(payload))
		if c.hub != nil && cc.ClientID != "" {
			c.hub.Publish(cc.ClientID, CCEvent{
				Type:    sseTypeFromGateway(eventType),
				CCID:    cc.ID,
				Handle:  ch.Handle,
				Kind:    ch.Kind,
				Payload: payload,
				InboxID: inboxID,
			})
		}
		return
	}
}

func (c *ccBotConn) handleChannelDelete(realChannelID string, payload json.RawMessage) {
	all, err := c.cc.ListByAPIKeyID(c.apiKeyID)
	if err != nil {
		return
	}
	for _, cc := range all {
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
	all, err := c.cc.ListByAPIKeyID(c.apiKeyID)
	if err != nil {
		return
	}
	for _, cc := range all {
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

func (c *ccBotConn) heartbeat(ws *websocket.Conn, done <-chan struct{}) {
	defer recoverCC(c.apiKeyID, "heartbeat")
	ticker := time.NewTicker(time.Duration(c.hbMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-done:
			return
		case <-ticker.C:
			c.sendHeartbeat(ws)
		}
	}
}

func (c *ccBotConn) sendHeartbeat(ws *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hb := gatewayPayload{Op: 1}
	if c.seq != nil {
		hb.D = json.RawMessage(strconv.Itoa(*c.seq))
	} else {
		hb.D = json.RawMessage("null")
	}
	if c.ws != nil {
		websocket.JSON.Send(ws, hb)
	}
}

// gatewayLookupAttempts bounds how many times getGatewayURL retries a transient
// failure before giving up and letting connectLoop's backoff take over. A short
// in-place retry keeps a momentary blip — e.g. the Docker embedded DNS at
// 127.0.0.11 briefly refusing a UDP query ("lookup discord.com ... connection
// refused") — from bubbling up as a connection error and escalating the
// reconnect backoff to a full minute long after the network has recovered.
const gatewayLookupAttempts = 4

// isTransientGatewayErr reports whether a /gateway/bot failure is worth retrying
// in place. A failure with no HTTP response — DNS resolution, dial, timeout, a
// reset connection — is transient; those are exactly the failures a DNS blip
// produces. A Discord 429 or 5xx is also transient. An authentication or other
// 4xx response is permanent: retrying a bad token just burns attempts.
func isTransientGatewayErr(err error) bool {
	if err == nil {
		return false
	}
	var derr *DiscordError
	if errors.As(err, &derr) {
		return derr.Status == http.StatusTooManyRequests || derr.Status >= 500
	}
	return true
}

// getGatewayURL is a lightweight helper that calls /gateway/bot to learn the
// WSS URL for the connect call. Used by ccBotConn so it doesn't depend on
// DiscordGateway. It retries transient transport/5xx errors with a short
// growing backoff (see gatewayLookupAttempts) so a brief DNS hiccup doesn't
// trip the reconnect backoff.
func getGatewayURL(botToken string) (string, error) {
	bot := NewDiscordBot()
	var lastErr error
	for attempt := 0; attempt < gatewayLookupAttempts; attempt++ {
		if attempt > 0 {
			// 200ms, 400ms, 800ms — total < 1.5s, so a transient blip recovers
			// fast without holding the connection loop hostage.
			time.Sleep(time.Duration(100<<attempt) * time.Millisecond)
		}
		raw, err := bot.do(context.Background(), botToken, "GET", "/gateway/bot", nil)
		if err != nil {
			lastErr = err
			if isTransientGatewayErr(err) {
				continue
			}
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
	return "", lastErr
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
