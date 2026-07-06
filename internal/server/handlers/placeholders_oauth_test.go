package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestCreatePlaceholderForOAuthJWTUsesJWTPhantom(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	realAccess := testJWT()
	encAccess, err := crypto.Encrypt(realAccess)
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.1.real-refresh")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-oauth-ph','oauth client','hash')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token,token_endpoint,subscription_info)
		VALUES ('key-oauth-ph',?,'codex oauth',?,?, 'https://auth.openai.com/oauth/token', '{"credential_kind":"codex_oauth"}')`,
		openaiSvc.ID, encAccess, encRefresh); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewPlaceholderHandler(
		queries.NewPlaceholderQueries(db),
		svcQ,
		queries.NewClientQueries(db),
	).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	req := httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(`{
		"env_name":"OPENAI_API_KEY",
		"service_id":"`+openaiSvc.ID+`",
		"api_key_id":"key-oauth-ph",
		"client_id":"client-oauth-ph",
		"requires_approval":false
	}`))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Placeholder string `json:"placeholder"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if parts := strings.Split(got.Placeholder, "."); len(parts) != 3 {
		t.Fatalf("OAuth access token should produce JWT phantom, got %q", got.Placeholder)
	}
	if !services.IsPlaceholder(got.Placeholder) {
		t.Fatalf("JWT phantom should be detected as placeholder: %q", got.Placeholder)
	}

	resolver := services.NewKeyResolver(
		crypto,
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		queries.NewGroupQueries(db),
		queries.NewApprovalQueries(db),
	)
	resolved, err := resolver.Resolve(got.Placeholder, "client-oauth-ph")
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Permitted || resolved.RealKey != realAccess || !resolved.IsRefreshable {
		t.Fatalf("resolve = %+v", resolved)
	}
}
