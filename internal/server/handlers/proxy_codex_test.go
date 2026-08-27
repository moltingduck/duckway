package handlers

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestRewriteCodexRefreshRequestForm(t *testing.T) {
	body := []byte("grant_type=refresh_token&refresh_token=rt.duckway.fake&client_id=wrong")
	got, contentType := rewriteCodexRefreshRequest(body, "application/x-www-form-urlencoded", "rt.real.secret", "app_codex_test")
	if contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("contentType = %q", contentType)
	}
	vals, err := url.ParseQuery(string(got))
	if err != nil {
		t.Fatal(err)
	}
	if vals.Get("refresh_token") != "rt.real.secret" {
		t.Fatalf("refresh_token = %q", vals.Get("refresh_token"))
	}
	if vals.Get("client_id") != "app_codex_test" {
		t.Fatalf("client_id = %q", vals.Get("client_id"))
	}
}

func TestRewriteCodexRefreshRequestFormWithoutContentType(t *testing.T) {
	body := []byte("grant_type=refresh_token&refresh_token=rt.duckway.fake&client_id=codex")
	got, contentType := rewriteCodexRefreshRequest(body, "", "rt.real.secret", "codex")
	if contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("contentType = %q", contentType)
	}
	vals, err := url.ParseQuery(string(got))
	if err != nil {
		t.Fatal(err)
	}
	if vals.Get("refresh_token") != "rt.real.secret" {
		t.Fatalf("refresh_token = %q", vals.Get("refresh_token"))
	}
}

func TestRewriteCodexRefreshRequestJSON(t *testing.T) {
	body := []byte(`{"grant_type":"refresh_token","refresh_token":"rt.duckway.fake","client_id":"wrong"}`)
	got, contentType := rewriteCodexRefreshRequest(body, "application/json", "rt.real.secret", "app_codex_test")
	if contentType != "application/json" {
		t.Fatalf("contentType = %q", contentType)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["refresh_token"] != "rt.real.secret" {
		t.Fatalf("refresh_token = %#v", obj["refresh_token"])
	}
	if obj["client_id"] != "app_codex_test" {
		t.Fatalf("client_id = %#v", obj["client_id"])
	}
}

func TestRewriteCodexRefreshRequestJSONAddsClientID(t *testing.T) {
	body := []byte(`{"grant_type":"refresh_token","refresh_token":"rt.duckway.fake"}`)
	got, contentType := rewriteCodexRefreshRequest(body, "application/json", "rt.real.secret", "app_codex_test")
	if contentType != "application/json" {
		t.Fatalf("contentType = %q", contentType)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["refresh_token"] != "rt.real.secret" {
		t.Fatalf("refresh_token = %#v", obj["refresh_token"])
	}
	if obj["client_id"] != "app_codex_test" {
		t.Fatalf("client_id = %#v", obj["client_id"])
	}
}

func TestRewriteCodexRefreshResponseReturnsOnlyFakeTokens(t *testing.T) {
	realAccess := testCodexJWT(`{"exp":1893456000,"scope":"access","email":"real@example.com","sub":"auth0|real","https://api.openai.com/auth":{"chatgpt_account_id":"acct-real","chatgpt_user_id":"user-real","poid":"org-real"}}`)
	realID := testCodexJWT(`{"exp":1893456000,"kind":"id","email":"real@example.com","name":"Real User","sub":"auth0|real"}`)
	body := []byte(`{"access_token":"` + realAccess + `","refresh_token":"rt.real.secret","id_token":"` + realID + `","expires_in":3600}`)
	got := rewriteCodexRefreshResponse(body, "ph-openai", "sk-proj-dw_fake")
	var obj map[string]interface{}
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		tok, _ := obj[key].(string)
		if tok == "" {
			t.Fatalf("%s missing in rewritten response: %s", key, got)
		}
		if tok == realAccess || tok == realID || strings.Contains(tok, "rt.real") {
			t.Fatalf("%s leaked real token: %q", key, tok)
		}
	}
	if !strings.HasPrefix(obj["refresh_token"].(string), "rt.duckway.sk-proj-dw_fake") {
		t.Fatalf("unexpected fake refresh token: %q", obj["refresh_token"])
	}
	if obj["expires_in"].(float64) != 3600 {
		t.Fatalf("expires_in was not preserved: %#v", obj["expires_in"])
	}
	for label, tok := range map[string]string{"access": obj["access_token"].(string), "id": obj["id_token"].(string)} {
		claims := decodeJWTClaimsForTest(t, tok)
		encoded, _ := json.Marshal(claims)
		for _, forbidden := range []string{"real@example.com", "Real User", "auth0|real", "acct-real", "user-real", "org-real"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("%s token leaked %q in claims: %s", label, forbidden, encoded)
			}
		}
		if claims["iss"] != "https://auth.openai.com" {
			t.Fatalf("%s token iss = %#v", label, claims["iss"])
		}
	}
}

