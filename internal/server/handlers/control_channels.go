package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

// ControlChannelHandler covers admin CRUD for the CC concept. CC v2:
// 1:1 client↔CC. Create both provisions the management channel and
// issues the bot phantom token in one go.
type ControlChannelHandler struct {
	cc           *queries.ControlChannelQueries
	apiKeys      *queries.APIKeyQueries
	placeholders *queries.PlaceholderQueries
	services     *queries.ServiceQueries
	clients      *queries.ClientQueries
	settings     *queries.SettingsQueries
	crypto       *svc.Crypto
	bot          *svc.DiscordBot
	hub          *svc.CCEventHub         // optional, set via SetHub
	approvals    *svc.CCApprovalRegistry // optional, set via SetApprovals
}

func (h *ControlChannelHandler) discordBotToken(apiKeyID string) (string, error) {
	key, err := h.apiKeys.GetByID(apiKeyID)
	if err != nil {
		return "", fmt.Errorf("api_key not found")
	}
	svcRow, err := h.services.GetByID(key.ServiceID)
	if err != nil {
		return "", fmt.Errorf("service lookup failed")
	}
	if svcRow.Name != "discord" {
		return "", fmt.Errorf("api_key is for %s, not discord", svcRow.Name)
	}
	botToken, err := h.crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		return "", fmt.Errorf("decrypt bot token: %w", err)
	}
	return botToken, nil
}

