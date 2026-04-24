package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/middleware"
	svc "github.com/hackerduck/duckway/internal/server/services"
)

type OAuthHandler struct {
	oauthQ       *queries.OAuthQueries
	placeholderQ *queries.PlaceholderQueries
	serviceQ     *queries.ServiceQueries
	crypto       *svc.Crypto
}

func NewOAuthHandler(oauthQ *queries.OAuthQueries, placeholderQ *queries.PlaceholderQueries, serviceQ *queries.ServiceQueries, crypto *svc.Crypto) *OAuthHandler {
	return &OAuthHandler{oauthQ: oauthQ, placeholderQ: placeholderQ, serviceQ: serviceQ, crypto: crypto}
}

// Admin: list OAuth credentials
func (h *OAuthHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.oauthQ.List()
	if err != nil {
		jsonError(w, "failed to list", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []queries.OAuthCredential{}
	}
	jsonResponse(w, list)
}

// Admin: upload OAuth credentials (from Claude .credentials.json)
func (h *OAuthHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string `json:"name"`
		ServiceID        string `json:"service_id"`
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresAt        int64  `json:"expires_at"`
		SubscriptionType string `json:"subscription_type"`
		RateLimitTier    string `json:"rate_limit_tier"`
		Scopes           string `json:"scopes"`
		TokenEndpoint    string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	if req.AccessToken == "" || req.RefreshToken == "" {
		jsonError(w, "access_token and refresh_token required", http.StatusBadRequest)
		return
	}

	if req.ServiceID == "" {
		// Default to anthropic service
		svcObj, err := h.serviceQ.GetByName("anthropic")
		if err != nil {
			jsonError(w, "anthropic service not found", http.StatusBadRequest)
			return
		}
		req.ServiceID = svcObj.ID
	}

	if req.Name == "" {
		req.Name = "Claude OAuth"
	}
	if req.TokenEndpoint == "" {
		req.TokenEndpoint = "https://console.anthropic.com/v1/oauth/token"
	}
	if req.Scopes == "" {
		req.Scopes = "[]"
	}

	// Encrypt tokens
	encAccess, err := h.crypto.Encrypt(req.AccessToken)
	if err != nil {
		jsonError(w, "encryption failed", http.StatusInternalServerError)
		return
	}
	encRefresh, err := h.crypto.Encrypt(req.RefreshToken)
	if err != nil {
		jsonError(w, "encryption failed", http.StatusInternalServerError)
		return
	}

	id, _ := svc.GenerateToken(16)
	cred := &queries.OAuthCredential{
		ID:               id,
		ServiceID:        req.ServiceID,
		Name:             req.Name,
		AccessToken:      encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        req.ExpiresAt,
		TokenEndpoint:    req.TokenEndpoint,
		Scopes:           req.Scopes,
		SubscriptionType: req.SubscriptionType,
		RateLimitTier:    req.RateLimitTier,
		IsActive:         true,
	}

	if err := h.oauthQ.Create(cred); err != nil {
		jsonError(w, "failed to create: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]string{"id": id, "status": "created"})
}

// Admin: delete OAuth credential
func (h *OAuthHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.oauthQ.Delete(id); err != nil {
		jsonError(w, "failed to delete", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Client: get Claude credentials for this client (phantom tokens)
func (h *OAuthHandler) ClientGetCredentials(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}

	// Find OAuth credentials for the Anthropic service
	anthSvc, err := h.serviceQ.GetByName("anthropic")
	if err != nil {
		jsonResponse(w, map[string]interface{}{}) // No anthropic service
		return
	}

	oauthCred, err := h.oauthQ.GetByServiceID(anthSvc.ID)
	if err != nil {
		jsonResponse(w, map[string]interface{}{}) // No OAuth credentials
		return
	}

	// Find the client's phantom token for the Anthropic service
	ph, err := h.placeholderQ.GetByClientAndService(client.ID, anthSvc.ID)
	if err != nil {
		jsonResponse(w, map[string]interface{}{}) // No phantom token assigned
		return
	}

	// Return credential structure matching Claude's .credentials.json
	jsonResponse(w, map[string]interface{}{
		"claudeAiOauth": map[string]interface{}{
			"accessToken":      ph.Placeholder,     // Phantom token — proxy swaps for real
			"refreshToken":     ph.Placeholder,      // Same phantom — refresh handled by server
			"expiresAt":        9999999999999,        // Never expires locally
			"scopes":           json.RawMessage(oauthCred.Scopes),
			"subscriptionType": oauthCred.SubscriptionType,
			"rateLimitTier":    oauthCred.RateLimitTier,
		},
	})
}