func TestHandleOpenAIAuthProxyExchangesRealRefreshTokenServerSide(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt("real-access-token")
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.real.refresh")
	if err != nil {
		t.Fatal(err)
	}

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-openai-auth','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info)
		VALUES ('key-openai-auth',?,'codex oauth',?,?, 'https://auth.openai.com/oauth/token', '{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex"}')`,
		openaiSvc.ID, encAccess, encRefresh); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES ('ph-openai-auth','OPENAI_API_KEY','sk-proj-dw_fake_auth',?,'key-openai-auth','client-openai-auth',0)`, openaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	var upstreamBody string
	var upstreamAuth string
	var upstreamDuckway string
	var upstreamAcceptEncoding string
	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://auth.openai.com/oauth/token" {
			t.Fatalf("unexpected upstream URL: %s", req.URL.String())
		}
		body, _ := io.ReadAll(req.Body)
		upstreamBody = string(body)
		upstreamAuth = req.Header.Get("Authorization")
		upstreamAcceptEncoding = req.Header.Get("Accept-Encoding")
		for key := range req.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-duckway-") {
				upstreamDuckway = key
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"` + testCodexJWT(`{"exp":1893456001,"scope":"access"}`) + `","refresh_token":"rt.real.new","id_token":"` + testCodexJWT(`{"exp":1893456001,"kind":"id"}`) + `","expires_in":3600}`)),
			Request:    req,
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/proxy/openai-auth/oauth/token", strings.NewReader("grant_type=refresh_token&refresh_token=rt.duckway.sk-proj-dw_fake_auth"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic codex-client-auth")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Duckway-Internal", "secret")
	client := &models.Client{ID: "client-openai-auth", Name: "client"}
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, client))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	vals, err := url.ParseQuery(upstreamBody)
	if err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	if vals.Get("refresh_token") != "rt.real.refresh" {
		t.Fatalf("upstream refresh_token = %q, body=%s", vals.Get("refresh_token"), upstreamBody)
	}
	if vals.Get("client_id") != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Fatalf("upstream client_id = %q, body=%s", vals.Get("client_id"), upstreamBody)
	}
	if upstreamAuth != "Basic codex-client-auth" {
		t.Fatalf("Authorization header was not preserved: %q", upstreamAuth)
	}
	if upstreamAcceptEncoding != "" {
		t.Fatalf("Accept-Encoding header should not be forwarded: %q", upstreamAcceptEncoding)
	}
	if upstreamDuckway != "" {
		t.Fatalf("Duckway internal header leaked upstream: %s", upstreamDuckway)
	}
	if strings.Contains(rec.Body.String(), "rt.real") {
		t.Fatalf("response leaked real token: %s", rec.Body.String())
	}
	key, err := queries.NewAPIKeyQueries(db).GetByID("key-openai-auth")
	if err != nil {
		t.Fatal(err)
	}
	storedAccess, err := crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		t.Fatal(err)
	}
	if storedAccess != testCodexJWT(`{"exp":1893456001,"scope":"access"}`) {
		t.Fatalf("stored access token was not updated: %q", storedAccess)
	}
	storedRefresh, err := crypto.Decrypt(key.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if storedRefresh != "rt.real.new" {
		t.Fatalf("stored refresh token was not rotated: %q", storedRefresh)
	}
	var subInfo map[string]interface{}
	if err := json.Unmarshal([]byte(key.SubscriptionInfo), &subInfo); err != nil {
		t.Fatal(err)
	}
	if subInfo["id_token"] != testCodexJWT(`{"exp":1893456001,"kind":"id"}`) {
		t.Fatalf("stored id_token was not updated: %#v", subInfo)
	}
	if subInfo["last_refresh"] == "" {
		t.Fatalf("last_refresh was not stored: %#v", subInfo)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(obj["refresh_token"].(string), "rt.duckway.sk-proj-dw_fake_auth") {
		t.Fatalf("unexpected fake refresh token: %#v", obj)
	}
}

func TestHandleOpenAIAuthProxyRedactsSecretsFromUpstreamError(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt("real-access-token")
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.real.refresh")
	if err != nil {
		t.Fatal(err)
	}

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-openai-auth-error','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info)
		VALUES ('key-openai-auth-error',?,'codex oauth',?,?, 'https://auth.openai.com/oauth/token', '{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex"}')`,
		openaiSvc.ID, encAccess, encRefresh); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES ('ph-openai-auth-error','OPENAI_API_KEY','sk-proj-dw_fake_auth_error',?,'key-openai-auth-error','client-openai-auth-error',0)`, openaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad rt.real.refresh ` + testCodexJWT(`{"exp":1893456001,"kind":"id"}`) + `"}}`)),
			Request:    req,
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/proxy/openai-auth/oauth/token", strings.NewReader("grant_type=refresh_token&refresh_token=rt.duckway.sk-proj-dw_fake_auth_error"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: "client-openai-auth-error", Name: "client"}))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "rt.real.refresh") || strings.Contains(rec.Body.String(), "eyJ") {
		t.Fatalf("response leaked upstream secret: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[REDACTED") {
		t.Fatalf("response did not include redaction marker: %s", rec.Body.String())
	}
}

