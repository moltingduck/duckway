package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestClientServicesReturnsOnlyAuthenticatedClientAssignmentState(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	config := &Config{
		DataDir:       t.TempDir(),
		EncryptionKey: bytes.Repeat([]byte{1}, 32),
		SessionSecret: bytes.Repeat([]byte{2}, 32),
	}
	s := &Server{config: config, db: db, mux: http.NewServeMux(), stopCh: make(chan struct{})}
	if err := s.seedDefaultServices(); err != nil {
		t.Fatal(err)
	}
	shared := s.initShared()
	s.SetupGatewayRoutes(shared)

	clientTokens := map[string]string{
		"client-services-a": "client-services-token-a",
		"client-services-b": "client-services-token-b",
	}
	clientQ := queries.NewClientQueries(db)
	for id, token := range clientTokens {
		if err := clientQ.Create(&models.Client{
			ID: id, ShortID: id, Name: id, TokenHash: services.HashToken(token),
		}); err != nil {
			t.Fatal(err)
		}
	}
	openAI, err := queries.NewServiceQueries(db).GetByName("openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted)
		VALUES ('client-services-key', ?, 'client services key', 'encrypted')`, openAI.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys
		(id, env_name, placeholder, service_id, api_key_id, client_id)
		VALUES ('client-services-ph', 'OPENAI_API_KEY', 'sk-dw-client-services', ?, 'client-services-key', 'client-services-a')`, openAI.ID); err != nil {
		t.Fatal(err)
	}

	for clientID, token := range clientTokens {
		req := httptest.NewRequest(http.MethodGet, "/client/services", nil)
		req.Header.Set("X-Duckway-Token", token)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", clientID, rec.Code, rec.Body.String())
		}
		var rows []map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		foundOpenAI := false
		for _, row := range rows {
			if row["name"] != "openai" {
				continue
			}
			foundOpenAI = true
			wantAssigned := clientID == "client-services-a"
			if assigned, ok := row["assigned"].(bool); !ok || assigned != wantAssigned {
				t.Fatalf("%s OpenAI assigned=%#v, want %v", clientID, row["assigned"], wantAssigned)
			}
			for _, forbidden := range []string{"api_key_id", "placeholder", "group_id", "client_id"} {
				if _, exists := row[forbidden]; exists {
					t.Fatalf("%s response exposed %s: %#v", clientID, forbidden, row)
				}
			}
		}
		if !foundOpenAI {
			t.Fatalf("%s response missing OpenAI service", clientID)
		}
	}

	for clientID, token := range clientTokens {
		req := httptest.NewRequest(http.MethodGet, "/client/sync", nil)
		req.Header.Set("X-Duckway-Token", token)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s sync status=%d body=%s", clientID, rec.Code, rec.Body.String())
		}
		var snapshot struct {
			Revision string                   `json:"revision"`
			Keys     []map[string]interface{} `json:"keys"`
			Services []map[string]interface{} `json:"services"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Revision == "" {
			t.Fatalf("%s sync response has no revision", clientID)
		}
		wantKeys := 0
		if clientID == "client-services-a" {
			wantKeys = 1
		}
		if len(snapshot.Keys) != wantKeys {
			t.Fatalf("%s sync keys=%d, want %d", clientID, len(snapshot.Keys), wantKeys)
		}
		foundOpenAI := false
		for _, route := range snapshot.Services {
			if route["name"] == "openai" {
				foundOpenAI = true
				wantAssigned := clientID == "client-services-a"
				if route["assigned"] != wantAssigned {
					t.Fatalf("%s sync OpenAI assigned=%#v, want %v", clientID, route["assigned"], wantAssigned)
				}
			}
		}
		if !foundOpenAI {
			t.Fatalf("%s sync response missing OpenAI route", clientID)
		}
	}
}
