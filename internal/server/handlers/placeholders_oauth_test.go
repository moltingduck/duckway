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

func TestCreatePlaceholderForGitHubPATUsesFineGrainedPhantom(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	realPAT := "github_pat_" + strings.Repeat("a", 82)
	encPAT, err := crypto.Encrypt(realPAT)
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	ghSvc := &models.Service{
		ID:           "svc-gh-pat",
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
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-gh-pat','github client','hash-gh')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-gh-pat',?,'github pat',?)`, ghSvc.ID, encPAT); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewPlaceholderHandler(
		queries.NewPlaceholderQueries(db),
		svcQ,
		queries.NewClientQueries(db),
	).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	req := httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(`{
		"service_id":"`+ghSvc.ID+`",
		"api_key_id":"key-gh-pat",
		"client_id":"client-gh-pat",
		"requires_approval":false
	}`))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		EnvName     string `json:"env_name"`
		Placeholder string `json:"placeholder"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.EnvName != "GITHUB_TOKEN" {
		t.Fatalf("env_name = %q, want GITHUB_TOKEN", got.EnvName)
	}
	if !strings.HasPrefix(got.Placeholder, "github_pat_") {
		t.Fatalf("placeholder prefix wrong: %q", got.Placeholder)
	}
	if len(got.Placeholder) != 93 {
		t.Fatalf("placeholder len = %d, want 93: %q", len(got.Placeholder), got.Placeholder)
	}
	if !strings.Contains(got.Placeholder, "dw_") || !services.IsPlaceholder(got.Placeholder) {
		t.Fatalf("placeholder marker missing: %q", got.Placeholder)
	}

	resolver := services.NewKeyResolver(
		crypto,
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		queries.NewGroupQueries(db),
		queries.NewApprovalQueries(db),
	)
	resolved, err := resolver.Resolve(got.Placeholder, "client-gh-pat")
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Permitted || resolved.RealKey != realPAT {
		t.Fatalf("resolve = %+v", resolved)
	}
}

func TestCreatePlaceholderUpdatesExistingPlaceholderToken(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	classicPAT := "ghp_" + strings.Repeat("a", 36)
	fineGrainedPAT := "github_pat_" + strings.Repeat("b", 82)
	encClassic, err := crypto.Encrypt(classicPAT)
	if err != nil {
		t.Fatal(err)
	}
	encFineGrained, err := crypto.Encrypt(fineGrainedPAT)
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	ghSvc := &models.Service{
		ID:           "svc-gh-update-placeholder",
		Name:         "github",
		DisplayName:  "GitHub",
		UpstreamURL:  "https://api.github.com",
		HostPattern:  "github.com",
		AuthType:     "bearer",
		AuthHeader:   "Authorization",
		AuthPrefix:   "Bearer ",
		KeyPrefix:    "github_pat_",
		KeyLength:    93,
		DeliveryMode: "proxy",
		IsActive:     true,
	}
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-gh-update','github client','hash-gh-update')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted) VALUES ('key-gh-classic',?,'classic',?), ('key-gh-fine',?,'fine',?)`,
		ghSvc.ID, encClassic, ghSvc.ID, encFineGrained); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewPlaceholderHandler(
		queries.NewPlaceholderQueries(db),
		svcQ,
		queries.NewClientQueries(db),
	).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	createBody := func(keyID string) string {
		return `{
			"service_id":"` + ghSvc.ID + `",
			"api_key_id":"` + keyID + `",
			"client_id":"client-gh-update",
			"requires_approval":false
		}`
	}
	rec1 := httptest.NewRecorder()
	h.Create(rec1, httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(createBody("key-gh-classic"))))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first status = %d body=%s", rec1.Code, rec1.Body.String())
	}
	var first struct {
		ID          string `json:"id"`
		Placeholder string `json:"placeholder"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Placeholder, "ghp_") {
		t.Fatalf("first placeholder = %q, want ghp_ prefix", first.Placeholder)
	}

	rec2 := httptest.NewRecorder()
	h.Create(rec2, httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(createBody("key-gh-fine"))))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status = %d body=%s", rec2.Code, rec2.Body.String())
	}
	var second struct {
		ID          string `json:"id"`
		Placeholder string `json:"placeholder"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("id = %q, want existing id %q", second.ID, first.ID)
	}
	if !strings.HasPrefix(second.Placeholder, "github_pat_") {
		t.Fatalf("second placeholder = %q, want github_pat_ prefix", second.Placeholder)
	}
	if second.Placeholder == first.Placeholder {
		t.Fatalf("placeholder was not updated: %q", second.Placeholder)
	}
}

