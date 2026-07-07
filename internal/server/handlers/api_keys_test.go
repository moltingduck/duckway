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

func TestAPIKeyCreateAcceptsGitHubAppCredentialAndRedactsSecret(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	ghSvc := testGitHubService("svc-gh-api-key")
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto)
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
	}
	credJSON, _ := json.Marshal(cred)
	body := `{"service_id":"` + ghSvc.ID + `","name":"github app","key":` + strconvQuote(string(credJSON)) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/keys/"+created.ID, nil)
	req.SetPathValue("id", created.ID)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", rec.Code, rec.Body.String())
	}
	gotBody := rec.Body.String()
	if !strings.Contains(gotBody, "github_app app_id=99 installation_id=123") {
		t.Fatalf("missing github app preview: %s", gotBody)
	}
	if strings.Contains(gotBody, "PRIVATE KEY") {
		t.Fatalf("response leaked private key: %s", gotBody)
	}
}

func TestAPIKeyCreateRejectsInvalidGitHubAppPrivateKey(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	ghSvc := testGitHubService("svc-gh-api-key-bad")
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto)
	credJSON := `{"type":"github_app","app_id":99,"installation_id":123,"private_key":"not pem"}`
	body := `{"service_id":"` + ghSvc.ID + `","name":"bad github app","key":` + strconvQuote(credJSON) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func testGitHubService(id string) *models.Service {
	return &models.Service{
		ID:           id,
		Name:         "github",
		DisplayName:  "GitHub API + Git",
		UpstreamURL:  "https://api.github.com",
		HostPattern:  "api.github.com,github.com",
		AuthType:     "bearer",
		AuthHeader:   "Authorization",
		AuthPrefix:   "Bearer ",
		KeyPrefix:    "github_pat_",
		KeyLength:    93,
		DeliveryMode: "proxy",
		IsActive:     true,
	}
}
