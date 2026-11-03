package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func (e *ntEnv) settings() map[string]string {
	e.t.Helper()
	var m map[string]string
	json.Unmarshal(e.do("GET", "/api/settings", nil).Body.Bytes(), &m)
	return m
}

func TestSettingsTTLRoundTripAndSectionSaves(t *testing.T) {
	e := newNtEnv(t)
	if rec := e.do("POST", "/api/settings", map[string]string{"approval_request_ttl_minutes": "15"}); rec.Code != 200 {
		t.Fatalf("save ttl: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.settings()["approval_request_ttl_minutes"]; got != "15" {
		t.Fatalf("ttl = %q", got)
	}
	// saving another section leaves the TTL alone
	if rec := e.do("POST", "/api/settings", map[string]string{"gateway_url": " https://gw.example.com ", "proxy_port": "18081"}); rec.Code != 200 {
		t.Fatalf("save gateway: %d %s", rec.Code, rec.Body.String())
	}
	s := e.settings()
	if s["approval_request_ttl_minutes"] != "15" || s["gateway_url"] != "https://gw.example.com" || s["proxy_port"] != "18081" {
		t.Fatalf("unexpected settings: %v", s)
	}
	// statusline saved verbatim, other keys pass through
	script := "#!/bin/sh\necho hi\n"
	e.do("POST", "/api/settings", map[string]string{"agent_statusline_script": script, "request_log_capture_enabled": "1"})
	s = e.settings()
	if s["agent_statusline_script"] != script || s["request_log_capture_enabled"] != "1" {
		t.Fatalf("statusline/other keys not stored: %v", s)
	}
}

func TestSettingsServerValidationIsAuthoritative(t *testing.T) {
	e := newNtEnv(t)
	e.do("POST", "/api/settings", map[string]string{"approval_request_ttl_minutes": "30", "gateway_url": "https://ok.example.com"})
	for name, body := range map[string]map[string]string{
		"ttl zero":      {"approval_request_ttl_minutes": "0"},
		"ttl too big":   {"approval_request_ttl_minutes": "10081"},
		"ttl text":      {"approval_request_ttl_minutes": "soon"},
		"ttl empty":     {"approval_request_ttl_minutes": ""},
		"url scheme":    {"gateway_url": "ftp://x"},
		"url no host":   {"gateway_url": "http://"},
		"url junk":      {"gateway_url": "not a url"},
		"port low":      {"proxy_port": "80"},
		"port high":     {"proxy_port": "70000"},
		"script huge":   {"agent_statusline_script": strings.Repeat("x", 64*1024+1)},
		"mixed invalid": {"gateway_url": "https://new.example.com", "proxy_port": "x"},
	} {
		rec := e.do("POST", "/api/settings", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400 got %d", name, rec.Code)
		}
	}
	s := e.settings()
	if s["approval_request_ttl_minutes"] != "30" || s["gateway_url"] != "https://ok.example.com" {
		t.Fatalf("invalid save changed stored values (must be all-or-nothing): %v", s)
	}
	// an empty gateway URL is valid (combined mode)
	if rec := e.do("POST", "/api/settings", map[string]string{"gateway_url": ""}); rec.Code != 200 {
		t.Fatalf("empty gateway url rejected: %d", rec.Code)
	}
}

func TestChangePasswordMinimumLength(t *testing.T) {
	e := newNtEnv(t)
	for _, tc := range []struct {
		pw   string
		want int
	}{{"short-pass1", http.StatusBadRequest}} {
		rec := e.do("POST", "/api/auth/change-password", map[string]string{"current_password": "x", "new_password": tc.pw})
		if rec.Code != tc.want {
			t.Errorf("%q: %d %s", tc.pw, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "12") {
			t.Errorf("message should state the 12 character minimum: %s", rec.Body.String())
		}
	}
}

func TestSettingsDocsAndNotificationsPagesRenderNewStructure(t *testing.T) {
	e := newNtEnv(t)
	get := func(p string) string { return e.do("GET", p, nil).Body.String() }
	st := get("/admin/settings")
	for _, want := range []string{"Gateway", "Approvals", "Admin password", "Agent statusline", `data-action="stDiscard"`} {
		if !strings.Contains(st, want) {
			t.Errorf("settings missing %q", want)
		}
	}
	if strings.Contains(st, "tailscale-combined") {
		t.Error("Architecture table must have moved from Settings to Docs")
	}
	dc := get("/admin/docs")
	if !strings.Contains(dc, "tailscale-combined") || !strings.Contains(dc, `id="architecture"`) {
		t.Error("docs must contain the Architecture profile table")
	}
	if n := strings.Count(dc, "<h1"); n != 1 {
		t.Errorf("docs has %d h1, want 1", n)
	}
	if strings.Contains(dc, " style=") {
		t.Error("docs has inline styles")
	}
	nt := get("/admin/notifications")
	if strings.Contains(nt, "<pre") {
		t.Error("notifications must not render config in a <pre>")
	}
}

func TestSettingsAPIHidesAndRejectsReservedKeys(t *testing.T) {
	s, shared := newClientDetailServer(t)
	for _, k := range []string{"session_epoch", "whats_new_seen_version:duckway"} {
		body := `{"` + k + `":"99"}`
		rec := svcDo(s, shared, http.MethodPost, "/api/settings", body, true)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field"`) {
			t.Errorf("POST %s => %d %s, want 400", k, rec.Code, rec.Body.String())
		}
	}
	// A mixed batch is rejected whole: nothing is written.
	if rec := svcDo(s, shared, http.MethodPost, "/api/settings", `{"gateway_url":"http://ok.example","session_epoch":"5"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("mixed batch => %d", rec.Code)
	}
	svcExec(t, s, `INSERT INTO settings (key, value) VALUES ('session_epoch','3') ON CONFLICT(key) DO UPDATE SET value='3'`)
	svcExec(t, s, `INSERT INTO settings (key, value) VALUES ('whats_new_seen_version:duckway','2026.11')`)
	rec := svcDo(s, shared, http.MethodGet, "/api/settings", "", true)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "session_epoch") || strings.Contains(rec.Body.String(), "whats_new_seen_version") || strings.Contains(rec.Body.String(), "ok.example") {
		t.Fatalf("GET exposes reserved keys or wrote a rejected batch: %d %s", rec.Code, rec.Body.String())
	}
}

func TestApprovalScopeKeyIsReservedInSettingsAPI(t *testing.T) {
	s, shared := newClientDetailServer(t)
	if rec := svcDo(s, shared, http.MethodPost, "/api/settings", `{"approval_scope_key":"x"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST => %d", rec.Code)
	}
	svcExec(t, s, `INSERT INTO settings (key, value) VALUES ('approval_scope_key','topsecretkey') ON CONFLICT(key) DO UPDATE SET value='topsecretkey'`)
	if rec := svcDo(s, shared, http.MethodGet, "/api/settings", "", true); strings.Contains(rec.Body.String(), "topsecretkey") || strings.Contains(rec.Body.String(), "approval_scope_key") {
		t.Fatalf("GET exposes the key: %s", rec.Body.String())
	}
}

// The console API and the no-JS form refuse to change a decided approval.
func TestDecidedApprovalCannotBeRevivedThroughConsole(t *testing.T) {
	s, shared := newClientDetailServer(t)
	seedDetailClient(t, s, "c1", "alpha", "tok1")
	svcExec(t, s, `PRAGMA foreign_keys = OFF`)
	svcExec(t, s, `INSERT INTO approvals (id, placeholder_id, status, expires_at) VALUES ('ap-rej','ph1','rejected',NULL), ('ap-ok','ph1','approved','2030-01-01 00:00:00')`)
	rec := svcDo(s, shared, http.MethodPost, "/api/approvals/ap-rej/approve", `{}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("api approve of a rejected approval => %d %s", rec.Code, rec.Body.String())
	}
	rec = svcDo(s, shared, http.MethodPost, "/api/approvals/ap-ok/reject", ``, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("api reject of an approved approval => %d", rec.Code)
	}
	for _, path := range []string{"/admin/approvals/ap-rej/approve", "/admin/approvals/ap-ok/reject"} {
		rec = svcDo(s, shared, http.MethodPost, path, ``, true)
		if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "notice=already-decided") {
			t.Fatalf("%s => %d Location=%q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	var st1, st2, exp string
	_ = s.db.QueryRow(`SELECT status FROM approvals WHERE id='ap-rej'`).Scan(&st1)
	_ = s.db.QueryRow(`SELECT status, expires_at FROM approvals WHERE id='ap-ok'`).Scan(&st2, &exp)
	if st1 != "rejected" || st2 != "approved" || exp != "2030-01-01 00:00:00" {
		t.Fatalf("decisions changed: %q %q %q", st1, st2, exp)
	}
}
