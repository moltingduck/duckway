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
	"path"
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
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

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
	_, ok, err := parseGitHubAppCredential(raw)
	if err != nil {
		return err
	}
	if ok && serviceName != "github" {
		return fmt.Errorf("github_app credential can only be used with github service")
	}
	return nil
}

func (h *ProxyHandler) mintGitHubInstallationToken(ctx context.Context, cred *githubAppCredential, method, upstreamPath string) (string, error) {
	jwt, err := githubAppJWT(cred.AppID, cred.PrivateKey, time.Now())
	if err != nil {
		return "", err
	}

	body := githubInstallationTokenRequest{
		Repositories: githubTokenRepositories(upstreamPath),
		Permissions:  githubTokenPermissions(method, upstreamPath),
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	baseURL := strings.TrimRight(cred.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	tokenURL := fmt.Sprintf("%s/app/installations/%d/access_tokens", baseURL, cred.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github app token request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github app token request returned %d", resp.StatusCode)
	}

	var parsed githubInstallationTokenResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decode github app token response: %w", err)
	}
	if parsed.Token == "" {
		return "", fmt.Errorf("github app token response missing token")
	}
	return parsed.Token, nil
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

func githubTokenRepositories(upstreamPath string) []string {
	owner, repo := githubRepoFromPath(upstreamPath)
	if owner == "" || repo == "" {
		return nil
	}
	return []string{repo}
}

func githubTokenPermissions(method, upstreamPath string) map[string]string {
	if githubRequestNeedsWrite(method, upstreamPath) {
		return map[string]string{"contents": "write"}
	}
	return map[string]string{"contents": "read"}
}

func githubRequestNeedsWrite(method, upstreamPath string) bool {
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
