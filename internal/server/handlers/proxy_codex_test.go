package handlers

import (
	"context"
	"encoding/json"
	"io"
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
	body := []byte("grant_type=refresh_token&refresh_token=rt.duckway.fake&client_id=codex")
	got, contentType := rewriteCodexRefreshRequest(body, "application/x-www-form-urlencoded", "rt.real.secret")
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
	if vals.Get("client_id") != "codex" {
		t.Fatalf("client_id was not preserved: %q", vals.Get("client_id"))
	}
}

func TestRewriteCodexRefreshRequestJSON(t *testing.T) {
	body := []byte(`{"grant_type":"refresh_token","refresh_token":"rt.duckway.fake","client_id":"codex"}`)
	got, contentType := rewriteCodexRefreshRequest(body, "application/json", "rt.real.secret")
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
	if obj["client_id"] != "codex" {
		t.Fatalf("client_id was not preserved: %#v", obj["client_id"])
	}
}

func TestRewriteCodexRefreshResponseReturnsOnlyFakeTokens(t *testing.T) {
	body := []byte(`{"access_token":"real-access","refresh_token":"rt.real.secret","id_token":"real.id.token","expires_in":3600}`)
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
		if strings.Contains(tok, "real") {
			t.Fatalf("%s leaked real token: %q", key, tok)
		}
	}
	if !strings.HasPrefix(obj["refresh_token"].(string), "rt.duckway.sk-proj-dw_fake") {
		t.Fatalf("unexpected fake refresh token: %q", obj["refresh_token"])
	}
	if obj["expires_in"].(float64) != 3600 {
		t.Fatalf("expires_in was not preserved: %#v", obj["expires_in"])
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
	h := NewProxyHandler(
		svcQ,
		queries.NewAPIKeyQueries(db),
		services.NewKeyResolver(crypto, queries.NewAPIKeyQueries(db), queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db)),
		nil,
		queries.NewApprovalQueries(db),
		nil,
		nil,
	)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://auth.openai.com/oauth/token" {
			t.Fatalf("unexpected upstream URL: %s", req.URL.String())
		}
		body, _ := io.ReadAll(req.Body)
		upstreamBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"real-access-new","refresh_token":"rt.real.new","id_token":"real.id.token","expires_in":3600}`)),
			Request:    req,
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/proxy/openai-auth/oauth/token", strings.NewReader("grant_type=refresh_token&refresh_token=rt.duckway.fake"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
	if strings.Contains(rec.Body.String(), "real-access") || strings.Contains(rec.Body.String(), "rt.real") || strings.Contains(rec.Body.String(), "real.id") {
		t.Fatalf("response leaked real token: %s", rec.Body.String())
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(obj["refresh_token"].(string), "rt.duckway.sk-proj-dw_fake_auth") {
		t.Fatalf("unexpected fake refresh token: %#v", obj)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
