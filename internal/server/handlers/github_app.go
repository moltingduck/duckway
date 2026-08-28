package handlers

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

type githubAppCredential struct {
	Type           string `json:"type"`
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKey     string `json:"private_key"`
	BaseURL        string `json:"base_url,omitempty"`
}

type githubInstallationTokenRequest struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

type githubInstallationTokenResponse struct {
	Token       string            `json:"token"`
	ExpiresAt   string            `json:"expires_at"`
	Permissions map[string]string `json:"permissions,omitempty"`
}

type githubMintedInstallationToken struct {
	Token       string
	ExpiresAt   time.Time
	Permissions map[string]string
}

type githubAppTokenCache struct {
	token     string
	expiresAt time.Time
}

type githubAppMintCall struct {
	done  chan struct{}
	token string
	err   error
}

const maxGitHubAppTokenResponseBytes = 1024 * 1024

func parseGitHubAppCredential(realKey string) (*githubAppCredential, bool, error) {
	trimmed := strings.TrimSpace(realKey)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false, nil
	}
	var cred githubAppCredential
	if err := json.Unmarshal([]byte(trimmed), &cred); err != nil {
		return nil, true, fmt.Errorf("parse github_app credential JSON: %w", err)
	}
	if cred.Type != "github_app" {
		return nil, false, nil
	}
	if cred.AppID <= 0 || cred.InstallationID <= 0 || strings.TrimSpace(cred.PrivateKey) == "" {
		return nil, true, fmt.Errorf("github_app credential requires app_id, installation_id, and private_key")
	}
	if _, err := parseRSAPrivateKey(cred.PrivateKey); err != nil {
		return nil, true, err
	}
	return &cred, true, nil
}

func validateGitHubCredentialForService(serviceName, raw string) error {
	if serviceName != "github" {
		if isGitHubAppCredentialJSON(raw) {
			return fmt.Errorf("github_app credential can only be used with github service")
		}
		return nil
	}
	_, ok, err := parseGitHubAppCredential(raw)
	if err != nil {
		return err
	}
	if ok {
		return validateGitHubAppBaseURL(raw)
	}
	return nil
}

func isGitHubAppCredentialJSON(raw string) bool {
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &obj) != nil {
		return false
	}
	return obj.Type == "github_app"
}

func validateGitHubAppBaseURL(raw string) error {
	var cred githubAppCredential
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &cred) != nil || cred.BaseURL == "" {
		return nil
	}
	return validateGitHubAppBaseURLValue(cred.BaseURL)
}

func validateGitHubAppBaseURLValue(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("github_app base_url must be a valid URL")
	}
	host := strings.Split(u.Host, ":")[0]
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (host == "127.0.0.1" || host == "::1" || host == "localhost") {
		return nil
	}
	return fmt.Errorf("github_app base_url must use https")
}

func (h *ProxyHandler) mintGitHubInstallationToken(ctx context.Context, cred *githubAppCredential, method, upstreamPath, rawQuery, permissionConfig string) (token string, err error) {
	owner, repo := githubRepoFromPath(upstreamPath)
	if owner == "" || repo == "" {
		return "", fmt.Errorf("github app token mint requires a repository-scoped path")
	}
	permissions := githubTokenPermissions(method, upstreamPath, rawQuery)
	cacheKey := githubAppCacheKey(cred, owner, repo, permissions, permissionConfig)
	now := time.Now()

	h.githubAppMu.Lock()
	if h.githubAppTokens == nil {
		h.githubAppTokens = make(map[string]githubAppTokenCache)
	}
	if cached, ok := h.githubAppTokens[cacheKey]; ok && now.Before(cached.expiresAt.Add(-5*time.Minute)) {
		h.githubAppMu.Unlock()
		return cached.token, nil
	}
	if h.githubAppMints == nil {
		h.githubAppMints = make(map[string]*githubAppMintCall)
	}
	if pending := h.githubAppMints[cacheKey]; pending != nil {
		h.githubAppMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-pending.done:
			return pending.token, pending.err
		}
	}
	call := &githubAppMintCall{done: make(chan struct{})}
	h.githubAppMints[cacheKey] = call
	h.githubAppMu.Unlock()
	var expiresAt time.Time
	defer func() {
		h.githubAppMu.Lock()
		if err == nil {
			h.githubAppTokens[cacheKey] = githubAppTokenCache{token: token, expiresAt: expiresAt}
		}
		call.token = token
		call.err = err
		close(call.done)
		delete(h.githubAppMints, cacheKey)
		h.githubAppMu.Unlock()
	}()

	jwt, err := githubAppJWT(cred.AppID, cred.PrivateKey, time.Now())
	if err != nil {
		return "", err
	}

	body := githubInstallationTokenRequest{
		Repositories: []string{repo},
		Permissions:  permissions,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	baseURL := strings.TrimRight(cred.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if err := validateGitHubAppBaseURLValue(baseURL); err != nil {
		return "", err
	}
	minted, err := mintGitHubInstallationToken(ctx, h.httpClient, cred, baseURL, jwt, bodyBytes, now)
	if err != nil {
		return "", err
	}
	expiresAt = minted.ExpiresAt
	return minted.Token, nil
}

func (h *ProxyHandler) invalidateGitHubInstallationToken(cred *githubAppCredential, method, upstreamPath, rawQuery, permissionConfig, rejectedToken string) {
	owner, repo := githubRepoFromPath(upstreamPath)
	if owner == "" || repo == "" {
		return
	}
	cacheKey := githubAppCacheKey(cred, owner, repo, githubTokenPermissions(method, upstreamPath, rawQuery), permissionConfig)
	h.githubAppMu.Lock()
	defer h.githubAppMu.Unlock()
	if cached, ok := h.githubAppTokens[cacheKey]; ok && cached.token == rejectedToken {
		delete(h.githubAppTokens, cacheKey)
	}
}

func mintGitHubInstallationToken(ctx context.Context, httpClient *http.Client, cred *githubAppCredential, baseURL, jwt string, bodyBytes []byte, now time.Time) (*githubMintedInstallationToken, error) {
	tokenURL := fmt.Sprintf("%s/app/installations/%d/access_tokens", baseURL, cred.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github app token request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxGitHubAppTokenResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github app token request returned %d", resp.StatusCode)
	}
	if len(bytes.TrimSpace(respBody)) == 0 {
		return nil, fmt.Errorf("github app token response was empty (status %d)", resp.StatusCode)
	}

	var parsed githubInstallationTokenResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "unknown"
		}
		return nil, fmt.Errorf("github app token response was not JSON (status %d, content-type %s)", resp.StatusCode, ct)
	}
	if parsed.Token == "" {
		return nil, fmt.Errorf("github app token response missing token")
	}
	expiresAt, err := time.Parse(time.RFC3339, parsed.ExpiresAt)
	if err != nil || expiresAt.IsZero() {
		expiresAt = now.Add(50 * time.Minute)
	}
	return &githubMintedInstallationToken{Token: parsed.Token, ExpiresAt: expiresAt, Permissions: parsed.Permissions}, nil
}