func TestCreatePlaceholderForGitHubAppInstallationTokenUsesMatchingPhantom(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	realToken := "ghs_123456_" + strings.Repeat("z", 120)
	encToken, err := crypto.Encrypt(realToken)
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	ghSvc := &models.Service{
		ID:           "svc-gh-app",
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
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-gh-app','github app client','hash-gh-app')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-gh-app',?,'github app token',?)`, ghSvc.ID, encToken); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewPlaceholderHandler(
		queries.NewPlaceholderQueries(db),
		svcQ,
		queries.NewClientQueries(db),
	).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	req := httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(`{
		"service_id":"`+ghSvc.ID+`",
		"api_key_id":"key-gh-app",
		"client_id":"client-gh-app",
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
	if !strings.HasPrefix(got.Placeholder, "ghs_") {
		t.Fatalf("placeholder prefix wrong: %q", got.Placeholder)
	}
	if len(got.Placeholder) != len(realToken) {
		t.Fatalf("placeholder len = %d, want %d: %q", len(got.Placeholder), len(realToken), got.Placeholder)
	}
	if !strings.Contains(got.Placeholder, "dw_") || !services.IsPlaceholder(got.Placeholder) {
		t.Fatalf("placeholder marker missing: %q", got.Placeholder)
	}
}

func TestCreatePlaceholderStoresGitHubRepoPermissionConfig(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encToken, err := crypto.Encrypt("ghs_" + strings.Repeat("r", 80))
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	ghSvc := &models.Service{
		ID:           "svc-gh-repo-scope",
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
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-gh-repo-scope','github repo client','hash-gh-repo')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-gh-repo-scope',?,'github app token',?)`, ghSvc.ID, encToken); err != nil {
		t.Fatal(err)
	}

	acl := `{"version":"1","provider":"github","rules":[{"name":"deploy-read-only","endpoints":[{"method":"GET","path":"/OWNER/REPO.git/info/refs","allow":true},{"method":"POST","path":"/OWNER/REPO.git/git-upload-pack","allow":true}],"deny_all_other":true}]}`
	h := handlers.NewPlaceholderHandler(
		queries.NewPlaceholderQueries(db),
		svcQ,
		queries.NewClientQueries(db),
	).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	req := httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(`{
		"service_id":"`+ghSvc.ID+`",
		"api_key_id":"key-gh-repo-scope",
		"client_id":"client-gh-repo-scope",
		"requires_approval":false,
		"permission_config":`+strconvQuote(acl)+`
	}`))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID               string  `json:"id"`
		PermissionConfig *string `json:"permission_config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PermissionConfig == nil || *got.PermissionConfig != acl {
		t.Fatalf("response permission_config = %v, want ACL", got.PermissionConfig)
	}
	stored, err := queries.NewPlaceholderQueries(db).GetByID(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PermissionConfig == nil || *stored.PermissionConfig != acl {
		t.Fatalf("stored permission_config = %v, want ACL", stored.PermissionConfig)
	}
}

func TestCreatePlaceholderRejectsGitHubAppMinterWithoutRepoPermissionConfig(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
	}
	credJSON, _ := json.Marshal(cred)
	encCred, err := crypto.Encrypt(string(credJSON))
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	ghSvc := &models.Service{
		ID:           "svc-gh-app-minter-no-acl",
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
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-gh-app-minter-no-acl','github app client','hash-gh-app-no-acl')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-gh-app-minter-no-acl',?,'github app minter',?)`, ghSvc.ID, encCred); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewPlaceholderHandler(
		queries.NewPlaceholderQueries(db),
		svcQ,
		queries.NewClientQueries(db),
	).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	req := httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(`{
		"service_id":"`+ghSvc.ID+`",
		"api_key_id":"key-gh-app-minter-no-acl",
		"client_id":"client-gh-app-minter-no-acl",
		"requires_approval":false
	}`))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "repository-scoped permission_config") {
		t.Fatalf("response should explain missing repo ACL: %s", rec.Body.String())
	}
}

func TestCreatePlaceholderAcceptsGitHubAppMinterWithRepoPermissionConfig(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
	}
	credJSON, _ := json.Marshal(cred)
	encCred, err := crypto.Encrypt(string(credJSON))
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	ghSvc := &models.Service{
		ID:           "svc-gh-app-minter-acl",
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
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-gh-app-minter-acl','github app client','hash-gh-app-acl')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-gh-app-minter-acl',?,'github app minter',?)`, ghSvc.ID, encCred); err != nil {
		t.Fatal(err)
	}

	acl := `{"version":"1","provider":"github","rules":[{"name":"deploy-read-only","endpoints":[{"method":"GET","path":"/OWNER/REPO.git/info/refs","allow":true},{"method":"POST","path":"/OWNER/REPO.git/git-upload-pack","allow":true}],"deny_all_other":true}]}`
	h := handlers.NewPlaceholderHandler(
		queries.NewPlaceholderQueries(db),
		svcQ,
		queries.NewClientQueries(db),
	).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	req := httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(`{
		"service_id":"`+ghSvc.ID+`",
		"api_key_id":"key-gh-app-minter-acl",
		"client_id":"client-gh-app-minter-acl",
		"requires_approval":false,
		"permission_config":`+strconvQuote(acl)+`
	}`))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		PermissionConfig *string `json:"permission_config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PermissionConfig == nil || *got.PermissionConfig != acl {
		t.Fatalf("permission_config = %v, want ACL", got.PermissionConfig)
	}
}

func TestCreatePlaceholderUpdatesExistingClientServiceEnvAssignment(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	cred := map[string]interface{}{
		"type":            "github_app",
		"app_id":          99,
		"installation_id": 123,
		"private_key":     testRSAPrivateKeyPEM(t),
	}
	credJSON, _ := json.Marshal(cred)
	encCred, err := crypto.Encrypt(string(credJSON))
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	ghSvc := &models.Service{
		ID:           "svc-gh-app-minter-update",
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
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-gh-app-minter-update','github app client','hash-gh-app-update')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-gh-app-minter-update',?,'github app minter',?),
		       ('key-gh-app-minter-update-other',?,'github app minter other',?)`,
		ghSvc.ID, encCred, ghSvc.ID, encCred); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewPlaceholderHandler(
		queries.NewPlaceholderQueries(db),
		svcQ,
		queries.NewClientQueries(db),
	).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	create := func(keyID, acl string) (string, string, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(`{
			"service_id":"`+ghSvc.ID+`",
			"api_key_id":"`+keyID+`",
			"client_id":"client-gh-app-minter-update",
			"requires_approval":false,
			"permission_config":`+strconvQuote(acl)+`
		}`))
		rec := httptest.NewRecorder()
		h.Create(rec, req)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var got struct {
			ID               string  `json:"id"`
			EnvName          string  `json:"env_name"`
			Placeholder      string  `json:"placeholder"`
			PermissionConfig *string `json:"permission_config"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.PermissionConfig == nil || *got.PermissionConfig != acl {
			t.Fatalf("permission_config = %v, want %s", got.PermissionConfig, acl)
		}
		return got.ID, got.EnvName, got.Placeholder
	}

	acl1 := `{"version":"1","provider":"github","rules":[{"name":"deploy-read-only","endpoints":[{"method":"GET","path":"/OWNER/REPO.git/info/refs","allow":true},{"method":"POST","path":"/OWNER/REPO.git/git-upload-pack","allow":true}],"deny_all_other":true}]}`
	acl2 := `{"version":"1","provider":"github","rules":[{"name":"deploy-read-only","endpoints":[{"method":"GET","path":"/OWNER/OTHER.git/info/refs","allow":true},{"method":"POST","path":"/OWNER/OTHER.git/git-upload-pack","allow":true}],"deny_all_other":true}]}`
	id1, env1, ph1 := create("key-gh-app-minter-update", acl1)
	id2, env2, ph2 := create("key-gh-app-minter-update", acl2)
	if id2 != id1 || env2 != env1 || ph2 != ph1 {
		t.Fatalf("same client/minter assignment should overwrite existing placeholder, got (%s,%s,%s) then (%s,%s,%s)", id1, env1, ph1, id2, env2, ph2)
	}
	if !strings.HasPrefix(env1, "GITHUB_TOKEN_") {
		t.Fatalf("mintable env name should be suffixed, got %q", env1)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM placeholder_keys WHERE client_id='client-gh-app-minter-update' AND service_id=?`, ghSvc.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("placeholder count = %d, want 1", count)
	}
	stored, err := queries.NewPlaceholderQueries(db).GetByID(id1)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PermissionConfig == nil || *stored.PermissionConfig != acl2 {
		t.Fatalf("stored permission_config = %v, want overwritten ACL", stored.PermissionConfig)
	}
	id3, env3, _ := create("key-gh-app-minter-update-other", acl1)
	if id3 == id1 || env3 == env1 {
		t.Fatalf("different minter key should create a separate assignment, got (%s,%s) and (%s,%s)", id1, env1, id3, env3)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM placeholder_keys WHERE client_id='client-gh-app-minter-update' AND service_id=?`, ghSvc.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("placeholder count after second minter = %d, want 2", count)
	}
}

func TestCreatePlaceholderRejectsAPIKeyFromDifferentService(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encKey, err := crypto.Encrypt("sk-openai-real")
	if err != nil {
		t.Fatal(err)
	}
	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		openaiSvc = &models.Service{
			ID:          "svc-openai-cross",
			Name:        "openai",
			DisplayName: "OpenAI",
			UpstreamURL: "https://api.openai.com",
			AuthType:    "bearer",
			AuthHeader:  "Authorization",
			AuthPrefix:  "Bearer ",
			KeyPrefix:   "sk-",
			KeyLength:   51,
			IsActive:    true,
		}
		if err := svcQ.Create(openaiSvc); err != nil {
			t.Fatal(err)
		}
	}
	ghSvc := &models.Service{
		ID:           "svc-github-cross",
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
	if err := svcQ.Create(ghSvc); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES ('client-cross','cross client','hash-cross')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted)
		VALUES ('key-openai-cross',?,'openai key',?)`, openaiSvc.ID, encKey); err != nil {
		t.Fatal(err)
	}

	h := handlers.NewPlaceholderHandler(
		queries.NewPlaceholderQueries(db),
		svcQ,
		queries.NewClientQueries(db),
	).WithKeyLookup(queries.NewAPIKeyQueries(db), crypto)
	req := httptest.NewRequest(http.MethodPost, "/api/placeholders", strings.NewReader(`{
		"service_id":"`+ghSvc.ID+`",
		"api_key_id":"key-openai-cross",
		"client_id":"client-cross",
		"requires_approval":false
	}`))
	rec := httptest.NewRecorder()

	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
