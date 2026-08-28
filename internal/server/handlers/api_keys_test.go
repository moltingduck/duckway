package handlers_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestAPIKeyUpdateRejectsRefreshableSecretReplacement(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	openaiSvc := testOpenAIService("svc-openai-refreshable-api-key")
	if err := svcQ.Create(openaiSvc); err != nil {
		t.Fatal(err)
	}
	encAccess, err := crypto.Encrypt("old-access")
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, refresh_token)
		VALUES ('key-refreshable-api-edit', ?, 'refreshable', ?, ?)`, openaiSvc.ID, encAccess, encRefresh); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto)
	req := httptest.NewRequest(http.MethodPut, "/api/keys/key-refreshable-api-edit", strings.NewReader(`{"name":"refreshable","key":"new-access-only"}`))
	req.SetPathValue("id", "key-refreshable-api-edit")
	rec := httptest.NewRecorder()

	h.Update(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Refreshable Tokens") {
		t.Fatalf("unexpected error: %s", rec.Body.String())
	}
	key, err := apiKeyQ.GetByID("key-refreshable-api-edit")
	if err != nil {
		t.Fatal(err)
	}
	accessToken, err := crypto.Decrypt(key.KeyEncrypted)
	if err != nil {
		t.Fatal(err)
	}
	if accessToken != "old-access" {
		t.Fatalf("refreshable access token was modified through API Keys update: %q", accessToken)
	}
}

func TestAPIKeyCreateStoresAndRedactsUpstreamProxyURL(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	openaiSvc := testOpenAIService("svc-openai-proxy-url")
	if err := svcQ.Create(openaiSvc); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto)
	body := `{"service_id":"` + openaiSvc.ID + `","name":"proxied","key":"sk-real","upstream_proxy_url":"http://user:secret@proxy.example:8080"}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("create response leaked proxy password: %s", rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	stored, err := apiKeyQ.GetByID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.UpstreamProxyURL, "secret") {
		t.Fatalf("stored upstream proxy leaked secret: %q", stored.UpstreamProxyURL)
	}
	plainProxy, err := services.DecryptUpstreamProxyURL(crypto, stored.UpstreamProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if plainProxy != "http://user:secret@proxy.example:8080" {
		t.Fatalf("decrypted upstream proxy = %q", plainProxy)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/keys/"+created.ID, nil)
	req.SetPathValue("id", created.ID)
	rec = httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("response leaked proxy password: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "http://user@proxy.example:8080") {
		t.Fatalf("response missing redacted proxy URL: %s", rec.Body.String())
	}
}

func TestAPIKeyCreateRejectsInvalidUpstreamProxyURL(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	openaiSvc := testOpenAIService("svc-openai-bad-proxy-url")
	if err := svcQ.Create(openaiSvc); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto)
	body := `{"service_id":"` + openaiSvc.ID + `","name":"bad","key":"sk-real","upstream_proxy_url":"file:///tmp/proxy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
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

func TestAPIKeyListMarksGitHubAppCredentialAsMintable(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	ghSvc := testGitHubService("svc-gh-mintable-list")
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
	}
	credJSON, _ := json.Marshal(cred)
	encrypted, err := crypto.Encrypt(string(credJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := apiKeyQ.Create(&models.APIKey{
		ID:           "key-gh-mintable-list",
		ServiceID:    ghSvc.ID,
		Name:         "github app",
		KeyEncrypted: encrypted,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	rec := httptest.NewRecorder()
	handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto).List(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var keys []models.APIKey
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].IsMintable {
		t.Fatalf("keys = %+v, want one mintable key", keys)
	}
	if keys[0].KeyEncrypted != "" {
		t.Fatalf("list response leaked encrypted key: %+v", keys[0])
	}
}

func TestAPIKeyListGitHubAppRepositories(t *testing.T) {
	var mintCount, repoListCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/123/access_tokens":
			mintCount++
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Fatalf("missing JWT auth header: %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"token":"ghs_repo_list_token","expires_at":"2099-07-08T12:30:00Z","permissions":{"contents":"read"}}`))
		case "/installation/repositories":
			repoListCount++
			if got := r.Header.Get("Authorization"); got != "Bearer ghs_repo_list_token" {
				t.Fatalf("repository list Authorization = %q", got)
			}
			if got := r.URL.Query().Get("per_page"); got != "100" {
				t.Fatalf("per_page = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"total_count":2,"repositories":[{"full_name":"OWNER/REPO","private":true,"html_url":"https://github.com/OWNER/REPO"},{"full_name":"OWNER/PUBLIC","private":false,"html_url":"https://github.com/OWNER/PUBLIC"}]}`))
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	ghSvc := testGitHubService("svc-gh-repo-list")
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
		"base_url":        upstream.URL,
	}
	credJSON, _ := json.Marshal(cred)
	encrypted, err := crypto.Encrypt(string(credJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := apiKeyQ.Create(&models.APIKey{
		ID:           "key-gh-repo-list",
		ServiceID:    ghSvc.ID,
		Name:         "github app",
		KeyEncrypted: encrypted,
	}); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto).WithHTTPClient(upstream.Client())
	req := httptest.NewRequest(http.MethodGet, "/api/keys/key-gh-repo-list/github-app/repositories", nil)
	req.SetPathValue("id", "key-gh-repo-list")
	rec := httptest.NewRecorder()

	h.ListGitHubAppRepositories(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		KeyID        string `json:"key_id"`
		TotalCount   int    `json:"total_count"`
		Repositories []struct {
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
			HTMLURL  string `json:"html_url"`
		} `json:"repositories"`
		InstallationPermissions map[string]string `json:"installation_permissions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.KeyID != "key-gh-repo-list" || resp.TotalCount != 2 || len(resp.Repositories) != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Repositories[0].FullName != "OWNER/REPO" || !resp.Repositories[0].Private || resp.Repositories[1].HTMLURL == "" {
		t.Fatalf("unexpected repositories: %+v", resp.Repositories)
	}
	if mintCount != 1 || repoListCount != 1 {
		t.Fatalf("mintCount=%d repoListCount=%d, want 1/1", mintCount, repoListCount)
	}
	if resp.InstallationPermissions["contents"] != "read" {
		t.Fatalf("installation permissions = %+v", resp.InstallationPermissions)
	}
}

func TestAPIKeyListGitHubAppRepositoriesRejectsCrossOrigin(t *testing.T) {
	h := handlers.NewAPIKeyHandler(nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/keys/key/github-app/repositories", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()

	h.ListGitHubAppRepositories(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyListGitHubAppRepositoriesRejectsNonMinter(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	ghSvc := testGitHubService("svc-gh-repo-list-pat")
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	encrypted, err := crypto.Encrypt("github_pat_" + strings.Repeat("a", 82))
	if err != nil {
		t.Fatal(err)
	}
	if err := apiKeyQ.Create(&models.APIKey{
		ID:           "key-gh-repo-list-pat",
		ServiceID:    ghSvc.ID,
		Name:         "github pat",
		KeyEncrypted: encrypted,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/keys/key-gh-repo-list-pat/github-app/repositories", nil)
	req.SetPathValue("id", "key-gh-repo-list-pat")
	rec := httptest.NewRecorder()
	handlers.NewAPIKeyHandler(apiKeyQ, svcQ, crypto).ListGitHubAppRepositories(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not a GitHub App installation minter") {
		t.Fatalf("response should explain non-minter key: %s", rec.Body.String())
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

func TestAPIKeyTestGitHubAppMinterAcceptsLargeGitHubResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"token":       "ghs_real_secret_should_not_leak",
			"expires_at":  "2026-07-08T12:30:00Z",
			"permissions": map[string]string{"contents": "read"},
			"filler":      strings.Repeat("x", 8192),
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode upstream response: %v", err)
		}
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
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ghs_real_secret") {
		t.Fatalf("response leaked token: %s", rec.Body.String())
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

func TestGitHubAppMinterLive(t *testing.T) {
	if os.Getenv("DUCKWAY_TEST_GITHUB_APP_LIVE") != "1" {
		t.Skip("set DUCKWAY_TEST_GITHUB_APP_LIVE=1 to run")
	}
	configPath := os.Getenv("DUCKWAY_GITHUB_APP_LIVE_CONFIG")
	if configPath == "" {
		configPath = findGitHubAppLiveConfig(t)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read live config %s: %v", configPath, err)
	}
	var cfg struct {
		AppID          int64  `json:"app_id"`
		InstallationID int64  `json:"installation_id"`
		PrivateKey     string `json:"private_key"`
		Repository     string `json:"repository"`
		BaseURL        string `json:"base_url,omitempty"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse live config %s: %v", configPath, err)
	}
	if cfg.AppID <= 0 || cfg.InstallationID <= 0 || strings.TrimSpace(cfg.PrivateKey) == "" || strings.TrimSpace(cfg.Repository) == "" {
		t.Fatalf("live config %s requires app_id, installation_id, private_key, and repository", configPath)
	}
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          cfg.AppID,
		"installation_id": cfg.InstallationID,
		"private_key":     cfg.PrivateKey,
	}
	if cfg.BaseURL != "" {
		cred["base_url"] = cfg.BaseURL
	}
	credJSON, _ := json.Marshal(cred)
	body := `{"credential":` + strconvQuote(string(credJSON)) + `,"repository":` + strconvQuote(cfg.Repository) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys/github-app/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handlers.NewAPIKeyHandler(nil, nil, nil).TestGitHubAppMinter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("live GitHub App minter status = %d body=%s", rec.Code, rec.Body.String())
	}
	resp := rec.Body.String()
	for _, secretMarker := range []struct {
		name  string
		value string
	}{
		{name: "installation token", value: "ghs_"},
		{name: "private key label", value: "PRIVATE KEY"},
		{name: "private key body", value: cfg.PrivateKey},
	} {
		if secretMarker.value != "" && strings.Contains(resp, secretMarker.value) {
			t.Fatalf("live response leaked %s", secretMarker.name)
		}
	}
	if !strings.Contains(resp, `"status":"ok"`) || !strings.Contains(resp, `"contents":"read"`) {
		t.Fatalf("live response missing success metadata: %s", resp)
	}
}

