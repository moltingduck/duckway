package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	refresher    *svc.TokenRefresher
}

type oauthTokenRequest struct {
	Name             string `json:"name"`
	ServiceID        string `json:"service_id"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresAt        int64  `json:"expires_at"`
	TokenEndpoint    string `json:"token_endpoint"`
	SubscriptionInfo string `json:"subscription_info"` // JSON string
}

func NewOAuthHandler(apiKeyQ *queries.APIKeyQueries, placeholderQ *queries.PlaceholderQueries, serviceQ *queries.ServiceQueries, crypto *svc.Crypto) *OAuthHandler {
	return &OAuthHandler{apiKeyQ: apiKeyQ, placeholderQ: placeholderQ, serviceQ: serviceQ, crypto: crypto}
}

// SetRefresher wires a TokenRefresher so the admin can trigger manual
// refreshes via POST /api/oauth/{id}/refresh. Optional — endpoint returns
// 503 if not set.
func (h *OAuthHandler) SetRefresher(r *svc.TokenRefresher) {
	h.refresher = r
}

// Refresh forces an immediate token refresh for the given refreshable key.
// Useful for verifying the refresh path works without waiting for natural
// expiry, or for recovering after a manual rotation upstream.
func (h *OAuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if h.refresher == nil {
		jsonError(w, "refresher not initialized", http.StatusServiceUnavailable)
		return
	}
	key, err := h.apiKeyQ.GetByID(id)
	if err != nil {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}
	if !key.IsRefreshable {
		jsonError(w, "not a refreshable key", http.StatusBadRequest)
		return
	}

	expiresAt, err := h.refresher.RefreshNow(id)
	if err != nil {
		jsonError(w, "refresh failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"status":     "refreshed",
		"expires_at": expiresAt,
		"id":         id,
	})
}

// Delete previews the blast radius for a refreshable token, then removes it
// after explicit confirmation. Client placeholders are deleted, key-suite
// entries are cleared, and CCs using this key are disabled so they can be
// reassigned without losing their channels.
func (h *OAuthHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if !req.Confirm {
		impact, err := h.apiKeyQ.RefreshableDeleteImpact(id)
		if err != nil {
			jsonError(w, "key not found or not refreshable: "+err.Error(), http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]interface{}{
			"requires_confirmation": true,
			"impact":                impact,
		})
		return
	}

	impact, err := h.apiKeyQ.DeleteRefreshableWithCleanup(id)
	if err != nil {
		jsonError(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"status": "deleted",
		"impact": impact,
	})
}

// Validate checks refreshable-token fields without storing or refreshing them.
// The upload form uses this as an explicit "Test" step, and Upload/Update use
// the same validation so bad JSON cannot bypass the UI.
func (h *OAuthHandler) Validate(w http.ResponseWriter, r *http.Request) {
	var req oauthTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	svcRow, warnings, err := h.validateOAuthTokenRequest(&req, true)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"ok":       true,
		"service":  svcRow.Name,
		"warnings": warnings,
	})
}

// Admin: upload refreshable API key (OAuth token with refresh)
func (h *OAuthHandler) Upload(w http.ResponseWriter, r *http.Request) {
	var req oauthTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	if _, _, err := h.validateOAuthTokenRequest(&req, true); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
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

	id, err := svc.GenerateToken(16)
	if err != nil {
		jsonError(w, "generate key ID: "+err.Error(), http.StatusInternalServerError)
		return
	}
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

// Admin: get refreshable key details (without decrypted tokens)
func (h *OAuthHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := h.apiKeyQ.GetByID(id)
	if err != nil {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}
	if !key.IsRefreshable {
		jsonError(w, "not a refreshable key", http.StatusBadRequest)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"id":                key.ID,
		"name":              key.Name,
		"service_id":        key.ServiceID,
		"service_name":      key.ServiceName,
		"token_endpoint":    key.TokenEndpoint,
		"subscription_info": key.SubscriptionInfo,
		"usage_snapshot":    key.UsageSnapshot,
		"expires_at":        key.ExpiresAt,
		"is_active":         key.IsActive,
		"usage_count":       key.UsageCount,
		"last_used_at":      key.LastUsedAt,
		"created_at":        key.CreatedAt,
	})
}

// Admin: update refreshable key (name, endpoint, subscription, optionally tokens)
func (h *OAuthHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := h.apiKeyQ.GetByID(id)
	if err != nil {
		jsonError(w, "key not found", http.StatusNotFound)
		return
	}
	if !key.IsRefreshable {
		jsonError(w, "not a refreshable key", http.StatusBadRequest)
		return
	}

	var req struct {
		Name             string `json:"name"`
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresAt        int64  `json:"expires_at"`
		TokenEndpoint    string `json:"token_endpoint"`
		SubscriptionInfo string `json:"subscription_info"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		req.Name = key.Name
	}
	if req.TokenEndpoint == "" {
		req.TokenEndpoint = key.TokenEndpoint
	}
	if req.SubscriptionInfo == "" {
		req.SubscriptionInfo = key.SubscriptionInfo
	}
	validateReq := oauthTokenRequest{
		Name:             req.Name,
		ServiceID:        key.ServiceID,
		AccessToken:      req.AccessToken,
		RefreshToken:     req.RefreshToken,
		ExpiresAt:        req.ExpiresAt,
		TokenEndpoint:    req.TokenEndpoint,
		SubscriptionInfo: req.SubscriptionInfo,
	}
	if req.AccessToken != "" || req.RefreshToken != "" {
		if _, _, err := h.validateOAuthTokenRequest(&validateReq, false); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if err := validateSubscriptionInfo(req.SubscriptionInfo); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	var encAccess, encRefresh string
	if req.AccessToken != "" {
		encAccess, err = h.crypto.Encrypt(req.AccessToken)
		if err != nil {
			jsonError(w, "encryption failed", http.StatusInternalServerError)
			return
		}
	}
	if req.RefreshToken != "" {
		encRefresh, err = h.crypto.Encrypt(req.RefreshToken)
		if err != nil {
			jsonError(w, "encryption failed", http.StatusInternalServerError)
			return
		}
	}

	if err := h.apiKeyQ.UpdateRefreshable(id, req.Name, encAccess, encRefresh, req.TokenEndpoint, req.SubscriptionInfo, req.ExpiresAt); err != nil {
		jsonError(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]string{"status": "updated"})
}

