package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type OAuthHandler struct {
	apiKeyQ      *queries.APIKeyQueries
	placeholderQ *queries.PlaceholderQueries
	serviceQ     *queries.ServiceQueries
	crypto       *svc.Crypto
}

func NewOAuthHandler(apiKeyQ *queries.APIKeyQueries, placeholderQ *queries.PlaceholderQueries, serviceQ *queries.ServiceQueries, crypto *svc.Crypto) *OAuthHandler {
	return &OAuthHandler{apiKeyQ: apiKeyQ, placeholderQ: placeholderQ, serviceQ: serviceQ, crypto: crypto}
}

// Admin: upload refreshable API key (OAuth token with refresh)
func (h *OAuthHandler) Upload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string `json:"name"`
		ServiceID        string `json:"service_id"`
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresAt        int64  `json:"expires_at"`
		TokenEndpoint    string `json:"token_endpoint"`
		SubscriptionInfo string `json:"subscription_info"` // JSON string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	if req.AccessToken == "" {
		jsonError(w, "access_token required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		req.Name = "OAuth Token"
	}
	if req.TokenEndpoint == "" {
		req.TokenEndpoint = "https://console.anthropic.com/v1/oauth/token"
	}

	encAccess, err := h.crypto.Encrypt(req.AccessToken)
	if err != nil {
		jsonError(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	var encRefresh string
	if req.RefreshToken != "" {
		encRefresh, err = h.crypto.Encrypt(req.RefreshToken)
		if err != nil {
			jsonError(w, "encryption failed", http.StatusInternalServerError)
			return
		}
	}

	id, _ := svc.GenerateToken(16)
	key := &models.APIKey{
		ID:               id,
		ServiceID:        req.ServiceID,
		Name:             req.Name,
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        req.ExpiresAt,
		TokenEndpoint:    req.TokenEndpoint,
		SubscriptionInfo: req.SubscriptionInfo,
		IsActive:         true,
	}

	if err := h.apiKeyQ.Create(key); err != nil {
		jsonError(w, "failed to create: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]string{"id": id, "status": "created"})
}

// Client: get credentials.json with phantom tokens for Claude/OAuth services.
func (h *OAuthHandler) ClientGetCredentials(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}

	// Find the Anthropic service
	anthSvc, err := h.serviceQ.GetByName("anthropic")
	if err != nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}

	// Find the client's phantom token for Anthropic
	ph, err := h.placeholderQ.GetByClientAndService(client.ID, anthSvc.ID)
	if err != nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}

	// Find the API key behind this phantom token to get subscription info
	var subInfo map[string]interface{}
	if ph.APIKeyID != nil {
		apiKey, err := h.apiKeyQ.GetByID(*ph.APIKeyID)
		if err == nil && apiKey.SubscriptionInfo != "" {
			json.Unmarshal([]byte(apiKey.SubscriptionInfo), &subInfo)
		}
	}

	subType := ""
	rateTier := ""
	scopes := json.RawMessage("[]")
	if subInfo != nil {
		if v, ok := subInfo["subscriptionType"].(string); ok {
			subType = v
		}
		if v, ok := subInfo["rateLimitTier"].(string); ok {
			rateTier = v
		}
		if v, ok := subInfo["scopes"]; ok {
			if b, err := json.Marshal(v); err == nil {
				scopes = b
			}
		}
	}

	jsonResponse(w, map[string]interface{}{
		"claudeAiOauth": map[string]interface{}{
			"accessToken":      ph.Placeholder,
			"refreshToken":     ph.Placeholder,
			"expiresAt":        9999999999999,
			"scopes":           scopes,
			"subscriptionType": subType,
			"rateLimitTier":    rateTier,
		},
	})
}
