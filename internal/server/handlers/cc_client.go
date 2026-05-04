package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

// CCClientHandler exposes the CC operations agents need: list assigned CCs,
// create/list/archive channels under a CC's category, post/edit/delete/read
// messages, long-poll the inbox.
//
// All endpoints authenticate via the existing ClientAuth middleware
// (X-Duckway-Token). Real Discord IDs (channel_id, guild_id, category_id)
// never leave the server — agents work with opaque "handles" instead.
type CCClientHandler struct {
	cc      *queries.ControlChannelQueries
	apiKeys *queries.APIKeyQueries
	crypto  *svc.Crypto
	bot     *svc.DiscordBot
}

func NewCCClientHandler(cc *queries.ControlChannelQueries, apiKeys *queries.APIKeyQueries, crypto *svc.Crypto, bot *svc.DiscordBot) *CCClientHandler {
	return &CCClientHandler{cc: cc, apiKeys: apiKeys, crypto: crypto, bot: bot}
}

// resolveAssignment looks up the (client, cc) binding and returns the bot
// token + cc detail. Returns nil + writes the response on auth/scoping
// failure.
func (h *CCClientHandler) resolveAssignment(w http.ResponseWriter, r *http.Request, ccID string) (*models.Client, *models.ControlChannel, string, bool) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return nil, nil, "", false
	}
	if _, err := h.cc.GetAssignment(client.ID, ccID); err != nil {
		jsonError(w, "client not assigned to this CC", http.StatusForbidden)
		return nil, nil, "", false
	}
	cc, err := h.cc.GetByID(ccID)
	if err != nil {
		jsonError(w, "cc not found", http.StatusNotFound)
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

// resolveHandle translates a handle on a path into the real Discord channel
// id, after verifying the handle belongs to the requested CC. Path-level
// ACL: a client cannot mention a handle that isn't in their CC even if they
// guess a valid one.
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

// GET /client/cc — list CCs this client is assigned to.
func (h *CCClientHandler) ListAssigned(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}
	out, err := h.cc.AssignmentsForClient(client.ID)
	if err != nil {
		jsonError(w, "list failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []models.ClientCCDetail{}
	}
	jsonResponse(w, out)
}

// GET /client/cc/{cc_id}/channels — list channels in this CC. handle/name/
// topic only. real channel_id stays server-side.
func (h *CCClientHandler) ListChannels(w http.ResponseWriter, r *http.Request) {
	ccID := r.PathValue("cc_id")
	if _, _, _, ok := h.resolveAssignment(w, r, ccID); !ok {
		return
	}
	chans, err := h.cc.ListChannels(ccID)
	if err != nil {
		jsonError(w, "list channels: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type publicCh struct {
		Handle    string `json:"handle"`
		Name      string `json:"name"`
		Topic     string `json:"topic"`
		IsHome    bool   `json:"is_home"`
		Archived  bool   `json:"archived"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]publicCh, 0, len(chans))
	for _, c := range chans {
		out = append(out, publicCh{
			Handle: c.Handle, Name: c.Name, Topic: c.Topic,
			IsHome: c.IsHome, Archived: c.Archived, CreatedAt: c.CreatedAt,
		})
	}
	jsonResponse(w, out)
}

// POST /client/cc/{cc_id}/channels — create new channel under the CC's
// category. body: {name, topic?}. Returns the new handle.
func (h *CCClientHandler) CreateChannel(w http.ResponseWriter, r *http.Request) {
	ccID := r.PathValue("cc_id")
	client, cc, botTok, ok := h.resolveAssignment(w, r, ccID)
	if !ok {
		return
	}
	var req struct {
		Name  string `json:"name"`
		Topic string `json:"topic"`
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
		IsHome:    false,
	}
	if err := h.cc.CreateChannel(row); err != nil {
		// Local persist failed — undo the Discord side so we don't leak a
		// channel without a handle.
		_ = h.bot.ArchiveChannel(r.Context(), botTok, created.ID, created.Name)
		jsonError(w, "persist channel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]interface{}{
		"handle": handle, "name": created.Name, "topic": req.Topic,
	})
}

// POST /client/cc/{cc_id}/channels/{handle}/archive — archive channel.
func (h *CCClientHandler) ArchiveChannel(w http.ResponseWriter, r *http.Request) {
	ccID := r.PathValue("cc_id")
	handle := r.PathValue("handle")
	_, _, botTok, ok := h.resolveAssignment(w, r, ccID)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, ccID, handle)
	if !ok {
		return
	}
	if ch.IsHome {
		jsonError(w, "cannot archive a home channel — unassign the CC instead", http.StatusBadRequest)
		return
	}
	if err := h.bot.ArchiveChannel(r.Context(), botTok, ch.ChannelID, ch.Name); err != nil {
		jsonError(w, "discord archive: "+err.Error(), http.StatusBadGateway)
		return
	}
	_ = h.cc.MarkChannelArchived(handle)
	jsonResponse(w, map[string]string{"status": "archived"})
}

// POST /client/cc/{cc_id}/channels/{handle}/messages — post a message.
func (h *CCClientHandler) PostMessage(w http.ResponseWriter, r *http.Request) {
	ccID := r.PathValue("cc_id")
	handle := r.PathValue("handle")
	_, _, botTok, ok := h.resolveAssignment(w, r, ccID)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, ccID, handle)
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

// PATCH /client/cc/{cc_id}/channels/{handle}/messages/{message_id} — edit.
func (h *CCClientHandler) EditMessage(w http.ResponseWriter, r *http.Request) {
	ccID := r.PathValue("cc_id")
	handle := r.PathValue("handle")
	msgID := r.PathValue("message_id")
	_, _, botTok, ok := h.resolveAssignment(w, r, ccID)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, ccID, handle)
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

// DELETE /client/cc/{cc_id}/channels/{handle}/messages/{message_id} — delete.
func (h *CCClientHandler) DeleteMessage(w http.ResponseWriter, r *http.Request) {
	ccID := r.PathValue("cc_id")
	handle := r.PathValue("handle")
	msgID := r.PathValue("message_id")
	_, _, botTok, ok := h.resolveAssignment(w, r, ccID)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, ccID, handle)
	if !ok {
		return
	}
	if err := h.bot.DeleteMessage(r.Context(), botTok, ch.ChannelID, msgID); err != nil {
		jsonError(w, "discord delete: "+err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}

// GET /client/cc/{cc_id}/channels/{handle}/messages?limit=N — read recent.
func (h *CCClientHandler) GetMessages(w http.ResponseWriter, r *http.Request) {
	ccID := r.PathValue("cc_id")
	handle := r.PathValue("handle")
	_, _, botTok, ok := h.resolveAssignment(w, r, ccID)
	if !ok {
		return
	}
	ch, ok := h.resolveHandle(w, ccID, handle)
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
	// Repackage so we don't leak channel_id; expose handle instead.
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

// GET /client/cc/{cc_id}/inbox — long-poll for queued gateway events.
//
// Query params:
//
//	since=N        — only events with id > N (cursor)
//	channels=h1,h2 — optional handle filter (comma-separated)
//	timeout=30     — seconds to wait for new events (default 25, max 60).
//	                 Returns immediately if events are already buffered.
//	limit=N        — max events to return per call (default 200, max 500).
//
// Response: {"cursor": <highest_id>, "events": [...]}
func (h *CCClientHandler) PullInbox(w http.ResponseWriter, r *http.Request) {
	ccID := r.PathValue("cc_id")
	if _, _, _, ok := h.resolveAssignment(w, r, ccID); !ok {
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

	// Validate filter handles belong to this CC. Defensive — keeps a
	// compromised agent from peeking into another CC's events even if the
	// inbox accidentally cross-tagged.
	for _, h2 := range handles {
		if ch, err := h.cc.GetChannelByHandle(h2); err != nil || ch.CCID != ccID {
			jsonError(w, "channel filter contains handle outside this cc", http.StatusForbidden)
			return
		}
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for {
		events, err := h.cc.PullInbox(ccID, since, handles, limit)
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
		// Cheap idle wait. Long-poll callers should set timeout=0 for
		// no-wait mode if they're polling on their own cadence.
		select {
		case <-r.Context().Done():
			jsonResponse(w, map[string]interface{}{"cursor": since, "events": []models.InboxEvent{}})
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
