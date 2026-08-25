package handlers_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestClientGetCodexCredentials(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt("codex-access-token")
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("codex-refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-1','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	subInfo := `{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex","account_id":"acct-1","id_token":"` + testJWT() + `","last_refresh":"2026-07-01T00:00:00Z"}`
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info)
		VALUES ('key-codex',?,'codex oauth',?,?, 'https://auth.openai.com/oauth/token', ?)`,
		openaiSvc.ID, encAccess, encRefresh, subInfo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES ('ph-openai','OPENAI_API_KEY','sk-proj-dw_fake',?,'key-codex','client-1',0)`, openaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		crypto,
	)
	req := httptest.NewRequest(http.MethodGet, "/client/codex-credentials", nil)
	client := &models.Client{ID: "client-1", Name: "client"}
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, client))
	rec := httptest.NewRecorder()

	h.ClientGetCodexCredentials(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	tokens, _ := got["tokens"].(map[string]interface{})
	if got["auth_mode"] != "chatgpt" || tokens["account_id"] != "duckway-account" {
		t.Fatalf("unexpected response: %#v", got)
	}
	accessToken, _ := tokens["access_token"].(string)
	refreshToken, _ := tokens["refresh_token"].(string)
	idToken, _ := tokens["id_token"].(string)
	for label, tok := range map[string]string{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"id_token":      idToken,
	} {
		if tok == "" {
			t.Fatalf("%s missing in response: %#v", label, got)
		}
		if strings.Contains(tok, "codex-access-token") || strings.Contains(tok, "codex-refresh-token") || tok == testJWT() {
			t.Fatalf("%s leaked real token: %q", label, tok)
		}
	}
	if !looksLikeJWTForTest(accessToken) || !looksLikeJWTForTest(idToken) {
		t.Fatalf("expected fake JWT-shaped tokens: %#v", tokens)
	}
	if !strings.HasPrefix(refreshToken, "rt.duckway.sk-proj-dw_fake") {
		t.Fatalf("unexpected fake refresh token: %q", refreshToken)
	}
	if strings.Contains(rec.Body.String(), "acct-1") {
		t.Fatalf("response leaked real account id: %s", rec.Body.String())
	}
}

func TestValidateCodexOAuth(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		services.NewCrypto([]byte("0123456789abcdef0123456789abcdef")),
	)

	body := `{
		"service_id": "` + openaiSvc.ID + `",
		"access_token": "` + testJWT() + `",
		"refresh_token": "rt.1.good",
		"token_endpoint": "https://auth.openai.com/oauth/token",
		"subscription_info": "{\"credential_kind\":\"codex_oauth\",\"auth_mode\":\"chatgpt\",\"source\":\"codex\",\"id_token\":\"` + testJWT() + `\"}"
	}`
	rec := httptest.NewRecorder()
	h.Validate(rec, httptest.NewRequest(http.MethodPost, "/api/oauth/validate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestValidateOAuthTestsConfiguredUpstreamProxy(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}

	var proxyMethod string
	var proxyTarget string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyMethod = r.Method
		proxyTarget = r.URL.String()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(proxy.Close)

	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		services.NewCrypto([]byte("0123456789abcdef0123456789abcdef")),
	)
	body := `{
		"service_id": "` + openaiSvc.ID + `",
		"access_token": "access-token",
		"refresh_token": "refresh-token",
		"token_endpoint": "http://provider.example/oauth/token",
		"upstream_proxy_url": "` + proxy.URL + `",
		"subscription_info": "{}"
	}`
	rec := httptest.NewRecorder()
	h.Validate(rec, httptest.NewRequest(http.MethodPost, "/api/oauth/validate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if proxyMethod != http.MethodHead {
		t.Fatalf("proxy method = %q, want HEAD", proxyMethod)
	}
	if proxyTarget != "http://provider.example/oauth/token" {
		t.Fatalf("proxy target = %q", proxyTarget)
	}
	var got struct {
		UpstreamProxyTested bool     `json:"upstream_proxy_tested"`
		Warnings            []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.UpstreamProxyTested {
		t.Fatalf("upstream_proxy_tested not set: %s", rec.Body.String())
	}
	if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[len(got.Warnings)-1], "upstream proxy") {
		t.Fatalf("proxy success warning missing: %+v", got.Warnings)
	}
}

func TestValidateOAuthFailsWhenConfiguredUpstreamProxyReturns407(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	t.Cleanup(proxy.Close)

	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		services.NewCrypto([]byte("0123456789abcdef0123456789abcdef")),
	)
	body := `{
		"service_id": "` + openaiSvc.ID + `",
		"access_token": "access-token",
		"refresh_token": "refresh-token",
		"token_endpoint": "http://provider.example/oauth/token",
		"upstream_proxy_url": "` + proxy.URL + `",
		"subscription_info": "{}"
	}`
	rec := httptest.NewRecorder()
	h.Validate(rec, httptest.NewRequest(http.MethodPost, "/api/oauth/validate", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "proxy authentication required") {
		t.Fatalf("expected proxy auth failure, got %s", rec.Body.String())
	}
}

func TestUploadOAuthTestsConfiguredUpstreamProxy(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}

	var proxyTarget string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyTarget = r.URL.String()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(proxy.Close)

	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		services.NewCrypto([]byte("0123456789abcdef0123456789abcdef")),
	)
	body := `{
		"name": "oauth token",
		"service_id": "` + openaiSvc.ID + `",
		"access_token": "access-token",
		"refresh_token": "refresh-token",
		"token_endpoint": "http://provider.example/oauth/token",
		"upstream_proxy_url": "` + proxy.URL + `",
		"subscription_info": "{}"
	}`
	rec := httptest.NewRecorder()
	h.Upload(rec, httptest.NewRequest(http.MethodPost, "/api/oauth", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if proxyTarget != "http://provider.example/oauth/token" {
		t.Fatalf("proxy target = %q", proxyTarget)
	}
}

func TestValidateUpdateOAuthUsesExistingTokensAndUpstreamProxy(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	var proxyTarget string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyTarget = r.URL.String()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(proxy.Close)

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt(testJWT())
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.1.good")
	if err != nil {
		t.Fatal(err)
	}
	encProxy, err := services.EncryptUpstreamProxyURL(crypto, proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info,upstream_proxy_url)
		VALUES ('key-edit-validate',?,'oauth token',?,?, 'http://provider.example/oauth/token', '{}', ?)`,
		openaiSvc.ID, encAccess, encRefresh, encProxy); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewOAuthHandler(queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), svcQ, crypto)
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/key-edit-validate/validate", strings.NewReader(`{
		"name":"oauth token",
		"token_endpoint":"http://provider.example/oauth/token",
		"subscription_info":"{}"
	}`))
	req.SetPathValue("id", "key-edit-validate")
	rec := httptest.NewRecorder()

	h.ValidateUpdate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if proxyTarget != "http://provider.example/oauth/token" {
		t.Fatalf("proxy target = %q", proxyTarget)
	}
	var got struct {
		UpstreamProxyTested bool     `json:"upstream_proxy_tested"`
		Warnings            []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.UpstreamProxyTested {
		t.Fatalf("upstream proxy was not tested: %s", rec.Body.String())
	}
	if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[len(got.Warnings)-1], "upstream proxy") {
		t.Fatalf("proxy success warning missing: %+v", got.Warnings)
	}
}

func TestUpdateRefreshablePreservesRedactedUpstreamProxy(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt(testJWT())
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.1.good")
	if err != nil {
		t.Fatal(err)
	}
	realProxy := "http://proxy-user:proxy-pass@proxy.example:8080"
	encProxy, err := services.EncryptUpstreamProxyURL(crypto, realProxy)
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info,upstream_proxy_url)
		VALUES ('key-redacted-proxy',?,'oauth token',?,?, 'https://provider.example/oauth/token', '{}', ?)`,
		openaiSvc.ID, encAccess, encRefresh, encProxy); err != nil {
		t.Fatal(err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	h := handlers.NewOAuthHandler(apiKeyQ, queries.NewPlaceholderQueries(db), svcQ, crypto)
	redactedProxy := services.RedactProxyURL(realProxy)
	req := httptest.NewRequest(http.MethodPut, "/api/oauth/key-redacted-proxy", strings.NewReader(`{
		"name":"renamed oauth token",
		"token_endpoint":"https://provider.example/oauth/token",
		"upstream_proxy_url":`+strconvQuote(redactedProxy)+`,
		"subscription_info":"{}"
	}`))
	req.SetPathValue("id", "key-redacted-proxy")
	rec := httptest.NewRecorder()

	h.Update(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	key, err := apiKeyQ.GetByID("key-redacted-proxy")
	if err != nil {
		t.Fatal(err)
	}
	storedProxy, err := services.DecryptUpstreamProxyURL(crypto, key.UpstreamProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if storedProxy != realProxy {
		t.Fatalf("stored proxy = %q, want original proxy", storedProxy)
	}
}

func TestUpdateRefreshableCanReactivateKey(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt(testJWT())
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.1.good")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	subInfo := `{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex","id_token":"` + testJWT() + `"}`
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info,is_active)
		VALUES ('key-reactivate',?,'codex oauth',?,?, 'https://auth.openai.com/oauth/token', ?, 0)`,
		openaiSvc.ID, encAccess, encRefresh, subInfo); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		crypto,
	)
	req := httptest.NewRequest(http.MethodPut, "/api/oauth/key-reactivate", strings.NewReader(`{
		"name":"codex oauth",
		"token_endpoint":"https://auth.openai.com/oauth/token",
		"subscription_info":`+strconvQuote(subInfo)+`,
		"is_active":true
	}`))
	req.SetPathValue("id", "key-reactivate")
	rec := httptest.NewRecorder()

	h.Update(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	key, err := queries.NewAPIKeyQueries(db).GetByID("key-reactivate")
	if err != nil {
		t.Fatal(err)
	}
	if !key.IsActive {
		t.Fatalf("key was not reactivated: %+v", key)
	}
}

func TestRefreshReturnsActiveStatusAfterReactivatingKey(t *testing.T) {
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q}`, testJWT())
	}))
	t.Cleanup(tokenEndpoint.Close)

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt("old-access-token")
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,is_active)
		VALUES ('key-refresh-active',?,'codex oauth',?,?,?,0)`,
		openaiSvc.ID, encAccess, encRefresh, tokenEndpoint.URL); err != nil {
		t.Fatal(err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	h := handlers.NewOAuthHandler(
		apiKeyQ,
		queries.NewPlaceholderQueries(db),
		svcQ,
		crypto,
	)
	h.SetRefresher(services.NewTokenRefresher(apiKeyQ, crypto))

	req := httptest.NewRequest(http.MethodPost, "/api/oauth/key-refresh-active/refresh", nil)
	req.SetPathValue("id", "key-refresh-active")
	rec := httptest.NewRecorder()

	h.Refresh(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Status   string `json:"status"`
		IsActive bool   `json:"is_active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "refreshed" || !got.IsActive {
		t.Fatalf("refresh response did not expose active status: %+v body=%s", got, rec.Body.String())
	}
	key, err := apiKeyQ.GetByID("key-refresh-active")
	if err != nil {
		t.Fatal(err)
	}
	if !key.IsActive {
		t.Fatalf("key should be active after refresh: %+v", key)
	}
}

func TestUpdateRefreshableWithReplacementTokensIgnoresStaleInactiveCheckbox(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt("old-access-token")
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("old-refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info,is_active)
		VALUES ('key-replace-reactivate',?,'oauth token',?,?, 'https://provider.example/oauth/token', '{}', 0)`,
		openaiSvc.ID, encAccess, encRefresh); err != nil {
		t.Fatal(err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	h := handlers.NewOAuthHandler(
		apiKeyQ,
		queries.NewPlaceholderQueries(db),
		svcQ,
		crypto,
	)
	req := httptest.NewRequest(http.MethodPut, "/api/oauth/key-replace-reactivate", strings.NewReader(`{
		"name":"oauth token",
		"access_token":"new-access-token",
		"refresh_token":"new-refresh-token",
		"token_endpoint":"https://provider.example/oauth/token",
		"subscription_info":"{}",
		"is_active":false
	}`))
	req.SetPathValue("id", "key-replace-reactivate")
	rec := httptest.NewRecorder()

	h.Update(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	key, err := apiKeyQ.GetByID("key-replace-reactivate")
	if err != nil {
		t.Fatal(err)
	}
	if !key.IsActive {
		t.Fatalf("replacement tokens should repair inactive key despite stale checkbox: %+v", key)
	}
}

func TestUpdateRefreshableWithReplacementRefreshTokenUsesExistingAccessToken(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt(testJWT())
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.1.old")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	subInfo := `{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex","id_token":"` + testJWT() + `"}`
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info,is_active)
		VALUES ('key-refresh-replace',?,'codex oauth',?,?, 'https://auth.openai.com/oauth/token', ?, 0)`,
		openaiSvc.ID, encAccess, encRefresh, subInfo); err != nil {
		t.Fatal(err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	h := handlers.NewOAuthHandler(
		apiKeyQ,
		queries.NewPlaceholderQueries(db),
		svcQ,
		crypto,
	)
	req := httptest.NewRequest(http.MethodPut, "/api/oauth/key-refresh-replace", strings.NewReader(`{
		"name":"codex oauth",
		"refresh_token":"rt.1.new",
		"token_endpoint":"https://auth.openai.com/oauth/token",
		"subscription_info":`+strconvQuote(subInfo)+`,
		"is_active":false
	}`))
	req.SetPathValue("id", "key-refresh-replace")
	rec := httptest.NewRecorder()

	h.Update(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	key, err := apiKeyQ.GetByID("key-refresh-replace")
	if err != nil {
		t.Fatal(err)
	}
	refreshToken, err := crypto.Decrypt(key.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if refreshToken != "rt.1.new" {
		t.Fatalf("refresh token was not replaced, got %q", refreshToken)
	}
	accessToken, err := crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		t.Fatal(err)
	}
	if accessToken != testJWT() {
		t.Fatalf("access token should be preserved, got %q", accessToken)
	}
	if !key.IsActive {
		t.Fatalf("replacement refresh token should repair inactive key: %+v", key)
	}
}

func TestUpdateRefreshableWithReplacementRefreshTokenReactivatesWhenActiveOmitted(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt(testJWT())
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.1.old")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	subInfo := `{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex","id_token":"` + testJWT() + `"}`
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info,is_active)
		VALUES ('key-refresh-replace-no-active',?,'codex oauth',?,?, 'https://auth.openai.com/oauth/token', ?, 0)`,
		openaiSvc.ID, encAccess, encRefresh, subInfo); err != nil {
		t.Fatal(err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	h := handlers.NewOAuthHandler(apiKeyQ, queries.NewPlaceholderQueries(db), svcQ, crypto)
	req := httptest.NewRequest(http.MethodPut, "/api/oauth/key-refresh-replace-no-active", strings.NewReader(`{
		"name":"codex oauth",
		"refresh_token":"rt.1.new",
		"token_endpoint":"https://auth.openai.com/oauth/token",
		"subscription_info":`+strconvQuote(subInfo)+`
	}`))
	req.SetPathValue("id", "key-refresh-replace-no-active")
	rec := httptest.NewRecorder()

	h.Update(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	key, err := apiKeyQ.GetByID("key-refresh-replace-no-active")
	if err != nil {
		t.Fatal(err)
	}
	if !key.IsActive {
		t.Fatalf("replacement refresh token should reactivate key even without is_active")
	}
}

func TestValidateCodexOAuthRejectsBadShape(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		services.NewCrypto([]byte("0123456789abcdef0123456789abcdef")),
	)

	body := `{
		"service_id": "` + openaiSvc.ID + `",
		"access_token": "not-a-jwt",
		"refresh_token": "not-rt",
		"token_endpoint": "https://auth.openai.com/oauth/token",
		"subscription_info": "{\"credential_kind\":\"codex_oauth\",\"auth_mode\":\"chatgpt\",\"source\":\"codex\"}"
	}`
	rec := httptest.NewRecorder()
	h.Validate(rec, httptest.NewRequest(http.MethodPost, "/api/oauth/validate", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "must look like a JWT") {
		t.Fatalf("unexpected error: %s", rec.Body.String())
	}
}

func TestValidateCodexOAuthRejectsSpoofedTokenEndpoint(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		services.NewCrypto([]byte("0123456789abcdef0123456789abcdef")),
	)
	subInfo := `{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex","id_token":"` + testJWT() + `"}`
	for _, endpoint := range []string{
		"https://auth.openai.com.evil.test/oauth/token",
		"https://auth.openai.com/not-token",
		"http://auth.openai.com/oauth/token",
		"https://user@auth.openai.com/oauth/token",
	} {
		body := `{
			"service_id": "` + openaiSvc.ID + `",
			"access_token": "` + testJWT() + `",
			"refresh_token": "rt.1.good",
			"token_endpoint": "` + endpoint + `",
			"subscription_info": ` + strconvQuote(subInfo) + `
		}`
		rec := httptest.NewRecorder()
		h.Validate(rec, httptest.NewRequest(http.MethodPost, "/api/oauth/validate", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("endpoint %q status = %d, want 400; body=%s", endpoint, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "token_endpoint must be https://auth.openai.com/oauth/token") {
			t.Fatalf("endpoint %q error did not mention strict endpoint: %s", endpoint, rec.Body.String())
		}
	}
}

func TestValidateCodexOAuthRequiresIDToken(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		services.NewCrypto([]byte("0123456789abcdef0123456789abcdef")),
	)

	body := `{
		"service_id": "` + openaiSvc.ID + `",
		"access_token": "` + testJWT() + `",
		"refresh_token": "rt.1.good",
		"token_endpoint": "https://auth.openai.com/oauth/token",
		"subscription_info": "{\"credential_kind\":\"codex_oauth\",\"auth_mode\":\"chatgpt\",\"source\":\"codex\"}"
	}`
	rec := httptest.NewRecorder()
	h.Validate(rec, httptest.NewRequest(http.MethodPost, "/api/oauth/validate", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "id_token required") {
		t.Fatalf("unexpected error: %s", rec.Body.String())
	}
}

func TestValidateCodexOAuthRejectsDuckwayPhantomTokens(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	h := handlers.NewOAuthHandler(
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		svcQ,
		services.NewCrypto([]byte("0123456789abcdef0123456789abcdef")),
	)

	for _, tc := range []struct {
		name         string
		accessToken  string
		refreshToken string
		idToken      string
		want         string
	}{
		{
			name:         "phantom refresh token",
			accessToken:  testJWT(),
			refreshToken: "rt.duckway.sk-proj-dw_fake",
			idToken:      testJWT(),
			want:         "refresh_token is a Duckway phantom token",
		},
		{
			name:         "phantom access token",
			accessToken:  codexPhantomJWTForTest("access"),
			refreshToken: "rt.1.good",
			idToken:      testJWT(),
			want:         "access_token is a Duckway phantom token",
		},
		{
			name:         "phantom id token",
			accessToken:  testJWT(),
			refreshToken: "rt.1.good",
			idToken:      codexPhantomJWTForTest("id"),
			want:         "id_token is a Duckway phantom token",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subInfo := `{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex","id_token":"` + tc.idToken + `"}`
			body := `{
				"service_id": "` + openaiSvc.ID + `",
				"access_token": "` + tc.accessToken + `",
				"refresh_token": "` + tc.refreshToken + `",
				"token_endpoint": "https://auth.openai.com/oauth/token",
				"subscription_info": ` + strconvQuote(subInfo) + `
			}`
			rec := httptest.NewRecorder()
			h.Validate(rec, httptest.NewRequest(http.MethodPost, "/api/oauth/validate", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("error missing %q: %s", tc.want, rec.Body.String())
			}
		})
	}
}

func testJWT() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1893456000}`))
	return header + "." + payload + ".sig"
}

func codexPhantomJWTForTest(kind string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"duckway-phantom","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"auth0|duckway-phantom","jti":"dw-phantom-` + kind + `","https://api.openai.com/auth":{"chatgpt_account_id":"duckway-account"}}`))
	return header + "." + payload + ".sig"
}

func looksLikeJWTForTest(token string) bool {
	return len(strings.Split(token, ".")) == 3
}
