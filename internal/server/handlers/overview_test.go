package handlers_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
)

const xssName = `<img src=x onerror=alert(1)>"'&`

func overviewDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ovExec(t *testing.T, db *sql.DB, q string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func getOverview(t *testing.T, db *sql.DB) handlers.OverviewResponse {
	t.Helper()
	h := handlers.NewOverviewHandler(queries.NewOverviewQueries(db), queries.NewSettingsQueries(db))
	w := httptest.NewRecorder()
	h.Get(w, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
	var r handlers.OverviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func kinds(r handlers.OverviewResponse) map[string][]handlers.OverviewItem {
	m := map[string][]handlers.OverviewItem{}
	for _, it := range r.Attention.Items {
		m[it.Kind] = append(m[it.Kind], it)
	}
	return m
}

func ts(d time.Duration) string { return time.Now().UTC().Add(d).Format(queries.OverviewTimeFormat) }

func TestOverviewEmptyState(t *testing.T) {
	db := overviewDB(t)
	r := getOverview(t, db)
	if r.Attention.Count != 0 || r.Attention.Items == nil || len(r.Attention.Items) != 0 {
		t.Fatalf("expected empty non-nil items, got %+v", r.Attention)
	}
	if r.Stats.Clients.Total != 0 || r.Stats.Requests24h != 0 {
		t.Fatalf("stats %+v", r.Stats)
	}
	if len(r.Posture) < 3 {
		t.Fatalf("posture %+v", r.Posture)
	}
}

func TestOverviewAttentionKinds(t *testing.T) {
	db := overviewDB(t)
	ovExec(t, db, `INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('s1', ?, 'S', 'https://e.com', 'e.com')`, "svc<b>")
	// clients: one stale (XSS name), one fresh/online, one never seen but new
	ovExec(t, db, `INSERT INTO clients (id, name, token_hash, last_seen_at, created_at) VALUES ('c1', ?, 'h1', ?, ?)`, xssName, ts(-48*time.Hour), ts(-72*time.Hour))
	ovExec(t, db, `INSERT INTO clients (id, name, token_hash, last_seen_at) VALUES ('c2', 'fresh', 'h2', ?)`, ts(-time.Minute))
	ovExec(t, db, `INSERT INTO clients (id, name, token_hash) VALUES ('c3', 'brandnew', 'h3')`)
	ovExec(t, db, `INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k1', 's1', 'plain', 'x')`)
	ovExec(t, db, `INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id) VALUES ('p1', 'E', 'ph', 's1', 'k1', 'c1')`)
	ovExec(t, db, `INSERT INTO approvals (id, placeholder_id, status, created_at) VALUES ('a1', 'p1', 'pending', ?)`, ts(-10*time.Minute))
	ovExec(t, db, `INSERT INTO approvals (id, placeholder_id, status, created_at) VALUES ('a2', 'p1', 'pending', ?)`, ts(-2*time.Minute))
	ovExec(t, db, `INSERT INTO approvals (id, placeholder_id, status) VALUES ('a3', 'p1', 'approved')`)
	// credentials: expiring (non-refreshable), overdue refreshable, deactivated refreshable, healthy refreshable
	ovExec(t, db, `INSERT INTO api_keys (id, service_id, name, key_encrypted, expires_at) VALUES ('k2', 's1', 'expiring', 'x', ?)`, time.Now().Add(24*time.Hour).UnixMilli())
	ovExec(t, db, `INSERT INTO api_keys (id, service_id, name, key_encrypted, expires_at, refresh_token) VALUES ('k3', 's1', 'overdue', 'x', ?, 'r')`, time.Now().Add(-2*time.Hour).UnixMilli())
	ovExec(t, db, `INSERT INTO api_keys (id, service_id, name, key_encrypted, refresh_token, is_active) VALUES ('k4', 's1', 'dead', 'x', 'r', 0)`)
	ovExec(t, db, `INSERT INTO api_keys (id, service_id, name, key_encrypted, expires_at, refresh_token) VALUES ('k5', 's1', 'healthy', 'x', ?, 'r')`, time.Now().Add(time.Hour).UnixMilli())
	ovExec(t, db, `INSERT INTO api_keys (id, service_id, name, key_encrypted, expires_at) VALUES ('k6', 's1', 'farfuture', 'x', ?)`, time.Now().Add(90*24*time.Hour).UnixMilli())
	// rollout paused with a failure
	ovExec(t, db, `INSERT INTO client_update_rollouts (id, target_version, status, max_concurrency, start_interval_seconds) VALUES ('r1', 'v9.9', 'paused', 1, 1)`)
	ovExec(t, db, `INSERT INTO client_update_jobs (id, rollout_id, client_id, status) VALUES ('j1', 'r1', 'c1', 'failed')`)
	ovExec(t, db, `INSERT INTO client_update_jobs (id, rollout_id, client_id, status) VALUES ('j2', 'r1', 'c2', 'queued')`)
	// canary without recipient
	ovExec(t, db, `INSERT INTO canary_tokens (id, client_id, token_type, canary_token, auth_token, token_value, memo, deploy_path, deploy_content) VALUES ('t1', 'c2', 'aws', 'a', 'b', 'c', 'm', '/p', 'x')`)
	// request log
	ovExec(t, db, `INSERT INTO request_log (service_name, method, path, status_code) VALUES ('s', 'GET', '/', 200)`)
	ovExec(t, db, `INSERT INTO request_log (service_name, method, path, status_code, created_at) VALUES ('s', 'GET', '/old', 200, ?)`, ts(-30*time.Hour))

	r := getOverview(t, db)
	k := kinds(r)
	ap := k["approvals"]
	if len(ap) != 1 || ap[0].Count != 2 || !strings.Contains(ap[0].Meta, "svc<b>") || !strings.Contains(ap[0].Meta, xssName) || !strings.Contains(ap[0].Meta, "expires in") || ap[0].Href != "/admin/approvals" {
		t.Fatalf("approvals item: %+v", ap)
	}
	if len(k["credentials"]) != 3 {
		t.Fatalf("credential items: %+v", k["credentials"])
	}
	for _, it := range k["credentials"] {
		if len(it.Names) != 1 || it.Count != 1 {
			t.Fatalf("cred item %+v", it)
		}
	}
	cl := k["clients"]
	if len(cl) != 1 || cl[0].Count != 1 || cl[0].Names[0] != xssName || !strings.Contains(cl[0].Meta, xssName) {
		t.Fatalf("clients item: %+v", cl)
	}
	ro := k["rollout"]
	if len(ro) != 1 || ro[0].Severity != "critical" || !strings.Contains(ro[0].Meta, "paused") || !strings.Contains(ro[0].Meta, "1 of 2 jobs failed") {
		t.Fatalf("rollout item: %+v", ro)
	}
	if len(k["canary"]) != 1 || k["canary"][0].Href != "/admin/canary" {
		t.Fatalf("canary item: %+v", k["canary"])
	}
	if r.Attention.Count != len(r.Attention.Items) || r.Attention.Count != 7 {
		t.Fatalf("count %d items %d", r.Attention.Count, len(r.Attention.Items))
	}
	if r.Stats.Clients.Total != 3 || r.Stats.Clients.Online != 1 || r.Stats.Credentials.Total != 6 || r.Stats.Credentials.Expiring != 3 ||
		r.Stats.PhantomTokens != 1 || r.Stats.Requests24h != 1 {
		t.Fatalf("stats %+v", r.Stats)
	}

	// Recipient configured clears the canary item; more stale clients truncate names.
	ovExec(t, db, `INSERT INTO canary_settings (id, email) VALUES ('default', 'ops@example.com')`)
	for i, n := range []string{"s2", "s3", "s4", "s5"} {
		ovExec(t, db, `INSERT INTO clients (id, name, token_hash, last_seen_at) VALUES (?, ?, ?, ?)`, n, n, "h"+n, ts(-time.Duration(30+i)*time.Hour))
	}
	r = getOverview(t, db)
	k = kinds(r)
	if len(k["canary"]) != 0 {
		t.Fatal("canary item should clear")
	}
	if c := k["clients"][0]; c.Count != 5 || len(c.Names) != 3 || !strings.HasSuffix(c.Meta, "and 2 more") {
		t.Fatalf("stale clients: %+v", c)
	}
}

func TestOverviewHealthyRolloutAndSQLInjectionShapedNames(t *testing.T) {
	db := overviewDB(t)
	ovExec(t, db, `INSERT INTO clients (id, name, token_hash, last_seen_at) VALUES ('c1', ?, 'h', ?)`, "x'; DROP TABLE clients;--", ts(-time.Hour))
	ovExec(t, db, `INSERT INTO client_update_rollouts (id, target_version, status, max_concurrency, start_interval_seconds) VALUES ('r1', 'v1', 'active', 1, 1)`)
	ovExec(t, db, `INSERT INTO client_update_jobs (id, rollout_id, client_id, status) VALUES ('j1', 'r1', 'c1', 'healthy')`)
	r := getOverview(t, db)
	if r.Attention.Count != 0 {
		t.Fatalf("healthy active rollout should not alert: %+v", r.Attention)
	}
}
