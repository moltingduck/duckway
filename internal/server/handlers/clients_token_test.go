package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestRotateClientTokenRevokesOldTokenAndPreservesClient(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	clients := queries.NewClientQueries(db)
	oldToken := "old-client-token"
	if _, err := db.Exec(`INSERT INTO clients (id, short_id, name, token_hash, is_active)
		VALUES ('client-rotate', 'rotate', 'rotate me', ?, 1)`, services.HashToken(oldToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern)
		VALUES ('svc-rotate', 'rotate', 'Rotate', 'https://rotate.example', 'rotate.example')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted)
		VALUES ('key-rotate', 'svc-rotate', 'key', 'encrypted')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id)
		VALUES ('ph-rotate', 'ROTATE_KEY', 'dw-phantom', 'svc-rotate', 'key-rotate', 'client-rotate')`); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewClientHandler(clients, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/clients/client-rotate/rotate-token", nil)
	req.SetPathValue("id", "client-rotate")
	rec := httptest.NewRecorder()
	h.RotateToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != "client-rotate" || body.Name != "rotate me" || body.Token == "" || body.Token == oldToken {
		t.Fatalf("unexpected response: %+v", body)
	}
	if _, err := clients.GetByTokenHash(services.HashToken(oldToken)); err == nil {
		t.Fatal("old client token still authenticates")
	}
	rotated, err := clients.GetByTokenHash(services.HashToken(body.Token))
	if err != nil || rotated.ID != "client-rotate" {
		t.Fatalf("new token did not preserve client identity: client=%+v err=%v", rotated, err)
	}
	var placeholders int
	if err := db.QueryRow(`SELECT COUNT(*) FROM placeholder_keys WHERE client_id='client-rotate'`).Scan(&placeholders); err != nil || placeholders != 1 {
		t.Fatalf("assignments changed during rotation: count=%d err=%v", placeholders, err)
	}
}

func TestRotateClientTokenNotFound(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := handlers.NewClientHandler(queries.NewClientQueries(db), nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/clients/missing/rotate-token", nil)
	req.SetPathValue("id", "missing")
	rec := httptest.NewRecorder()
	h.RotateToken(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
