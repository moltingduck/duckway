package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

// CCClientHandler exposes the CC operations agents need: list channels under
// the client's CC, create / archive task channels, post / read / edit /
// delete messages, long-poll the inbox, AND a server-sent-event stream of
// live gateway events for the `duckway cc watch` daemon.
//
// CC v2: 1:1 client↔CC, so cc_id is implicit (looked up by client_id).
// All real Discord IDs (channel_id, guild_id, category_id) stay server-side.
type CCClientHandler struct {
	cc        *queries.ControlChannelQueries
	apiKeys   *queries.APIKeyQueries
	crypto    *svc.Crypto
	bot       *svc.DiscordBot
	hub       *svc.CCEventHub
	approvals *svc.CCApprovalRegistry
}

func NewCCClientHandler(cc *queries.ControlChannelQueries, apiKeys *queries.APIKeyQueries, crypto *svc.Crypto, bot *svc.DiscordBot, hub *svc.CCEventHub, approvals *svc.CCApprovalRegistry) *CCClientHandler {
	return &CCClientHandler{cc: cc, apiKeys: apiKeys, crypto: crypto, bot: bot, hub: hub, approvals: approvals}
}

// resolveCC fetches the client's CC + decrypted bot token, or writes the
// appropriate response on failure.
func (h *CCClientHandler) resolveCC(w http.ResponseWriter, r *http.Request) (*models.Client, *models.ControlChannel, string, bool) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return nil, nil, "", false
	}
	cc, err := h.cc.GetByClientID(client.ID)
	if err != nil {
		jsonError(w, "no CC assigned to this client", http.StatusNotFound)
		return nil, nil, "", false
	}
	if !cc.IsActive {
		jsonError(w, "cc is inactive", http.StatusForbidden)
		return nil, nil, "", false
	}
	key, err := h.apiKeys.GetByID(cc.APIKeyID)
	if err != nil {
		jsonError(w, "bot token lookup failed", http.StatusInternalServerError)
		return nil, nil, "", false
	}
	tok, err := h.crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		jsonError(w, "decrypt bot token", http.StatusInternalServerError)
		return nil, nil, "", false
	}
	return client, cc, tok, true
}

// resolveHandle checks the handle belongs to the requested CC.
func (h *CCClientHandler) resolveHandle(w http.ResponseWriter, ccID, handle string) (*models.CCChannel, bool) {
	ch, err := h.cc.GetChannelByHandle(handle)
	if err != nil {
		jsonError(w, "channel handle not found", http.StatusNotFound)
		return nil, false
	}
	if ch.CCID != ccID {
		jsonError(w, "handle does not belong to this cc", http.StatusForbidden)
		return nil, false
	}
	return ch, true
}

// GET /client/cc — return the (single) CC the client is bound to.
func (h *CCClientHandler) GetMyCC(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}
	cc, err := h.cc.GetByClientID(client.ID)
	if err != nil {
		jsonResponse(w, map[string]interface{}{"assigned": false})
		return
	}
	mgmt, _ := h.cc.GetManagementChannel(cc.ID)
	mgmtHandle := ""
	if mgmt != nil {
		mgmtHandle = mgmt.Handle
	}
	jsonResponse(w, map[string]interface{}{
		"assigned":              true,
		"cc_id":                 cc.ID,
		"cc_name":               cc.Name,
		"agent_type":            cc.AgentType,
		"management_handle":     mgmtHandle,
	})
}

