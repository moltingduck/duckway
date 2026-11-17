package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
	"github.com/hackerduck/duckway/web"
)

type approverEnv struct {
	s      *Server
	ss     *SharedServices
	db     *sql.DB
	client string // a valid client token
}

// newApproverEnv builds a full (admin + gateway) server with one client, one
// phantom token and no approvals.
func newApproverEnv(t *testing.T) *approverEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &Server{
		config: &Config{DataDir: dir, EncryptionKey: bytes.Repeat([]byte{1}, 32), SessionSecret: bytes.Repeat([]byte{2}, 32)},
		db:     db, mux: http.NewServeMux(), stopCh: make(chan struct{}),
	}
	ss := s.initShared()
	s.SetupAdminRoutes(web.Content, ss)
	s.SetupGatewayRoutes(ss)
	t.Cleanup(s.Shutdown)
	for _, q := range []string{
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('s1','svc','Svc','https://e.example','e.example')`,
		`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k1','s1','prod-key','enc')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	const clientToken = "client-secret-token"
	if err := queries.NewClientQueries(db).Create(&models.Client{ID: "c1", ShortID: "c1", Name: "build-agent", TokenHash: services.HashToken(clientToken)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, approval_ttl_minutes) VALUES ('p1','OPENAI_API_KEY','ph-1','s1','k1','c1', 90)`); err != nil {
		t.Fatal(err)
	}
	return &approverEnv{s: s, ss: ss, db: db, client: clientToken}
}

func (e *approverEnv) addApproval(t *testing.T, id string) {
	t.Helper()
	info, _ := json.Marshal(map[string]string{"method": "POST", "path": "/v1/" + id, "query": "a=1", "rule": "approve:" + id})
	if _, err := e.db.Exec(`INSERT INTO approvals (id, placeholder_id, status, request_info, scope) VALUES (?, 'p1', 'pending', ?, ?)`, id, string(info), "rule:"+id); err != nil {
		t.Fatal(err)
	}
}

func (e *approverEnv) admin(user, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://admin.example"+path, strings.NewReader(body))
	req.AddCookie(e.ss.AdminAuth.CreateSession(user, req))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	testSameOrigin(req)
	rec := httptest.NewRecorder()
	e.s.ServeHTTP(rec, req)
	return rec
}

func (e *approverEnv) call(headers map[string]string, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://admin.example"+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.s.ServeHTTP(rec, req)
	return rec
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func (e *approverEnv) newToken(t *testing.T, name string) (id, token string) {
	t.Helper()
	rec := e.admin("alice", "POST", "/api/approvers", `{"name":"`+name+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create approver: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out["id"], out["token"]
}

func TestApproverTokenAdminAPIShowsTokenOnceAndStoresOnlyHash(t *testing.T) {
	e := newApproverEnv(t)
	id, token := e.newToken(t, "alice laptop")
	if !strings.HasPrefix(token, "dwa_") || len(token) != len("dwa_")+64 {
		t.Fatalf("token shape: %q", token)
	}
	rec := e.admin("alice", "GET", "/api/approvers", "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), "token_hash") {
		t.Fatalf("list must never expose the token or hash: %d %s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["id"] != id || list[0]["name"] != "alice laptop" || list[0]["revoked_at"] != nil {
		t.Fatalf("list: %v", list)
	}
	var stored string
	if err := e.db.QueryRow(`SELECT token_hash FROM approver_tokens WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token || stored != services.HashToken(token) {
		t.Fatalf("only the SHA-256 may be stored, got %q", stored)
	}
	// Validation and uniqueness.
	if rec := e.admin("alice", "POST", "/api/approvers", `{"name":"bad:name"}`); rec.Code != 400 {
		t.Fatalf("invalid name: %d", rec.Code)
	}
	if rec := e.admin("alice", "POST", "/api/approvers", `{"name":"alice laptop"}`); rec.Code != 409 {
		t.Fatalf("duplicate name: %d", rec.Code)
	}
	// Admin-only: anonymous is refused.
	if rec := e.call(nil, "GET", "/api/approvers", ""); rec.Code != 401 {
		t.Fatalf("anonymous list: %d", rec.Code)
	}
	if rec := e.call(nil, "POST", "/api/approvers", `{"name":"x"}`); rec.Code != 401 {
		t.Fatalf("anonymous create: %d", rec.Code)
	}
}

func TestApproverRevokeStopsAccessImmediately(t *testing.T) {
	e := newApproverEnv(t)
	id, token := e.newToken(t, "ci")
	if rec := e.call(bearer(token), "GET", "/approver/approvals", ""); rec.Code != 200 {
		t.Fatalf("before revoke: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.admin("alice", "DELETE", "/api/approvers/"+id, ""); rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.call(bearer(token), "GET", "/approver/approvals", ""); rec.Code != 401 {
		t.Fatalf("after revoke: %d", rec.Code)
	}
	if rec := e.admin("alice", "DELETE", "/api/approvers/nope", ""); rec.Code != 404 {
		t.Fatalf("unknown id: %d", rec.Code)
	}
	var rev sql.NullString
	_ = e.db.QueryRow(`SELECT revoked_at FROM approver_tokens WHERE id = ?`, id).Scan(&rev)
	if !rev.Valid {
		t.Fatal("revoked_at not set")
	}
}

func TestApproverTokensAndClientTokensAreIsolated(t *testing.T) {
	e := newApproverEnv(t)
	_, approver := e.newToken(t, "ci")

	// A client token (every way a client presents one) never authenticates /approver/.
	for name, h := range map[string]map[string]string{
		"client bearer": bearer(e.client), "client header": {"X-Duckway-Token": e.client}, "none": nil,
	} {
		for _, p := range []string{"/approver/approvals", "/approver/approvals/x", "/approver/events"} {
			if rec := e.call(h, "GET", p, ""); rec.Code != 401 {
				t.Errorf("%s on %s: %d, want 401", name, p, rec.Code)
			}
		}
		for _, p := range []string{"approve", "reject"} {
			if rec := e.call(h, "POST", "/approver/approvals/x/"+p, `{}`); rec.Code != 401 {
				t.Errorf("%s %s: %d, want 401", name, p, rec.Code)
			}
		}
	}
	// An admin session cookie does not authenticate /approver/ either.
	req := httptest.NewRequest("GET", "http://admin.example/approver/approvals", nil)
	req.AddCookie(e.ss.AdminAuth.CreateSession("alice", req))
	rec := httptest.NewRecorder()
	e.s.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("admin cookie on /approver/: %d", rec.Code)
	}
	// An approver token authenticates nothing else.
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/approvals/list"}, {"GET", "/api/approvers"}, {"POST", "/api/approvals/x/approve"}, {"GET", "/api/events"},
		{"GET", "/client/keys"}, {"GET", "/client/sync"}, {"GET", "/client/plugins"}, {"POST", "/client/audit"},
		{"GET", "/proxy/v1/models"}, {"POST", "/proxy/v1/chat"},
	} {
		for hname, h := range map[string]map[string]string{"bearer": bearer(approver), "x-token": {"X-Duckway-Token": approver}} {
			if rec := e.call(h, c.method, c.path, ""); rec.Code != 401 {
				t.Errorf("approver token (%s) on %s %s: %d, want 401", hname, c.method, c.path, rec.Code)
			}
		}
	}
	// And the client token still works where it should (sanity: isolation is not blanket denial).
	if rec := e.call(bearer(e.client), "GET", "/client/services", ""); rec.Code != 200 {
		t.Errorf("client token on /client/services: %d", rec.Code)
	}
}

func TestApproverAPIExposesOnlyApprovalFields(t *testing.T) {
	e := newApproverEnv(t)
	e.addApproval(t, "ap1")
	_, token := e.newToken(t, "ci")

	rec := e.call(bearer(token), "GET", "/approver/approvals?status=pending", "")
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	allowed := map[string]bool{"id": true, "status": true, "client_name": true, "service": true, "method": true, "path": true, "query": true,
		"body_hash": true, "rule": true, "held_reason": true, "created_at": true, "approved_at": true, "expires_at": true,
		"default_minutes": true, "decided_by": true, "decided_via": true}
	for k := range list[0] {
		if !allowed[k] {
			t.Errorf("unexpected field %q in the approver API", k)
		}
	}
	a := list[0]
	if a["client_name"] != "build-agent" || a["service"] != "svc" || a["method"] != "POST" || a["path"] != "/v1/ap1" ||
		a["query"] != "a=1" || a["default_minutes"] != float64(90) || !strings.Contains(a["held_reason"].(string), "approve:ap1") {
		t.Fatalf("fields: %v", a)
	}
	body := rec.Body.String()
	for _, secret := range []string{"prod-key", "OPENAI_API_KEY", "ph-1", "enc", "token_hash", "acl", "permission_config"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q: %s", secret, body)
		}
	}
	one := e.call(bearer(token), "GET", "/approver/approvals/ap1", "")
	if one.Code != 200 || !strings.Contains(one.Body.String(), `"id":"ap1"`) {
		t.Fatalf("get: %d %s", one.Code, one.Body.String())
	}
	if rec := e.call(bearer(token), "GET", "/approver/approvals/missing", ""); rec.Code != 404 {
		t.Fatalf("missing: %d", rec.Code)
	}
	if rec := e.call(bearer(token), "GET", "/approver/approvals?status=bogus", ""); rec.Code != 400 {
		t.Fatalf("bad status: %d", rec.Code)
	}
}

func TestApproverDecisionsAreAttributedAndRaceSafe(t *testing.T) {
	e := newApproverEnv(t)
	var hooks []string
	var mu sync.Mutex
	e.s.OnDecision = func(_ context.Context, id, by, via, status string) {
		mu.Lock()
		defer mu.Unlock()
		hooks = append(hooks, id+"|"+by+"|"+via+"|"+status)
	}
	e.addApproval(t, "ap1")
	e.addApproval(t, "ap2")
	_, token := e.newToken(t, "ci pipeline")

	rec := e.call(tuiClient(token), "POST", "/approver/approvals/ap1/approve", `{"duration_minutes":30}`)
	if rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	var okBody map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &okBody)
	if okBody["decided_by"] != "approver:ci pipeline" || okBody["decided_via"] != "tui" || okBody["status"] != "approved" {
		t.Fatalf("body: %v", okBody)
	}
	// A second decision (any surface) is a 409 naming the existing decider.
	for _, rec := range []*httptest.ResponseRecorder{
		e.call(tuiClient(token), "POST", "/approver/approvals/ap1/reject", ``),
		e.call(tuiClient(token), "POST", "/approver/approvals/ap1/approve", `{}`),
		e.admin("bob", "POST", "/api/approvals/ap1/approve", `{}`),
		e.admin("bob", "POST", "/api/approvals/ap1/reject", ``),
	} {
		var b map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		if rec.Code != 409 || b["decided_by"] != "approver:ci pipeline" || b["decided_via"] != "tui" || b["status"] != "approved" {
			t.Fatalf("conflict: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := e.call(tuiClient(token), "POST", "/approver/approvals/missing/approve", `{}`); rec.Code != 404 {
		t.Fatalf("unknown approval: %d", rec.Code)
	}
	if rec := e.call(tuiClient(token), "POST", "/approver/approvals/ap2/reject", ``); rec.Code != 200 {
		t.Fatalf("reject: %d", rec.Code)
	}
	var by, via string
	_ = e.db.QueryRow(`SELECT decided_by, decided_via FROM approvals WHERE id='ap2'`).Scan(&by, &via)
	if by != "approver:ci pipeline" || via != "tui" {
		t.Fatalf("stored: %q %q", by, via)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hooks) != 2 || hooks[0] != "ap1|approver:ci pipeline|tui|approved" || hooks[1] != "ap2|approver:ci pipeline|tui|rejected" {
		t.Fatalf("OnDecision hook: %v", hooks)
	}
}

func TestWebDecisionPathsRecordAdminAndConcurrentDecisionGets409(t *testing.T) {
	e := newApproverEnv(t)
	for _, id := range []string{"w1", "w2", "w3", "race"} {
		e.addApproval(t, id)
	}
	if rec := e.admin("alice", "POST", "/api/approvals/w1/approve", `{}`); rec.Code != 200 {
		t.Fatalf("api approve: %d", rec.Code)
	}
	if rec := e.admin("alice", "POST", "/api/approvals/w2/reject", ``); rec.Code != 200 {
		t.Fatalf("api reject: %d", rec.Code)
	}
	if rec := e.admin("carol", "POST", "/admin/approvals/w3/approve", ``); rec.Code != http.StatusSeeOther {
		t.Fatalf("form approve: %d", rec.Code)
	}
	check := func(id, status, by string) {
		t.Helper()
		var st, b, via string
		if err := e.db.QueryRow(`SELECT status, decided_by, decided_via FROM approvals WHERE id=?`, id).Scan(&st, &b, &via); err != nil {
			t.Fatal(err)
		}
		if st != status || b != by || via != "web" {
			t.Fatalf("%s: %s %s %s", id, st, b, via)
		}
	}
	check("w1", "approved", "admin:alice")
	check("w2", "rejected", "admin:alice")
	check("w3", "approved", "admin:carol")

	// The form route sends a lost race back with a notice.
	rec := e.admin("dave", "POST", "/admin/approvals/w3/reject", ``)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "notice=already-decided") {
		t.Fatalf("form conflict: %d %v", rec.Code, rec.Header())
	}
	// Concurrent decisions: exactly one 200, the rest 409 naming that decider.
	var ok, conflict atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			verb := "approve"
			if i%2 == 1 {
				verb = "reject"
			}
			rec := e.admin("user"+string(rune('a'+i)), "POST", "/api/approvals/race/"+verb, `{}`)
			switch rec.Code {
			case 200:
				ok.Add(1)
			case 409:
				var b map[string]string
				_ = json.Unmarshal(rec.Body.Bytes(), &b)
				if !strings.HasPrefix(b["decided_by"], "admin:user") || b["decided_via"] != "web" {
					t.Errorf("conflict body: %s", rec.Body.String())
				}
				conflict.Add(1)
			default:
				t.Errorf("status %d: %s", rec.Code, rec.Body.String())
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 1 || conflict.Load() != 5 {
		t.Fatalf("ok=%d conflict=%d", ok.Load(), conflict.Load())
	}
	// The history API carries the decider.
	list := e.admin("alice", "GET", "/api/approvals/list?status=approved,rejected", "")
	if !strings.Contains(list.Body.String(), `"decided_by":"admin:alice"`) || !strings.Contains(list.Body.String(), `"decided_via":"web"`) {
		t.Fatalf("history missing decider: %s", list.Body.String())
	}
}

func TestSweeperExpiryIsAttributedAndReported(t *testing.T) {
	e := newApproverEnv(t)
	var hooks atomic.Int32
	e.s.OnDecision = func(_ context.Context, id, by, via, status string) {
		if id == "old" && by == "system:sweeper" && via == "sweeper" && status == "ignored" {
			hooks.Add(1)
		}
	}
	e.addApproval(t, "old")
	_, _ = e.db.Exec(`UPDATE approvals SET created_at = datetime('now','-5 hours') WHERE id='old'`)
	n, err := e.ss.Decider.Expire(context.Background(), 60)
	if err != nil || n != 1 || hooks.Load() != 1 {
		t.Fatalf("expire: %d %v hooks=%d", n, err, hooks.Load())
	}
	var st, by, via string
	_ = e.db.QueryRow(`SELECT status, decided_by, decided_via FROM approvals WHERE id='old'`).Scan(&st, &by, &via)
	if st != "ignored" || by != "system:sweeper" || via != "sweeper" {
		t.Fatalf("stored: %s %s %s", st, by, via)
	}
}

func TestApproverEventsStreamCarriesOnlyApprovalChanges(t *testing.T) {
	e := newApproverEnv(t)
	_, token := e.newToken(t, "tui")
	ts := httptest.NewServer(e.s)
	t.Cleanup(ts.Close)

	// No token / a client token: no stream.
	for _, h := range []map[string]string{nil, bearer(e.client)} {
		rec := e.call(h, "GET", "/approver/events", "")
		if rec.Code != 401 {
			t.Fatalf("unauthenticated stream: %d", rec.Code)
		}
	}
	req, _ := http.NewRequest("GET", ts.URL+"/approver/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	wait := func(want string, d time.Duration) {
		t.Helper()
		timeout := time.After(d)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream closed waiting for %q", want)
				}
				if l == want {
					return
				}
			case <-timeout:
				t.Fatalf("timeout waiting for %q", want)
			}
		}
	}
	wait("event: hello", 3*time.Second)
	time.Sleep(300 * time.Millisecond) // watcher baseline
	e.addApproval(t, "live1")
	// A request-log change would normally publish the "logs" topic: it must not reach approvers.
	_, _ = e.db.Exec(`INSERT INTO request_log (placeholder_id, client_id, service_name, method, path) VALUES ('p1','c1','svc','GET','/x')`)
	wait("event: approvals", 6*time.Second)
	time.Sleep(300 * time.Millisecond)
	var rest []string
	for drained := false; !drained; {
		select {
		case l := <-lines:
			rest = append(rest, l)
		default:
			drained = true
		}
	}
	for _, l := range rest {
		if l == "event: logs" || l == "event: badge" || l == "event: rollouts" {
			t.Fatalf("approver stream leaked %q", l)
		}
	}
}

// tuiClient is bearer(token) plus the header ducklord's approvals panel sends.
func tuiClient(token string) map[string]string {
	h := bearer(token)
	h["X-Duckway-Approver-Client"] = "ducklord-tui"
	return h
}

// decided_via is "tui" only when the client says so; any other holder of an
// approver token is "api".
func TestApproverDecidedViaTUIOnlyWhenClientSaysSo(t *testing.T) {
	e := newApproverEnv(t)
	e.addApproval(t, "v1")
	e.addApproval(t, "v2")
	_, token := e.newToken(t, "script")
	if rec := e.call(bearer(token), "POST", "/approver/approvals/v1/approve", `{}`); rec.Code != 200 {
		t.Fatalf("approve: %d", rec.Code)
	}
	if rec := e.call(tuiClient(token), "POST", "/approver/approvals/v2/reject", ``); rec.Code != 200 {
		t.Fatalf("reject: %d", rec.Code)
	}
	for id, want := range map[string]string{"v1": "api", "v2": "tui"} {
		var via string
		_ = e.db.QueryRow(`SELECT decided_via FROM approvals WHERE id=?`, id).Scan(&via)
		if via != want {
			t.Errorf("%s decided_via = %q, want %q", id, via, want)
		}
	}
}