func githubAppCacheKey(cred *githubAppCredential, owner, repo string, permissions map[string]string, permissionConfig string) string {
	privateKeyFingerprint := sha256.Sum256([]byte(cred.PrivateKey))
	policyFingerprint := sha256.Sum256([]byte(strings.TrimSpace(permissionConfig)))
	parts := []string{
		strconv.FormatInt(cred.AppID, 10),
		strconv.FormatInt(cred.InstallationID, 10),
		strings.TrimRight(cred.BaseURL, "/"),
		fmt.Sprintf("%x", privateKeyFingerprint),
		fmt.Sprintf("%x", policyFingerprint),
		strings.ToLower(owner + "/" + repo),
	}
	keys := make([]string, 0, len(permissions))
	for key := range permissions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		val := permissions[key]
		parts = append(parts, key+"="+val)
	}
	return strings.Join(parts, "|")
}

func githubAppJWT(appID int64, privateKeyPEM string, now time.Time) (string, error) {
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]int64{
		"iat": now.Add(-1 * time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseRSAPrivateKey(privateKeyPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("decode github app private key PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse github app private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github app private key must be RSA")
	}
	return key, nil
}

func githubTokenPermissions(method, upstreamPath, rawQuery string) map[string]string {
	scope, level := githubPermissionScope(method, upstreamPath, rawQuery)
	return map[string]string{scope: level}
}

func githubPermissionScope(method, upstreamPath, rawQuery string) (string, string) {
	level := "read"
	if githubRequestNeedsWrite(method, upstreamPath, rawQuery) {
		level = "write"
	}
	clean := path.Clean("/" + strings.TrimPrefix(upstreamPath, "/"))
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) >= 4 && parts[0] == "repos" {
		switch parts[3] {
		case "issues":
			return "issues", level
		case "pulls":
			return "pull_requests", level
		case "contents", "git":
			return "contents", level
		case "releases":
			return "contents", level
		case "actions":
			return "actions", level
		}
	}
	return "contents", level
}

func githubRequestNeedsWrite(method, upstreamPath, rawQuery string) bool {
	if strings.EqualFold(method, http.MethodGet) &&
		strings.HasSuffix(path.Clean("/"+strings.TrimPrefix(upstreamPath, "/")), ".git/info/refs") {
		values, err := url.ParseQuery(rawQuery)
		if err == nil && values.Get("service") == "git-receive-pack" {
			return true
		}
	}
	if strings.EqualFold(method, http.MethodPost) && strings.Contains(upstreamPath, "/git-receive-pack") {
		return true
	}
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return !strings.Contains(upstreamPath, "/git-upload-pack")
	default:
		return false
	}
}

func githubRepoFromPath(upstreamPath string) (string, string) {
	clean := path.Clean("/" + strings.TrimPrefix(upstreamPath, "/"))
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" {
		return parts[1], parts[2]
	}
	if len(parts) >= 2 && strings.HasSuffix(parts[1], ".git") {
		return parts[0], strings.TrimSuffix(parts[1], ".git")
	}
	return "", ""
}

func maskGitHubAppCredentialPreview(raw string) string {
	cred, ok, err := parseGitHubAppCredential(raw)
	if err != nil || !ok {
		return maskKey(raw)
	}
	return "github_app app_id=" + strconv.FormatInt(cred.AppID, 10) + " installation_id=" + strconv.FormatInt(cred.InstallationID, 10)
}
