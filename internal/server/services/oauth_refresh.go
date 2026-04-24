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
)

type OAuthRefresher struct {
	oauthQ *queries.OAuthQueries
	crypto *Crypto
	client *http.Client
	stopCh chan struct{}
}

func NewOAuthRefresher(oauthQ *queries.OAuthQueries, crypto *Crypto) *OAuthRefresher {
	return &OAuthRefresher{
		oauthQ: oauthQ,
		crypto: crypto,
		client: &http.Client{Timeout: 30 * time.Second},
		stopCh: make(chan struct{}),
	}
}

// Start begins the background refresh loop (every 5 minutes).
func (r *OAuthRefresher) Start() {
	go r.refreshLoop()
	log.Printf("OAuth refresh job started (checking every 5 minutes)")
}

func (r *OAuthRefresher) Stop() {
	close(r.stopCh)
}

func (r *OAuthRefresher) refreshLoop() {
	// Check immediately on startup
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

func (r *OAuthRefresher) refreshExpiring() {
	// Find tokens expiring within 10 minutes
	expiring, err := r.oauthQ.ListExpiring(10)
	if err != nil {
		log.Printf("[oauth-refresh] Error listing expiring tokens: %v", err)
		return
	}

	for _, cred := range expiring {
		if err := r.refreshToken(&cred); err != nil {
			log.Printf("[oauth-refresh] Failed to refresh %s (%s): %v", cred.Name, cred.ID, err)
		} else {
			log.Printf("[oauth-refresh] Refreshed %s (%s)", cred.Name, cred.ID)
		}
	}
}

// RefreshToken refreshes a single OAuth credential.
func (r *OAuthRefresher) refreshToken(cred *queries.OAuthCredential) error {
	// Decrypt refresh token
	refreshToken, err := r.crypto.Decrypt(cred.RefreshToken)
	if err != nil {
		return fmt.Errorf("decrypt refresh token: %w", err)
	}

	// Call token endpoint
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if cred.ClientIDOAuth != "" {
		form.Set("client_id", cred.ClientIDOAuth)
	}

	resp, err := r.client.Post(cred.TokenEndpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("token endpoint request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"` // seconds
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("parse token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return fmt.Errorf("empty access token in response")
	}

	// Encrypt and store new access token
	encAccess, err := r.crypto.Encrypt(tokenResp.AccessToken)
	if err != nil {
		return fmt.Errorf("encrypt new access token: %w", err)
	}

	expiresAt := time.Now().UnixMilli() + tokenResp.ExpiresIn*1000
	if err := r.oauthQ.UpdateTokens(cred.ID, encAccess, expiresAt); err != nil {
		return fmt.Errorf("store refreshed token: %w", err)
	}

	// If a new refresh token was returned, update it too
	if tokenResp.RefreshToken != "" && tokenResp.RefreshToken != refreshToken {
		encRefresh, err := r.crypto.Encrypt(tokenResp.RefreshToken)
		if err == nil {
			r.oauthQ.UpdateRefreshToken(cred.ID, encRefresh)
		}
	}

	return nil
}