func TestAPIKeyDeleteRefreshablePreviewsImpact(t *testing.T) {
	db, apiKeyQ := seedAPIKeyDeleteRefreshable(t)
	h := handlers.NewAPIKeyHandler(apiKeyQ, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/keys/key-refresh-api", strings.NewReader(`{"confirm":false}`))
	req.SetPathValue("id", "key-refresh-api")
	rec := httptest.NewRecorder()

	h.Delete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		RequiresConfirmation bool `json:"requires_confirmation"`
		Impact               struct {
			KeyName         string        `json:"key_name"`
			KeySuites       []interface{} `json:"key_suites"`
			Clients         []interface{} `json:"clients"`
			ControlChannels []interface{} `json:"control_channels"`
		} `json:"impact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.RequiresConfirmation || resp.Impact.KeyName != "refresh api key" {
		t.Fatalf("unexpected preview response: %+v", resp)
	}
	if len(resp.Impact.KeySuites) != 1 || len(resp.Impact.Clients) != 1 || len(resp.Impact.ControlChannels) != 1 {
		t.Fatalf("unexpected impact counts: %+v", resp.Impact)
	}
	var keyCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id = 'key-refresh-api'`).Scan(&keyCount); err != nil {
		t.Fatal(err)
	}
	if keyCount != 1 {
		t.Fatalf("preview deleted key, count=%d", keyCount)
	}
}