// GET /client/cc/channels — list channels in the client's CC.
func (h *CCClientHandler) ListChannels(w http.ResponseWriter, r *http.Request) {
	_, cc, _, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	chans, err := h.cc.ListChannels(cc.ID)
	if err != nil {
		jsonError(w, "list channels: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type publicCh struct {
		Handle    string `json:"handle"`
		Name      string `json:"name"`
		Topic     string `json:"topic"`
		Kind      string `json:"kind"`
		SessionID string `json:"session_id,omitempty"`
		Cwd       string `json:"cwd,omitempty"`
		Archived  bool   `json:"archived"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]publicCh, 0, len(chans))
	for _, c := range chans {
		out = append(out, publicCh{
			Handle: c.Handle, Name: c.Name, Topic: c.Topic, Kind: c.Kind,
			SessionID: c.SessionID, Cwd: c.Cwd, Archived: c.Archived, CreatedAt: c.CreatedAt,
		})
	}
	jsonResponse(w, out)
}

// POST /client/cc/channels — create a new task channel.
// body: {name, topic?, cwd?}.
func (h *CCClientHandler) CreateChannel(w http.ResponseWriter, r *http.Request) {
	client, cc, botTok, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	var req struct {
		Name  string `json:"name"`
		Topic string `json:"topic"`
		Cwd   string `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		jsonError(w, "name required", http.StatusBadRequest)
		return
	}

	var cfg struct {
		GuildID    string `json:"guild_id"`
		CategoryID string `json:"category_id"`
	}
	_ = json.Unmarshal([]byte(cc.Config), &cfg)
	if cfg.GuildID == "" || cfg.CategoryID == "" {
		jsonError(w, "cc config missing guild_id/category_id", http.StatusInternalServerError)
		return
	}

	created, err := h.bot.CreateChannel(r.Context(), botTok, svc.CreateChannelOpts{
		GuildID:  cfg.GuildID,
		ParentID: cfg.CategoryID,
		Name:     req.Name,
		Topic:    req.Topic,
	})
	if err != nil {
		jsonError(w, "discord create channel: "+err.Error(), http.StatusBadGateway)
		return
	}

	handle, _ := svc.GenerateToken(12)
	handle = "dwch_" + handle
	clientID := client.ID
	row := &models.CCChannel{
		Handle: handle, CCID: cc.ID,
		ClientID:  &clientID,
		ChannelID: created.ID,
		Name:      created.Name,
		Topic:     req.Topic,
		Kind:      "task",
		Cwd:       req.Cwd,
	}
	if err := h.cc.CreateChannel(row); err != nil {
		_ = h.bot.ArchiveChannel(r.Context(), botTok, created.ID, created.Name)
		jsonError(w, "persist channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]interface{}{
		"handle": handle, "name": created.Name, "topic": req.Topic, "cwd": req.Cwd, "kind": "task",
	})
}

// POST /client/cc/channels/{handle}/archive — archive a task channel.
func (h *CCClientHandler) ArchiveChannel(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	_, cc, botTok, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, cc.ID, handle)
	if !ok {
		return
	}
	if ch.Kind == "management" {
		jsonError(w, "cannot archive the management channel — delete the CC instead", http.StatusBadRequest)
		return
	}
	if err := h.bot.ArchiveChannel(r.Context(), botTok, ch.ChannelID, ch.Name); err != nil {
		jsonError(w, "discord archive: "+err.Error(), http.StatusBadGateway)
		return
	}
	_ = h.cc.MarkChannelArchived(handle)
	jsonResponse(w, map[string]string{"status": "archived"})
}

// POST /client/cc/channels/{handle}/messages — post a message.
func (h *CCClientHandler) PostMessage(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	_, cc, botTok, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, cc.ID, handle)
	if !ok {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		jsonError(w, "content required", http.StatusBadRequest)
		return
	}
	id, err := h.bot.PostMessage(r.Context(), botTok, ch.ChannelID, req.Content)
	if err != nil {
		jsonError(w, "discord post: "+err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]string{"message_id": id})
}

// PATCH /client/cc/channels/{handle}/messages/{message_id} — edit.
func (h *CCClientHandler) EditMessage(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	msgID := r.PathValue("message_id")
	_, cc, botTok, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, cc.ID, handle)
	if !ok {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := h.bot.EditMessage(r.Context(), botTok, ch.ChannelID, msgID, req.Content); err != nil {
		jsonError(w, "discord edit: "+err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]string{"status": "edited"})
}

