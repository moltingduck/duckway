package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
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

type loanAuthFixture struct {
	db        *sql.DB
	crypto    *services.Crypto
	loan      *handlers.LoanHandler
	keyGroups *handlers.KeyGroupHandler
	service   string
	groupID   string
	apiKeyID  string
	bound     *models.Client
	unbound   *models.Client
}

func newLoanAuthFixture(t *testing.T) *loanAuthFixture {
	t.Helper()

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	svcQ := queries.NewServiceQueries(db)
	apiKeyQ := queries.NewAPIKeyQueries(db)
	clientQ := queries.NewClientQueries(db)

	serviceName := "loan-auth-test"
	if err := svcQ.Create(&models.Service{
		ID:           "svc-loan-auth",
		Name:         serviceName,
		DisplayName:  "Loan Auth Test",
		UpstreamURL:  "https://example.invalid",
		HostPattern:  "example.invalid",
		AuthType:     "bearer",
		AuthHeader:   "Authorization",
		AuthPrefix:   "Bearer ",
		DeliveryMode: "loan_proxy",
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	bound := &models.Client{ID: "client-bound", ShortID: "bound1", Name: "bound", TokenHash: "bound-hash", CanaryEnabled: true}
	unbound := &models.Client{ID: "client-unbound", ShortID: "unbnd1", Name: "unbound", TokenHash: "unbound-hash", CanaryEnabled: true}
	if err := clientQ.Create(bound); err != nil {
		t.Fatalf("seed bound client: %v", err)
	}
	if err := clientQ.Create(unbound); err != nil {
		t.Fatalf("seed unbound client: %v", err)
	}

	encrypted, err := crypto.Encrypt("sk-real-loan-token")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	apiKeyID := "key-loan-auth"
	if err := apiKeyQ.Create(&models.APIKey{
		ID:           apiKeyID,
		ServiceID:    "svc-loan-auth",
		Name:         "loan key",
		KeyEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	if _, err := db.Exec(`UPDATE api_keys SET usage_snapshot = '{}' WHERE id = ?`, apiKeyID); err != nil {
		t.Fatalf("seed api key usage snapshot: %v", err)
	}

	group, err := queries.CreateKeyGroup(db, "loan group", "", serviceName, "score")
	if err != nil {
		t.Fatalf("seed key group: %v", err)
	}
	if err := queries.AddKeyToGroup(db, group.ID, apiKeyID, 0); err != nil {
		t.Fatalf("seed key group member: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO placeholder_keys
			(id, env_name, placeholder, service_id, api_key_id, client_id, requires_approval, key_group_id)
		VALUES
			('ph-bound-group', 'TEST_TOKEN', 'dk_bound_group', 'svc-loan-auth', ?, ?, 0, ?)`,
		apiKeyID, bound.ID, group.ID,
	); err != nil {
		t.Fatalf("seed group binding placeholder: %v", err)
	}

	resolver := services.NewKeyResolver(crypto, apiKeyQ, queries.NewPlaceholderQueries(db), queries.NewGroupQueries(db), queries.NewApprovalQueries(db))
	loan := handlers.NewLoanHandler(resolver, svcQ, queries.NewApprovalQueries(db), queries.NewRequestLogQueries(db), nil).WithCrypto(crypto).WithDB(db)
	return &loanAuthFixture{
		db:        db,
		crypto:    crypto,
		loan:      loan,
		keyGroups: handlers.NewKeyGroupHandler(db),
		service:   serviceName,
		groupID:   group.ID,
		apiKeyID:  apiKeyID,
		bound:     bound,
		unbound:   unbound,
	}
}

func requestWithClient(r *http.Request, client *models.Client) *http.Request {
	ctx := context.WithValue(r.Context(), middleware.ClientKey, client)
	return r.WithContext(ctx)
}

func TestLoanGroupRequiresClientBinding(t *testing.T) {
	f := newLoanAuthFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/client/loan?service="+f.service+"&group="+f.groupID, nil)
	rr := httptest.NewRecorder()
	f.loan.Issue(rr, requestWithClient(req, f.unbound))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("sk-real-loan-token")) {
		t.Fatal("unbound client received real token")
	}
}

func TestLoanGroupAllowsBoundClient(t *testing.T) {
	f := newLoanAuthFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/client/loan?service="+f.service+"&group="+f.groupID, nil)
	rr := httptest.NewRecorder()
	f.loan.Issue(rr, requestWithClient(req, f.bound))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		RealToken string `json:"real_token"`
		GroupID   string `json:"group_id"`
		APIKeyID  string `json:"api_key_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.RealToken != "sk-real-loan-token" || resp.GroupID != f.groupID || resp.APIKeyID != f.apiKeyID {
		t.Fatalf("unexpected loan response: %+v", resp)
	}
}

func TestLoanRejectsGitHubAppCredential(t *testing.T) {
	f := newLoanAuthFixture(t)
	cred := `{"type":"github_app","app_id":99,"installation_id":123,"private_key":` + strconvQuote(testRSAPrivateKeyPEM(t)) + `}`
	encCred, err := f.crypto.Encrypt(cred)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE api_keys SET key_encrypted = ? WHERE id = ?`, encCred, f.apiKeyID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/client/loan?service="+f.service, nil)
	rr := httptest.NewRecorder()
	f.loan.Issue(rr, requestWithClient(req, f.bound))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("PRIVATE KEY")) || bytes.Contains(rr.Body.Bytes(), []byte("github_app")) {
		t.Fatalf("loan response leaked github app credential: %s", rr.Body.String())
	}
}

func TestLoanGroupRejectsGitHubAppCredential(t *testing.T) {
	f := newLoanAuthFixture(t)
	cred := `{"type":"github_app","app_id":99,"installation_id":123,"private_key":` + strconvQuote(testRSAPrivateKeyPEM(t)) + `}`
	encCred, err := f.crypto.Encrypt(cred)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE api_keys SET key_encrypted = ? WHERE id = ?`, encCred, f.apiKeyID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/client/loan?service="+f.service+"&group="+f.groupID, nil)
	rr := httptest.NewRecorder()
	f.loan.Issue(rr, requestWithClient(req, f.bound))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("PRIVATE KEY")) || bytes.Contains(rr.Body.Bytes(), []byte("github_app")) {
		t.Fatalf("group loan response leaked github app credential: %s", rr.Body.String())
	}
}

func TestLoanGroupRejectsServiceMismatch(t *testing.T) {
	f := newLoanAuthFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/client/loan?service=anthropic&group="+f.groupID, nil)
	rr := httptest.NewRecorder()
	f.loan.Issue(rr, requestWithClient(req, f.bound))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestLoanRejectsServiceConfiguredForPhantomProxyMode(t *testing.T) {
	f := newLoanAuthFixture(t)
	if _, err := f.db.Exec(`UPDATE services SET delivery_mode = 'proxy' WHERE name = ?`, f.service); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/client/loan?service="+f.service, nil)
	rr := httptest.NewRecorder()
	f.loan.Issue(rr, requestWithClient(req, f.bound))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("sk-real-loan-token")) {
		t.Fatal("proxy-mode service returned real token")
	}
}

func TestMarkExhaustedRequiresGroupBinding(t *testing.T) {
	f := newLoanAuthFixture(t)

	body := bytes.NewBufferString(`{"group_id":"` + f.groupID + `","api_key_id":"` + f.apiKeyID + `","reset_at":"2099-01-01T00:00:00Z"}`)
	req := httptest.NewRequest(http.MethodPost, "/client/loan/exhaust", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.loan.MarkExhausted(rr, requestWithClient(req, f.unbound))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	var exhausted sql.NullString
	if err := f.db.QueryRow(`SELECT exhausted_until FROM key_group_members WHERE group_id = ? AND api_key_id = ?`, f.groupID, f.apiKeyID).Scan(&exhausted); err != nil {
		t.Fatalf("query exhausted_until: %v", err)
	}
	if exhausted.Valid {
		t.Fatalf("unbound client changed exhausted_until to %q", exhausted.String)
	}
}

func TestReportUsageRequiresAPIKeyBinding(t *testing.T) {
	f := newLoanAuthFixture(t)

	body := bytes.NewBufferString(`{"api_key_id":"` + f.apiKeyID + `","headers":{"x-ratelimit-remaining-tokens":"123"}}`)
	req := httptest.NewRequest(http.MethodPost, "/client/usage", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.keyGroups.ReportUsage(rr, requestWithClient(req, f.unbound))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}

	body = bytes.NewBufferString(`{"api_key_id":"` + f.apiKeyID + `","headers":{"x-ratelimit-remaining-tokens":"123"}}`)
	req = httptest.NewRequest(http.MethodPost, "/client/usage", body)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	f.keyGroups.ReportUsage(rr, requestWithClient(req, f.bound))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var snapshot string
	if err := f.db.QueryRow(`SELECT usage_snapshot FROM api_keys WHERE id = ?`, f.apiKeyID).Scan(&snapshot); err != nil {
		t.Fatalf("query usage snapshot: %v", err)
	}
	if !bytes.Contains([]byte(snapshot), []byte(`"tokens_remaining":123`)) {
		t.Fatalf("usage snapshot was not updated from bound client: %s", snapshot)
	}
}

func TestLoanAuditRejectsCrossClientPlaceholderID(t *testing.T) {
	f := newLoanAuthFixture(t)

	body := bytes.NewBufferString(`[{"placeholder_id":"ph-bound-group","service":"` + f.service + `","method":"GET","path":"/v1/test","status":200}]`)
	req := httptest.NewRequest(http.MethodPost, "/client/audit", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.loan.Audit(rr, requestWithClient(req, f.unbound))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}

	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE placeholder_id = 'ph-bound-group'`).Scan(&count); err != nil {
		t.Fatalf("count request log: %v", err)
	}
	if count != 0 {
		t.Fatalf("cross-client audit wrote %d log rows", count)
	}
}

func TestLoanAuditRejectsPlaceholderServiceMismatch(t *testing.T) {
	f := newLoanAuthFixture(t)

	body := bytes.NewBufferString(`[{"placeholder_id":"ph-bound-group","service":"other-service","method":"GET","path":"/v1/test","status":200}]`)
	req := httptest.NewRequest(http.MethodPost, "/client/audit", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.loan.Audit(rr, requestWithClient(req, f.bound))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
