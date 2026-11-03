package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database/queries"
)

type draftEnv struct {
	t      *testing.T
	s      *Server
	shared *SharedServices
}

func (e *draftEnv) do(method, path, body string, admin bool, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd *bytes.Buffer
	if body != "" {
		rd = bytes.NewBufferString(body)
	} else {
		rd = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if admin {
		req.AddCookie(e.shared.AdminAuth.CreateSession("duckway", req))
	}
	rec := httptest.NewRecorder()
	e.s.ServeHTTP(rec, req)
	return rec
}

func newDraftEnv(t *testing.T) *draftEnv {
	s, shared := newShellTestServer(t)
	s.SetupGatewayRoutes(shared)
	return &draftEnv{t: t, s: s, shared: shared}
}

const heartbeatBody = `{"protocol_version":1,"version":"v1","os":"linux","arch":"amd64","boot_id":"boot","install_path":"/tmp/duckway","install_writable":true,"capabilities":["managed_update_v1"],"components":{"proxy":"running"}}`

func TestClientDraftRoutesRequireAdminAuth(t *testing.T) {
	e := newDraftEnv(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/client-drafts"},
		{http.MethodPost, "/api/client-drafts"},
		{http.MethodGet, "/api/client-drafts/x"},
		{http.MethodPatch, "/api/client-drafts/x"},
		{http.MethodPost, "/api/client-drafts/x/issue-token"},
		{http.MethodDelete, "/api/client-drafts/x"},
	} {
		rec := e.do(tc.method, tc.path, `{"name":"x"}`, false, nil)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
			t.Errorf("%s %s anonymous status %d, want auth failure", tc.method, tc.path, rec.Code)
		}
	}
}

func TestClientDraftCannotAuthenticateUntilTokenIssued(t *testing.T) {
	e := newDraftEnv(t)
	rec := e.do(http.MethodPost, "/api/client-drafts", `{"name":"build-runner-02","update_policy":"manual"}`, true, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("draft creation must not expose a token: %s", rec.Body.String())
	}
	var d struct {
		ID, Name, UpdatePolicy string
	}
	var raw map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	d.ID, _ = raw["id"].(string)
	if d.ID == "" || raw["name"] != "build-runner-02" || raw["update_policy"] != "manual" || raw["step"].(float64) != 1 {
		t.Fatalf("unexpected draft: %v", raw)
	}

	// A draft is not a usable client: not listed, no token matches it, and
	// even a leaked stored hash or activating the row cannot authenticate.
	cq := queries.NewClientQueries(e.s.db)
	list, _ := cq.List()
	for _, c := range list {
		if c.ID == d.ID {
			t.Fatal("draft appeared in the client list")
		}
	}
	var hash string
	if err := e.s.db.QueryRow(`SELECT token_hash FROM clients WHERE id=?`, d.ID).Scan(&hash); err != nil || hash == "" {
		t.Fatalf("draft token hash: %q %v", hash, err)
	}
	if _, err := cq.GetByTokenHash(hash); err == nil {
		t.Fatal("draft authenticated by stored hash")
	}
	if _, err := e.s.db.Exec(`UPDATE clients SET is_active=1 WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cq.GetByTokenHash(hash); err == nil {
		t.Fatal("activated draft authenticated by stored hash")
	}
	if rec := e.do(http.MethodPost, "/client/control/heartbeat", heartbeatBody, false, map[string]string{"X-Duckway-Token": "guess"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("heartbeat with guessed token: %d", rec.Code)
	}
	if _, err := e.s.db.Exec(`UPDATE clients SET is_active=0 WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}

	// Save progress, then issue the token (the Install step).
	if rec := e.do(http.MethodPatch, "/api/client-drafts/"+d.ID, `{"step":3,"suite_id":""}`, true, nil); rec.Code != http.StatusOK {
		t.Fatalf("patch draft: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(http.MethodPost, "/api/client-drafts/"+d.ID+"/issue-token", "", true, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("issue token: %d %s", rec.Code, rec.Body.String())
	}
	var issued map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &issued)
	token := issued["token"]
	if len(token) < 32 {
		t.Fatalf("token not returned: %v", issued)
	}
	if rec := e.do(http.MethodPost, "/client/control/heartbeat", heartbeatBody, false, map[string]string{"X-Duckway-Token": token}); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat with issued token: %d %s", rec.Code, rec.Body.String())
	}
	// One-time: the draft is gone, a second issue is refused, and the client is now listed.
	if rec := e.do(http.MethodPost, "/api/client-drafts/"+d.ID+"/issue-token", "", true, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second issue status %d, want 404", rec.Code)
	}
	list, _ = cq.List()
	found := false
	for _, c := range list {
		if c.ID == d.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("finished client missing from list")
	}
}