// DELETE /client/cc/channels/{handle}/messages/{message_id} — delete.
func (h *CCClientHandler) DeleteMessage(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	msgID := r.PathValue("message_id")
	_, cc, botTok, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, cc.ID, handle)
	if !ok {
		return
	}
	if err := h.bot.DeleteMessage(r.Context(), botTok, ch.ChannelID, msgID); err != nil {
		jsonError(w, "discord delete: "+err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}

// GET /client/cc/channels/{handle}/messages?limit=N — read recent.
func (h *CCClientHandler) GetMessages(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	_, cc, botTok, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, cc.ID, handle)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	msgs, err := h.bot.GetMessages(r.Context(), botTok, ch.ChannelID, limit)
	if err != nil {
		jsonError(w, "discord get messages: "+err.Error(), http.StatusBadGateway)
		return
	}
	type publicMsg struct {
		MessageID string `json:"message_id"`
		Handle    string `json:"channel_handle"`
		Content   string `json:"content"`
		Author    string `json:"author"`
		AuthorID  string `json:"author_id"`
		IsBot     bool   `json:"is_bot"`
		Timestamp string `json:"timestamp"`
	}
	out := make([]publicMsg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, publicMsg{
			MessageID: m.ID, Handle: handle, Content: m.Content,
			Author: m.Author.Username, AuthorID: m.Author.ID, IsBot: m.Author.Bot,
			Timestamp: m.Timestamp,
		})
	}
	jsonResponse(w, out)
}

