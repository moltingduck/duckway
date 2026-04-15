package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

type CanaryHandler struct {
	canaryQ *queries.CanaryQueries
	service *services.CanaryService
}

func NewCanaryHandler(canaryQ *queries.CanaryQueries, service *services.CanaryService) *CanaryHandler {
	return &CanaryHandler{canaryQ: canaryQ, service: service}
}

// Admin: get canary settings
func (h *CanaryHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.canaryQ.GetSettings()
	if err != nil {
		jsonError(w, "failed to get settings", http.StatusInternalServerError)
		return
	}

	var types []map[string]string
	for _, t := range services.SupportedCanaryTypes {
		defaultStr := "false"
		if t.DefaultEnabled {
			defaultStr = "true"
		}
		types = append(types, map[string]string{
			"type":            t.Type,
			"name":            t.DisplayName,
			"description":     t.Description,
			"category":        t.Category,
			"deploy_path":     t.DeployPath,
			"deploy_mode":     t.DeployMode,
			"default_enabled": defaultStr,
		})
	}

	jsonResponse(w, map[string]interface{}{
		"email":           settings.Email,
		"enabled_types":   json.RawMessage(settings.EnabledTypes),
		"exclude_clients": json.RawMessage(settings.ExcludeClients),
		"available_types": types,
	})
}

// Admin: save canary settings
func (h *CanaryHandler) SaveSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email          string   `json:"email"`
		EnabledTypes   []string `json:"enabled_types"`
		ExcludeClients []string `json:"exclude_clients"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.EnabledTypes == nil {
		req.EnabledTypes = []string{}
	}
	if req.ExcludeClients == nil {
		req.ExcludeClients = []string{}
	}

	typesJSON, _ := json.Marshal(req.EnabledTypes)
	excludeJSON, _ := json.Marshal(req.ExcludeClients)
	settings := &queries.CanarySettings{
		Email:          req.Email,
		EnabledTypes:   string(typesJSON),
		ExcludeClients: string(excludeJSON),
	}

	if err := h.canaryQ.SaveSettings(settings); err != nil {
		jsonError(w, "failed to save settings", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

// Admin: list canary tokens for a client
func (h *CanaryHandler) ListByClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientId")
	tokens, err := h.canaryQ.ListByClient(clientID)
	if err != nil {
		jsonError(w, "failed to list canary tokens", http.StatusInternalServerError)
		return
	}
	if tokens == nil {
		tokens = []queries.CanaryToken{}
	}
	jsonResponse(w, tokens)
}

// Admin: manually (re)generate canary tokens for a client
func (h *CanaryHandler) GenerateForClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientId")
	clientName := r.URL.Query().Get("name")
	if clientName == "" {
		clientName = clientID
	}

	shortID := r.URL.Query().Get("short_id")
	if err := h.service.RegenerateForClient(clientID, clientName, shortID); err != nil {
		jsonError(w, "failed to generate canary tokens: "+err.Error(), http.StatusInternalServerError)
		return
	}

	tokens, _ := h.canaryQ.ListByClient(clientID)
	jsonResponse(w, tokens)
}

// Client: get canary tokens to deploy.
// If the client has canary_enabled but no tokens yet, generate them on-demand.
func (h *CanaryHandler) ClientGetCanaries(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client not found", http.StatusUnauthorized)
		return
	}

	// Auto-generate if client has canary enabled but no tokens yet
	if client.CanaryEnabled {
		existing, _ := h.canaryQ.ListByClient(client.ID)
		if len(existing) == 0 {
			h.service.GenerateForClient(client.ID, client.Name, client.ShortID)
		}
	}

	tokens, err := h.canaryQ.ListByClient(client.ID)
	if err != nil {
		jsonError(w, "failed to list canary tokens", http.StatusInternalServerError)
		return
	}

	type canaryDeploy struct {
		TokenType     string `json:"token_type"`
		DeployPath    string `json:"deploy_path"`
		DeployMode    string `json:"deploy_mode"`
		DeployContent string `json:"deploy_content"`
	}

	var deploys []canaryDeploy
	for _, t := range tokens {
		mode := "create"
		ct := services.FindCanaryType(t.TokenType)
		if ct != nil && ct.DeployMode != "" {
			mode = ct.DeployMode
		}
		deploys = append(deploys, canaryDeploy{
			TokenType:     t.TokenType,
			DeployPath:    t.DeployPath,
			DeployMode:    mode,
			DeployContent: t.DeployContent,
		})
	}
	if deploys == nil {
		deploys = []canaryDeploy{}
	}

	jsonResponse(w, deploys)
}
