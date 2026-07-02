package handlers_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

func testJWT() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1893456000}`))
	return header + "." + payload + ".sig"
}

func looksLikeJWTForTest(token string) bool {
	return len(strings.Split(token, ".")) == 3
}
