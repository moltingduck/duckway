package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

const (
	aclWithPolicy  = `{"version":"2","provider":"github","repositories":{"o/r":{"capabilities":{"clone":true,"push":true},"branch_policy":{"rules":[{"branches":"**","role":"read"}]}}}}`
	aclCaseVariant = `{"version":"2","provider":"github","repositories":{"o/r":{"capabilities":{"clone":true,"push":true},"Branch_Policy":{"rules":[{"branches":"**","role":"read"}]}}}}`
	aclBuilder     = `{"version":"1","provider":"github","rules":[{"name":"x","deny_all_other":true,"endpoints":[{"method":"GET","path":"/repos/o/r","allow":true}],"github_capabilities":{"o/r":{"contents":"write"}},"GITHUB_BRANCH_POLICIES":{"o/r":{"branch_policy":{"rules":[{"branches":"**","role":"read"}]}}}}]}`
	aclPlain       = `{"version":"2","provider":"github","repositories":{"o/r":{"capabilities":{"clone":true}}}}`
)

func TestServiceDefaultACLRejectsBranchPolicies(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	serviceQ := queries.NewServiceQueries(db)
	h := NewServiceHandler(serviceQ)
	if err := serviceQ.Create(&models.Service{ID: "svc-acl", Name: "acl", UpstreamURL: "https://example.test", HostPattern: "example.test", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]string{"v2": aclWithPolicy, "case variant": aclCaseVariant, "builder": aclBuilder} {
		body, _ := json.Marshal(map[string]string{"default_acl": cfg})
		req := httptest.NewRequest(http.MethodPut, "/api/services/svc-acl", strings.NewReader(string(body)))
		req.SetPathValue("id", "svc-acl")
		rec := httptest.NewRecorder()
		h.Update(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "branch rules are only supported on phantom token / client grants") {
			t.Errorf("update %s: %d %s", name, rec.Code, rec.Body.String())
		}
		cbody, _ := json.Marshal(map[string]string{"name": "n-" + strings.ReplaceAll(name, " ", ""), "upstream_url": "https://example.test", "default_acl": cfg})
		crec := httptest.NewRecorder()
		h.Create(crec, httptest.NewRequest(http.MethodPost, "/api/services", strings.NewReader(string(cbody))))
		if crec.Code != http.StatusBadRequest || !strings.Contains(crec.Body.String(), "branch rules are only supported") {
			t.Errorf("create %s: %d %s", name, crec.Code, crec.Body.String())
		}
	}
	if s, _ := serviceQ.GetByID("svc-acl"); s.DefaultACL != "" {
		t.Fatalf("rejected ACL was stored: %q", s.DefaultACL)
	}
	// A config without a policy is still accepted.
	body, _ := json.Marshal(map[string]string{"default_acl": aclPlain})
	req := httptest.NewRequest(http.MethodPut, "/api/services/svc-acl", strings.NewReader(string(body)))
	req.SetPathValue("id", "svc-acl")
	rec := httptest.NewRecorder()
	h.Update(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain ACL: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyACLRejectsBranchPolicies(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	serviceQ := queries.NewServiceQueries(db)
	keyQ := queries.NewAPIKeyQueries(db)
	if err := serviceQ.Create(&models.Service{ID: "svc-key", Name: "keysvc", UpstreamURL: "https://example.test", HostPattern: "example.test", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted) VALUES ('key-acl','svc-key','k','x')`); err != nil {
		t.Fatal(err)
	}
	h := NewAPIKeyHandler(keyQ, serviceQ, nil)
	set := func(cfg string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"acl": cfg})
		req := httptest.NewRequest(http.MethodPost, "/api/keys/key-acl/acl", strings.NewReader(string(body)))
		req.SetPathValue("id", "key-acl")
		rec := httptest.NewRecorder()
		h.SetACL(rec, req)
		return rec
	}
	for name, cfg := range map[string]string{"v2": aclWithPolicy, "case variant": aclCaseVariant, "builder": aclBuilder} {
		if rec := set(cfg); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "branch rules are only supported on phantom token / client grants") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	var stored string
	if err := db.QueryRow(`SELECT COALESCE(acl,'') FROM api_keys WHERE id='key-acl'`).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("rejected ACL was stored: %q %v", stored, err)
	}
	if rec := set(aclPlain); rec.Code != http.StatusOK {
		t.Fatalf("plain ACL: %d %s", rec.Code, rec.Body.String())
	}
}