func TestHandleOpenAIAuthProxyRetriesAfterConcurrentRefreshRotation(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt("real-access-token")
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.real.old")
	if err != nil {
		t.Fatal(err)
	}

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-openai-auth-race','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info)
		VALUES ('key-openai-auth-race',?,'codex oauth',?,?, 'https://auth.openai.com/oauth/token', '{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex"}')`,
		openaiSvc.ID, encAccess, encRefresh); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES ('ph-openai-auth-race','OPENAI_API_KEY','sk-proj-dw_fake_auth_race',?,'key-openai-auth-race','client-openai-auth-race',0)`, openaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	var calls int
	h := NewProxyHandler(
		svcQ,
		apiKeyQ,
		services.NewKeyResolver(crypto, apiKeyQ, queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(req.Body)
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		switch calls {
		case 1:
			if vals.Get("refresh_token") != "rt.real.old" {
				t.Fatalf("first refresh_token = %q", vals.Get("refresh_token"))
			}
			rotatedRefresh, err := crypto.Encrypt("rt.real.new")
			if err != nil {
				t.Fatal(err)
			}
			if err := apiKeyQ.UpdateRefreshToken("key-openai-auth-race", rotatedRefresh); err != nil {
				t.Fatal(err)
			}
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"refresh_token_invalidated","message":"invalidated"}}`)),
				Request:    req,
			}, nil
		case 2:
			if vals.Get("refresh_token") != "rt.real.new" {
				t.Fatalf("second refresh_token = %q", vals.Get("refresh_token"))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"access_token":"` + testCodexJWT(`{"exp":1893456001,"scope":"access"}`) + `","refresh_token":"rt.real.newer","id_token":"` + testCodexJWT(`{"exp":1893456001,"kind":"id"}`) + `","expires_in":3600}`)),
				Request:    req,
			}, nil
		default:
			t.Fatalf("unexpected upstream call %d", calls)
		}
		return nil, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/proxy/openai-auth/oauth/token", strings.NewReader("grant_type=refresh_token&refresh_token=rt.duckway.sk-proj-dw_fake_auth_race"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: "client-openai-auth-race", Name: "client"}))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
	key, err := apiKeyQ.GetByID("key-openai-auth-race")
	if err != nil {
		t.Fatal(err)
	}
	storedRefresh, err := crypto.Decrypt(key.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if storedRefresh != "rt.real.newer" {
		t.Fatalf("stored refresh = %q", storedRefresh)
	}
}

