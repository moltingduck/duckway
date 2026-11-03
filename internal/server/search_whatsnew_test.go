package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/server/handlers"
)

const (
	secTokenHash = "SECRET-TOKEN-HASH-zzz"
	secKeyEnc    = "SECRET-KEY-ENCRYPTED-zzz"
	secRefresh   = "SECRET-REFRESH-zzz"
	secPlaceh    = "SECRET-PLACEHOLDER-zzz"
	secNotifCfg  = "SECRET-NOTIF-CONFIG-zzz"
)

func seedSearch(t *testing.T, s *Server) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO services (id,name,display_name,upstream_url,host_pattern) VALUES ('svc1','alphasvc','Alpha Gateway','https://x','x')`)
	exec(`INSERT INTO api_keys (id,service_id,name,key_encrypted,refresh_token) VALUES ('k1','svc1','prod-key-alpha',?,?)`, secKeyEnc, secRefresh)
	exec(`INSERT INTO clients (id,name,token_hash) VALUES ('c1','alpha-laptop',?)`, secTokenHash)
	exec(`INSERT INTO clients (id,name,token_hash) VALUES ('c2','<img src=x onerror=alert(1)>alpha',?)`, secTokenHash+"2")
	exec(`INSERT INTO clients (id,name,token_hash) VALUES ('c3','100%_real',?)`, secTokenHash+"3")
	exec(`INSERT INTO clients (id,name,token_hash) VALUES ('c4','100xyreal',?)`, secTokenHash+"4")
	exec(`INSERT INTO api_key_groups (id,service_id,name) VALUES ('g1','svc1','alpha rotation')`)
	exec(`INSERT INTO key_groups (id,name) VALUES ('lg1','alpha lease')`)
	exec(`INSERT INTO key_suites (id,name) VALUES ('b1','alpha bundle')`)
	exec(`INSERT INTO placeholder_keys (id,env_name,placeholder,service_id,api_key_id,client_id) VALUES ('p1','ALPHA_API_KEY',?,'svc1','k1','c1')`, secPlaceh)
	exec(`INSERT INTO notification_channels (id,channel_type,name,config) VALUES ('n1','webhook','alpha hook',?)`, secNotifCfg)
}

func searchGet(t *testing.T, s *Server, shared *SharedServices, q string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+url.QueryEscape(q), nil)
	if auth {
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

type searchResp struct {
	Results []handlers.SearchResult `json:"results"`
}

func TestSearchAPIRequiresAuth(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedSearch(t, s)
	rec := searchGet(t, s, shared, "alpha", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated search = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "alpha") {
		t.Fatal("unauthenticated response leaked data")
	}
}

func TestSearchAPIFindsEveryKindWithoutSecrets(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedSearch(t, s)
	rec := searchGet(t, s, shared, "ALPHA", true) // case-insensitive
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{secTokenHash, secKeyEnc, secRefresh, secPlaceh, secNotifCfg} {
		if strings.Contains(body, secret) {
			t.Fatalf("search response leaked secret %q", secret)
		}
	}
	var resp searchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, r := range resp.Results {
		if r.Kind == "" || r.ID == "" || r.Name == "" || !strings.HasPrefix(r.URL, "/admin/") {
			t.Errorf("malformed result %+v", r)
		}
		kinds[r.Kind+"|"+r.Name] = r.URL
	}
	for key, wantURL := range map[string]string{
		"client|alpha-laptop":         "/admin/clients/c1",
		"credential|prod-key-alpha":   "/admin/credentials?q=prod-key-alpha",
		"key_pool|alpha rotation":     "/admin/key-pools?pool=rotation%3Ag1",
		"key_pool|alpha lease":        "/admin/key-pools?pool=lease%3Alg1",
		"bundle|alpha bundle":         "/admin/key-bundles",
		"phantom_token|ALPHA_API_KEY": "/admin/phantom-tokens/p1",
		"notification|alpha hook":     "/admin/notifications",
	} {
		if got, ok := kinds[key]; !ok || got != wantURL {
			t.Errorf("result %q = %q (present=%v), want %q", key, got, ok, wantURL)
		}
	}
	// service matched by name column too
	rec = searchGet(t, s, shared, "alphasvc", true)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Results) == 0 || resp.Results[0].Kind != "service" || resp.Results[0].URL != "/admin/services?open=svc1" {
		t.Fatalf("service search: %+v", resp.Results)
	}
	// hostile name is returned verbatim as JSON data (escaping is the client's job)
	if !strings.Contains(body, "\\u003cimg src=x onerror=alert(1)\\u003ealpha") {
		t.Errorf("hostile name not returned as inert JSON data: %s", body)
	}
}

func TestSearchAPIEscapesLikeWildcards(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedSearch(t, s)
	rec := searchGet(t, s, shared, "100%_", true)
	var resp searchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Name != "100%_real" {
		t.Fatalf("'%%_' must match literally, got %+v", resp.Results)
	}
	for _, q := range []string{"%", "_", `\`} {
		rec := searchGet(t, s, shared, q, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("q=%q: %d", q, rec.Code)
		}
		resp = searchResp{}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		for _, r := range resp.Results {
			if !strings.Contains(strings.ToLower(r.Name), q) && r.Kind != "service" {
				t.Errorf("q=%q matched non-literal %q", q, r.Name)
			}
		}
	}
}

func TestSearchAPIValidationAndLimit(t *testing.T) {
	s, shared := newShellTestServer(t)
	seedSearch(t, s)
	for _, q := range []string{"", "   "} {
		if rec := searchGet(t, s, shared, q, true); rec.Code != http.StatusBadRequest {
			t.Errorf("q=%q: %d, want 400", q, rec.Code)
		}
	}
	for i := 0; i < 40; i++ {
		if _, err := s.db.Exec(`INSERT INTO clients (id,name,token_hash) VALUES (?,?,?)`,
			"bulk"+string(rune('a'+i%26))+string(rune('a'+i/26)), "bulk-client-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "h"+string(rune('a'+i%26))+string(rune('a'+i/26))); err != nil {
			t.Fatal(err)
		}
	}
	rec := searchGet(t, s, shared, "bulk", true)
	var resp searchResp
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Results) != 20 {
		t.Fatalf("limit: got %d results, want 20", len(resp.Results))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("search must be no-store")
	}
}

// The query set must run unchanged on PostgreSQL: no SQLite-only functions or collations.
func TestSearchSQLIsPortable(t *testing.T) {
	banned := regexp.MustCompile(`(?i)COLLATE\s+NOCASE|\bGLOB\b|\bIFNULL\b|\bINSTR\b|datetime\(|\bILIKE\b|\bsubstr\(|\bTOTAL\(|\|\|\s*char\(`)
	for _, q := range handlers.SearchSQLForTest() {
		if banned.MatchString(q) {
			t.Errorf("non-portable SQL: %s", q)
		}
		if !strings.Contains(q, "LOWER(") || !strings.Contains(q, `ESCAPE '\'`) || !strings.Contains(q, "LIMIT ?") {
			t.Errorf("query must use LOWER(), ESCAPE and LIMIT ?: %s", q)
		}
		for _, secret := range []string{"token_hash", "key_encrypted", "refresh_token", "placeholder,", "config", "password", "secret"} {
			if strings.Contains(strings.ToLower(q), secret) {
				t.Errorf("query selects/mentions secret column %q: %s", secret, q)
			}
		}
	}
}

func whatsNewGet(t *testing.T, s *Server, shared *SharedServices, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/docs", nil)
	req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestWhatsNewShowsOncePerSessionThenNeverAfterDismiss(t *testing.T) {
	s, shared := newShellTestServer(t)
	rec := whatsNewGet(t, s, shared)
	if !strings.Contains(rec.Body.String(), `data-autoopen="1"`) {
		t.Fatal("first load must auto-open the dialog")
	}
	var wn *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "duckway_whatsnew" {
			wn = c
		}
	}
	if wn == nil || wn.MaxAge != 0 || !wn.HttpOnly {
		t.Fatalf("expected a session-scoped HttpOnly marker cookie, got %+v", wn)
	}
	// same session: not shown again
	if strings.Contains(whatsNewGet(t, s, shared, wn).Body.String(), `data-autoopen="1"`) {
		t.Fatal("dialog must not reopen on every page load in the same session")
	}
	// the dialog and its markup still exist (re-openable from Docs)
	body := whatsNewGet(t, s, shared, wn).Body.String()
	for _, want := range []string{`id="wn-dialog"`, `data-action="whatsNewOpen"`, `href="/admin/docs#whats-new"`, `id="whats-new-h"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
	// new login (no marker cookie): shown again until dismissed for good
	if !strings.Contains(whatsNewGet(t, s, shared).Body.String(), `data-autoopen="1"`) {
		t.Fatal("next login must show the dialog again")
	}
}

func TestWhatsNewDismissEndpoint(t *testing.T) {
	s, shared := newShellTestServer(t)
	post := func(body string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/whats-new/dismiss", strings.NewReader(body))
		testSameOrigin(req)
		if auth {
			req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(`{"version":"`+handlers.WhatsNewVersion+`"}`, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dismiss = %d, want 401", rec.Code)
	}
	if rec := post(`{"version":"9999"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong version = %d, want 400", rec.Code)
	}
	if rec := post(`not json`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d, want 400", rec.Code)
	}
	if strings.Contains(whatsNewGet(t, s, shared).Body.String(), `data-autoopen=""`) {
		t.Fatal("precondition: dialog should auto-open before dismissal")
	}
	if rec := post(`{"version":"`+handlers.WhatsNewVersion+`"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d: %s", rec.Code, rec.Body.String())
	}
	body := whatsNewGet(t, s, shared).Body.String()
	if strings.Contains(body, `data-autoopen="1"`) {
		t.Fatal("dialog must never auto-open after Don't show again")
	}
	if !strings.Contains(body, `id="wn-dialog"`) {
		t.Fatal("dialog must remain re-openable from Docs after dismissal")
	}
}

// Every "where things went" row must point at a page that really exists: either a
// registered moved-page target or a live console route.
func TestWhatsNewMovesMatchRealPages(t *testing.T) {
	s, shared := newShellTestServer(t)
	targets := map[string]bool{}
	for _, p := range handlers.MovedPaths() {
		m, _ := handlers.LookupMoved(p)
		targets[strings.SplitN(m.NewPath, "?", 2)[0]] = true
	}
	for _, mv := range handlers.WhatsNewMovesForTest() {
		if !targets[mv.Path] && mv.Path != "/admin/" {
			t.Errorf("row %q → %q is not a moved-page target", mv.Old, mv.Path)
		}
		req := httptest.NewRequest(http.MethodGet, mv.Path, nil)
		req.AddCookie(shared.AdminAuth.CreateSession("duckway", req))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s answers %d", mv.Path, rec.Code)
		}
	}
}

func TestShellSearchRowAndTourEntryPoints(t *testing.T) {
	s, shared := newShellTestServer(t)
	body := whatsNewGet(t, s, shared).Body.String()
	for _, want := range []string{
		`id="nav-search"`, `data-action="openPalette" aria-label="Search"`,
		`data-action="whatsNewTour">Show me around</button>`,
		`data-action="tourStart">Take the tour</button>`,
		`/static/tour.js`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
	// Show me around sits between Release notes and Got it.
	n, tour, got := strings.Index(body, `data-action="whatsNewNotes"`), strings.Index(body, `data-action="whatsNewTour"`), strings.Index(body, `data-action="whatsNewClose"`)
	if n < 0 || n >= tour || tour >= got {
		t.Errorf("button order wrong: notes=%d tour=%d close=%d", n, tour, got)
	}
}
