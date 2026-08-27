package services_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

const (
	liveClaudeTokenEndpoint = "https://console.anthropic.com/v1/oauth/token"
	liveCodexTokenEndpoint  = "https://auth.openai.com/oauth/token"
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

	resp := liveOAuthRefresh(t, liveClaudeTokenEndpoint, map[string]string{
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

	resp := liveOAuthRefresh(t, liveCodexTokenEndpoint, map[string]string{
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

func TestClaudeCodeOAuthLiveDuckwayUploadRefreshE2EIfCredentialsExist(t *testing.T) {
	requireLiveOAuthOptIn(t, liveClaudeOptInEnv)
	path, ok := liveCredentialPath(t, liveClaudeCredentialsPathEnv, liveClaudeCredentialsName)
	if !ok {
		t.Skipf("missing %s/%s; copy ~/.claude/.credentials.json there to run this live E2E test", liveCredentialDirName, liveClaudeCredentialsName)
	}
	enforcePrivateLiveCredentialFile(t, path)
	unlock := acquireLiveCredentialLock(t, path)
	defer unlock()

	doc := readLiveCredentialJSON(t, path)
	oauth := liveObject(doc, "claudeAiOauth")
	if oauth == nil {
		t.Fatalf("live Claude credentials %s must contain claudeAiOauth", path)
	}
	accessToken := liveString(oauth, "accessToken", "access_token")
	refreshToken := liveString(oauth, "refreshToken", "refresh_token")
	if accessToken == "" || refreshToken == "" {
		t.Fatalf("live Claude credentials %s must contain claudeAiOauth accessToken and refreshToken", path)
	}

	result := runLiveDuckwayOAuthE2E(t, liveDuckwayOAuthInput{
		provider:          "Claude Code",
		serviceName:       "anthropic",
		accessToken:       accessToken,
		refreshToken:      refreshToken,
		expiresAt:         liveInt(oauth, "expiresAt", "expires_at"),
		tokenEndpoint:     liveClaudeTokenEndpoint,
		subscriptionInfo:  liveClaudeSubscriptionInfo(t, oauth),
		clientEnvName:     "ANTHROPIC_AUTH_TOKEN",
		clientPlaceholder: "sk-ant-dw_live_claude_e2e_placeholder",
	})

	oauth["accessToken"] = result.accessToken
	oauth["refreshToken"] = result.refreshToken
	if result.expiresAt > 0 {
		oauth["expiresAt"] = result.expiresAt
	}
	doc["claudeAiOauth"] = oauth
	writeLiveCredentialJSON(t, path, doc)
}

func TestClaudeCodeOAuthLiveDuckwayLLME2EIfCredentialsExist(t *testing.T) {
	requireLiveOAuthOptIn(t, liveClaudeOptInEnv)
	path, ok := liveCredentialPath(t, liveClaudeCredentialsPathEnv, liveClaudeCredentialsName)
	if !ok {
		t.Skipf("missing %s/%s; copy ~/.claude/.credentials.json there to run this live E2E test", liveCredentialDirName, liveClaudeCredentialsName)
	}
	enforcePrivateLiveCredentialFile(t, path)
	doc := readLiveCredentialJSON(t, path)
	oauth := liveObject(doc, "claudeAiOauth")
	if oauth == nil {
		t.Fatalf("live Claude credentials %s must contain claudeAiOauth", path)
	}
	accessToken := liveString(oauth, "accessToken", "access_token")
	refreshToken := liveString(oauth, "refreshToken", "refresh_token")
	if accessToken == "" || refreshToken == "" {
		t.Fatalf("live Claude credentials %s must contain claudeAiOauth accessToken and refreshToken", path)
	}

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	serviceQ := queries.NewServiceQueries(db)
	serviceRow := ensureLiveOAuthService(t, serviceQ, "anthropic")
	encryptedAccess, err := crypto.Encrypt(accessToken)
	if err != nil {
		t.Fatal(err)
	}
	encryptedRefresh, err := crypto.Encrypt(refreshToken)
	if err != nil {
		t.Fatal(err)
	}
	const clientID = "client-live-claude-llm"
	const placeholder = "sk-ant-dw_live_claude_llm_placeholder"
	if _, err := db.Exec(`INSERT INTO clients (id, short_id, name, token_hash) VALUES (?, ?, ?, ?)`, clientID, "livcld", "live Claude LLM client", services.HashToken("unused")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, refresh_token) VALUES ('key-live-claude-llm', ?, 'live Claude LLM', ?, ?)`, serviceRow.ID, encryptedAccess, encryptedRefresh); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, requires_approval) VALUES ('ph-live-claude-llm', 'ANTHROPIC_AUTH_TOKEN', ?, ?, 'key-live-claude-llm', ?, 0)`, placeholder, serviceRow.ID, clientID); err != nil {
		t.Fatal(err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	placeholderQ := queries.NewPlaceholderQueries(db)
	groupQ := queries.NewGroupQueries(db)
	approvalQ := queries.NewApprovalQueries(db)
	resolver := services.NewKeyResolver(crypto, apiKeyQ, placeholderQ, groupQ, approvalQ)
	handler := handlers.NewProxyHandler(serviceQ, apiKeyQ, resolver, nil, approvalQ, nil, nil)
	body := `{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"Reply with OK"}]}`
	req := httptest.NewRequest(http.MethodPost, "/proxy/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+placeholder)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: clientID, Name: "live Claude LLM client"}))
	rec := httptest.NewRecorder()
	handler.Handle(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("Claude live LLM request failed: status=%d body=%s", rec.Code, redactLiveMessage(rec.Body.String()))
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Claude live LLM response: %v", err)
	}
	if response["id"] == nil || response["content"] == nil {
		t.Fatalf("Claude live LLM response missing id or content")
	}
}

func TestCodexOAuthLiveDuckwayUploadRefreshE2EIfCredentialsExist(t *testing.T) {
	requireLiveOAuthOptIn(t, liveCodexOptInEnv)
	path, ok := liveCredentialPath(t, liveCodexAuthPathEnv, liveCodexAuthName)
	if !ok {
		t.Skipf("missing %s/%s; copy ~/.codex/auth.json there to run this live E2E test", liveCredentialDirName, liveCodexAuthName)
	}
	enforcePrivateLiveCredentialFile(t, path)
	unlock := acquireLiveCredentialLock(t, path)
	defer unlock()

	doc := readLiveCredentialJSON(t, path)
	tokens := liveObject(doc, "tokens")
	if tokens == nil {
		tokens = doc
	}
	accessToken := liveString(tokens, "access_token", "accessToken")
	refreshToken := liveString(tokens, "refresh_token", "refreshToken")
	idToken := liveString(tokens, "id_token", "idToken")
	if accessToken == "" || refreshToken == "" || idToken == "" {
		t.Fatalf("live Codex auth %s must contain tokens.access_token, tokens.refresh_token, and tokens.id_token", path)
	}
	clientID := firstNonEmpty(liveString(tokens, "client_id", "clientId"), liveString(doc, "client_id", "clientId"), "app_EMoamEEZ73f0CkXaXp7hrann")

	result := runLiveDuckwayOAuthE2E(t, liveDuckwayOAuthInput{
		provider:          "Codex",
		serviceName:       "openai",
		accessToken:       accessToken,
		refreshToken:      refreshToken,
		expiresAt:         firstPositive(decodeJWTExpirationMillis(accessToken), liveInt(tokens, "expires_at", "expiresAt")),
		tokenEndpoint:     liveCodexTokenEndpoint,
		subscriptionInfo:  liveCodexSubscriptionInfo(t, doc, tokens, clientID, idToken),
		clientEnvName:     "OPENAI_API_KEY",
		clientPlaceholder: "sk-proj-dw_live_codex_e2e_placeholder",
	})

	tokens["access_token"] = result.accessToken
	tokens["refresh_token"] = result.refreshToken
	if nextIDToken := liveString(result.subscriptionInfo, "id_token"); nextIDToken != "" {
		tokens["id_token"] = nextIDToken
	}
	tokens["client_id"] = clientID
	doc["tokens"] = tokens
	doc["auth_mode"] = firstNonEmpty(liveString(doc, "auth_mode"), "chatgpt")
	doc["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	writeLiveCredentialJSON(t, path, doc)
}

type liveDuckwayOAuthInput struct {
	provider          string
	serviceName       string
	accessToken       string
	refreshToken      string
	expiresAt         int64
	tokenEndpoint     string
	subscriptionInfo  string
	clientEnvName     string
	clientPlaceholder string
}

type liveDuckwayOAuthResult struct {
	accessToken      string
	refreshToken     string
	expiresAt        int64
	subscriptionInfo map[string]interface{}
}

func runLiveDuckwayOAuthE2E(t *testing.T, in liveDuckwayOAuthInput) liveDuckwayOAuthResult {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open live E2E database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	apiKeyQ := queries.NewAPIKeyQueries(db)
	placeholderQ := queries.NewPlaceholderQueries(db)
	serviceQ := queries.NewServiceQueries(db)
	serviceRow := ensureLiveOAuthService(t, serviceQ, in.serviceName)
	h := handlers.NewOAuthHandler(apiKeyQ, placeholderQ, serviceQ, crypto)
	h.SetRefresher(services.NewTokenRefresher(apiKeyQ, crypto))

	payload := map[string]interface{}{
		"name":              in.provider + " live OAuth E2E",
		"service_id":        serviceRow.ID,
		"access_token":      in.accessToken,
		"refresh_token":     in.refreshToken,
		"expires_at":        in.expiresAt,
		"token_endpoint":    in.tokenEndpoint,
		"subscription_info": in.subscriptionInfo,
	}
	body := liveJSONBody(t, payload)
	validateRec := httptest.NewRecorder()
	h.Validate(validateRec, httptest.NewRequest(http.MethodPost, "/api/oauth/validate", bytes.NewReader(body)))
	if validateRec.Code != http.StatusOK {
		t.Fatalf("%s live Duckway validate failed: status=%d body=%s", in.provider, validateRec.Code, redactLiveMessage(validateRec.Body.String()))
	}

	uploadRec := httptest.NewRecorder()
	h.Upload(uploadRec, httptest.NewRequest(http.MethodPost, "/api/oauth/upload", bytes.NewReader(body)))
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("%s live Duckway upload failed: status=%d body=%s", in.provider, uploadRec.Code, redactLiveMessage(uploadRec.Body.String()))
	}
	var uploadResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(uploadRec.Body.Bytes(), &uploadResp); err != nil || uploadResp.ID == "" {
		t.Fatalf("%s live Duckway upload returned invalid response", in.provider)
	}

	clientID := "client-live-oauth-e2e"
	if _, err := db.Exec(`INSERT INTO clients (id, short_id, name, token_hash) VALUES (?, ?, ?, ?)`, clientID, "livoau", "live oauth e2e client", services.HashToken("unused")); err != nil {
		t.Fatalf("create live E2E client: %v", err)
	}
	if err := placeholderQ.Create(&models.PlaceholderKey{
		ID:          "ph-live-oauth-e2e",
		EnvName:     in.clientEnvName,
		Placeholder: in.clientPlaceholder,
		ServiceID:   serviceRow.ID,
		APIKeyID:    &uploadResp.ID,
		ClientID:    clientID,
	}); err != nil {
		t.Fatalf("create live E2E placeholder: %v", err)
	}
	assertLiveClientCredentialsArePhantom(t, h, in, clientID)

	refreshReq := httptest.NewRequest(http.MethodPost, "/api/oauth/"+uploadResp.ID+"/refresh", nil)
	refreshReq.SetPathValue("id", uploadResp.ID)
	refreshRec := httptest.NewRecorder()
	h.Refresh(refreshRec, refreshReq)
	if refreshRec.Code != http.StatusOK {
		handleLiveDuckwayRefreshFailure(t, in.provider, refreshRec)
	}

	key, err := apiKeyQ.GetByID(uploadResp.ID)
	if err != nil {
		t.Fatalf("reload %s live E2E key: %v", in.provider, err)
	}
	accessToken, err := crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		t.Fatalf("decrypt %s live E2E access token: %v", in.provider, err)
	}
	refreshToken, err := crypto.Decrypt(key.RefreshToken)
	if err != nil {
		t.Fatalf("decrypt %s live E2E refresh token: %v", in.provider, err)
	}
	if accessToken == "" || refreshToken == "" {
		t.Fatalf("%s live E2E refresh stored empty tokens", in.provider)
	}
	if accessToken == in.accessToken && refreshToken == in.refreshToken {
		t.Fatalf("%s live E2E refresh did not rotate or update tokens", in.provider)
	}
	subInfo := map[string]interface{}{}
	if key.SubscriptionInfo != "" {
		_ = json.Unmarshal([]byte(key.SubscriptionInfo), &subInfo)
	}
	return liveDuckwayOAuthResult{
		accessToken:      accessToken,
		refreshToken:     refreshToken,
		expiresAt:        key.ExpiresAt,
		subscriptionInfo: subInfo,
	}
}

func ensureLiveOAuthService(t *testing.T, serviceQ *queries.ServiceQueries, serviceName string) *models.Service {
	t.Helper()
	serviceRow, err := serviceQ.GetByName(serviceName)
	if err == nil {
		return serviceRow
	}
	if err != sql.ErrNoRows {
		t.Fatalf("get %s service for live E2E: %v", serviceName, err)
	}

	serviceRow = &models.Service{ID: "svc-live-oauth-e2e-" + serviceName, Name: serviceName, DeliveryMode: "proxy", IsActive: true}
	switch serviceName {
	case "anthropic":
		serviceRow.DisplayName = "Anthropic API"
		serviceRow.UpstreamURL = "https://api.anthropic.com"
		serviceRow.HostPattern = "api.anthropic.com"
		serviceRow.AuthType = "header"
		serviceRow.AuthHeader = "x-api-key"
		serviceRow.AuthPrefix = ""
		serviceRow.KeyPrefix = "sk-ant-"
		serviceRow.KeyLength = 108
		serviceRow.KeyDirectory = ".config/anthropic/credentials"
	case "openai":
		serviceRow.DisplayName = "OpenAI API"
		serviceRow.UpstreamURL = "https://api.openai.com"
		serviceRow.HostPattern = "api.openai.com"
		serviceRow.AuthType = "bearer"
		serviceRow.AuthHeader = "Authorization"
		serviceRow.AuthPrefix = "Bearer "
		serviceRow.KeyPrefix = "sk-"
		serviceRow.KeyLength = 164
		serviceRow.KeyDirectory = ".config/openai/credentials"
	default:
		t.Fatalf("unsupported live OAuth service %q", serviceName)
	}
	if err := serviceQ.Create(serviceRow); err != nil {
		t.Fatalf("seed %s service for live E2E: %v", serviceName, err)
	}
	serviceRow, err = serviceQ.GetByName(serviceName)
	if err != nil {
		t.Fatalf("reload seeded %s service for live E2E: %v", serviceName, err)
	}
	return serviceRow
}

func assertLiveClientCredentialsArePhantom(t *testing.T, h *handlers.OAuthHandler, in liveDuckwayOAuthInput, clientID string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/client/oauth-credentials", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: clientID, Name: "live oauth e2e client"}))
	rec := httptest.NewRecorder()
	switch in.serviceName {
	case "anthropic":
		h.ClientGetCredentials(rec, req)
	case "openai":
		h.ClientGetCodexCredentials(rec, req)
	default:
		return
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s live client credential endpoint failed: status=%d body=%s", in.provider, rec.Code, redactLiveMessage(rec.Body.String()))
	}
	body := rec.Body.String()
	for _, secret := range []string{in.accessToken, in.refreshToken} {
		if secret != "" && strings.Contains(body, secret) {
			t.Fatalf("%s live client credential response leaked real token", in.provider)
		}
	}
	if in.serviceName == "anthropic" && !strings.Contains(body, in.clientPlaceholder) {
		t.Fatalf("%s live client credential response did not include expected phantom placeholder", in.provider)
	}
	if in.serviceName == "openai" && !strings.Contains(body, "rt.duckway.") {
		t.Fatalf("%s live client credential response did not include Codex phantom refresh token", in.provider)
	}
}

func handleLiveDuckwayRefreshFailure(t *testing.T, provider string, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	redacted := redactLiveMessage(body)
	lower := strings.ToLower(body)
	if rec.Code == http.StatusBadGateway &&
		os.Getenv(liveCredentialStrictEnv) != "1" &&
		(strings.Contains(lower, "invalid_grant") ||
			strings.Contains(lower, "invalid_refresh_token") ||
			strings.Contains(lower, "refresh_token_expired") ||
			strings.Contains(lower, "refresh_token_reused") ||
			strings.Contains(lower, "refresh_token_invalidated")) {
		t.Skipf("%s live Duckway E2E credential is present but cannot refresh (%s); sign in again and replace the ignored credential file, or set %s=1 to fail strictly", provider, redacted, liveCredentialStrictEnv)
	}
	t.Fatalf("%s live Duckway refresh failed: status=%d body=%s", provider, rec.Code, redacted)
}

func liveClaudeSubscriptionInfo(t *testing.T, oauth map[string]interface{}) string {
	t.Helper()
	out := map[string]interface{}{}
	for _, key := range []string{"subscriptionType", "rateLimitTier", "scopes"} {
		if value, ok := oauth[key]; ok {
			out[key] = value
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal Claude subscription_info: %v", err)
	}
	return string(data)
}

func liveCodexSubscriptionInfo(t *testing.T, doc, tokens map[string]interface{}, clientID, idToken string) string {
	t.Helper()
	out := map[string]interface{}{
		"credential_kind": "codex_oauth",
		"auth_mode":       firstNonEmpty(liveString(doc, "auth_mode"), "chatgpt"),
		"source":          "codex",
		"client_id":       clientID,
		"id_token":        idToken,
	}
	for _, key := range []string{"account_id", "last_refresh"} {
		if value := firstNonEmpty(liveString(tokens, key), liveString(doc, key)); value != "" {
			out[key] = value
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal Codex subscription_info: %v", err)
	}
	return string(data)
}

func liveJSONBody(t *testing.T, value interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON body: %v", err)
	}
	return data
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

func liveInt(obj map[string]interface{}, keys ...string) int64 {
	for _, key := range keys {
		switch value := obj[key].(type) {
		case float64:
			return int64(value)
		case int64:
			return value
		case int:
			return int64(value)
		case json.Number:
			n, _ := value.Int64()
			return n
		}
	}
	return 0
}

func firstPositive(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
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