func (h *OAuthHandler) validateOAuthTokenRequest(req *oauthTokenRequest, requireTokens bool) (*models.Service, []string, error) {
	if req.ServiceID == "" {
		return nil, nil, fmt.Errorf("service_id required")
	}
	svcRow, err := h.serviceQ.GetByID(req.ServiceID)
	if err != nil {
		return nil, nil, fmt.Errorf("service not found")
	}
	req.AccessToken = strings.TrimSpace(req.AccessToken)
	req.RefreshToken = strings.TrimSpace(req.RefreshToken)
	req.TokenEndpoint = strings.TrimSpace(req.TokenEndpoint)
	if req.AccessToken == "" {
		return nil, nil, fmt.Errorf("access_token required")
	}
	if requireTokens && req.RefreshToken == "" {
		return nil, nil, fmt.Errorf("refresh_token required for refreshable tokens")
	}
	if req.TokenEndpoint == "" {
		req.TokenEndpoint = defaultTokenEndpointForService(svcRow.Name)
	}
	subInfo, err := parseSubscriptionInfo(req.SubscriptionInfo)
	if err != nil {
		return nil, nil, err
	}
	warnings := []string{}
	if isCodexOAuthInfo(subInfo, req.TokenEndpoint) || (svcRow.Name == "openai" && strings.Contains(req.TokenEndpoint, "auth.openai.com")) {
		if err := validateCodexOAuth(req, subInfo); err != nil {
			return nil, nil, err
		}
		if req.ExpiresAt == 0 && jwtExpMillis(req.AccessToken) == 0 {
			warnings = append(warnings, "access token is not a JWT with exp; expiry will not be auto-filled")
		}
	}
	return svcRow, warnings, nil
}

func defaultTokenEndpointForService(serviceName string) string {
	switch serviceName {
	case "openai":
		return "https://auth.openai.com/oauth/token"
	default:
		return "https://console.anthropic.com/v1/oauth/token"
	}
}

func validateSubscriptionInfo(raw string) error {
	_, err := parseSubscriptionInfo(raw)
	return err
}

func parseSubscriptionInfo(raw string) (map[string]interface{}, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]interface{}{}, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("subscription_info must be valid JSON: %w", err)
	}
	return out, nil
}

func validateCodexOAuth(req *oauthTokenRequest, subInfo map[string]interface{}) error {
	if authMode, _ := subInfo["auth_mode"].(string); authMode != "" && authMode != "chatgpt" {
		return fmt.Errorf("codex oauth auth_mode must be chatgpt")
	}
	if !strings.Contains(req.TokenEndpoint, "auth.openai.com") {
		return fmt.Errorf("codex oauth token_endpoint must be https://auth.openai.com/oauth/token")
	}
	if !looksLikeJWT(req.AccessToken) {
		return fmt.Errorf("codex oauth access_token must look like a JWT from ~/.codex/auth.json")
	}
	if req.RefreshToken == "" {
		return fmt.Errorf("codex oauth refresh_token required")
	}
	if !strings.HasPrefix(req.RefreshToken, "rt.") {
		return fmt.Errorf("codex oauth refresh_token should start with rt prefix")
	}
	if idToken, _ := subInfo["id_token"].(string); !looksLikeJWT(idToken) {
		return fmt.Errorf("codex oauth id_token required from ~/.codex/auth.json")
	}
	return nil
}

func looksLikeJWT(token string) bool {
	return len(strings.Split(token, ".")) == 3
}

