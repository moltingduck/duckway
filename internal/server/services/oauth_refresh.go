package services

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

// Claude Code's public OAuth client_id — baked into the official Claude Code
// binary, required by Anthropic's token endpoint for refresh requests.
const claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// TokenRefresher automatically refreshes API keys that have a refresh_token set.
// Works for any OAuth-based key (Claude, GitHub Apps, etc.)
type TokenRefresher struct {
	apiKeyQ  *queries.APIKeyQueries
	crypto   *Crypto
	client   *http.Client
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewTokenRefresher(apiKeyQ *queries.APIKeyQueries, crypto *Crypto) *TokenRefresher {
	return &TokenRefresher{
		apiKeyQ: apiKeyQ,
		crypto:  crypto,
		client:  &http.Client{Timeout: 30 * time.Second},
		stopCh:  make(chan struct{}),
	}
}

func (r *TokenRefresher) Start() {
	go r.refreshLoop()
	log.Printf("Token refresh job started (checking every 5 minutes)")
}

func (r *TokenRefresher) Stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
}

func (r *TokenRefresher) refreshLoop() {
	r.refreshExpiring()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.refreshExpiring()
		}
	}
}

func (r *TokenRefresher) refreshExpiring() {
	expiring, err := r.apiKeyQ.ListExpiring(10)
	if err != nil {
		log.Printf("[token-refresh] Error listing expiring keys: %v", err)
		return
	}

	for i := range expiring {
		if err := r.refreshKey(&expiring[i]); err != nil {
			log.Printf("[token-refresh] Failed to refresh %s (%s): %v", expiring[i].Name, expiring[i].ID, err)
		} else {
			log.Printf("[token-refresh] Refreshed %s (%s)", expiring[i].Name, expiring[i].ID)
		}
	}
}

// RefreshNow performs an immediate refresh for the given key id, bypassing
// the schedule. Returns the new expires_at on success.
func (r *TokenRefresher) RefreshNow(id string) (int64, error) {
	key, err := r.apiKeyQ.GetByID(id)
	if err != nil {
		return 0, fmt.Errorf("key not found: %w", err)
	}
	if key.RefreshToken == "" {
		return 0, fmt.Errorf("key has no refresh_token (not refreshable)")
	}
	if err := r.refreshKey(key); err != nil {
		return 0, err
	}
	// Re-read to get the persisted expires_at
	updated, err := r.apiKeyQ.GetByID(id)
	if err != nil {
		return 0, err
	}
	return updated.ExpiresAt, nil
}

func (r *TokenRefresher) refreshKey(key *models.APIKey) error {
	refreshToken, err := r.crypto.Decrypt(key.RefreshToken)
	if err != nil {
		return fmt.Errorf("decrypt refresh token: %w", err)
	}

	if key.TokenEndpoint == "" {
		return fmt.Errorf("no token_endpoint configured")
	}

	// Anthropic's OAuth token endpoint expects JSON with client_id, not the
	// generic OAuth 2.0 form-urlencoded body. Detect by token endpoint host or
	// the refresh token's prefix (sk-ant-ort01-... / sk-ant-oart01-...).
	useAnthropic := strings.Contains(key.TokenEndpoint, "anthropic") ||
		strings.HasPrefix(refreshToken, "sk-ant-ort01") ||
		strings.HasPrefix(refreshToken, "sk-ant-oart01")

	var resp *http.Response
	if useAnthropic {
		clientID := claudeOAuthClientID
		// Allow override via subscription_info JSON {"clientId": "..."}
		if key.SubscriptionInfo != "" {
			var si map[string]interface{}
			if json.Unmarshal([]byte(key.SubscriptionInfo), &si) == nil {
				if v, ok := si["clientId"].(string); ok && v != "" {
					clientID = v
				}
			}
		}
		body, _ := json.Marshal(map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": refreshToken,
			"client_id":     clientID,
		})
		req, buildErr := http.NewRequest("POST", key.TokenEndpoint, bytes.NewReader(body))
		if buildErr != nil {
			return fmt.Errorf("build token request: %w", buildErr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err = r.client.Do(req)
	} else {
		// Generic OAuth 2.0 — form-urlencoded body
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refreshToken},
		}
		if clientID := oauthMetadataString(key.SubscriptionInfo, "client_id", "clientId"); clientID != "" {
			form.Set("client_id", clientID)
		}
		resp, err = r.client.Post(key.TokenEndpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	}
	if err != nil {
		return fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		// Detect permanent failures that will never recover. Stop retrying
		// immediately and deactivate the key to avoid ban from repeated attempts.
		if isPermanentOAuthError(resp.StatusCode, body) {
			_ = r.apiKeyQ.Deactivate(key.ID)
			return fmt.Errorf("permanent OAuth error — key %s deactivated: %s", key.ID, string(body))
		}
		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return fmt.Errorf("empty access token")
	}

	encAccess, err := r.crypto.Encrypt(tokenResp.AccessToken)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	expiresIn := tokenResp.ExpiresIn
	expiresAt := int64(0)
	if expiresIn <= 0 {
		expiresAt = jwtExpiresAtMillis(tokenResp.AccessToken)
	}
	if expiresAt <= 0 {
		if expiresIn <= 0 {
			// Server didn't return expires_in and the access token is not a JWT
			// with exp; use a 1-hour fallback so the key isn't immediately
			// re-queued for refresh on the next tick.
			expiresIn = 3600
		}
		expiresAt = time.Now().UnixMilli() + expiresIn*1000
	}
	if err := r.apiKeyQ.UpdateTokens(key.ID, encAccess, expiresAt); err != nil {
		return fmt.Errorf("store: %w", err)
	}

	if tokenResp.RefreshToken != "" && tokenResp.RefreshToken != refreshToken {
		encRefresh, err := r.crypto.Encrypt(tokenResp.RefreshToken)
		if err != nil {
			log.Printf("[token-refresh] Warning: failed to encrypt new refresh token for %s: %v", key.ID, err)
		} else if err := r.apiKeyQ.UpdateRefreshToken(key.ID, encRefresh); err != nil {
			log.Printf("[token-refresh] Warning: failed to store rotated refresh token for %s: %v", key.ID, err)
		}
	}

	return nil
}

func oauthMetadataString(raw string, keys ...string) string {
	if raw == "" {
		return ""
	}
	var metadata map[string]interface{}
	if json.Unmarshal([]byte(raw), &metadata) != nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func jwtExpiresAtMillis(token string) int64 {
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

// isPermanentOAuthError returns true for errors that will never succeed on
// retry: invalid/revoked/expired refresh tokens, bad client credentials, etc.
// Transient errors (500, 503, network timeout) return false so the next
// scheduled tick still tries.
func isPermanentOAuthError(statusCode int, body []byte) bool {
	// 401 without a Retry-After is always permanent for OAuth.
	if statusCode == 401 {
		return true
	}
	if statusCode != 400 {
		return false
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &errResp) != nil {
		return false
	}
	switch errResp.Error {
	case "invalid_grant", // revoked / used / expired refresh token
		"invalid_client",      // wrong client_id/secret
		"unauthorized_client", // client not allowed this grant type
		"unsupported_grant_type":
		return true
	}
	return false
}