func TestHandleOpenAIAuthProxyResolvesSubmittedPhantomRefreshToken(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encA, err := crypto.Encrypt("real-access-a")
	if err != nil {
		t.Fatal(err)
	}
	refreshA, err := crypto.Encrypt("rt.real.a")
	if err != nil {
		t.Fatal(err)
	}
	encB, err := crypto.Encrypt("real-access-b")
	if err != nil {
		t.Fatal(err)
	}
	refreshB, err := crypto.Encrypt("rt.real.b")
	if err != nil {
		t.Fatal(err)
	}

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-openai-auth-multi','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info)
		VALUES
		('key-openai-auth-a',?,'codex oauth a',?,?, 'https://auth.openai.com/oauth/token', '{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex"}'),
		('key-openai-auth-b',?,'codex oauth b',?,?, 'https://auth.openai.com/oauth/token', '{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex"}')`,
		openaiSvc.ID, encA, refreshA, openaiSvc.ID, encB, refreshB); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES
		('ph-openai-auth-a','OPENAI_API_KEY_A','sk-proj-dw_fake_auth_a',?,'key-openai-auth-a','client-openai-auth-multi',0),
		('ph-openai-auth-b','OPENAI_API_KEY_B','sk-proj-dw_fake_auth_b',?,'key-openai-auth-b','client-openai-auth-multi',0)`,
		openaiSvc.ID, openaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	var upstreamBody string
	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		upstreamBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"` + testCodexJWT(`{"exp":1893456001,"scope":"access-b"}`) + `","refresh_token":"rt.real.b.new","id_token":"` + testCodexJWT(`{"exp":1893456001,"kind":"id"}`) + `","expires_in":3600}`)),
			Request:    req,
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/proxy/openai-auth/oauth/token", strings.NewReader("grant_type=refresh_token&refresh_token=rt.duckway.sk-proj-dw_fake_auth_b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: "client-openai-auth-multi", Name: "client"}))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	vals, err := url.ParseQuery(upstreamBody)
	if err != nil {
		t.Fatal(err)
	}
	if vals.Get("refresh_token") != "rt.real.b" {
		t.Fatalf("upstream refresh_token = %q, body=%s", vals.Get("refresh_token"), upstreamBody)
	}
	keyA, err := queries.NewAPIKeyQueries(db).GetByID("key-openai-auth-a")
	if err != nil {
		t.Fatal(err)
	}
	storedRefreshA, err := crypto.Decrypt(keyA.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if storedRefreshA != "rt.real.a" {
		t.Fatalf("key A refresh token changed: %q", storedRefreshA)
	}
	keyB, err := queries.NewAPIKeyQueries(db).GetByID("key-openai-auth-b")
	if err != nil {
		t.Fatal(err)
	}
	storedRefreshB, err := crypto.Decrypt(keyB.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if storedRefreshB != "rt.real.b.new" {
		t.Fatalf("key B refresh token = %q", storedRefreshB)
	}
}

func TestHandleOpenAIChatGPTProxyUsesOpenAIKeyForNativeCodex(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	realAccess := testCodexJWT(`{"exp":1893456001,"scope":"access"}`)
	encAccess, err := crypto.Encrypt(realAccess)
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.real.refresh")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-chatgpt','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info)
		VALUES ('key-chatgpt',?,'codex oauth',?,?, 'https://auth.openai.com/oauth/token', '{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex"}')`,
		openaiSvc.ID, encAccess, encRefresh); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES ('ph-chatgpt','OPENAI_API_KEY','sk-proj-dw_fake_chatgpt',?,'key-chatgpt','client-chatgpt',0)`, openaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	var upstreamURL string
	var upstreamAuth string
	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamURL = req.URL.String()
		upstreamAuth = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    req,
		}, nil
	})}

	for _, auth := range []string{
		"Bearer sk-proj-dw_fake_chatgpt",
		"Bearer real-chatgpt-access-token",
	} {
		upstreamURL = ""
		upstreamAuth = ""
		req := httptest.NewRequest(http.MethodGet, "/proxy/openai-chatgpt/backend-api/codex/models?client_version=test", nil)
		req.Header.Set("Authorization", auth)
		client := &models.Client{ID: "client-chatgpt", Name: "client"}
		req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, client))
		rec := httptest.NewRecorder()

		h.Handle(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("auth %q status = %d body=%s", auth, rec.Code, rec.Body.String())
		}
		if upstreamURL != "https://chatgpt.com/backend-api/codex/models?client_version=test" {
			t.Fatalf("auth %q upstream URL = %q", auth, upstreamURL)
		}
		if upstreamAuth != "Bearer "+realAccess {
			t.Fatalf("auth %q upstream Authorization = %q", auth, upstreamAuth)
		}
	}

	var wsAuth, wsConnection, wsUpgrade string
	wsCalls := 0
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		wsCalls++
		wsAuth = req.Header.Get("Authorization")
		wsConnection = req.Header.Get("Connection")
		wsUpgrade = req.Header.Get("Upgrade")
		proxyEnd, upstreamEnd := net.Pipe()
		go func() {
			defer upstreamEnd.Close()
			payload := make([]byte, 4)
			if _, err := io.ReadFull(upstreamEnd, payload); err == nil && string(payload) == "ping" {
				_, _ = upstreamEnd.Write([]byte("pong"))
			}
		}()
		return &http.Response{
			StatusCode: http.StatusSwitchingProtocols,
			Status:     "101 Switching Protocols",
			Proto:      "HTTP/1.1",
			Header: http.Header{
				"Connection":           []string{"Upgrade"},
				"Upgrade":              []string{"websocket"},
				"Sec-Websocket-Accept": []string{"mock"},
			},
			Body: proxyEnd, Request: req,
		}, nil
	})}
	for _, tc := range []struct {
		name, method, path, auth, key string
		wantStatus                    int
	}{
		{"wrong method", http.MethodPost, "/proxy/openai-chatgpt/backend-api/codex/responses", "Bearer sk-proj-dw_fake_chatgpt", "dGVzdC1rZXktMTIzNDU2Nw==", http.StatusBadRequest},
		{"wrong path", http.MethodGet, "/proxy/openai-chatgpt/backend-api/codex/other", "Bearer sk-proj-dw_fake_chatgpt", "dGVzdC1rZXktMTIzNDU2Nw==", http.StatusBadRequest},
		{"bad key", http.MethodGet, "/proxy/openai-chatgpt/backend-api/codex/responses", "Bearer sk-proj-dw_fake_chatgpt", "bad", http.StatusBadRequest},
		{"real token", http.MethodGet, "/proxy/openai-chatgpt/backend-api/codex/responses", "Bearer real-chatgpt-access-token", "dGVzdC1rZXktMTIzNDU2Nw==", http.StatusForbidden},
	} {
		t.Run("wss rejects "+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", tc.auth)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", tc.key)
			client := &models.Client{ID: "client-chatgpt", Name: "client"}
			req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, client))
			rec := httptest.NewRecorder()
			h.Handle(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
	if wsCalls != 0 {
		t.Fatalf("rejected websocket requests reached upstream %d times", wsCalls)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client := &models.Client{ID: "client-chatgpt", Name: "client"}
		h.Handle(w, r.WithContext(context.WithValue(r.Context(), middleware.ClientKey, client)))
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	wsHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"duckway-phantom"}`))
	wsPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://duckway.dev/placeholder":"sk-proj-dw_fake_chatgpt"}`))
	wsPhantom := wsHeader + "." + wsPayload + ".signature"
	_, _ = fmt.Fprintf(conn, "GET /proxy/openai-chatgpt/backend-api/codex/responses HTTP/1.1\r\nHost: duckway\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGVzdC1rZXktMTIzNDU2Nw==\r\n\r\nping", wsPhantom)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("websocket status=%d, want 101; body=%s", resp.StatusCode, body)
	}
	pong := make([]byte, 4)
	if _, err := io.ReadFull(reader, pong); err != nil {
		t.Fatal(err)
	}
	if string(pong) != "pong" {
		t.Fatalf("websocket payload=%q, want pong", pong)
	}
	if wsAuth != "Bearer "+realAccess || !strings.EqualFold(wsConnection, "Upgrade") || !strings.EqualFold(wsUpgrade, "websocket") {
		t.Fatalf("upstream websocket headers auth=%q connection=%q upgrade=%q", wsAuth, wsConnection, wsUpgrade)
	}
}

