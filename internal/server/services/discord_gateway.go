package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"

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
	appID     string // filled on READY; interactions carry their own
	onApprove func(approvalID string) error
	onReject  func(approvalID string) error
	// decide, when set, handles every decision (buttons, reactions, commands)
	// with the actor attached so the approver allowlist can be enforced.
	decide DecisionFunc

	// REST access (overridable in tests).
	apiBase string
	httpc   *http.Client

	// Maps message ID → approval ID (for reaction-based approval)
	pendingApprovals sync.Map // string → string
	// Maps approval ID → message ID, so a decision made elsewhere can still
	// update the message (best effort, in-memory).
	messageByApproval sync.Map // string → string

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
	Application struct {
		ID string `json:"id"`
	} `json:"application"`
}

type messageCreateData struct {
	Content   string `json:"content"`
	ChannelID string `json:"channel_id"`
	Author    struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"author"`
	Member struct {
		Roles []string `json:"roles"`
	} `json:"member"`
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
		Roles []string `json:"roles"`
	} `json:"member"`
}

func NewDiscordGateway(botToken, channelID string, onApprove, onReject func(string) error) *DiscordGateway {
	return &DiscordGateway{
		botToken:  botToken,
		channelID: channelID,
		onApprove: onApprove,
		onReject:  onReject,
		apiBase:   discordAPIBase(),
		httpc:     http.DefaultClient,
		stopCh:    make(chan struct{}),
	}
}

// WithDecider routes every decision through fn (which receives the actor and
// enforces the approver allowlist) instead of the plain approve/reject
// callbacks.
func (g *DiscordGateway) WithDecider(fn DecisionFunc) *DiscordGateway {
	g.decide = fn
	return g
}

func discordAPIBase() string {
	if b := os.Getenv("DUCKWAY_DISCORD_BASE_URL"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://discord.com/api/v10"
}

// decideAs applies a decision through the wired DecisionFunc only. With none
// wired it fails closed: the plain onApprove/onReject callbacks carry no actor
// and would bypass the approver allowlist.
func (g *DiscordGateway) decideAs(approvalID string, approve bool, minutes int, actor ChatActor) error {
	if g.decide == nil {
		return ErrChannelNotAuthoritative
	}
	return g.decide(approvalID, approve, minutes, actor)
}

// Message component custom IDs on held-request messages.
const (
	discordIDApprove   = "dwa:a:"   // approve for the token's default limit
	discordIDApprove1h = "dwa:a1h:" // approve for one hour
	discordIDReject    = "dwa:r:"
)

// ApprovalComponents is the button row for a held-request message: Approve,
// Approve for 1 h, Reject.
func ApprovalComponents(approvalID string) []interface{} {
	btn := func(style int, label, id string) map[string]interface{} {
		return map[string]interface{}{"type": 2, "style": style, "label": label, "custom_id": id + approvalID}
	}
	return []interface{}{map[string]interface{}{"type": 1, "components": []interface{}{
		btn(3, "Approve", discordIDApprove),
		btn(1, "Approve for 1 h", discordIDApprove1h),
		btn(4, "Reject", discordIDReject),
	}}}
}

// RegisterApprovalMessage maps a Discord message ID to an approval ID.
// Called after sending an approval notification so reactions can be tracked.
func (g *DiscordGateway) RegisterApprovalMessage(messageID, approvalID string) {
	g.pendingApprovals.Store(messageID, approvalID)
}

// SendApprovalMessage sends an approval notification with pre-added reactions.
func (g *DiscordGateway) SendApprovalMessage(notif ApprovalNotification) {
	notif = notif.mdSafe()
	embed := map[string]interface{}{
		"title": "🔑 Duckway Approval Required",
		"color": 16750848,
		"description": fmt.Sprintf(
			"**Client:** `%s`\n**Service:** `%s`\n**Request:** `%s %s`\n**Approval ID:** `%s`\n\n"+
				"Use the buttons below, or react ✅ to approve (token default) / ❌ to reject. Only listed approvers can decide.",
			notif.ClientName, notif.ServiceName, notif.Method, notif.requestTarget(), notif.ApprovalID) + ruleLine(notif.Rule) + bodyLine(notif.BodyHash),
	}

	body, _ := json.Marshal(map[string]interface{}{
		"embeds":     []interface{}{embed},
		"components": ApprovalComponents(notif.ApprovalID),
	})

	apiURL := fmt.Sprintf("%s/channels/%s/messages", g.apiBase, g.channelID)
	req, _ := http.NewRequest("POST", apiURL, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bot "+g.botToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpc.Do(req)
	if err != nil {
		log.Printf("[discord-gw] Failed to send approval message: %v", scrubNetErr(err))
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var msg struct {
		ID string `json:"id"`
	}
	json.Unmarshal(respBody, &msg)

	if msg.ID == "" {
		log.Printf("[discord-gw] No message ID in response: %s", string(respBody))
		return
	}

	// Register for reaction tracking
	g.RegisterApprovalMessage(msg.ID, notif.ApprovalID)
	g.messageByApproval.Store(notif.ApprovalID, msg.ID)

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
			log.Printf("[discord-gw] Connection error: %v", scrubNetErr(err))
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
		g.appID = rd.Application.ID
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

	case "INTERACTION_CREATE":
		var in interactionData
		if json.Unmarshal(data, &in) == nil {
			go g.handleInteraction(in)
		}

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
	actor := ChatActor{Kind: "discord", UserID: reaction.UserID, Username: reaction.Member.User.Username, RoleIDs: reaction.Member.Roles}

	var approve bool
	switch reaction.Emoji.Name {
	case "✅":
		approve = true
	case "❌":
	default:
		return
	}
	if err := g.decideAs(approvalID, approve, 0, actor); err != nil {
		if errors.Is(err, ErrNotApprover) {
			// Reactions leave no private channel: say it briefly in the channel.
			g.sendMessage(g.channelID, fmt.Sprintf("<@%s> not an approver: only listed users can decide approvals.", actor.UserID))
			return
		}
		g.sendMessage(g.channelID, fmt.Sprintf("Could not decide `%s`: %s", approvalID, decisionFailure(err)))
		return
	}
	g.finishMessage(reaction.MessageID, approvalID, resultLine(approve, 0, actor))
}

// resultLine is the line appended to a held-request message after a decision:
// the decider and the time (Discord renders <t:unix:f> in the viewer's zone).
func resultLine(approve bool, minutes int, actor ChatActor) string {
	verb := "Rejected"
	if approve {
		verb = "Approved"
		if minutes > 0 {
			verb += " for " + fmtMinutes(minutes)
		}
	}
	return fmt.Sprintf("%s by <@%s> <t:%d:f>", verb, actor.UserID, time.Now().Unix())
}

// alreadyDecidedLine renders a lost race for chat: "Already decided (approved) by X".
func alreadyDecidedLine(err error) string {
	var ad *AlreadyDecidedError
	if errors.As(err, &ad) {
		line := "Already decided (" + ad.Decision.Status + ")"
		if ad.Decision.DecidedBy != "" {
			line += " by " + ad.Decision.DecidedBy
		}
		return line
	}
	return "Already decided"
}

// finishMessage removes the buttons from a held-request message and shows the
// result line. Best effort: a failure is only logged.
func (g *DiscordGateway) finishMessage(messageID, approvalID, line string) {
	g.pendingApprovals.Delete(messageID)
	g.messageByApproval.Delete(approvalID)
	g.editResult(g.channelID, messageID, line)
}

func (g *DiscordGateway) editResult(channelID, messageID, line string) {
	url := fmt.Sprintf("%s/channels/%s/messages/%s", g.apiBase, channelID, messageID)
	body, _ := json.Marshal(map[string]interface{}{
		"content":          line,
		"components":       []interface{}{},
		"allowed_mentions": map[string]interface{}{"parse": []string{}},
	})
	g.discordDo("PATCH", url, strings.NewReader(string(body)), "application/json")
}

// MarkDecided updates the held-request message for an approval decided
// elsewhere (web console, TUI, expiry). Best effort: only messages this process
// sent are known.
func (g *DiscordGateway) MarkDecided(approvalID, line string) {
	v, ok := g.messageByApproval.LoadAndDelete(approvalID)
	if !ok {
		return
	}
	msgID := v.(string)
	g.pendingApprovals.Delete(msgID)
	g.editResult(g.channelID, msgID, line)
}

// interactionData is the part of INTERACTION_CREATE the gateway uses.
type interactionData struct {
	ID            string `json:"id"`
	Token         string `json:"token"`
	ApplicationID string `json:"application_id"`
	Type          int    `json:"type"`
	ChannelID     string `json:"channel_id"`
	Data          struct {
		CustomID string `json:"custom_id"`
	} `json:"data"`
	Member *struct {
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
		Roles []string `json:"roles"`
	} `json:"member"`
	User *struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Message struct {
		ID string `json:"id"`
	} `json:"message"`
}

// handleInteraction answers a button press. The press is acknowledged at once
// (Discord allows 3 s) with a deferred message update; the decision, the
// message edit and any private reply follow.
func (g *DiscordGateway) handleInteraction(in interactionData) {
	const componentType = 3
	if in.Type != componentType || !strings.HasPrefix(in.Data.CustomID, "dwa:") {
		return
	}
	g.ackInteraction(in, map[string]interface{}{"type": 6}) // DEFERRED_UPDATE_MESSAGE

	var approvalID string
	var approve bool
	var minutes int
	switch {
	case strings.HasPrefix(in.Data.CustomID, discordIDApprove1h):
		approvalID, approve, minutes = strings.TrimPrefix(in.Data.CustomID, discordIDApprove1h), true, 60
	case strings.HasPrefix(in.Data.CustomID, discordIDApprove):
		approvalID, approve = strings.TrimPrefix(in.Data.CustomID, discordIDApprove), true
	case strings.HasPrefix(in.Data.CustomID, discordIDReject):
		approvalID = strings.TrimPrefix(in.Data.CustomID, discordIDReject)
	default:
		return
	}

	actor := ChatActor{Kind: "discord"}
	switch {
	case in.Member != nil:
		actor.UserID, actor.Username, actor.RoleIDs = in.Member.User.ID, in.Member.User.Username, in.Member.Roles
	case in.User != nil:
		actor.UserID, actor.Username = in.User.ID, in.User.Username
	}
	if in.ChannelID != g.channelID || approvalID == "" {
		g.followup(in, "This button does not belong to the approval channel.")
		return
	}
	if err := g.decideAs(approvalID, approve, minutes, actor); err != nil {
		switch {
		case errors.Is(err, ErrNotApprover):
			g.followup(in, "Not an approver: ask an admin to add you to this channel's approver list.")
		case errors.Is(err, queries.ErrApprovalNotPending):
			g.followup(in, alreadyDecidedLine(err))
			// Show the existing result on the message too.
			g.finishMessage(in.Message.ID, approvalID, alreadyDecidedLine(err))
		default:
			g.followup(in, "Could not decide: "+decisionFailure(err))
		}
		return
	}
	g.finishMessage(in.Message.ID, approvalID, resultLine(approve, minutes, actor))
}

// ackInteraction posts the interaction callback (no bot authorization needed).
func (g *DiscordGateway) ackInteraction(in interactionData, payload map[string]interface{}) {
	body, _ := json.Marshal(payload)
	g.discordDo("POST", fmt.Sprintf("%s/interactions/%s/%s/callback", g.apiBase, in.ID, in.Token), strings.NewReader(string(body)), "application/json")
}

// followup sends a reply only the presser sees (ephemeral).
func (g *DiscordGateway) followup(in interactionData, text string) {
	app := in.ApplicationID
	if app == "" {
		app = g.applicationID()
	}
	body, _ := json.Marshal(map[string]interface{}{"content": text, "flags": 64, "allowed_mentions": map[string]interface{}{"parse": []string{}}})
	g.discordDo("POST", fmt.Sprintf("%s/webhooks/%s/%s", g.apiBase, app, in.Token), strings.NewReader(string(body)), "application/json")
}

func (g *DiscordGateway) applicationID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.appID
}

func (g *DiscordGateway) handleCommand(msg messageCreateData) {
	content := strings.TrimSpace(msg.Content)
	actor := ChatActor{Kind: "discord", UserID: msg.Author.ID, Username: msg.Author.Username, RoleIDs: msg.Member.Roles}

	var approve bool
	var approvalID string
	switch {
	case strings.HasPrefix(content, "!approve "):
		approve = true
		approvalID = strings.TrimSpace(strings.TrimPrefix(content, "!approve "))
	case strings.HasPrefix(content, "!reject "):
		approvalID = strings.TrimSpace(strings.TrimPrefix(content, "!reject "))
	default:
		return
	}
	if approvalID == "" {
		return
	}
	if err := g.decideAs(approvalID, approve, 0, actor); err != nil {
		g.react(msg.ChannelID, msg.ID, "❌")
		if errors.Is(err, ErrNotApprover) {
			g.sendMessage(msg.ChannelID, "Not an approver: only users on this channel's approver list can decide approvals.")
			return
		}
		g.sendMessage(msg.ChannelID, fmt.Sprintf("Could not decide `%s`: %s", approvalID, decisionFailure(err)))
		return
	}
	if approve {
		g.react(msg.ChannelID, msg.ID, "✅")
	} else {
		g.react(msg.ChannelID, msg.ID, "🚫")
	}
	g.sendMessage(msg.ChannelID, fmt.Sprintf("%s (`%s`)", resultLine(approve, 0, actor), approvalID))
	// The held-request message (if this process sent it) loses its buttons too.
	if v, ok := g.messageByApproval.Load(approvalID); ok {
		g.finishMessage(v.(string), approvalID, resultLine(approve, 0, actor))
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
		log.Printf("[discord-gw] build request %s: %v", method, scrubNetErr(err))
		return
	}
	req.Header.Set("Authorization", "Bot "+g.botToken)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := g.httpc.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func (g *DiscordGateway) react(channelID, messageID, emoji string) {
	url := fmt.Sprintf("%s/channels/%s/messages/%s/reactions/%s/@me", g.apiBase, channelID, messageID, urlEscapeEmoji(emoji))
	g.discordDo("PUT", url, nil, "")
}

func (g *DiscordGateway) sendMessage(channelID, content string) {
	url := fmt.Sprintf("%s/channels/%s/messages", g.apiBase, channelID)
	body, _ := json.Marshal(map[string]interface{}{"content": content, "allowed_mentions": map[string]interface{}{"parse": []string{}}})
	g.discordDo("POST", url, strings.NewReader(string(body)), "application/json")
}

func (g *DiscordGateway) getGatewayURL() (string, error) {
	req, err := http.NewRequest("GET", g.apiBase+"/gateway/bot", nil)
	if err != nil {
		return "", fmt.Errorf("build gateway request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+g.botToken)
	resp, err := g.httpc.Do(req)
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
