package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

// ControlChannelHandler covers admin CRUD for the CC concept and the
// Discord-bot side-effects that go with assignment (auto-creating a home
// channel, archiving on tear-down).
type ControlChannelHandler struct {
	cc           *queries.ControlChannelQueries
	apiKeys      *queries.APIKeyQueries
	placeholders *queries.PlaceholderQueries
	services     *queries.ServiceQueries
	clients      *queries.ClientQueries
	settings     *queries.SettingsQueries
	crypto       *svc.Crypto
	bot          *svc.DiscordBot
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

// GET /api/cc/{id} — detail with assignments.
func (h *ControlChannelHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.cc.GetByID(id)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	c.Assignments, _ = h.cc.AssignmentsForCC(id)
	if c.Assignments == nil {
		c.Assignments = []models.ClientCCDetail{}
	}
	jsonResponse(w, c)
}

// POST /api/cc — create.
func (h *ControlChannelHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string          `json:"name"`
		ServiceID string          `json:"service_id"`
		APIKeyID  string          `json:"api_key_id"`
		Config    json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.ServiceID == "" || req.APIKeyID == "" {
		jsonError(w, "name, service_id, api_key_id are required", http.StatusBadRequest)
		return
	}

	// Validate the API key belongs to the same service.
	key, err := h.apiKeys.GetByID(req.APIKeyID)
	if err != nil {
		jsonError(w, "api_key not found", http.StatusBadRequest)
		return
	}
	if key.ServiceID != req.ServiceID {
		jsonError(w, "api_key does not belong to the chosen service", http.StatusBadRequest)
		return
	}

	// Service-specific config validation.
	svcRow, err := h.services.GetByID(req.ServiceID)
	if err != nil {
		jsonError(w, "service not found", http.StatusBadRequest)
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

	id, _ := svc.GenerateToken(8)
	c := &models.ControlChannel{
		ID:        id,
		Name:      req.Name,
		ServiceID: req.ServiceID,
		APIKeyID:  req.APIKeyID,
		Config:    configStr,
		IsActive:  true,
	}
	if err := h.cc.Create(c); err != nil {
		jsonError(w, "create failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-read with the JOIN so service_name + api_key_name are populated.
	full, err := h.cc.GetByID(id)
	if err != nil {
		jsonResponse(w, c) // fall back to the unjoined row
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, full)
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
		Name     string          `json:"name"`
		Config   json.RawMessage `json:"config"`
		IsActive *bool           `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	name := req.Name
	if name == "" {
		name = cur.Name
	}
	configStr := cur.Config
	if len(req.Config) > 0 {
		configStr = string(req.Config)
	}
	active := cur.IsActive
	if req.IsActive != nil {
		active = *req.IsActive
	}
	if err := h.cc.Update(id, name, configStr, active); err != nil {
		jsonError(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "updated"})
}

// DELETE /api/cc/{id} — archive every Discord channel belonging to this CC,
// then drop the row (FK cascade clears assignments + channel cache).
// Discord errors are logged but don't block the delete — the archive is a
// best-effort cleanup; if the bot lost access, the row should still go.
func (h *ControlChannelHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cc, err := h.cc.GetByID(id)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	// Best-effort archive of every channel belonging to this CC.
	channels, _ := h.cc.ListChannels(id)
	if len(channels) > 0 && h.bot != nil {
		botToken, decErr := h.decryptBotToken(cc.APIKeyID)
		if decErr == nil {
			ctx := r.Context()
			for _, ch := range channels {
				if ch.ChannelID == "" || ch.Archived {
					continue
				}
				if err := h.bot.ArchiveChannel(ctx, botToken, ch.ChannelID, ch.Name); err != nil {
					// Log but don't fail the delete.
					continue
				}
			}
		}
	}

	// Collect placeholder ids so we can drop them after the cascade.
	asn, _ := h.cc.AssignmentsForCC(id)

	if err := h.cc.Delete(id); err != nil {
		jsonError(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, a := range asn {
		_ = h.placeholders.Delete(a.PlaceholderID)
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}

// GET /api/clients/{id}/cc — list CC assignments for a client.
func (h *ControlChannelHandler) ListAssignmentsForClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	if _, err := h.clients.GetByID(clientID); err != nil {
		jsonError(w, "client not found", http.StatusNotFound)
		return
	}
	out, err := h.cc.AssignmentsForClient(clientID)
	if err != nil {
		jsonError(w, "list failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []models.ClientCCDetail{}
	}
	jsonResponse(w, out)
}

// POST /api/clients/{id}/cc — assign CC to client. Calls Discord to provision
// a home channel under the CC's category, issues a phantom token bound to
// the bot key + this client, and persists the assignment row. Any failure
// rolls back the partial state so retries are safe.
func (h *ControlChannelHandler) AssignToClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	cl, err := h.clients.GetByID(clientID)
	if err != nil {
		jsonError(w, "client not found", http.StatusNotFound)
		return
	}
	var req struct {
		CCID      string `json:"cc_id"`
		AgentType string `json:"agent_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.CCID == "" {
		jsonError(w, "cc_id required", http.StatusBadRequest)
		return
	}
	if req.AgentType == "" {
		req.AgentType = "claude_code"
	}
	switch req.AgentType {
	case "claude_code", "openclaw", "harmes", "cursor", "copilot_cli":
	default:
		jsonError(w, "unknown agent_type: "+req.AgentType, http.StatusBadRequest)
		return
	}

	cc, err := h.cc.GetByID(req.CCID)
	if err != nil {
		jsonError(w, "cc not found", http.StatusBadRequest)
		return
	}
	if !cc.IsActive {
		jsonError(w, "cc is inactive", http.StatusBadRequest)
		return
	}
	if existing, _ := h.cc.GetAssignment(clientID, req.CCID); existing != nil {
		jsonError(w, "client already assigned to this CC", http.StatusConflict)
		return
	}

	svcRow, err := h.services.GetByID(cc.ServiceID)
	if err != nil {
		jsonError(w, "service lookup failed", http.StatusInternalServerError)
		return
	}

	// 1. Decrypt bot token.
	botToken, err := h.decryptBotToken(cc.APIKeyID)
	if err != nil {
		jsonError(w, "decrypt bot token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 2. Provision the channel via Discord. Currently only `discord` is
	// supported — other services would plug in here.
	var realChannelID, channelName string
	switch svcRow.Name {
	case "discord":
		var cfg struct {
			GuildID    string `json:"guild_id"`
			CategoryID string `json:"category_id"`
		}
		if err := json.Unmarshal([]byte(cc.Config), &cfg); err != nil || cfg.GuildID == "" || cfg.CategoryID == "" {
			jsonError(w, "cc config missing guild_id/category_id", http.StatusInternalServerError)
			return
		}
		ch, err := h.bot.CreateChannel(r.Context(), botToken, svc.CreateChannelOpts{
			GuildID:  cfg.GuildID,
			ParentID: cfg.CategoryID,
			Name:     cl.Name,
			Topic:    "Duckway agent channel for " + cl.Name,
		})
		if err != nil {
			jsonError(w, "discord create channel: "+err.Error(), http.StatusBadGateway)
			return
		}
		realChannelID = ch.ID
		channelName = ch.Name
	default:
		jsonError(w, "service "+svcRow.Name+" not supported for CC assignment yet", http.StatusBadRequest)
		return
	}

	// 3. Persist channel row.
	handle, _ := svc.GenerateToken(12)
	handle = "dwch_" + handle
	homeChan := &models.CCChannel{
		Handle:    handle,
		CCID:      cc.ID,
		ClientID:  &clientID,
		ChannelID: realChannelID,
		Name:      channelName,
		IsHome:    true,
	}
	if err := h.cc.CreateChannel(homeChan); err != nil {
		// Roll back the Discord channel — we can't honour the assign.
		_ = h.bot.ArchiveChannel(r.Context(), botToken, realChannelID, channelName)
		jsonError(w, "persist channel: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 4. Issue phantom token bound to bot api_key + this client. env_name
	// must be unique per (client, service) — placeholder_keys has a UNIQUE
	// constraint on that triple — so a client can be assigned to multiple
	// CCs on the same bot (and therefore same service). Suffix with a
	// short slice of cc_id to keep them distinct.
	envName := "DUCKWAY_CC_" + cc.ID
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
		_ = h.cc.MarkChannelArchived(handle)
		_ = h.bot.ArchiveChannel(r.Context(), botToken, realChannelID, channelName)
		jsonError(w, "generate placeholder: "+err.Error(), http.StatusInternalServerError)
		return
	}
	phID, _ := svc.GenerateToken(16)
	apiKeyIDPtr := cc.APIKeyID
	ph := &models.PlaceholderKey{
		ID:                 phID,
		EnvName:            envName,
		Placeholder:        placeholderStr,
		ServiceID:          cc.ServiceID,
		APIKeyID:           &apiKeyIDPtr,
		ClientID:           clientID,
		RequiresApproval:   false,
		ApprovalTTLMinutes: 1440,
		IsActive:           true,
	}
	if err := h.placeholders.Create(ph); err != nil {
		_ = h.cc.MarkChannelArchived(handle)
		_ = h.bot.ArchiveChannel(r.Context(), botToken, realChannelID, channelName)
		jsonError(w, "issue placeholder: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 5. Persist assignment row.
	a := &models.ClientCC{
		ClientID:      clientID,
		CCID:          cc.ID,
		AgentType:     req.AgentType,
		HomeHandle:    handle,
		PlaceholderID: phID,
	}
	if err := h.cc.Assign(a); err != nil {
		_ = h.placeholders.Delete(phID)
		_ = h.cc.MarkChannelArchived(handle)
		_ = h.bot.ArchiveChannel(r.Context(), botToken, realChannelID, channelName)
		jsonError(w, "assign failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]interface{}{
		"status":            "assigned",
		"cc_id":             cc.ID,
		"client_id":         clientID,
		"agent_type":        req.AgentType,
		"home_handle":       handle,
		"home_channel_id":   realChannelID,
		"home_channel_name": channelName,
		"placeholder_id":    phID,
	})
}

// DELETE /api/clients/{id}/cc/{cc_id} — archive the home channel on Discord
// (rename + drop parent_id), drop the placeholder, then drop the assignment.
// Discord errors don't block local cleanup — once we've decided to unassign,
// we want the row gone even if the bot can't reach Discord.
func (h *ControlChannelHandler) UnassignFromClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	ccID := r.PathValue("cc_id")
	a, err := h.cc.GetAssignment(clientID, ccID)
	if err != nil {
		jsonError(w, "assignment not found", http.StatusNotFound)
		return
	}
	cc, err := h.cc.GetByID(ccID)
	if err == nil && h.bot != nil {
		ch, _ := h.cc.GetChannelByHandle(a.HomeHandle)
		if ch != nil && ch.ChannelID != "" && !ch.Archived {
			botToken, decErr := h.decryptBotToken(cc.APIKeyID)
			if decErr == nil {
				_ = h.bot.ArchiveChannel(r.Context(), botToken, ch.ChannelID, ch.Name)
			}
		}
	}
	if err := h.cc.Unassign(clientID, ccID); err != nil {
		jsonError(w, "unassign failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = h.cc.MarkChannelArchived(a.HomeHandle)
	_ = h.placeholders.Delete(a.PlaceholderID)
	jsonResponse(w, map[string]string{"status": "unassigned"})
}

// decryptBotToken fetches an api_key by id and returns its decrypted value.
func (h *ControlChannelHandler) decryptBotToken(apiKeyID string) (string, error) {
	key, err := h.apiKeys.GetByID(apiKeyID)
	if err != nil {
		return "", err
	}
	return h.crypto.Decrypt(key.KeyEncrypted)
}
