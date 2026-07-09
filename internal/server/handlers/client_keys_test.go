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
)

func TestClientGetKeysIncludesPermissionConfig(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	svcQ := queries.NewServiceQueries(db)
	clientQ := queries.NewClientQueries(db)
	placeholderQ := queries.NewPlaceholderQueries(db)
	svc := &models.Service{
		ID:          "svc-client-keys-github",
		Name:        "github",
		DisplayName: "GitHub",
		UpstreamURL: "https://github.com",
		AuthType:    "bearer",
		AuthHeader:  "Authorization",
		AuthPrefix:  "Bearer ",
		KeyPrefix:   "github_pat_",
		KeyLength:   93,
		IsActive:    true,
	}
	if err := svcQ.Create(svc); err != nil {
		t.Fatal(err)
	}
	c := &models.Client{ID: "client-keys", Name: "client keys", TokenHash: "hash"}
	if err := clientQ.Create(c); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('key-client-keys-github', ?, 'github key', 'encrypted')`, svc.ID); err != nil {
		t.Fatal(err)
	}
	acl := `{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"GET","path":"/OWNER/REPO.git/info/refs","allow":true}],"deny_all_other":true}]}`
	if err := placeholderQ.Create(&models.PlaceholderKey{
		ID:               "ph-client-keys-github",
		EnvName:          "GITHUB_TOKEN",
		Placeholder:      "github_pat_dw_fake",
		ServiceID:        svc.ID,
		APIKeyID:         stringPtr("key-client-keys-github"),
		ClientID:         c.ID,
		PermissionConfig: &acl,
		IsActive:         true,
	}); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewClientHandler(clientQ, placeholderQ, svcQ, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/client/keys", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, c))
	rec := httptest.NewRecorder()
	h.GetKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got []struct {
		ServiceName      string `json:"service_name"`
		PermissionConfig string `json:"permission_config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ServiceName != "github" || got[0].PermissionConfig != acl {
		t.Fatalf("response = %+v, want github key with permission_config", got)
	}
}

func stringPtr(s string) *string { return &s }