func TestHandleXAIGrokProxyUsesXAIKey(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	realKey := "xai-real-secret"
	encAccess, err := crypto.Encrypt(realKey)
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	xaiSvc, err := svcQ.GetByName("xai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-xai-grok','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-xai-grok',?,'xai key',?)`, xaiSvc.ID, encAccess); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES ('ph-xai-grok','XAI_API_KEY','xai-dw_fake_grok',?,'key-xai-grok','client-xai-grok',0)`, xaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	var upstreamURL string
	var upstreamAuth string
	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamURL = req.URL.String()
		upstreamAuth = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    req,
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/proxy/xai-grok/v1/chat/completions?stream=true", strings.NewReader(`{"model":"grok-4.5"}`))
	req.Header.Set("Authorization", "Bearer xai-dw_fake_grok")
	req.Header.Set("Content-Type", "application/json")
	client := &models.Client{ID: "client-xai-grok", Name: "client"}
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, client))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamURL != "https://cli-chat-proxy.grok.com/v1/chat/completions?stream=true" {
		t.Fatalf("upstream URL = %q", upstreamURL)
	}
	if upstreamAuth != "Bearer "+realKey {
		t.Fatalf("upstream Authorization = %q", upstreamAuth)
	}
}

func TestHandleXAIGrokProxyRequiresConfiguredHostPattern(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`UPDATE services SET host_pattern = 'api.x.ai' WHERE name = 'xai'`); err != nil {
		t.Fatal(err)
	}

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt("xai-real-secret")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	xaiSvc, err := svcQ.GetByName("xai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-xai-deny','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-xai-deny',?,'xai key',?)`, xaiSvc.ID, encAccess); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES ('ph-xai-deny','XAI_API_KEY','xai-dw_fake_deny',?,'key-xai-deny','client-xai-deny',0)`, xaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	called := false
	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/proxy/xai-grok/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer xai-dw_fake_deny")
	client := &models.Client{ID: "client-xai-deny", Name: "client"}
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, client))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called when Grok host is not configured")
	}
}