func TestAPIKeyDeleteRefreshableConfirmCleansReferences(t *testing.T) {
	db, apiKeyQ := seedAPIKeyDeleteRefreshable(t)
	h := handlers.NewAPIKeyHandler(apiKeyQ, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/keys/key-refresh-api", strings.NewReader(`{"confirm":true}`))
	req.SetPathValue("id", "key-refresh-api")
	rec := httptest.NewRecorder()

	h.Delete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	for label, query := range map[string]string{
		"key suite entries": `SELECT COUNT(*) FROM key_suite_entries WHERE api_key_id = 'key-refresh-api'`,
		"placeholders":      `SELECT COUNT(*) FROM placeholder_keys WHERE api_key_id = 'key-refresh-api'`,
	} {
		var count int
		if err := db.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("%s count: %v", label, err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0", label, count)
		}
	}
	var ccActive int
	if err := db.QueryRow(`SELECT is_active FROM control_channels WHERE id = 'cc-refresh-api'`).Scan(&ccActive); err != nil {
		t.Fatal(err)
	}
	if ccActive != 0 {
		t.Fatalf("control channel active = %d, want 0", ccActive)
	}
	key, err := apiKeyQ.GetByID("key-refresh-api")
	if err != nil {
		t.Fatal(err)
	}
	if key.IsRefreshable || key.IsActive || !strings.HasPrefix(key.Name, "Deleted refreshable token:") {
		t.Fatalf("retained key not marked deleted: %+v", key)
	}
}

func seedAPIKeyDeleteRefreshable(t *testing.T) (*sql.DB, *queries.APIKeyQueries) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec := func(query string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern)
		VALUES ('svc-refresh-api', 'refresh-api', 'Refresh API', 'https://refresh.example', 'refresh.example')`)
	exec(`INSERT INTO clients (id, name, token_hash)
		VALUES ('client-refresh-api', 'refresh api client', 'hash-refresh-api')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, refresh_token)
		VALUES ('key-refresh-api', 'svc-refresh-api', 'refresh api key', 'encrypted-access', 'encrypted-refresh')`)
	exec(`INSERT INTO key_suites (id, name, description)
		VALUES ('suite-refresh-api', 'Refresh API Suite', '')`)
	exec(`INSERT INTO key_suite_entries (id, suite_id, service_id, api_key_id, env_name)
		VALUES ('entry-refresh-api', 'suite-refresh-api', 'svc-refresh-api', 'key-refresh-api', 'REFRESH_API_KEY')`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, suite_id)
		VALUES ('ph-refresh-api', 'REFRESH_API_KEY', 'sk-refresh-api', 'svc-refresh-api', 'key-refresh-api', 'client-refresh-api', 'suite-refresh-api')`)
	exec(`INSERT INTO control_channels (id, name, service_id, api_key_id, client_id, agent_type, placeholder_id, config, is_active)
		VALUES ('cc-refresh-api', 'Refresh API CC', 'svc-refresh-api', 'key-refresh-api', 'client-refresh-api', 'codex', 'ph-refresh-api', '{}', 1)`)
	return db, queries.NewAPIKeyQueries(db)
}

func findGitHubAppLiveConfig(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "secrets", "github-app-live.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return filepath.Join("secrets", "github-app-live.json")
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
