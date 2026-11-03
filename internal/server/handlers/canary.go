package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

// Admin: list canary tokens for a client with management URLs
func (h *CanaryHandler) ListByClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientId")
	w.Header().Set("Cache-Control", "no-store")
	tokens, err := h.canaryQ.ListByClient(clientID)
	if err != nil {
		jsonError(w, "failed to list canary tokens", http.StatusInternalServerError)
		return
	}

	result := maskedCanaryTokens(tokens)
	for i, t := range tokens {
		result[i].ManageURL = services.ManageURL(t.CanaryToken, t.AuthToken)
	}
	jsonResponse(w, result)
}

// canaryTokenView is the admin-facing token row. It deliberately omits the
// management auth token, the decoy credential values and the deploy content.
type canaryTokenView struct {
	ID         string `json:"id"`
	ClientID   string `json:"client_id"`
	TokenType  string `json:"token_type"`
	Memo       string `json:"memo"`
	DeployPath string `json:"deploy_path"`
	CreatedAt  string `json:"created_at"`
	// CanaryToken is the public canarytokens.org token id (shown shortened).
	CanaryToken string `json:"canary_token"`
	ManageURL   string `json:"manage_url"`
}

func maskedCanaryTokens(tokens []queries.CanaryToken) []canaryTokenView {
	out := make([]canaryTokenView, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, canaryTokenView{ID: t.ID, ClientID: t.ClientID, TokenType: t.TokenType, Memo: t.Memo,
			DeployPath: t.DeployPath, CreatedAt: t.CreatedAt, CanaryToken: t.CanaryToken})
	}
	return out
}

// Overview — GET /api/canary/overview. Fleet view: per client token counts
// plus whether an alert destination exists. Canary alerts are sent by
// canarytokens.org to the canary alert email only; notification channels do
// not receive canary triggers, so only the email counts.
func (h *CanaryHandler) Overview(w http.ResponseWriter, r *http.Request) {
	settings, err := h.canaryQ.GetSettings()
	if err != nil {
		jsonError(w, "failed to get settings", http.StatusInternalServerError)
		return
	}
	clients, err := h.canaryQ.Overview()
	if err != nil {
		jsonError(w, "failed to load overview", http.StatusInternalServerError)
		return
	}
	type row struct {
		queries.CanaryClientOverview
		Excluded bool `json:"excluded"`
	}
	// Exclusion matches by client name OR ID, exactly like the service. An
	// unreadable list is logged and surfaced instead of silently ignored.
	exclusionUnreadable := false
	rows := make([]row, 0, len(clients))
	for _, c := range clients {
		excluded, exErr := services.CanaryExcluded(settings.ExcludeClients, c.Name, c.ClientID)
		if exErr != nil {
			exclusionUnreadable = true
		}
		rows = append(rows, row{CanaryClientOverview: c, Excluded: excluded})
	}
	if exclusionUnreadable {
		log.Printf("canary overview: exclude_clients is not valid JSON; exclusion list unreadable")
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, map[string]interface{}{
		"alert_configured":     settings.Email != "",
		"exclusion_unreadable": exclusionUnreadable,
		"last_triggered_known": false,
		"clients":              rows,
	})
}

// Admin: delete a single canary token (from canarytokens.org, then the DB).
// ?local_only=1 removes just the row, for tokens already gone remotely.
func (h *CanaryHandler) DeleteToken(w http.ResponseWriter, r *http.Request) {
	tokenID := r.PathValue("tokenId")
	localOnly := r.URL.Query().Get("local_only") == "1"
	if err := h.service.DeleteToken(tokenID, localOnly); err != nil {
		switch {
		case errors.Is(err, services.ErrCanaryTokenNotFound):
			jsonError(w, "token not found", http.StatusNotFound)
		case errors.Is(err, services.ErrCanaryRemoteDeleteFailed):
			jsonError(w, err.Error(), http.StatusBadGateway)
		default:
			log.Printf("canary delete token %s: %v", tokenID, err)
			jsonError(w, "failed to delete token", http.StatusInternalServerError)
		}
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}

// Admin: delete ALL canary tokens for a client. Tokens that cannot be deleted
// on canarytokens.org are kept; ?local_only=1 removes the rows regardless.
func (h *CanaryHandler) DeleteClientTokens(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientId")
	localOnly := r.URL.Query().Get("local_only") == "1"
	deleted, failed, err := h.service.DeleteClientTokens(clientID, localOnly)
	if err != nil {
		log.Printf("canary delete client tokens %s: %v", clientID, err)
		jsonError(w, "failed to delete tokens", http.StatusInternalServerError)
		return
	}
	if failed > 0 {
		jsonError(w, fmt.Sprintf("%d tokens could not be deleted on canarytokens.org and were kept (%d deleted)", failed, deleted), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]interface{}{"status": "deleted", "deleted": deleted})
}

// Admin: manually (re)generate canary tokens for a client
func (h *CanaryHandler) GenerateForClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientId")
	// The name and short ID come from the server's own client record, never
	// from the query string: the exclusion check matches on the name.
	clientName, shortID, found, lookupErr := h.canaryQ.ClientNameAndShortID(clientID)
	if lookupErr != nil {
		log.Printf("canary generate: look up client %s: %v", clientID, lookupErr)
		jsonError(w, "failed to look up client", http.StatusInternalServerError)
		return
	}
	if !found {
		jsonError(w, "client not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	res, err := h.service.RegenerateForClient(clientID, clientName, shortID)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrCanaryNoAlertEmail), errors.Is(err, services.ErrCanaryClientExcluded),
			errors.Is(err, services.ErrCanaryExclusionUnreadable):
			jsonError(w, err.Error(), http.StatusConflict)
		case errors.Is(err, services.ErrCanaryNothingGenerated):
			jsonError(w, err.Error(), http.StatusBadGateway)
		default:
			log.Printf("canary generate failed for client %s: %v", clientID, err)
			jsonError(w, "failed to generate canary tokens", http.StatusInternalServerError)
		}
		return
	}

	tokens, err := h.canaryQ.ListByClient(clientID)
	if err != nil {
		log.Printf("canary generate: list tokens for client %s: %v", clientID, err)
		jsonError(w, "tokens were generated but the list could not be loaded", http.StatusInternalServerError)
		return
	}
	view := maskedCanaryTokens(tokens)
	for i, t := range tokens {
		view[i].ManageURL = services.ManageURL(t.CanaryToken, t.AuthToken)
	}
	var notice string
	if res.FailedTypes > 0 {
		notice = fmt.Sprintf("%d types could not be regenerated; their existing tokens were kept", res.FailedTypes)
	}
	jsonResponse(w, map[string]interface{}{
		"tokens":        view,
		"created":       res.Created,
		"replaced":      res.Replaced,
		"failed_types":  res.FailedTypes,
		"delete_failed": res.DeleteFailed,
		"notice":        notice,
	})
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
		existing, lerr := h.canaryQ.ListByClient(client.ID)
		if lerr != nil {
			log.Printf("client canaries: list tokens for %s: %v", client.ID, lerr)
			jsonError(w, "failed to list canary tokens", http.StatusInternalServerError)
			return
		}
		if len(existing) == 0 {
			if gerr := h.service.GenerateForClient(client.ID, client.Name, client.ShortID); gerr != nil {
				log.Printf("client canaries: generate for %s: %v", client.ID, gerr)
			}
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