func TestHandleXAIGrokProxyRequiresExplicitPhantom(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt("xai-real-secret")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	xaiSvc, err := svcQ.GetByName("xai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-xai-noauth','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-xai-noauth',?,'xai key',?)`, xaiSvc.ID, encAccess); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES ('ph-xai-noauth','XAI_API_KEY','xai-dw_fake_noauth',?,'key-xai-noauth','client-xai-noauth',0)`, xaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	called := false
	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/proxy/xai-grok/v1/chat/completions", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: "client-xai-noauth", Name: "client"}))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called without an explicit Duckway phantom")
	}
}

func TestHandleXAIApiProxyRequiresConfiguredHostPattern(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`UPDATE services SET host_pattern = 'cli-chat-proxy.grok.com' WHERE name = 'xai'`); err != nil {
		t.Fatal(err)
	}

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt("xai-real-secret")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	xaiSvc, err := svcQ.GetByName("xai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-xai-api','client',?)`, services.HashToken("tok")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-xai-api',?,'xai key',?)`, xaiSvc.ID, encAccess); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES ('ph-xai-api','XAI_API_KEY','xai-dw_fake_api',?,'key-xai-api','client-xai-api',0)`, xaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	called := false
	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/proxy/xai-api/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer xai-dw_fake_api")
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: "client-xai-api", Name: "client"}))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called when xAI API host is not configured")
	}
}

func TestHandleXAIGrokProxyRejectsWrongClientOrServicePlaceholder(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encXAI, err := crypto.Encrypt("xai-real-secret")
	if err != nil {
		t.Fatal(err)
	}
	encOpenAI, err := crypto.Encrypt("sk-real-secret")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	xaiSvc, err := svcQ.GetByName("xai")
	if err != nil {
		t.Fatal(err)
	}
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"client-xai-a", "client-xai-b"} {
		if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES (?,?,?)`, id, id, services.HashToken("tok-"+id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-xai-a',?,'xai key',?), ('key-openai-a',?,'openai key',?)`, xaiSvc.ID, encXAI, openaiSvc.ID, encOpenAI); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id,requires_approval)
		VALUES
		('ph-xai-a','XAI_API_KEY','xai-dw_fake_a',?,'key-xai-a','client-xai-a',0),
		('ph-openai-a','OPENAI_API_KEY','sk-dw_fake_openai',?,'key-openai-a','client-xai-a',0)`,
		xaiSvc.ID, openaiSvc.ID); err != nil {
		t.Fatal(err)
	}

	called := false
	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	).WithCrypto(crypto)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}

	tests := []struct {
		name     string
		clientID string
		auth     string
	}{
		{name: "wrong client", clientID: "client-xai-b", auth: "Bearer xai-dw_fake_a"},
		{name: "wrong service", clientID: "client-xai-a", auth: "Bearer sk-dw_fake_openai"},
		{name: "no client", clientID: "", auth: "Bearer xai-dw_fake_a"},
	}
	for _, tt := range tests {
		called = false
		req := httptest.NewRequest(http.MethodPost, "/proxy/xai-grok/v1/chat/completions", nil)
		req.Header.Set("Authorization", tt.auth)
		if tt.clientID != "" {
			req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, &models.Client{ID: tt.clientID, Name: tt.clientID}))
		}
		rec := httptest.NewRecorder()

		h.Handle(rec, req)
		if tt.clientID == "" {
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s status = %d body=%s", tt.name, rec.Code, rec.Body.String())
			}
		} else if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d body=%s", tt.name, rec.Code, rec.Body.String())
		}
		if called {
			t.Fatalf("%s called upstream", tt.name)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testCodexJWT(payload string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".real-signature"
}

func decodeJWTClaimsForTest(t *testing.T, token string) map[string]interface{} {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not JWT-shaped: %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}
