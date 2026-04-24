package services

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

// TokenRefresher automatically refreshes API keys that have a refresh_token set.
// Works for any OAuth-based key (Claude, GitHub Apps, etc.)
type TokenRefresher struct {
	apiKeyQ *queries.APIKeyQueries
	crypto  *Crypto
	client  *http.Client
	stopCh  chan struct{}
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
	close(r.stopCh)
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

func (r *TokenRefresher) refreshKey(key *models.APIKey) error {
	refreshToken, err := r.crypto.Decrypt(key.RefreshToken)
	if err != nil {
		return fmt.Errorf("decrypt refresh token: %w", err)
	}

	if key.TokenEndpoint == "" {
		return fmt.Errorf("no token_endpoint configured")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}

	resp, err := r.client.Post(key.TokenEndpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
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

	expiresAt := time.Now().UnixMilli() + tokenResp.ExpiresIn*1000
	if err := r.apiKeyQ.UpdateTokens(key.ID, encAccess, expiresAt); err != nil {
		return fmt.Errorf("store: %w", err)
	}

	if tokenResp.RefreshToken != "" && tokenResp.RefreshToken != refreshToken {
		encRefresh, err := r.crypto.Encrypt(tokenResp.RefreshToken)
		if err == nil {
			r.apiKeyQ.UpdateRefreshToken(key.ID, encRefresh)
		}
	}

	return nil
}
