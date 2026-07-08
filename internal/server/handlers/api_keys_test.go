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

func TestAPIKeyCreateRejectsGitHubAppCredentialForNonGitHubService(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	openaiSvc := testOpenAIService("svc-openai-api-key")
	if err := svcQ.Create(openaiSvc); err != nil {
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
	body := `{"service_id":"` + openaiSvc.ID + `","name":"wrong service","key":` + strconvQuote(string(credJSON)) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyCreateRejectsGitHubAppCredentialWithUnsafeBaseURL(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	ghSvc := testGitHubService("svc-gh-api-key-evil-base")
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto)
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
		"base_url":        "http://evil.example",
	}
	credJSON, _ := json.Marshal(cred)
	body := `{"service_id":"` + ghSvc.ID + `","name":"evil base","key":` + strconvQuote(string(credJSON)) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyCreateAllowsMalformedJSONForNonGitHubService(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	openaiSvc := testOpenAIService("svc-openai-json-key")
	if err := svcQ.Create(openaiSvc); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto)
	body := `{"service_id":"` + openaiSvc.ID + `","name":"json shaped","key":"{not-json"}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyTestGitHubAppMinterMintsReadOnlyRepoToken(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"ghs_real_secret_should_not_leak","expires_at":"2026-07-08T12:30:00Z","permissions":{"contents":"read"}}`))
	}))
	defer upstream.Close()

	h := handlers.NewAPIKeyHandler(nil, nil, nil).WithHTTPClient(upstream.Client())
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
		"base_url":        upstream.URL,
	}
	credJSON, _ := json.Marshal(cred)
	body := `{"credential":` + strconvQuote(string(credJSON)) + `,"repository":"OWNER/REPO"}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys/github-app/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()

	h.TestGitHubAppMinter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/app/installations/123/access_tokens" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("missing JWT auth header: %q", gotAuth)
	}
	if len(gotBody.Repositories) != 1 || gotBody.Repositories[0] != "REPO" {
		t.Fatalf("repositories = %+v", gotBody.Repositories)
	}
	if gotBody.Permissions["contents"] != "read" || len(gotBody.Permissions) != 1 {
		t.Fatalf("permissions = %+v", gotBody.Permissions)
	}
	resp := rec.Body.String()
	for _, leaked := range []string{"ghs_real_secret", "PRIVATE KEY", "Authorization", "token"} {
		if strings.Contains(strings.ToLower(resp), strings.ToLower(leaked)) {
			t.Fatalf("response leaked %q: %s", leaked, resp)
		}
	}
	if !strings.Contains(resp, `"repository":"OWNER/REPO"`) || !strings.Contains(resp, `"contents":"read"`) {
		t.Fatalf("response missing safe metadata: %s", resp)
	}
}

func TestAPIKeyTestGitHubAppMinterRejectsBadRepo(t *testing.T) {
	h := handlers.NewAPIKeyHandler(nil, nil, nil)
	for _, repo := range []string{"", "OWNER", "OWNER/REPO/EXTRA", "https://github.com/OWNER/REPO", "../REPO", "OWNER/REPO.git"} {
		body := `{"credential":"{}","repository":` + strconvQuote(repo) + `}`
		req := httptest.NewRequest(http.MethodPost, "/api/keys/github-app/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		h.TestGitHubAppMinter(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("repo %q status = %d body=%s", repo, rec.Code, rec.Body.String())
		}
	}
}

func TestAPIKeyTestGitHubAppMinterRejectsCrossOrigin(t *testing.T) {
	h := handlers.NewAPIKeyHandler(nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/keys/github-app/test", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()

	h.TestGitHubAppMinter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyTestGitHubAppMinterUpstreamFailureDoesNotLeakBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ghs_real_secret_should_not_leak PRIVATE KEY", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	h := handlers.NewAPIKeyHandler(nil, nil, nil).WithHTTPClient(upstream.Client())
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
		"base_url":        upstream.URL,
	}
	credJSON, _ := json.Marshal(cred)
	body := `{"credential":` + strconvQuote(string(credJSON)) + `,"repository":"OWNER/REPO"}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys/github-app/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.TestGitHubAppMinter(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	resp := rec.Body.String()
	if strings.Contains(resp, "ghs_real_secret") || strings.Contains(resp, "PRIVATE KEY") {
		t.Fatalf("response leaked upstream body: %s", resp)
	}
}

func TestAPIKeyTestGitHubAppMinterEmptySuccessBodyIsDiagnostic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	h := handlers.NewAPIKeyHandler(nil, nil, nil).WithHTTPClient(upstream.Client())
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
		"base_url":        upstream.URL,
	}
	credJSON, _ := json.Marshal(cred)
	body := `{"credential":` + strconvQuote(string(credJSON)) + `,"repository":"OWNER/REPO"}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys/github-app/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.TestGitHubAppMinter(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "response was empty") {
		t.Fatalf("response should explain empty body: %s", rec.Body.String())
	}
}

func TestAPIKeyTestGitHubAppMinterMalformedSuccessBodyIsDiagnostic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`<html></html>`))
	}))
	defer upstream.Close()

	h := handlers.NewAPIKeyHandler(nil, nil, nil).WithHTTPClient(upstream.Client())
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
		"base_url":        upstream.URL,
	}
	credJSON, _ := json.Marshal(cred)
	body := `{"credential":` + strconvQuote(string(credJSON)) + `,"repository":"OWNER/REPO"}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys/github-app/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.TestGitHubAppMinter(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "response was not JSON") || strings.Contains(rec.Body.String(), "<html>") {
		t.Fatalf("response should explain malformed body without leaking body: %s", rec.Body.String())
	}
}

func TestAPIKeyTestGitHubAppMinterRejectsMissingContentsPermission(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"ghs_real_secret_should_not_leak","expires_at":"2026-07-08T12:30:00Z","permissions":{"issues":"read"}}`))
	}))
	defer upstream.Close()

	h := handlers.NewAPIKeyHandler(nil, nil, nil).WithHTTPClient(upstream.Client())
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
		"base_url":        upstream.URL,
	}
	credJSON, _ := json.Marshal(cred)
	body := `{"credential":` + strconvQuote(string(credJSON)) + `,"repository":"OWNER/REPO"}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys/github-app/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.TestGitHubAppMinter(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	resp := rec.Body.String()
	if !strings.Contains(resp, "contents: read") {
		t.Fatalf("response should explain missing permission: %s", resp)
	}
	if strings.Contains(resp, "ghs_real_secret") {
		t.Fatalf("response leaked token: %s", resp)
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

func testOpenAIService(id string) *models.Service {
	return &models.Service{
		ID:          id,
		Name:        id,
		DisplayName: "OpenAI",
		UpstreamURL: "https://api.openai.com",
		AuthType:    "bearer",
		AuthHeader:  "Authorization",
		AuthPrefix:  "Bearer ",
		KeyPrefix:   "sk-",
		KeyLength:   51,
		IsActive:    true,
	}
}
