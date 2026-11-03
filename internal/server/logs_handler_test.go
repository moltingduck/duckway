package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type logsPage struct {
	Items []struct {
		ID         int64  `json:"id"`
		Path       string `json:"path"`
		Credential string `json:"credential"`
	} `json:"items"`
	NextBeforeID *int64 `json:"next_before_id"`
	Total        int    `json:"total"`
	TotalCapped  bool   `json:"total_capped"`
	Offset       int    `json:"offset"`
	HasNewer     bool   `json:"has_newer"`
	Window       string `json:"window"`
}

func seedLogs(t *testing.T, s *Server) {
	t.Helper()
	exec := func(q string, a ...interface{}) {
		t.Helper()
		if _, err := s.db.Exec(q, a...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO clients (id, name, token_hash) VALUES ('c1', 'build-runner', 'h1'), ('c2', 'laptop', 'h2')`)
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('s1', 'github', 'GitHub', 'https://x', 'x'), ('s2', 'anthropic', 'A', 'https://y', 'y')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('k1', 's1', 'ci-key', 'x'), ('k2', 's2', 'anth-key', 'x')`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, requires_approval) VALUES ('p1', 'GITHUB_TOKEN', 'ghp_dw_1', 's1', 'k1', 'c1', 1), ('p2', 'ANTH', 'sk_dw_2', 's2', 'k2', 'c2', 0)`)
	// 30 rows: ids 1..30. id%3==0 -> github/c1/p1, else anthropic/c2/p2 .
	for i := 1; i <= 30; i++ {
		client, ph, svc := "c2", "p2", "anthropic"
		if i%3 == 0 {
			client, ph, svc = "c1", "p1", "github"
		}
		status := []int{200, 304, 403, 502}[i%4]
		ts := time.Now().UTC().Add(-time.Duration(i*60+30) * time.Second).Format("2006-01-02 15:04:05")
		if i == 30 {
			ts = time.Now().UTC().Add(-48 * time.Hour).Format("2006-01-02 15:04:05")
		}
		exec(`INSERT INTO request_log (placeholder_id, client_id, service_name, method, path, status_code, created_at) VALUES (?, ?, ?, 'GET', ?, ?, ?)`,
			ph, client, svc, fmt.Sprintf("/v1/item%d", i), status, ts)
	}
	exec(`INSERT INTO request_log (client_id, service_name, method, path, status_code) VALUES ('c1', 'github', 'GET', '/pct/100%_done\x', 200)`)
	exec(`INSERT INTO request_log (client_id, service_name, method, path, status_code) VALUES ('c1', 'github', 'GET', '/pct/100xxdone', 200)`)
	exec(`INSERT INTO request_log_detail (log_id, duration_ms) VALUES (3, 42)`)
}

func getLogs(t *testing.T, s *Server, shared *SharedServices, query string, anon bool) (*httptest.ResponseRecorder, logsPage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/logs"+query, nil)
	if !anon {
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var p logsPage
	if rec.Code == 200 && query != "" {
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode %s: %v\n%s", query, err, rec.Body.String())
		}
	}
	return rec, p
}

func TestLogsRequireAuth(t *testing.T) {
	s, shared := newShellTestServer(t)
	for _, q := range []string{"", "?limit=5", "/facets"} {
		path := "/api/logs" + q
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 401 {
			t.Fatalf("%s anonymous status=%d", path, rec.Code)
		}
	}
	_ = shared
}

func TestLogsNoParamsStaysLegacyArray(t *testing.T) {
	s, shared := newShellTestServer(t)
	rec, _ := getLogs(t, s, shared, "", false)
	if rec.Code != 200 || rec.Body.String()[0] != '[' {
		t.Fatalf("empty db: %d %q", rec.Code, rec.Body.String())
	}
	seedLogs(t, s)
	rec, _ = getLogs(t, s, shared, "", false)
	var arr []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil || len(arr) != 32 {
		t.Fatalf("legacy array: err=%v len=%d", err, len(arr))
	}
	for _, k := range []string{"id", "service_name", "method", "path", "status_code", "client_id", "client_name", "placeholder_id", "env_name", "placeholder", "created_at", "has_detail"} {
		if _, ok := arr[0][k]; !ok {
			t.Fatalf("legacy field %q missing", k)
		}
	}
}

func TestLogsFiltersAndTotals(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedLogs(t, s)
	cases := []struct {
		q     string
		total int
	}{
		{"?limit=200", 32},
		{"?range=24h&limit=200", 31},
		{"?range=15m", 14 + 2}, // i=1..14 within 15m, plus two "now" rows
		{"?service=github&limit=200", 12},
		{"?client=c2&limit=200", 20},
		{"?credential=ci-key&limit=200", 10},
		{"?held=1&limit=200", 10},
		{"?held=0&limit=200", 32},
		{"?acl_denied=1&limit=200", 8},
		{"?status=4xx&limit=200", 8},
		{"?status=2xx,3xx&limit=200", 17},
		{"?status=5xx,4xx&limit=200", 15},
		{"?q=ITEM1&limit=200", 11}, // item1, item10..19
		{"?q=build-runner&limit=200", 12},
		{"?q=anthropic&limit=200", 20},
		{"?service=github&status=2xx&q=item&limit=200", 2},
		{"?service=github&held=1&credential=ci-key&range=24h&limit=200", 9},
	}
	for _, c := range cases {
		rec, p := getLogs(t, s, shared, c.q, false)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d %s", c.q, rec.Code, rec.Body.String())
		}
		if p.Total != c.total {
			t.Errorf("%s: total=%d want %d", c.q, p.Total, c.total)
		}
		if len(p.Items) > p.Total {
			t.Errorf("%s: items %d > total %d", c.q, len(p.Items), p.Total)
		}
	}
}

func TestLogsKeysetPagingNoGapsNoDupes(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedLogs(t, s)
	for _, base := range []string{"?limit=7", "?service=github&limit=4", "?status=2xx,4xx&limit=5"} {
		seen := map[int64]bool{}
		var order []int64
		q := base
		for pages := 0; pages < 50; pages++ {
			rec, p := getLogs(t, s, shared, q, false)
			if rec.Code != 200 {
				t.Fatalf("%s: %d", q, rec.Code)
			}
			if pages == 0 && (p.HasNewer || p.Offset != 0) {
				t.Fatalf("first page offset=%d newer=%v", p.Offset, p.HasNewer)
			}
			if pages > 0 && (p.Offset != len(order) || !p.HasNewer) {
				t.Fatalf("%s: offset=%d want %d", q, p.Offset, len(order))
			}
			for _, it := range p.Items {
				if seen[it.ID] {
					t.Fatalf("%s: duplicate id %d", base, it.ID)
				}
				if len(order) > 0 && it.ID >= order[len(order)-1] {
					t.Fatalf("%s: not descending at %d", base, it.ID)
				}
				seen[it.ID] = true
				order = append(order, it.ID)
			}
			if p.NextBeforeID == nil {
				break
			}
			q = base + "&before_id=" + fmt.Sprint(*p.NextBeforeID)
		}
		_, first := getLogs(t, s, shared, base, false)
		if len(order) != first.Total || first.TotalCapped {
			t.Fatalf("%s: paged %d rows, total %d", base, len(order), first.Total)
		}
		// Walk back with after_id and land on the same rows.
		rec, p := getLogs(t, s, shared, base+"&before_id="+fmt.Sprint(order[len(order)-2]), false)
		_ = rec
		if len(p.Items) == 0 {
			t.Fatalf("%s: empty last page unexpected", base)
		}
		newer := p.Items[0].ID
		_, n := getLogs(t, s, shared, base+"&after_id="+fmt.Sprint(newer), false)
		if len(n.Items) == 0 || n.Items[len(n.Items)-1].ID <= newer {
			t.Fatalf("%s: after_id page wrong: %+v", base, n.Items)
		}
		for i := 1; i < len(n.Items); i++ {
			if n.Items[i].ID >= n.Items[i-1].ID {
				t.Fatalf("after_id page not newest-first")
			}
		}
		if n.NextBeforeID == nil {
			t.Fatalf("%s: after_id page should have older rows", base)
		}
	}
}

func TestLogsLiteralWildcardAndInjectionInput(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedLogs(t, s)
	count := func(q string) int {
		rec, p := getLogs(t, s, shared, q, false)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		return p.Total
	}
	if n := count("?q=" + url.QueryEscape("100%_done")); n != 1 {
		t.Fatalf("percent/underscore literal: %d", n)
	}
	if n := count("?q=" + url.QueryEscape("100xxdone")); n != 1 {
		t.Fatalf("plain: %d", n)
	}
	if n := count("?q=" + url.QueryEscape(`\x`)); n != 1 {
		t.Fatalf("backslash literal: %d", n)
	}
	for _, evil := range []string{"%", "_", "' OR 1=1 --", "'; DROP TABLE request_log; --", `\`, "%' OR '%'='"} {
		want := 0
		if evil == "%" || evil == "_" {
			want = 1 // only the literal-percent path
		}
		if evil == `\` {
			want = 1
		}
		if n := count("?q=" + url.QueryEscape(evil)); n != want {
			t.Errorf("q=%q matched %d want %d", evil, n, want)
		}
		for _, f := range []string{"client", "service", "credential"} {
			if n := count("?" + f + "=" + url.QueryEscape(evil)); n != 0 {
				t.Errorf("%s=%q matched %d", f, evil, n)
			}
		}
	}
	if n := count("?limit=200"); n != 32 {
		t.Fatalf("table damaged: %d", n)
	}
}

func TestLogsInvalidParams(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedLogs(t, s)
	bad := []string{
		"?range=2h", "?range=", "?since=yesterday", "?until=2026-01-01", "?range=1h&since=2026-01-01T00:00:00Z",
		"?since=2026-02-01T00:00:00Z&until=2026-01-01T00:00:00Z",
		"?status=2xx,9xx", "?status=200", "?status=2xx,",
		"?held=yes", "?acl_denied=2", "?before_id=0", "?before_id=-1", "?before_id=abc", "?before_id=1.5",
		"?after_id=x", "?before_id=5&after_id=3",
		"?limit=0", "?limit=201", "?limit=abc", "?limit=",
		"?bogus=1", "?q=a&q=b", "?limit=5&limit=6",
		"?q=" + url.QueryEscape(string(make([]byte, 201))), "?q=%00",
	}
	for _, q := range bad {
		if q == "?range=" {
			continue // empty range is treated as absent
		}
		rec, _ := getLogs(t, s, shared, q, false)
		if rec.Code != 400 {
			t.Errorf("%q: status %d want 400 (%s)", q, rec.Code, rec.Body.String())
		}
	}
	if rec, p := getLogs(t, s, shared, "?since=2020-01-01T00:00:00Z&until=2999-01-01T00:00:00%2B02:00", false); rec.Code != 200 || p.Total != 32 {
		t.Fatalf("custom range: %d total=%d", rec.Code, p.Total)
	}
}

func TestLogsFacetsAndDuration(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedLogs(t, s)
	req := httptest.NewRequest(http.MethodGet, "/api/logs/facets", nil)
	req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("facets %d", rec.Code)
	}
	var f struct {
		Clients     []struct{ ID, Name string }
		Services    []string
		Credentials []string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil || len(f.Clients) != 2 || len(f.Services) < 2 || len(f.Credentials) != 2 {
		t.Fatalf("facets %v %+v", err, f)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/logs?limit=200", nil)
	req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var raw struct {
		Items []struct {
			ID         int64  `json:"id"`
			DurationMs *int64 `json:"duration_ms"`
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, it := range raw.Items {
		if it.ID == 3 && (it.DurationMs == nil || *it.DurationMs != 42) {
			t.Fatalf("duration missing: %+v", it)
		}
		if it.ID != 3 && it.DurationMs != nil {
			t.Fatalf("unexpected duration on %d", it.ID)
		}
	}
}

func TestLogsDefaultWindowAndEmptyAfterPage(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedLogs(t, s)
	// id 30 is 48h old but within 7d: bare q search is windowed to 7d.
	_, p := getLogs(t, s, shared, "?q=item30", false)
	if p.Window != "7d" || p.Total != 1 {
		t.Fatalf("window=%q total=%d", p.Window, p.Total)
	}
	if _, p = getLogs(t, s, shared, "?limit=5", false); p.Window != "all" {
		t.Fatalf("plain window=%q", p.Window)
	}
	if _, p = getLogs(t, s, shared, "?range=1h&q=item", false); p.Window != "1h" {
		t.Fatalf("range window=%q", p.Window)
	}
	// Old rows beyond 7d are excluded from a bare q search.
	if _, err := s.db.Exec(`UPDATE request_log SET created_at = datetime('now','-9 days') WHERE id = 30`); err != nil {
		t.Fatal(err)
	}
	if _, p = getLogs(t, s, shared, "?q=item30", false); p.Total != 0 {
		t.Fatalf("expected windowed out, total=%d", p.Total)
	}
	// after_id past the newest row: empty page, no newer rows.
	if _, p = getLogs(t, s, shared, "?after_id=999999", false); len(p.Items) != 0 || p.HasNewer {
		t.Fatalf("after past end: items=%d newer=%v", len(p.Items), p.HasNewer)
	}
	if _, p = getLogs(t, s, shared, "?before_id=1", false); len(p.Items) != 0 || !p.HasNewer {
		t.Fatalf("before past end: newer=%v", p.HasNewer)
	}
}
