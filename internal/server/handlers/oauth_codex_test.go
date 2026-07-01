package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	subInfo := `{"credential_kind":"codex_oauth","auth_mode":"chatgpt","source":"codex","account_id":"acct-1","last_refresh":"2026-07-01T00:00:00Z"}`
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
	if got["auth_mode"] != "chatgpt" || tokens["access_token"] != "codex-access-token" || tokens["refresh_token"] != "codex-refresh-token" || tokens["account_id"] != "acct-1" {
		t.Fatalf("unexpected response: %#v", got)
	}
}
