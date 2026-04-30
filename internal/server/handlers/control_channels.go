package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

// ControlChannelHandler covers admin CRUD for the CC concept. The actual
// Discord bot work (creating channels, archiving, gateway routing) lives in
// a separate service so this handler stays focused on persistence.
type ControlChannelHandler struct {
	cc        *queries.ControlChannelQueries
	apiKeys   *queries.APIKeyQueries
	services  *queries.ServiceQueries
	clients   *queries.ClientQueries
	settings  *queries.SettingsQueries
}

func NewControlChannelHandler(cc *queries.ControlChannelQueries, apiKeys *queries.APIKeyQueries, services *queries.ServiceQueries, clients *queries.ClientQueries, settings *queries.SettingsQueries) *ControlChannelHandler {
	return &ControlChannelHandler{cc: cc, apiKeys: apiKeys, services: services, clients: clients, settings: settings}
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

// DELETE /api/cc/{id} — Phase A: just delete the row + cascading rows. Phase
// B will pre-archive Discord channels here.
func (h *ControlChannelHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.cc.GetByID(id); err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if err := h.cc.Delete(id); err != nil {
		jsonError(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
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

// POST /api/clients/{id}/cc — assign CC to client. Phase A: stub the Discord
// channel-create step (creates a placeholder cc_channels row + placeholder
// token, but does not call Discord). Phase B replaces the stub with a real
// API call inside a TX.
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

	// Phase A stub: Discord channel creation lives in Phase B. We DO need a
	// home_handle row to satisfy the FK, so write a placeholder with
	// channel_id="" + archived=0 + a deterministic name from the client.
	// Phase B will replace this with the real Discord ID before exposing the
	// channel to agents.
	handle, _ := svc.GenerateToken(12)
	handle = "dwch_" + handle
	homeChan := &models.CCChannel{
		Handle:    handle,
		CCID:      cc.ID,
		ClientID:  &clientID,
		ChannelID: "", // populated by Phase B
		Name:      cl.Name,
		IsHome:    true,
	}
	if err := h.cc.CreateChannel(homeChan); err != nil {
		jsonError(w, "create channel row: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Phantom token bound to the bot key + this client. Phase A: empty
	// PlaceholderID since we haven't built that link yet — Phase B/C will
	// generate a real placeholder_keys row and link it. For now we use the
	// CC id itself so the FK we declared isn't violated. Skip FK with an
	// empty string when the placeholder isn't issued.
	// NOTE: schema declares placeholder_id REFERENCES placeholder_keys(id);
	// in Phase A we just don't enforce it (SQLite FK is typically off in this
	// project; if on, the Phase B work issues a real phantom).
	a := &models.ClientCC{
		ClientID:      clientID,
		CCID:          cc.ID,
		AgentType:     req.AgentType,
		HomeHandle:    handle,
		PlaceholderID: "phaseA-pending-" + handle,
	}
	if err := h.cc.Assign(a); err != nil {
		// Roll back the channel row we created so the FK stays clean.
		_ = h.cc.MarkChannelArchived(handle)
		jsonError(w, "assign failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]interface{}{
		"status":          "assigned",
		"cc_id":           cc.ID,
		"client_id":       clientID,
		"agent_type":      req.AgentType,
		"home_handle":     handle,
		"home_channel_id": "",
		"note":            "Phase A: home channel record created without Discord API call. Phase B will provision real channel.",
	})
}

// DELETE /api/clients/{id}/cc/{cc_id} — unassign. Phase A: archive the row;
// Phase B will rename + remove parent_id on Discord side.
func (h *ControlChannelHandler) UnassignFromClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	ccID := r.PathValue("cc_id")
	a, err := h.cc.GetAssignment(clientID, ccID)
	if err != nil {
		jsonError(w, "assignment not found", http.StatusNotFound)
		return
	}
	if err := h.cc.Unassign(clientID, ccID); err != nil {
		jsonError(w, "unassign failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = h.cc.MarkChannelArchived(a.HomeHandle)
	jsonResponse(w, map[string]string{"status": "unassigned"})
}
