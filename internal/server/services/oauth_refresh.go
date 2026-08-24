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
	"regexp"
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
	apiKeyQ      *queries.APIKeyQueries
	crypto       *Crypto
	client       *http.Client
	proxyClients *UpstreamProxyClientCache
	stopCh       chan struct{}
	stopOnce     sync.Once
}

var oauthRefreshLocks sync.Map // map api key id -> *sync.Mutex

// WithOAuthRefreshLock serializes refresh-token rotation for one stored API
// key across the scheduler, manual refreshes, and proxy-mediated Codex refreshes.
func WithOAuthRefreshLock(keyID string, fn func() error) error {
	if keyID == "" {
		return fn()
	}
	lockAny, _ := oauthRefreshLocks.LoadOrStore(keyID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func NewTokenRefresher(apiKeyQ *queries.APIKeyQueries, crypto *Crypto) *TokenRefresher {
	return &TokenRefresher{
		apiKeyQ:      apiKeyQ,
		crypto:       crypto,
		client:       &http.Client{Timeout: 30 * time.Second},
		proxyClients: NewUpstreamProxyClientCache(),
		stopCh:       make(chan struct{}),
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
		keyID := expiring[i].ID
		if err := WithOAuthRefreshLock(keyID, func() error {
			key, err := r.apiKeyQ.GetByID(keyID)
			if err != nil {
				return fmt.Errorf("reload key: %w", err)
			}
			if key.RefreshToken == "" {
				return fmt.Errorf("key has no refresh_token (not refreshable)")
			}
			// Another path may have refreshed while this key was waiting for
			// the rotation lock. Skip it if it no longer falls in the same
			// expiry window that ListExpiring(10) used above.
			if key.ExpiresAt > time.Now().Add(10*time.Minute).UnixMilli() {
				return nil
			}
			return r.refreshKey(key)
		}); err != nil {
			log.Printf("[token-refresh] Failed to refresh %s (%s): %v", expiring[i].Name, expiring[i].ID, err)
		} else {
			log.Printf("[token-refresh] Refreshed %s (%s)", expiring[i].Name, expiring[i].ID)
		}
	}
}

// RefreshNow performs an immediate refresh for the given key id, bypassing
// the schedule. Returns the new expires_at on success.
func (r *TokenRefresher) RefreshNow(id string) (int64, error) {
	if err := WithOAuthRefreshLock(id, func() error {
		key, err := r.apiKeyQ.GetByID(id)
		if err != nil {
			return fmt.Errorf("key not found: %w", err)
		}
		if key.RefreshToken == "" {
			return fmt.Errorf("key has no refresh_token (not refreshable)")
		}
		if err := r.refreshKey(key); err != nil {
			return err
		}
		if err := r.apiKeyQ.SetActive(id, true); err != nil {
			return fmt.Errorf("reactivate refreshed key: %w", err)
		}
		return nil
	}); err != nil {
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
	httpClient, upstreamProxyURL, err := r.clientForKey(key)
	if err != nil {
		return err
	}
	useCodexOpenAI := isCodexOpenAIRefresh(key)
	if useCodexOpenAI && !IsCodexOpenAIRefreshEndpoint(key.TokenEndpoint) {
		return fmt.Errorf("codex oauth token_endpoint must be https://auth.openai.com/oauth/token")
	}
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
		resp, err = httpClient.Do(req)
	} else if useCodexOpenAI {
		clientID := oauthMetadataString(key.SubscriptionInfo, "client_id", "clientId")
		if clientID == "" {
			clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
		}
		body, _ := json.Marshal(map[string]string{
			"client_id":     clientID,
			"grant_type":    "refresh_token",
			"refresh_token": refreshToken,
		})
		req, buildErr := http.NewRequest("POST", key.TokenEndpoint, bytes.NewReader(body))
		if buildErr != nil {
			return fmt.Errorf("build token request: %w", buildErr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err = httpClient.Do(req)
	} else {
		// Generic OAuth 2.0 — form-urlencoded body
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refreshToken},
		}
		if clientID := oauthMetadataString(key.SubscriptionInfo, "client_id", "clientId"); clientID != "" {
			form.Set("client_id", clientID)
		}
		req, buildErr := http.NewRequest("POST", key.TokenEndpoint, strings.NewReader(form.Encode()))
		if buildErr != nil {
			return fmt.Errorf("build token request: %w", buildErr)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err = httpClient.Do(req)
	}
	if err != nil {
		return fmt.Errorf("token request: %s", RedactProxyError(upstreamProxyURL, err))
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		// Detect permanent failures that will never recover. Stop retrying
		// immediately. Only deactivate when the current access token also
		// fails a live auth check; a revoked refresh token does not necessarily
		// mean the still-stored access token is unusable.
		if isPermanentOAuthError(resp.StatusCode, body) {
			if r.refreshTokenChanged(key.ID, refreshToken) {
				log.Printf("[token-refresh] Ignoring permanent OAuth error for %s because refresh token was already rotated", key.ID)
				return nil
			}
			latest, latestErr := r.apiKeyQ.GetByID(key.ID)
			if latestErr != nil {
				return fmt.Errorf("permanent OAuth error — key %s left in current active state because reload before access token test failed: %w", key.ID, latestErr)
			}
			if latest.RefreshToken != key.RefreshToken {
				log.Printf("[token-refresh] Ignoring permanent OAuth error for %s because refresh token was already rotated", key.ID)
				return nil
			}
			switch result, testErr := r.testStoredAccessToken(latest); result {
			case accessTokenTestPassed:
				return fmt.Errorf("permanent OAuth error — key %s left in current active state because access token test still succeeds: %s", key.ID, redactOAuthErrorBody(body, refreshToken))
			case accessTokenTestFailed:
				deactivated, deactivateErr := r.apiKeyQ.DeactivateIfCredentialSnapshot(
					latest.ID,
					latest.KeyEncrypted,
					latest.RefreshToken,
					latest.TokenEndpoint,
					latest.UpstreamProxyURL,
				)
				if deactivateErr != nil {
					return fmt.Errorf("permanent OAuth error — key %s access token test failed but deactivate failed: %w", key.ID, deactivateErr)
				}
				if !deactivated {
					return fmt.Errorf("permanent OAuth error — key %s left in current active state because credentials changed before deactivate", key.ID)
				}
				if testErr != nil {
					return fmt.Errorf("permanent OAuth error — key %s deactivated after access token test failed (%v): %s", key.ID, testErr, redactOAuthErrorBody(body, refreshToken))
				}
				return fmt.Errorf("permanent OAuth error — key %s deactivated after access token test failed: %s", key.ID, redactOAuthErrorBody(body, refreshToken))
			default:
				if testErr != nil {
					return fmt.Errorf("permanent OAuth error — key %s left in current active state because access token test was inconclusive (%v): %s", key.ID, testErr, redactOAuthErrorBody(body, refreshToken))
				}
				return fmt.Errorf("permanent OAuth error — key %s left in current active state because no access token test is available: %s", key.ID, redactOAuthErrorBody(body, refreshToken))
			}
		}
		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, redactOAuthErrorBody(body, refreshToken))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
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
	if isCodexOpenAIRefresh(key) {
		r.updateCodexRefreshMetadata(key.ID, key.SubscriptionInfo, tokenResp.IDToken)
	}

	return nil
}

type accessTokenTestResult int

const (
	accessTokenTestInconclusive accessTokenTestResult = iota
	accessTokenTestPassed
	accessTokenTestFailed
)

func (r *TokenRefresher) testStoredAccessToken(key *models.APIKey) (accessTokenTestResult, error) {
	if key.KeyEncrypted == "" {
		return accessTokenTestInconclusive, fmt.Errorf("missing access token")
	}
	accessToken, err := r.crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		return accessTokenTestInconclusive, fmt.Errorf("decrypt access token: %w", err)
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return accessTokenTestInconclusive, fmt.Errorf("empty access token")
	}

	req, err := buildAccessTokenTestRequest(key, accessToken)
	if err != nil {
		return accessTokenTestInconclusive, err
	}
	httpClient, upstreamProxyURL, err := r.clientForKey(key)
	if err != nil {
		return accessTokenTestInconclusive, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return accessTokenTestInconclusive, fmt.Errorf("token test request: %s", RedactProxyError(upstreamProxyURL, err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return accessTokenTestPassed, nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return accessTokenTestFailed, fmt.Errorf("token test returned %d", resp.StatusCode)
	}
	return accessTokenTestInconclusive, fmt.Errorf("token test returned %d", resp.StatusCode)
}

func buildAccessTokenTestRequest(key *models.APIKey, accessToken string) (*http.Request, error) {
	var endpoint string
	switch {
	case isCodexOpenAIRefresh(key) || key.ServiceName == "openai" || strings.Contains(key.TokenEndpoint, "auth.openai.com"):
		endpoint = "https://api.openai.com/v1/models"
	case key.ServiceName == "anthropic" || strings.Contains(key.TokenEndpoint, "anthropic"):
		endpoint = "https://api.anthropic.com/v1/models"
	default:
		return nil, fmt.Errorf("no access token test available for service %q", key.ServiceName)
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build access token test request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if strings.Contains(endpoint, "api.anthropic.com") {
		req.Header.Set("x-api-key", accessToken)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	return req, nil
}

func (r *TokenRefresher) updateCodexRefreshMetadata(keyID, subscriptionInfo, idToken string) {
	updated := mergeCodexRefreshMetadata(subscriptionInfo, idToken, time.Now().UTC())
	if updated == "" {
		return
	}
	if err := r.apiKeyQ.UpdateSubscriptionInfo(keyID, updated); err != nil {
		log.Printf("[token-refresh] Warning: failed to store Codex refresh metadata for %s: %v", keyID, err)
	}
}

func mergeCodexRefreshMetadata(raw, idToken string, now time.Time) string {
	metadata := map[string]interface{}{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &metadata)
		if metadata == nil {
			metadata = map[string]interface{}{}
		}
	}
	if idToken != "" {
		metadata["id_token"] = idToken
	}
	metadata["last_refresh"] = now.Format(time.RFC3339)
	out, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(out)
}

func (r *TokenRefresher) refreshTokenChanged(keyID, usedRefreshToken string) bool {
	key, err := r.apiKeyQ.GetByID(keyID)
	if err != nil || key.RefreshToken == "" {
		return false
	}
	currentRefreshToken, err := r.crypto.Decrypt(key.RefreshToken)
	if err != nil {
		return false
	}
	return currentRefreshToken != "" && currentRefreshToken != usedRefreshToken
}

func isCodexOpenAIRefresh(key *models.APIKey) bool {
	if IsCodexOpenAIRefreshEndpoint(key.TokenEndpoint) {
		return true
	}
	var metadata map[string]interface{}
	if json.Unmarshal([]byte(key.SubscriptionInfo), &metadata) != nil {
		return false
	}
	if v, _ := metadata["credential_kind"].(string); v == "codex_oauth" {
		return true
	}
	if v, _ := metadata["source"].(string); v == "codex" {
		return true
	}
	if v, _ := metadata["auth_mode"].(string); v == "chatgpt" {
		return true
	}
	return false
}

func IsCodexOpenAIRefreshEndpoint(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return u.Scheme == "https" &&
		u.User == nil &&
		u.Host == "auth.openai.com" &&
		u.Path == "/oauth/token" &&
		u.RawQuery == "" &&
		u.Fragment == ""
}

func (r *TokenRefresher) clientForKey(key *models.APIKey) (*http.Client, string, error) {
	upstreamProxyURL, err := DecryptUpstreamProxyURL(r.crypto, key.UpstreamProxyURL)
	if err != nil {
		return nil, "", fmt.Errorf("decrypt upstream proxy: %w", err)
	}
	if strings.TrimSpace(upstreamProxyURL) == "" {
		return r.client, "", nil
	}
	if r.proxyClients == nil {
		r.proxyClients = NewUpstreamProxyClientCache()
	}
	client, err := r.proxyClients.Client(upstreamProxyURL)
	return client, upstreamProxyURL, err
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
func IsPermanentOAuthError(statusCode int, body []byte) bool {
	return isPermanentOAuthError(statusCode, body)
}

func isPermanentOAuthError(statusCode int, body []byte) bool {
	// 401 without a Retry-After is always permanent for OAuth.
	if statusCode == 401 {
		return true
	}
	if statusCode != 400 {
		return false
	}
	var errResp struct {
		Error interface{} `json:"error"`
	}
	if json.Unmarshal(body, &errResp) != nil {
		return false
	}
	code := oauthErrorCode(errResp.Error)
	switch code {
	case "invalid_grant", // revoked / used / expired refresh token
		"invalid_refresh_token",
		"refresh_token_expired",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"invalid_client",      // wrong client_id/secret
		"unauthorized_client", // client not allowed this grant type
		"unsupported_grant_type":
		return true
	}
	return false
}

func oauthErrorCode(raw interface{}) string {
	switch v := raw.(type) {
	case string:
		return v
	case map[string]interface{}:
		if code, _ := v["code"].(string); code != "" {
			return code
		}
		if msg, _ := v["message"].(string); strings.Contains(strings.ToLower(msg), "invalid refresh token") {
			return "invalid_refresh_token"
		}
		if typ, _ := v["type"].(string); typ != "" {
			return typ
		}
	}
	return ""
}

var (
	oauthJWTRE          = regexp.MustCompile(`[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	oauthRefreshTokenRE = regexp.MustCompile(`rt\.[A-Za-z0-9._-]+`)
)

func redactOAuthErrorBody(body []byte, usedRefreshToken string) string {
	const maxBody = 2000
	text := string(body)
	if usedRefreshToken != "" {
		text = strings.ReplaceAll(text, usedRefreshToken, "[REDACTED_REFRESH_TOKEN]")
	}
	text = oauthRefreshTokenRE.ReplaceAllString(text, "[REDACTED_REFRESH_TOKEN]")
	text = oauthJWTRE.ReplaceAllString(text, "[REDACTED_JWT]")
	if len(text) > maxBody {
		return text[:maxBody] + "...[truncated]"
	}
	return text
}
