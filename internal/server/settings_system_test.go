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
