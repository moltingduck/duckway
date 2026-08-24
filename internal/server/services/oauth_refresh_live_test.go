package services_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	liveCredentialDirName        = "live-credentials"
	liveClaudeCredentialsName    = "claude-credentials.json"
	liveCodexAuthName            = "codex-auth.json"
	liveOAuthOptInEnv            = "DUCKWAY_TEST_OAUTH_LIVE"
	liveClaudeOptInEnv           = "DUCKWAY_TEST_CLAUDE_OAUTH_LIVE"
	liveCodexOptInEnv            = "DUCKWAY_TEST_CODEX_OAUTH_LIVE"
	liveCredentialStrictEnv      = "DUCKWAY_LIVE_CREDENTIALS_STRICT"
	liveClaudeCredentialsPathEnv = "DUCKWAY_CLAUDE_LIVE_CREDENTIALS"
	liveCodexAuthPathEnv         = "DUCKWAY_CODEX_LIVE_AUTH"
)

var (
	liveOAuthJWTRE          = regexp.MustCompile(`[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	liveOAuthRefreshTokenRE = regexp.MustCompile(`rt\.[A-Za-z0-9._-]+`)
)

func TestClaudeCodeOAuthLiveRefreshIfCredentialsExist(t *testing.T) {
	requireLiveOAuthOptIn(t, liveClaudeOptInEnv)
	path, ok := liveCredentialPath(t, liveClaudeCredentialsPathEnv, liveClaudeCredentialsName)
	if !ok {
		t.Skipf("missing %s/%s; copy ~/.claude/.credentials.json there to run this live test", liveCredentialDirName, liveClaudeCredentialsName)
	}
	enforcePrivateLiveCredentialFile(t, path)
	unlock := acquireLiveCredentialLock(t, path)
	defer unlock()

	doc := readLiveCredentialJSON(t, path)
	oauth, ok := doc["claudeAiOauth"].(map[string]interface{})
	if !ok {
		t.Fatalf("live Claude credentials %s must contain claudeAiOauth", path)
	}
	refreshToken := liveString(oauth, "refreshToken", "refresh_token")
	if refreshToken == "" {
		t.Fatalf("live Claude credentials %s missing claudeAiOauth.refreshToken", path)
	}

	resp := liveOAuthRefresh(t, "https://console.anthropic.com/v1/oauth/token", map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
	})
	if !resp.ok {
		handleLiveRefreshFailure(t, "Claude Code", resp)
		return
	}
	accessToken := resp.stringValue("access_token", "accessToken")
	if accessToken == "" {
		t.Fatalf("Claude Code live refresh succeeded but response had no access token")
	}
	oauth["accessToken"] = accessToken
	if nextRefresh := resp.stringValue("refresh_token", "refreshToken"); nextRefresh != "" {
		oauth["refreshToken"] = nextRefresh
	}
	if expiresIn := resp.intValue("expires_in", "expiresIn"); expiresIn > 0 {
		oauth["expiresAt"] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	}
	doc["claudeAiOauth"] = oauth
	writeLiveCredentialJSON(t, path, doc)
}

func TestCodexOAuthLiveRefreshIfCredentialsExist(t *testing.T) {
	requireLiveOAuthOptIn(t, liveCodexOptInEnv)
	path, ok := liveCredentialPath(t, liveCodexAuthPathEnv, liveCodexAuthName)
	if !ok {
		t.Skipf("missing %s/%s; copy ~/.codex/auth.json there to run this live test", liveCredentialDirName, liveCodexAuthName)
	}
	enforcePrivateLiveCredentialFile(t, path)
	unlock := acquireLiveCredentialLock(t, path)
	defer unlock()

	doc := readLiveCredentialJSON(t, path)
	tokens := liveObject(doc, "tokens")
	if tokens == nil {
		tokens = doc
	}
	refreshToken := liveString(tokens, "refresh_token", "refreshToken")
	if refreshToken == "" {
		t.Fatalf("live Codex auth %s missing tokens.refresh_token", path)
	}
	clientID := liveString(tokens, "client_id", "clientId")
	if clientID == "" {
		clientID = liveString(doc, "client_id", "clientId")
	}
	if clientID == "" {
		clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	}

	resp := liveOAuthRefresh(t, officialCodexRefreshEndpoint, map[string]string{
		"client_id":     clientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if !resp.ok {
		handleLiveRefreshFailure(t, "Codex", resp)
		return
	}
	for _, pair := range [][2]string{
		{"access_token", "accessToken"},
		{"refresh_token", "refreshToken"},
		{"id_token", "idToken"},
	} {
		if value := resp.stringValue(pair[0], pair[1]); value != "" {
			tokens[pair[0]] = value
		}
	}
	tokens["client_id"] = clientID
	doc["tokens"] = tokens
	doc["auth_mode"] = firstNonEmpty(liveString(doc, "auth_mode"), "chatgpt")
	doc["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	writeLiveCredentialJSON(t, path, doc)
}

func requireLiveOAuthOptIn(t *testing.T, providerEnv string) {
	t.Helper()
	if os.Getenv(liveOAuthOptInEnv) == "1" || os.Getenv(providerEnv) == "1" {
		return
	}
	t.Skipf("set %s=1 or %s=1 to run live OAuth refresh tests", liveOAuthOptInEnv, providerEnv)
}

type liveRefreshResponse struct {
	statusCode int
	body       map[string]interface{}
	ok         bool
}

func liveOAuthRefresh(t *testing.T, endpoint string, payload map[string]string) liveRefreshResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal live refresh body: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build live refresh request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("live OAuth refresh request failed: %v", err)
	}
	defer httpResp.Body.Close()
	var parsed map[string]interface{}
	_ = json.NewDecoder(httpResp.Body).Decode(&parsed)
	if parsed == nil {
		parsed = map[string]interface{}{}
	}
	return liveRefreshResponse{
		statusCode: httpResp.StatusCode,
		body:       parsed,
		ok:         httpResp.StatusCode >= 200 && httpResp.StatusCode < 300,
	}
}

func handleLiveRefreshFailure(t *testing.T, provider string, resp liveRefreshResponse) {
	t.Helper()
	code, message := liveOAuthError(resp.body)
	if isLivePermanentRefreshFailure(resp.statusCode, code) && os.Getenv(liveCredentialStrictEnv) != "1" {
		t.Skipf("%s live credential is present but cannot refresh (%d %s: %s); sign in again and replace the ignored credential file, or set %s=1 to fail strictly", provider, resp.statusCode, code, message, liveCredentialStrictEnv)
	}
	t.Fatalf("%s live refresh failed: http=%d code=%s message=%s", provider, resp.statusCode, code, message)
}

func liveOAuthError(body map[string]interface{}) (string, string) {
	raw := body["error"]
	if obj, ok := raw.(map[string]interface{}); ok {
		code := liveString(obj, "code", "type")
		message := liveString(obj, "message")
		return code, redactLiveMessage(message)
	}
	if s, ok := raw.(string); ok {
		return s, ""
	}
	return liveString(body, "code"), redactLiveMessage(liveString(body, "message"))
}

func isLivePermanentRefreshFailure(statusCode int, code string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	if statusCode == http.StatusUnauthorized {
		return true
	}
	switch code {
	case "invalid_grant", "invalid_refresh_token", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		return true
	default:
		return false
	}
}

func liveCredentialPath(t *testing.T, envName, fileName string) (string, bool) {
	t.Helper()
	if path := strings.TrimSpace(os.Getenv(envName)); path != "" {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s points to unreadable credential file %s: %v", envName, path, err)
		}
		return path, true
	}
	root := repoRootFromTest(t)
	path := filepath.Join(root, liveCredentialDirName, fileName)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return path, false
		}
		t.Fatalf("stat live credential file %s: %v", path, err)
	}
	return path, true
}

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repository root from %s", wd)
		}
	}
}

func enforcePrivateLiveCredentialFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live credential file %s: %v", path, err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("refusing to use live credential file %s with permissions %o; run chmod 600", path, info.Mode().Perm())
	}
}

func acquireLiveCredentialLock(t *testing.T, credentialPath string) func() {
	t.Helper()
	lockPath := credentialPath + ".lock"
	file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			t.Skipf("live credential lock already exists at %s; another live refresh may be running", lockPath)
		}
		t.Fatalf("create live credential lock %s: %v", lockPath, err)
	}
	_, _ = fmt.Fprintf(file, "pid=%d time=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	_ = file.Close()
	return func() {
		_ = os.Remove(lockPath)
	}
}

func readLiveCredentialJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read live credential file %s: %v", path, err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse live credential file %s: %v", path, err)
	}
	return doc
}

func writeLiveCredentialJSON(t *testing.T, path string, doc map[string]interface{}) {
	t.Helper()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal updated live credential file: %v", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		t.Fatalf("write temp live credential file %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		t.Fatalf("replace live credential file %s: %v", path, err)
	}
	_ = os.Chmod(path, 0600)
}

func liveObject(obj map[string]interface{}, key string) map[string]interface{} {
	if nested, ok := obj[key].(map[string]interface{}); ok {
		return nested
	}
	return nil
}

func liveString(obj map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (r liveRefreshResponse) stringValue(keys ...string) string {
	return liveString(r.body, keys...)
}

func (r liveRefreshResponse) intValue(keys ...string) int64 {
	for _, key := range keys {
		switch value := r.body[key].(type) {
		case float64:
			return int64(value)
		case int64:
			return value
		case json.Number:
			n, _ := value.Int64()
			return n
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func redactLiveMessage(message string) string {
	message = liveOAuthRefreshTokenRE.ReplaceAllString(message, "[REDACTED_REFRESH_TOKEN]")
	message = liveOAuthJWTRE.ReplaceAllString(message, "[REDACTED_JWT]")
	if len(message) > 500 {
		return message[:500] + "...[truncated]"
	}
	return message
}

func decodeJWTExpirationMillis(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload := parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4)
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(decoded, &claims) != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp * 1000
}

func TestDecodeJWTExpirationMillisForLiveHelper(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1893456000}`))
	if got := decodeJWTExpirationMillis("header." + payload + ".sig"); got != 1893456000000 {
		t.Fatalf("decodeJWTExpirationMillis = %d", got)
	}
}