// GET /client/cc/inbox — long-poll for queued gateway events.
func (h *CCClientHandler) PullInbox(w http.ResponseWriter, r *http.Request) {
	_, cc, _, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	timeout := 25
	if v := q.Get("timeout"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 60 {
			timeout = n
		}
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	var handles []string
	if v := q.Get("channels"); v != "" {
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				handles = append(handles, s)
			}
		}
	}
	for _, h2 := range handles {
		if ch, err := h.cc.GetChannelByHandle(h2); err != nil || ch.CCID != cc.ID {
			jsonError(w, "channel filter contains handle outside this cc", http.StatusForbidden)
			return
		}
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for {
		events, err := h.cc.PullInbox(cc.ID, since, handles, limit)
		if err != nil {
			jsonError(w, "inbox pull: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if len(events) > 0 || time.Now().After(deadline) || timeout == 0 {
			cursor := since
			for _, e := range events {
				if e.ID > cursor {
					cursor = e.ID
				}
			}
			if events == nil {
				events = []models.InboxEvent{}
			}
			jsonResponse(w, map[string]interface{}{
				"cursor": cursor,
				"events": events,
			})
			return
		}
		select {
		case <-r.Context().Done():
			jsonResponse(w, map[string]interface{}{"cursor": since, "events": []models.InboxEvent{}})
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// GET /client/cc/events — Server-Sent Events stream of live gateway
// dispatches for this client's CC. Used by `duckway cc watch` to wake up
// the moment a Discord message arrives instead of polling the inbox.
//
// Wire shape (one event per "frame"):
//
//	event: ready
//	data: {}
//
//	event: message_create
//	data: {"type":"message_create","cc_id":"...","channel_handle":"dwch_...","payload":{...}}
//
//	: heartbeat   ← every 30s, keeps the connection alive through proxies
//
// The inbox table is still authoritative for cold-start replay — if a
// daemon's connection drops it can /client/cc/inbox?since=<last_id> to
// fill the gap.
func (h *CCClientHandler) Events(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	if h.hub == nil {
		jsonError(w, "event hub not configured", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, "streaming not supported by this transport", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	sub, unsub := h.hub.Subscribe(client.ID)
	defer unsub()

	// Initial ready frame so the daemon knows the connection is live.
	fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		case <-heartbeat.C:
			// SSE comment frame — clients ignore it but it keeps any
			// idle-timeout proxies from killing the connection.
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// POST /client/cc/channels/{handle}/approval — start a reaction-based
// approval. Posts the question to the channel, pre-adds emoji
// reactions, blocks for up to timeout_seconds, returns the chosen
// option (or timed_out=true).
//
// body:
//
//	{
//	  "question": "Approve deploy?",          // required
//	  "options": ["approve","reject"],         // optional; default ["yes","no"]
//	  "timeout_seconds": 300,                  // optional 1..3600
//	  "required_reactors": ["123","456"]       // optional Discord user_ids
//	}
//
// response:
//
//	{
//	  "chosen": "approve",
//	  "emoji": "✅",
//	  "reactor_user_id": "123",
//	  "message_id": "1234567890",
//	  "timed_out": false
//	}
func (h *CCClientHandler) RequestApproval(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	_, cc, botTok, ok := h.resolveCC(w, r)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, cc.ID, handle)
	if !ok {
		return
	}
	if h.approvals == nil {
		jsonError(w, "approval registry not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Question         string   `json:"question"`
		Options          []string `json:"options"`
		TimeoutSeconds   int      `json:"timeout_seconds"`
		RequiredReactors []string `json:"required_reactors"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		jsonError(w, "question required", http.StatusBadRequest)
		return
	}
	if len(req.Options) == 0 {
		req.Options = []string{"yes", "no"}
	}
	if len(req.Options) > 10 {
		jsonError(w, "max 10 options", http.StatusBadRequest)
		return
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 300
	}
	if req.TimeoutSeconds > 3600 {
		req.TimeoutSeconds = 3600
	}

	// Build the question text + emoji map.
	emojiToOpt := map[string]string{}
	emojis := make([]string, len(req.Options))
	var body strings.Builder
	body.WriteString("**🗳️ Approval requested:** ")
	body.WriteString(req.Question)
	body.WriteString("\n")
	for i, opt := range req.Options {
		e := svc.DefaultEmojiForOption(i, len(req.Options))
		emojis[i] = e
		emojiToOpt[e] = opt
		fmt.Fprintf(&body, "  %s  %s\n", e, opt)
	}
	if len(req.RequiredReactors) > 0 {
		fmt.Fprintf(&body, "_Only these users can decide: <@%s>_", strings.Join(req.RequiredReactors, ">, <@"))
	}

	originalBody := body.String()
	msgID, err := h.bot.PostMessage(r.Context(), botTok, ch.ChannelID, originalBody)
	if err != nil {
		jsonError(w, "discord post: "+err.Error(), http.StatusBadGateway)
		return
	}
	for _, e := range emojis {
		if err := h.bot.AddReaction(r.Context(), botTok, ch.ChannelID, msgID, e); err != nil {
			// Best-effort — partial reactions still let humans choose
			// the ones the bot did manage to add.
		}
	}

	resultCh := h.approvals.Register(msgID, emojiToOpt, req.RequiredReactors)
	timer := time.NewTimer(time.Duration(req.TimeoutSeconds) * time.Second)
	defer timer.Stop()

	// editWithFinal rewrites the question message with the resolution
	// so humans see what was chosen + by whom without hunting for the
	// reaction. Best-effort — Discord errors don't change the response.
	editWithFinal := func(footer string) {
		final := originalBody + "\n" + footer
		_ = h.bot.EditMessage(r.Context(), botTok, ch.ChannelID, msgID, final)
	}

	select {
	case res := <-resultCh:
		editWithFinal(fmt.Sprintf("\n**✅ Decided:** %s %s by <@%s>",
			res.Emoji, res.Chosen, res.ReactorUserID))
		jsonResponse(w, res)
	case <-r.Context().Done():
		h.approvals.Cancel(msgID)
		jsonResponse(w, svc.ApprovalResult{MessageID: msgID, TimedOut: true})
	case <-timer.C:
		h.approvals.Cancel(msgID)
		editWithFinal("\n**⏱️ Timed out** without a response.")
		jsonResponse(w, svc.ApprovalResult{MessageID: msgID, TimedOut: true})
	}
}