// GET /api/cc/discord/setup?api_key_id=...
func (h *ControlChannelHandler) DiscordSetup(w http.ResponseWriter, r *http.Request) {
	apiKeyID := r.URL.Query().Get("api_key_id")
	if apiKeyID == "" {
		jsonError(w, "api_key_id is required", http.StatusBadRequest)
		return
	}
	botToken, err := h.discordBotToken(apiKeyID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	user, err := h.bot.CurrentUser(r.Context(), botToken)
	if err != nil {
		jsonError(w, "discord current user: "+err.Error(), http.StatusBadGateway)
		return
	}
	guilds, err := h.bot.ListGuilds(r.Context(), botToken)
	guildsErr := ""
	if err != nil {
		guildsErr = err.Error()
		guilds = []svc.Guild{}
	}
	jsonResponse(w, map[string]interface{}{
		"bot":              user,
		"invite_url":       svc.DiscordInviteURL(user.ID),
		"setup_invite_url": svc.DiscordSetupInviteURL(user.ID),
		"guilds":           guilds,
		"guilds_error":     guildsErr,
	})
}

// GET /api/cc/discord/categories?api_key_id=...&guild_id=...
func (h *ControlChannelHandler) DiscordCategories(w http.ResponseWriter, r *http.Request) {
	apiKeyID := r.URL.Query().Get("api_key_id")
	guildID := r.URL.Query().Get("guild_id")
	if apiKeyID == "" || guildID == "" {
		jsonError(w, "api_key_id and guild_id are required", http.StatusBadRequest)
		return
	}
	botToken, err := h.discordBotToken(apiKeyID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	channels, err := h.bot.ListGuildChannels(r.Context(), botToken, guildID)
	if err != nil {
		jsonError(w, "discord channels: "+err.Error(), http.StatusBadGateway)
		return
	}
	categories := make([]svc.Channel, 0)
	for _, ch := range channels {
		if ch.Type == 4 {
			categories = append(categories, ch)
		}
	}
	jsonResponse(w, map[string]interface{}{"categories": categories})
}

// POST /api/cc/discord/categories
func (h *ControlChannelHandler) DiscordCreateCategory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKeyID string `json:"api_key_id"`
		GuildID  string `json:"guild_id"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.APIKeyID == "" || req.GuildID == "" || req.Name == "" {
		jsonError(w, "api_key_id, guild_id, and name are required", http.StatusBadRequest)
		return
	}
	botToken, err := h.discordBotToken(req.APIKeyID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	category, err := h.bot.CreateCategory(r.Context(), botToken, req.GuildID, req.Name)
	if err != nil {
		jsonError(w, "discord create category: "+err.Error(), http.StatusBadGateway)
		return
	}
	resp := map[string]interface{}{
		"id":       category.ID,
		"name":     category.Name,
		"type":     category.Type,
		"guild_id": category.GuildID,
	}
	if category.ParentID != nil {
		resp["parent_id"] = *category.ParentID
	}
	if user, err := h.bot.CurrentUser(r.Context(), botToken); err != nil {
		resp["permission_warning"] = "category created, but bot identity lookup failed: " + err.Error()
	} else if err := h.bot.GrantCategoryAccess(r.Context(), botToken, category.ID, user.ID); err != nil {
		resp["permission_warning"] = "category created, but permission setup failed: " + err.Error()
	} else {
		resp["permissions_applied"] = true
	}
	jsonResponse(w, resp)
}

// POST /api/cc/discord/category-permissions
func (h *ControlChannelHandler) DiscordGrantCategoryPermissions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKeyID   string `json:"api_key_id"`
		CategoryID string `json:"category_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.APIKeyID == "" || req.CategoryID == "" {
		jsonError(w, "api_key_id and category_id are required", http.StatusBadRequest)
		return
	}
	botToken, err := h.discordBotToken(req.APIKeyID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	user, err := h.bot.CurrentUser(r.Context(), botToken)
	if err != nil {
		jsonError(w, "discord current user: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := h.bot.GrantCategoryAccess(r.Context(), botToken, req.CategoryID, user.ID); err != nil {
		jsonError(w, "discord grant category permissions: "+err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true})
}

// POST /api/cc/discord/preflight
func (h *ControlChannelHandler) DiscordPreflight(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKeyID   string `json:"api_key_id"`
		GuildID    string `json:"guild_id"`
		CategoryID string `json:"category_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.APIKeyID == "" || req.GuildID == "" || req.CategoryID == "" {
		jsonError(w, "api_key_id, guild_id, and category_id are required", http.StatusBadRequest)
		return
	}
	botToken, err := h.discordBotToken(req.APIKeyID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	type step struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Note string `json:"note,omitempty"`
	}
	steps := []step{}
	testName := fmt.Sprintf("duckway-preflight-%d", time.Now().Unix())
	created, err := h.bot.CreateChannel(r.Context(), botToken, svc.CreateChannelOpts{
		GuildID:  req.GuildID,
		ParentID: req.CategoryID,
		Name:     testName,
		Topic:    "Temporary channel created by Duckway to verify Discord permissions.",
	})
	if err != nil {
		steps = append(steps, step{Name: "create test channel", OK: false, Note: err.Error()})
		jsonResponse(w, map[string]interface{}{"ok": false, "steps": steps})
		return
	}
	steps = append(steps, step{Name: "create test channel", OK: true, Note: created.Name})
	cleanup := func() {
		if derr := h.bot.DeleteChannel(contextWithoutCancel(r.Context()), botToken, created.ID); derr != nil {
			steps = append(steps, step{Name: "delete test channel", OK: false, Note: derr.Error() + " — remove " + created.Name + " manually"})
		} else {
			steps = append(steps, step{Name: "delete test channel", OK: true})
		}
	}

	msgID, err := h.bot.PostMessage(r.Context(), botToken, created.ID, "Duckway Discord permission preflight.")
	if err != nil {
		steps = append(steps, step{Name: "send message", OK: false, Note: err.Error()})
		cleanup()
		jsonResponse(w, map[string]interface{}{"ok": false, "steps": steps})
		return
	}
	steps = append(steps, step{Name: "send message", OK: true})

	if err := h.bot.AddReaction(r.Context(), botToken, created.ID, msgID, "✅"); err != nil {
		steps = append(steps, step{Name: "add reaction", OK: false, Note: err.Error()})
		cleanup()
		jsonResponse(w, map[string]interface{}{"ok": false, "steps": steps})
		return
	}
	steps = append(steps, step{Name: "add reaction", OK: true})

	if _, err := h.bot.GetMessages(r.Context(), botToken, created.ID, 1); err != nil {
		steps = append(steps, step{Name: "read message history", OK: false, Note: err.Error()})
		cleanup()
		jsonResponse(w, map[string]interface{}{"ok": false, "steps": steps})
		return
	}
	steps = append(steps, step{Name: "read message history", OK: true})
	cleanup()
	ok := true
	for _, s := range steps {
		if !s.OK {
			ok = false
			break
		}
	}
	jsonResponse(w, map[string]interface{}{"ok": ok, "steps": steps})
}

func contextWithoutCancel(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func NewControlChannelHandler(cc *queries.ControlChannelQueries, apiKeys *queries.APIKeyQueries, placeholders *queries.PlaceholderQueries, services *queries.ServiceQueries, clients *queries.ClientQueries, settings *queries.SettingsQueries, crypto *svc.Crypto, bot *svc.DiscordBot) *ControlChannelHandler {
	return &ControlChannelHandler{
		cc: cc, apiKeys: apiKeys, placeholders: placeholders,
		services: services, clients: clients, settings: settings,
		crypto: crypto, bot: bot,
	}
}

// GET /api/cc — list all CCs.
func (h *ControlChannelHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.cc.List()
	if err != nil {
		jsonError(w, "list failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.ControlChannel{}
	}
	jsonResponse(w, list)
}

// GET /api/cc/{id} — detail with the channel list.
func (h *ControlChannelHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.cc.GetByID(id)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	c.Channels, _ = h.cc.ListChannels(id)
	if c.Channels == nil {
		c.Channels = []models.CCChannel{}
	}
	jsonResponse(w, c)
}

// POST /api/cc — create a CC for a specific client. Provisions the
// management channel under the configured Discord category and issues a
// phantom token bound to the bot api_key + this client. Refuses if the
// client already has a CC (1:1 invariant).
func (h *ControlChannelHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string          `json:"name"`
		ServiceID string          `json:"service_id"`
		APIKeyID  string          `json:"api_key_id"`
		ClientID  string          `json:"client_id"`
		AgentType string          `json:"agent_type"`
		Config    json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.ServiceID == "" || req.APIKeyID == "" || req.ClientID == "" {
		jsonError(w, "name, service_id, api_key_id, client_id are required", http.StatusBadRequest)
		return
	}
	if req.AgentType == "" {
		req.AgentType = "claude_code"
	}
	if !validCCAgentType(req.AgentType) {
		jsonError(w, "unknown agent_type: "+req.AgentType, http.StatusBadRequest)
		return
	}

	cl, err := h.clients.GetByID(req.ClientID)
	if err != nil {
		jsonError(w, "client not found", http.StatusBadRequest)
		return
	}
	if existing, _ := h.cc.GetByClientID(req.ClientID); existing != nil {
		jsonError(w, "client already has a CC: "+existing.Name, http.StatusConflict)
		return
	}

	key, err := h.apiKeys.GetByID(req.APIKeyID)
	if err != nil {
		jsonError(w, "api_key not found", http.StatusBadRequest)
		return
	}
	if key.ServiceID != req.ServiceID {
		jsonError(w, "api_key does not belong to the chosen service", http.StatusBadRequest)
		return
	}

	svcRow, err := h.services.GetByID(req.ServiceID)
	if err != nil {
		jsonError(w, "service not found", http.StatusBadRequest)
		return
	}
	if !isCCMessageService(svcRow.Name) {
		jsonError(w, "service "+svcRow.Name+" is not a message-channel service", http.StatusBadRequest)
		return
	}
	configStr := "{}"
	if len(req.Config) > 0 {
		configStr = string(req.Config)
	}
	if svcRow.Name == "discord" {
		var cfg struct {
			GuildID    string `json:"guild_id"`
			CategoryID string `json:"category_id"`
		}
		if err := json.Unmarshal([]byte(configStr), &cfg); err != nil {
			jsonError(w, "invalid discord config json", http.StatusBadRequest)
			return
		}
		if cfg.GuildID == "" || cfg.CategoryID == "" {
			jsonError(w, "discord config requires guild_id and category_id", http.StatusBadRequest)
			return
		}
	}

	// Decrypt bot token + provision management channel before persisting.
	botToken, err := h.crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		jsonError(w, "decrypt bot token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var realChannelID, channelName string
	switch svcRow.Name {
	case "discord":
		var cfg struct {
			GuildID    string `json:"guild_id"`
			CategoryID string `json:"category_id"`
		}
		_ = json.Unmarshal([]byte(configStr), &cfg)
		ch, err := h.bot.CreateChannel(r.Context(), botToken, svc.CreateChannelOpts{
			GuildID:  cfg.GuildID,
			ParentID: cfg.CategoryID,
			Name:     cl.Name + "-control",
			Topic:    "Duckway control channel for " + cl.Name + " — type !help to see commands.",
		})
		if err != nil {
			jsonError(w, "discord create channel: "+err.Error(), http.StatusBadGateway)
			return
		}
		realChannelID = ch.ID
		channelName = ch.Name
	default:
		jsonError(w, "service "+svcRow.Name+" not supported for CC yet", http.StatusBadRequest)
		return
	}

	// Issue phantom token — env_name suffixed with cc_id so a client can
	// theoretically hold multiple CCs across different services.
	ccID, _ := svc.GenerateToken(8)
	envName := "DUCKWAY_CC_" + ccID
	if len(envName) > 60 {
		envName = envName[:60]
	}
	keyPrefix := svcRow.KeyPrefix
	if keyPrefix == "" {
		keyPrefix = "dw_cc_"
	}
	keyLength := svcRow.KeyLength
	if keyLength <= 0 {
		keyLength = 64
	}
	placeholderStr, err := svc.GeneratePlaceholder(keyPrefix, keyLength)
	if err != nil {
		_ = h.bot.ArchiveChannel(r.Context(), botToken, realChannelID, channelName)
		jsonError(w, "generate placeholder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	phID, _ := svc.GenerateToken(16)
	apiKeyIDPtr := req.APIKeyID
	ph := &models.PlaceholderKey{
		ID:                 phID,
		EnvName:            envName,
		Placeholder:        placeholderStr,
		ServiceID:          req.ServiceID,
		APIKeyID:           &apiKeyIDPtr,
		ClientID:           req.ClientID,
		RequiresApproval:   false,
		ApprovalTTLMinutes: 1440,
		IsActive:           true,
	}
	if err := h.placeholders.Create(ph); err != nil {
		_ = h.bot.ArchiveChannel(r.Context(), botToken, realChannelID, channelName)
		jsonError(w, "issue placeholder: "+err.Error(), http.StatusInternalServerError)
		return
	}

	c := &models.ControlChannel{
		ID:            ccID,
		Name:          req.Name,
		ServiceID:     req.ServiceID,
		APIKeyID:      req.APIKeyID,
		ClientID:      req.ClientID,
		AgentType:     req.AgentType,
		PlaceholderID: phID,
		Config:        configStr,
		IsActive:      true,
	}
	if err := h.cc.Create(c); err != nil {
		_ = h.placeholders.Delete(phID)
		_ = h.bot.ArchiveChannel(r.Context(), botToken, realChannelID, channelName)
		jsonError(w, "persist cc: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Persist the management channel row.
	mgmtHandle, _ := svc.GenerateToken(12)
	mgmtHandle = "dwch_" + mgmtHandle
	clientIDPtr := req.ClientID
	if err := h.cc.CreateChannel(&models.CCChannel{
		Handle:    mgmtHandle,
		CCID:      ccID,
		ClientID:  &clientIDPtr,
		ChannelID: realChannelID,
		Name:      channelName,
		Topic:     "Duckway control channel — type !help",
		Kind:      "management",
	}); err != nil {
		// Roll back everything we created.
		_ = h.cc.Delete(ccID)
		_ = h.placeholders.Delete(phID)
		_ = h.bot.ArchiveChannel(r.Context(), botToken, realChannelID, channelName)
		jsonError(w, "persist channel: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Post a welcome / how-to message so the first thing the user sees
	// in the brand-new channel is the command list. Best-effort.
	welcome := svc.BuildWelcomeMessage(cl.Name)
	// Best-effort — channel exists; user can still type !help themselves.
	_, _ = h.bot.PostMessage(r.Context(), botToken, realChannelID, welcome)

	full, _ := h.cc.GetByID(ccID)
	if full == nil {
		full = c
	}
	full.Channels, _ = h.cc.ListChannels(ccID)
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, full)
}

func isCCMessageService(serviceName string) bool {
	switch serviceName {
	case "discord", "telegram":
		return true
	default:
		return false
	}
}

// PUT /api/cc/{id} — update name / config / is_active.
func (h *ControlChannelHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, err := h.cc.GetByID(id)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	var req struct {
		APIKeyID  string          `json:"api_key_id"`
		AgentType string          `json:"agent_type"`
		Name      string          `json:"name"`
		Config    json.RawMessage `json:"config"`
		IsActive  *bool           `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	name := req.Name
	if name == "" {
		name = cur.Name
	}
	agentType := cur.AgentType
	if req.AgentType != "" {
		if !validCCAgentType(req.AgentType) {
			jsonError(w, "unknown agent_type: "+req.AgentType, http.StatusBadRequest)
			return
		}
		agentType = req.AgentType
	}
	apiKeyID := cur.APIKeyID
	if req.APIKeyID != "" {
		key, err := h.apiKeys.GetByID(req.APIKeyID)
		if err != nil {
			jsonError(w, "api key not found", http.StatusBadRequest)
			return
		}
		if key.ServiceID != cur.ServiceID {
			jsonError(w, "api key must belong to the control channel service", http.StatusBadRequest)
			return
		}
		if !key.IsActive {
			jsonError(w, "api key is inactive", http.StatusBadRequest)
			return
		}
		apiKeyID = req.APIKeyID
	}
	configStr := cur.Config
	if len(req.Config) > 0 {
		configStr = string(req.Config)
	}
	active := cur.IsActive
	if req.IsActive != nil {
		active = *req.IsActive
	}
	if err := h.cc.Update(id, name, apiKeyID, agentType, configStr, active); err != nil {
		jsonError(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "updated"})
}

func validCCAgentType(agentType string) bool {
	switch agentType {
	case "claude_code", "codex":
		return true
	default:
		return false
	}
}

// DELETE /api/cc/{id} — archive every channel under the CC, drop the
// placeholder, then drop the row (cascades cc_channels rows).
func (h *ControlChannelHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cc, err := h.cc.GetByID(id)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	channels, _ := h.cc.ListChannels(id)
	if len(channels) > 0 && h.bot != nil {
		botToken, decErr := h.crypto.Decrypt("")
		if decErr == nil {
			botToken = ""
		}
		if k, kerr := h.apiKeys.GetByID(cc.APIKeyID); kerr == nil {
			if t, terr := h.crypto.Decrypt(k.KeyEncrypted); terr == nil {
				botToken = t
			}
		}
		if botToken != "" {
			ctx := r.Context()
			for _, ch := range channels {
				if ch.ChannelID == "" || ch.Archived {
					continue
				}
				_ = h.bot.ArchiveChannel(ctx, botToken, ch.ChannelID, ch.Name)
			}
		}
	}

	if err := h.cc.Delete(id); err != nil {
		jsonError(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if cc.PlaceholderID != "" {
		_ = h.placeholders.Delete(cc.PlaceholderID)
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}

// POST /api/cc/{id}/test — round-trip Discord with the CC's credentials.
// Unchanged from v1 — useful for verifying bot/category access after creating.
func (h *ControlChannelHandler) Test(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cc, err := h.cc.GetByID(id)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	svcRow, err := h.services.GetByID(cc.ServiceID)
	if err != nil {
		jsonError(w, "service lookup failed", http.StatusInternalServerError)
		return
	}
	if svcRow.Name != "discord" {
		jsonError(w, "test only supports discord CCs", http.StatusBadRequest)
		return
	}

	type step struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Note string `json:"note,omitempty"`
	}
	steps := []step{}

	key, err := h.apiKeys.GetByID(cc.APIKeyID)
	if err != nil {
		steps = append(steps, step{Name: "load bot key", OK: false, Note: err.Error()})
		jsonResponse(w, map[string]interface{}{"ok": false, "steps": steps})
		return
	}
	botToken, err := h.crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		steps = append(steps, step{Name: "decrypt bot token", OK: false, Note: err.Error()})
		jsonResponse(w, map[string]interface{}{"ok": false, "steps": steps})
		return
	}
	steps = append(steps, step{Name: "decrypt bot token", OK: true})

	var cfg struct {
		GuildID    string `json:"guild_id"`
		CategoryID string `json:"category_id"`
	}
	if err := json.Unmarshal([]byte(cc.Config), &cfg); err != nil || cfg.GuildID == "" || cfg.CategoryID == "" {
		steps = append(steps, step{Name: "parse cc config", OK: false, Note: "guild_id/category_id missing"})
		jsonResponse(w, map[string]interface{}{"ok": false, "steps": steps})
		return
	}
	steps = append(steps, step{Name: "parse cc config", OK: true,
		Note: fmt.Sprintf("guild=%s category=%s", cfg.GuildID, cfg.CategoryID)})

	testName := fmt.Sprintf("duckway-test-%d", time.Now().Unix())
	created, err := h.bot.CreateChannel(r.Context(), botToken, svc.CreateChannelOpts{
		GuildID:  cfg.GuildID,
		ParentID: cfg.CategoryID,
		Name:     testName,
		Topic:    "Temporary channel created by Duckway to verify CC connectivity.",
	})
	if err != nil {
		steps = append(steps, step{Name: "create test channel", OK: false, Note: err.Error()})
		jsonResponse(w, map[string]interface{}{"ok": false, "steps": steps})
		return
	}
	steps = append(steps, step{Name: "create test channel", OK: true,
		Note: fmt.Sprintf("name=%s id=%s", created.Name, created.ID)})

	if err := h.bot.DeleteChannel(r.Context(), botToken, created.ID); err != nil {
		steps = append(steps, step{Name: "delete test channel", OK: false,
			Note: err.Error() + " — please remove " + testName + " manually"})
		jsonResponse(w, map[string]interface{}{"ok": false, "steps": steps})
		return
	}
	steps = append(steps, step{Name: "delete test channel", OK: true})

	jsonResponse(w, map[string]interface{}{"ok": true, "steps": steps})
}

// POST /api/cc/{id}/test-agent — publish a synthetic "hi" message to the
// management channel so the connected duckway cc watch daemon starts the
// configured agent. This does not touch Discord; it tests the agent path.
func (h *ControlChannelHandler) TestAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cc, err := h.cc.GetByID(id)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if !cc.IsActive {
		jsonError(w, "control channel is inactive; reassign a key and activate it first", http.StatusBadRequest)
		return
	}
	if h.hub == nil {
		jsonError(w, "hub not configured", http.StatusServiceUnavailable)
		return
	}
	channels, err := h.cc.ListChannels(id)
	if err != nil {
		jsonError(w, "list channels failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var mgmt *models.CCChannel
	for i := range channels {
		if channels[i].Kind == "management" && !channels[i].Archived {
			mgmt = &channels[i]
			break
		}
	}
	if mgmt == nil {
		jsonError(w, "management channel not found", http.StatusBadRequest)
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"id":         fmt.Sprintf("duckway-admin-test-%d", time.Now().UnixNano()),
		"channel_id": mgmt.ChannelID,
		"content":    "hi",
		"author": map[string]interface{}{
			"id":       "duckway-admin",
			"username": "Duckway Admin",
			"bot":      false,
		},
	})
	subscribers := h.hub.SubscriberCount(cc.ClientID)
	h.hub.Publish(cc.ClientID, svc.CCEvent{
		Type:    "message_create",
		CCID:    cc.ID,
		Handle:  mgmt.Handle,
		Kind:    mgmt.Kind,
		Payload: payload,
	})
	jsonResponse(w, map[string]interface{}{
		"status":      "published",
		"message":     "hi",
		"handle":      mgmt.Handle,
		"agent_type":  cc.AgentType,
		"subscribers": subscribers,
	})
}

// SetHub wires an event hub into the handler so the debug InjectEvent
// endpoint can publish synthetic events for e2e tests. Optional.
func (h *ControlChannelHandler) SetHub(hub *svc.CCEventHub) { h.hub = hub }

// SetApprovals lets the debug InjectEvent endpoint resolve approval
// reactions in tests. Optional.
func (h *ControlChannelHandler) SetApprovals(r *svc.CCApprovalRegistry) { h.approvals = r }

// POST /api/cc/{id}/inject_event — DEBUG ONLY. Either:
//   - publishes a synthetic CCEvent to the hub (default), or
//   - triggers an approval reaction (when type="reaction_add", body
//     carries message_id + emoji + reactor_user_id).
//
// Used by the e2e suite to drive the daemon + approval flows without a
// real Discord WSS gateway. Gated on DUCKWAY_CC_DEBUG_INJECT=1.
func (h *ControlChannelHandler) InjectEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cc, err := h.cc.GetByID(id)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	var req struct {
		Type      string          `json:"type"`
		Handle    string          `json:"channel_handle"`
		Payload   json.RawMessage `json:"payload"`
		MessageID string          `json:"message_id"`
		Emoji     string          `json:"emoji"`
		UserID    string          `json:"reactor_user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	if req.Type == "reaction_add" {
		if h.approvals == nil {
			jsonError(w, "approvals registry not configured", http.StatusServiceUnavailable)
			return
		}
		ok := h.approvals.Resolve(req.MessageID, req.Emoji, req.UserID)
		jsonResponse(w, map[string]interface{}{"status": "published", "resolved": ok})
		return
	}

	if h.hub == nil {
		jsonError(w, "hub not configured", http.StatusServiceUnavailable)
		return
	}
	h.hub.Publish(cc.ClientID, svc.CCEvent{
		Type:    req.Type,
		CCID:    cc.ID,
		Handle:  req.Handle,
		Payload: req.Payload,
	})
	jsonResponse(w, map[string]string{"status": "published"})
}