func jwtExpMillis(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp * 1000
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

	// Build oauthAccount for .claude.json (minimizes info leaked to agents)
	displayName := "Duckway Agent"
	if subInfo != nil {
		if v, ok := subInfo["displayName"].(string); ok && v != "" {
			displayName = v
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
		"claudeConfig": map[string]interface{}{
			"userID": "duckway",
			"oauthAccount": map[string]interface{}{
				"accountUuid":                 "00000000-0000-0000-0000-000000000000",
				"emailAddress":                "agent@duckway.local",
				"organizationUuid":            "00000000-0000-0000-0000-000000000000",
				"hasExtraUsageEnabled":        false,
				"billingType":                 "stripe_subscription",
				"accountCreatedAt":            "2025-01-01T00:00:00.000000Z",
				"subscriptionCreatedAt":       "2025-01-01T00:00:00.000000Z",
				"ccOnboardingFlags":           map[string]interface{}{},
				"claudeCodeTrialEndsAt":       nil,
				"claudeCodeTrialDurationDays": nil,
				"displayName":                 displayName,
			},
			"hasCompletedOnboarding": true,
			"lastOnboardingVersion":  "2.1.165",
		},
	})
}

// ClientGetCodexCredentials returns a fake Codex ChatGPT-login auth.json for
// clients whose OpenAI placeholder is assigned to a Codex OAuth refreshable key.
// The client never receives real OAuth tokens: refresh calls are intercepted by
// the local proxy and exchanged at the gateway.
func (h *OAuthHandler) ClientGetCodexCredentials(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}

	openAISvc, err := h.serviceQ.GetByName("openai")
	if err != nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	ph, err := h.placeholderQ.GetByClientAndService(client.ID, openAISvc.ID)
	if err != nil || ph.APIKeyID == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	apiKey, err := h.apiKeyQ.GetByID(*ph.APIKeyID)
	if err != nil || apiKey.RefreshToken == "" {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	var subInfo map[string]interface{}
	if apiKey.SubscriptionInfo != "" {
		_ = json.Unmarshal([]byte(apiKey.SubscriptionInfo), &subInfo)
	}
	if !isCodexOAuthInfo(subInfo, apiKey.TokenEndpoint) {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	accessToken, err := h.crypto.Decrypt(apiKey.KeyEncrypted)
	if err != nil {
		jsonError(w, "decrypt codex access token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	idToken, _ := subInfo["id_token"].(string)
	tokens := codexPhantomTokensFromSource(ph.Placeholder, subInfo, accessToken, idToken)
	for _, key := range []string{"account_id"} {
		if v, ok := subInfo[key]; ok && v != "" {
			tokens[key] = v
		}
	}
	resp := map[string]interface{}{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens":         tokens,
	}
	if v, ok := subInfo["last_refresh"]; ok && v != "" {
		resp["last_refresh"] = v
	}
	jsonResponse(w, resp)
}

func codexPhantomTokens(placeholder string, subInfo map[string]interface{}) map[string]interface{} {
	return codexPhantomTokensFromSource(placeholder, subInfo, "", "")
}

func codexPhantomTokensFromSource(placeholder string, subInfo map[string]interface{}, accessToken, idToken string) map[string]interface{} {
	return map[string]interface{}{
		"access_token":  codexPhantomJWTFromSource("access", placeholder, subInfo, accessToken),
		"refresh_token": "rt.duckway." + placeholder,
		"id_token":      codexPhantomJWTFromSource("id", placeholder, subInfo, idToken),
	}
}

func codexPhantomJWTFromSource(kind, placeholder string, subInfo map[string]interface{}, source string) string {
	parts := strings.Split(source, ".")
	if len(parts) == 3 && parts[0] != "" && parts[1] != "" {
		sig := base64.RawURLEncoding.EncodeToString([]byte("duckway:" + kind + ":" + placeholder))
		return parts[0] + "." + parts[1] + "." + sig
	}
	return codexPhantomJWT(kind, placeholder, subInfo)
}

func codexPhantomJWT(kind, placeholder string, subInfo map[string]interface{}) string {
	accountID := "duckway"
	if v, _ := subInfo["account_id"].(string); v != "" {
		accountID = v
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := map[string]interface{}{
		"iss":         "https://duckway.local",
		"aud":         "codex",
		"sub":         accountID,
		"typ":         "duckway_codex_phantom_" + kind,
		"placeholder": placeholder,
		"exp":         time.Now().Add(24 * time.Hour).Unix(),
	}
	b, _ := json.Marshal(payload)
	return header + "." + base64.RawURLEncoding.EncodeToString(b) + ".duckway"
}

func isCodexOAuthInfo(subInfo map[string]interface{}, tokenEndpoint string) bool {
	if subInfo == nil {
		return false
	}
	if v, _ := subInfo["credential_kind"].(string); v == "codex_oauth" {
		return true
	}
	if v, _ := subInfo["source"].(string); v == "codex" {
		return true
	}
	if v, _ := subInfo["auth_mode"].(string); v == "chatgpt" && strings.Contains(tokenEndpoint, "auth.openai.com") {
		return true
	}
	return false
}
