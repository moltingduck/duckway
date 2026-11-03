package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestKeysListTokens7dAndPlaceholdersRequests7d(t *testing.T) {
	e := newPTEnv(t)
	now := time.Now().UTC()
	stamp := func(d time.Duration) string { return now.Add(-d).Format("2006-01-02 15:04:05") }
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := e.s.db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	for _, r := range []struct {
		key     string
		in, out int
		age     time.Duration
	}{
		{"key-pt", 100, 20, time.Hour},
		{"key-pt", 3, 4, 6 * 24 * time.Hour},
		{"key-pt", 9000, 9000, 8 * 24 * time.Hour}, // outside the window
		{"key-narrow", 5000, 5000, 20 * 24 * time.Hour},
	} {
		exec(`INSERT INTO conversation_usage (api_key_id, input_tokens, output_tokens, created_at) VALUES (?, ?, ?, ?)`, r.key, r.in, r.out, stamp(r.age))
	}
	for _, r := range []struct {
		ph  string
		age time.Duration
	}{
		{"ph-pt", time.Hour}, {"ph-pt", 5 * 24 * time.Hour}, {"ph-pt", 9 * 24 * time.Hour}, {"ph-narrow", 2 * time.Hour},
	} {
		exec(`INSERT INTO request_log (placeholder_id, client_id, service_name, method, path, status_code, created_at) VALUES (?, 'c-pt', 'svc', 'GET', '/x', 200, ?)`, r.ph, stamp(r.age))
	}

	rec := e.do(t, http.MethodGet, "/api/keys", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys status %d: %s", rec.Code, rec.Body.String())
	}
	var keys []struct {
		ID       string `json:"id"`
		Tokens7d *int64 `json:"tokens_7d"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	seen := map[string]*int64{}
	for _, k := range keys {
		seen[k.ID] = k.Tokens7d
	}
	if v := seen["key-pt"]; v == nil || *v != 127 {
		t.Errorf("key-pt tokens_7d = %v, want 127", v)
	}
	if v, ok := seen["key-narrow"]; !ok || v != nil {
		t.Errorf("key-narrow tokens_7d = %v (present %v), want null (no token data in window)", v, ok)
	}

	rec = e.do(t, http.MethodGet, "/api/placeholders/with-approvals?client_id=c-pt", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("placeholders status %d: %s", rec.Code, rec.Body.String())
	}
	var phs []struct {
		ID         string `json:"id"`
		Requests7d int64  `json:"requests_7d"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &phs); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, p := range phs {
		got[p.ID] = p.Requests7d
	}
	if got["ph-pt"] != 2 || got["ph-narrow"] != 1 || got["ph-xss"] != 0 {
		t.Errorf("requests_7d = %v, want ph-pt=2 ph-narrow=1 ph-xss=0", got)
	}
}

func TestClientsTabsRenderCountAndCurrent(t *testing.T) {
	e := newPTEnv(t)
	for path, current := range map[string]string{"/admin/clients": `href="/admin/clients" aria-current="page"`, "/admin/clients/rollouts": `href="/admin/clients/rollouts" aria-current="page"`} {
		body := e.do(t, http.MethodGet, path, "", true).Body.String()
		if !strings.Contains(body, current) {
			t.Errorf("%s: missing %q", path, current)
		}
		if !strings.Contains(body, `Clients · <span id="cl-nav-count">1</span>`) {
			t.Errorf("%s: missing Clients · N tab label", path)
		}
	}
}