func TestClientDraftValidationAndDiscard(t *testing.T) {
	e := newDraftEnv(t)
	for _, body := range []string{`{"name":""}`, `{"name":"Bad Name"}`, `{"name":"x'; DROP TABLE clients;--"}`, `{"name":"ok","update_policy":"weird"}`} {
		if rec := e.do(http.MethodPost, "/api/client-drafts", body, true, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, rec.Code)
		}
	}
	if rec := e.do(http.MethodPost, "/api/client-drafts", `{"name":"dup"}`, true, nil); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/client-drafts", `{"name":"dup"}`, true, nil); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name status %d, want 409", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/client-drafts", `{"name":"x"}`, true, map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin create status %d, want 403", rec.Code)
	}
	rec := e.do(http.MethodGet, "/api/client-drafts", "", true, nil)
	var list []map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("drafts: %s", rec.Body.String())
	}
	id := list[0]["id"].(string)
	if rec := e.do(http.MethodPatch, "/api/client-drafts/"+id, `{"step":9}`, true, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad step status %d", rec.Code)
	}
	// A normal (finished) client is not a draft.
	if _, err := e.s.db.Exec(`INSERT INTO clients (id, short_id, name, token_hash) VALUES ('real','r1','real','h')`); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(http.MethodDelete, "/api/client-drafts/real", "", true, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("discarding a finished client via drafts API: %d, want 404", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/client-drafts/real/issue-token", "", true, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("issuing token for finished client: %d, want 404", rec.Code)
	}
	if rec := e.do(http.MethodDelete, "/api/client-drafts/"+id, "", true, nil); rec.Code != http.StatusOK {
		t.Fatalf("discard: %d %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = e.s.db.QueryRow(`SELECT COUNT(*) FROM clients WHERE id=?`, id).Scan(&n)
	if n != 0 {
		t.Fatal("discarded draft client row still present")
	}
	_ = e.s.db.QueryRow(`SELECT COUNT(*) FROM client_drafts`).Scan(&n)
	if n != 0 {
		t.Fatal("draft marker rows remain")
	}
}

func TestClientDraftExcludedFromRollout(t *testing.T) {
	e := newDraftEnv(t)
	rec := e.do(http.MethodPost, "/api/client-drafts", `{"name":"draft-only"}`, true, nil)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	rq := queries.NewClientUpdateQueries(e.s.db)
	ro, err := rq.CreateRollout("v2", "{}", 1, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := rq.ListJobs(ro.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("draft client got %d rollout jobs", len(jobs))
	}
}

func createDraftID(t *testing.T, e *draftEnv, name string) string {
	t.Helper()
	rec := e.do(http.MethodPost, "/api/client-drafts", `{"name":"`+name+`"}`, true, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft: %d %s", rec.Code, rec.Body.String())
	}
	var raw map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	return raw["id"].(string)
}

func TestClientDraftDiscardAfterIssueKeepsClient(t *testing.T) {
	e := newDraftEnv(t)
	id := createDraftID(t, e, "issued-first")
	if rec := e.do(http.MethodPost, "/api/client-drafts/"+id+"/issue-token", "", true, nil); rec.Code != http.StatusOK {
		t.Fatalf("issue: %d", rec.Code)
	}
	if rec := e.do(http.MethodDelete, "/api/client-drafts/"+id, "", true, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("discard after issue: %d, want 404", rec.Code)
	}
	var n int
	_ = e.s.db.QueryRow(`SELECT COUNT(*) FROM clients WHERE id=?`, id).Scan(&n)
	if n != 1 {
		t.Fatal("issued client was deleted by draft discard")
	}
	// Queries-level: Discard requires exactly one marker row.
	if err := queries.NewClientDraftQueries(e.s.db).Discard(id); err != queries.ErrNotDraft {
		t.Fatalf("Discard of finished client: %v, want ErrNotDraft", err)
	}
}

func TestClientDraftRejectedByClientEndpointsAndFacets(t *testing.T) {
	e := newDraftEnv(t)
	id := createDraftID(t, e, "hidden-draft")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/clients/" + id, `{"is_active":true}`},
		{http.MethodGet, "/api/clients/" + id, ""},
		{http.MethodPost, "/api/clients/" + id + "/rotate-token", ""},
		{http.MethodGet, "/admin/clients/" + id, ""},
		{http.MethodGet, "/api/clients/" + id + "/impact", ""},
	} {
		if rec := e.do(tc.method, tc.path, tc.body, true, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
	var active int
	_ = e.s.db.QueryRow(`SELECT is_active FROM clients WHERE id=?`, id).Scan(&active)
	if active != 0 {
		t.Fatal("PUT activated a draft")
	}
	rec := e.do(http.MethodGet, "/api/logs/facets", "", true, nil)
	if strings.Contains(rec.Body.String(), "hidden-draft") {
		t.Fatalf("draft leaked into log facets: %s", rec.Body.String())
	}
}

func TestClientDraftNameUniqueMapsTo409(t *testing.T) {
	e := newDraftEnv(t)
	createDraftID(t, e, "first-one")
	id2 := createDraftID(t, e, "second-one")
	if rec := e.do(http.MethodPatch, "/api/client-drafts/"+id2, `{"name":"first-one"}`, true, nil); rec.Code != http.StatusConflict {
		t.Fatalf("rename to taken name: %d, want 409", rec.Code)
	}
	_, err := e.s.db.Exec(`UPDATE clients SET name='first-one' WHERE id=?`, id2)
	if !queries.IsUniqueViolation(err) {
		t.Fatalf("IsUniqueViolation(%v) = false", err)
	}
}

func TestClientPutIsPatchStyle(t *testing.T) {
	e := newDraftEnv(t)
	if _, err := e.s.db.Exec(`INSERT INTO clients (id, short_id, name, token_hash) VALUES ('pc','p1','patchy','h')`); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(http.MethodPut, "/api/clients/pc", `{"is_active":false}`, true, nil); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d", rec.Code)
	}
	// A rename that does not mention is_active must not re-enable the client.
	if rec := e.do(http.MethodPut, "/api/clients/pc", `{"name":"patchy-2"}`, true, nil); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d", rec.Code)
	}
	var name string
	var active, policyManual int
	_ = e.s.db.QueryRow(`SELECT name, is_active, update_policy='manual' FROM clients WHERE id='pc'`).Scan(&name, &active, &policyManual)
	if name != "patchy-2" || active != 0 {
		t.Fatalf("name=%q active=%d; rename reverted disable", name, active)
	}
	// Stale-read simulation: a handler that read before the disable must not undo it.
	cq := queries.NewClientQueries(e.s.db)
	n := "patchy-3"
	if err := cq.Patch("pc", &n, nil, nil); err != nil {
		t.Fatal(err)
	}
	_ = e.s.db.QueryRow(`SELECT is_active FROM clients WHERE id='pc'`).Scan(&active)
	if active != 0 {
		t.Fatal("Patch of name touched is_active")
	}
	if rec := e.do(http.MethodPut, "/api/clients/pc", `{"is_active":true}`, true, nil); rec.Code != http.StatusOK {
		t.Fatalf("enable: %d", rec.Code)
	}
	_ = e.s.db.QueryRow(`SELECT name FROM clients WHERE id='pc'`).Scan(&name)
	if name != "patchy-3" {
		t.Fatalf("enable reverted name: %q", name)
	}
}
