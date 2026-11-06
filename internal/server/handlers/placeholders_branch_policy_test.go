package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/services"
)

func TestPlaceholderStoresBranchPoliciesNormalised(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	enc, err := crypto.Encrypt("ghp_" + strings.Repeat("a", 36))
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	svc := &models.Service{ID: "svc-gh-norm", Name: "github", DisplayName: "GitHub", UpstreamURL: "https://api.github.com", HostPattern: "github.com",
		AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyPrefix: "ghp_", KeyLength: 40, DeliveryMode: "proxy", IsActive: true}
	if err := svcQ.Create(svc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-norm','c','hash-norm')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted) VALUES ('key-norm',?,'k',?)`, svc.ID, enc); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewPlaceholderHandler(queries.NewPlaceholderQueries(db), svcQ, queries.NewClientQueries(db)).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	cfg := `{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{"clone":true,"push":true},"branch_policy":{"rules":[{"branches":"refs/heads/agent/**","role":"agent"},{"branches":"**","role":"read"}],"tags":{"create":["refs/tags/v*"]}}}}}`
	body, _ := json.Marshal(map[string]interface{}{"service_id": svc.ID, "api_key_id": "key-norm", "client_id": "client-norm", "requires_approval": false, "permission_config": cfg})
	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var stored string
	if err := db.QueryRow(`SELECT permission_config FROM placeholder_keys WHERE client_id = 'client-norm'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "refs/heads/") || strings.Contains(stored, "refs/tags/") {
		t.Fatalf("stored config keeps ref prefixes: %s", stored)
	}
	if !strings.Contains(stored, `"agent/**"`) || !strings.Contains(stored, `"v*"`) {
		t.Fatalf("stored config lost patterns: %s", stored)
	}
	if err := services.ValidatePermissionConfig(stored); err != nil {
		t.Fatalf("stored config no longer validates: %v", err)
	}
}

func TestPlaceholderBranchPoliciesOnlyOnGitHubService(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	enc, err := crypto.Encrypt("sk-" + strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	other := &models.Service{ID: "svc-other", Name: "other", DisplayName: "Other", UpstreamURL: "https://api.example.test", HostPattern: "example.test",
		AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ", KeyPrefix: "sk-", KeyLength: 43, DeliveryMode: "proxy", IsActive: true}
	if err := svcQ.Create(other); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-other','c','hash-other')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted) VALUES ('key-other',?,'k',?)`, other.ID, enc); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewPlaceholderHandler(queries.NewPlaceholderQueries(db), svcQ, queries.NewClientQueries(db)).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	policy := `{"version":"2","provider":"github","repositories":{"o/r":{"capabilities":{"clone":true,"push":true},"Branch_Policy":{"rules":[{"branches":"**","role":"read"}]}}}}`
	plain := `{"version":"2","provider":"github","repositories":{"o/r":{"capabilities":{"clone":true}}}}`
	create := func(cfg string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]interface{}{"service_id": other.ID, "api_key_id": "key-other", "client_id": "client-other", "requires_approval": false, "permission_config": cfg})
		rec := httptest.NewRecorder()
		h.Create(rec, httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(string(body))))
		return rec
	}
	if rec := create(policy); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "branch rules are only supported on GitHub placeholders") {
		t.Fatalf("create with policy on non-github: %d %s", rec.Code, rec.Body.String())
	}
	rec := create(plain)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create without policy: %d %s", rec.Code, rec.Body.String())
	}
	var made struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	upd := func(cfg string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"permission_config": cfg})
		req := httptest.NewRequest(http.MethodPut, "/api/placeholders/"+made.ID, strings.NewReader(string(body)))
		req.SetPathValue("id", made.ID)
		rec := httptest.NewRecorder()
		h.Update(rec, req)
		return rec
	}
	if rec := upd(policy); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "only supported on GitHub placeholders") {
		t.Fatalf("update with policy on non-github: %d %s", rec.Code, rec.Body.String())
	}
	var stored string
	if err := db.QueryRow(`SELECT COALESCE(permission_config,'') FROM placeholder_keys WHERE id = ?`, made.ID).Scan(&stored); err != nil || strings.Contains(stored, "Branch_Policy") {
		t.Fatalf("rejected config stored: %q %v", stored, err)
	}
}
