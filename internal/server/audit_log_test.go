package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

const (
	auSecretKey   = "sk-audit-SECRET-key-value-0123456789"
	auSecretProxy = "http://proxyuser:proxypass-SECRET@proxy.example:3128"
	auHook        = "https://discord.com/api/webhooks/1/AUDIT-HOOK-SECRET"
)

func (e *ntEnv) must(rec *httptest.ResponseRecorder, wantMin, wantMax int) map[string]any {
	e.t.Helper()
	if rec.Code < wantMin || rec.Code > wantMax {
		e.t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return m
}

type auditRowT struct {
	ID                                                         int64
	Actor, Via, Addr, Action, Type, RID, Name, Summary, Before string
	After, Status                                              string
}

func (e *ntEnv) audit(where string, args ...any) []auditRowT {
	e.t.Helper()
	rows, err := e.s.db.Query(`SELECT id, actor, via, remote_addr, action, resource_type, resource_id, resource_name, summary, before_json, after_json, status
		FROM admin_audit_log WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRowT
	for rows.Next() {
		var r auditRowT
		if err := rows.Scan(&r.ID, &r.Actor, &r.Via, &r.Addr, &r.Action, &r.Type, &r.RID, &r.Name, &r.Summary, &r.Before, &r.After, &r.Status); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *ntEnv) one(action string) auditRowT {
	e.t.Helper()
	rows := e.audit("action = ?", action)
	if len(rows) == 0 {
		e.t.Fatalf("no audit row for %s", action)
	}
	return rows[len(rows)-1]
}

// One row per mutation family, with actor/via/addr, redacted before/after, and
// no secret anywhere in the table.
func TestAuditRowsPerMutationFamilyAndRedaction(t *testing.T) {
	e := newNtEnv(t)
	acl := `{"version":"1","provider":"audit","rules":[{"name":"r","endpoints":[{"method":"GET","path":"/*","allow":true}]}]}`
	acl2 := `{"version":"1","provider":"audit","rules":[{"name":"r2","endpoints":[{"method":"POST","path":"/v1/*","allow":true}]}]}`

	svc := e.must(e.do("POST", "/api/services", map[string]any{"name": "auditsvc", "display_name": "Audit", "upstream_url": "https://api.audit.example", "default_acl": acl}), 200, 201)
	svcID := svc["id"].(string)
	e.must(e.do("PUT", "/api/services/"+svcID, map[string]any{"default_acl": acl2}), 200, 200)
	r := e.one("service.update")
	if r.Actor != "admin:duckway" || r.Via != "web" || r.Type != "service" || r.RID != svcID || r.Name != "auditsvc" || r.Status != "ok" {
		t.Fatalf("service.update row: %+v", r)
	}
	if !strings.Contains(r.Before, `"r"`) || !strings.Contains(r.After, `"r2"`) {
		t.Fatalf("service before/after should carry default_acl: %s | %s", r.Before, r.After)
	}
	if e.one("service.create").After == "" {
		t.Fatal("create should record the after snapshot")
	}

	key := e.must(e.do("POST", "/api/keys", map[string]any{"service_id": svcID, "name": "prod-key", "key": auSecretKey, "upstream_proxy_url": auSecretProxy}), 200, 201)
	keyID := key["id"].(string)
	e.must(e.do("PUT", "/api/keys/"+keyID, map[string]any{"name": "prod-key-2", "key": auSecretKey + "-rotated"}), 200, 200)
	e.must(e.do("POST", "/api/keys/"+keyID+"/acl", map[string]any{"acl": acl}), 200, 200)
	ku := e.one("api_key.update")
	if !strings.Contains(ku.After, `"key_encrypted":"(changed)"`) || !strings.Contains(ku.Before, `"key_encrypted":"(set)"`) {
		t.Fatalf("key value should show (set)/(changed): %s | %s", ku.Before, ku.After)
	}
	if e.one("api_key.acl_update").After == "" {
		t.Fatal("acl update should record after")
	}

	cl := e.must(e.do("POST", "/api/clients", map[string]any{"name": "laptop"}), 200, 201)
	clID := cl["id"].(string)
	e.must(e.do("POST", "/api/clients/"+clID+"/rotate-token", nil), 200, 200)
	if rt := e.one("client.rotate_token"); !strings.Contains(rt.After, `"token_hash":"(changed)"`) {
		t.Fatalf("rotate: %s", rt.After)
	}

	ph := e.must(e.do("POST", "/api/placeholders", map[string]any{"env_name": "AUDIT_KEY", "service_id": svcID, "client_id": clID, "api_key_id": keyID, "permission_config": acl}), 200, 201)
	phID := ph["id"].(string)
	e.must(e.do("PUT", "/api/placeholders/"+phID, map[string]any{"permission_config": acl2, "requires_approval": false}), 200, 200)
	pu := e.one("placeholder.update")
	if pu.RID != phID || !strings.Contains(pu.After, `"requires_approval":false`) || !strings.Contains(pu.After, `"r2"`) || !strings.Contains(pu.Before, `"requires_approval":true`) || !strings.Contains(pu.After, keyID) {
		t.Fatalf("placeholder update: %+v", pu)
	}

	grp := e.must(e.do("POST", "/api/groups", map[string]any{"service_id": svcID, "name": "pool"}), 200, 201)
	grpID := grp["id"].(string)
	e.must(e.do("POST", "/api/groups/"+grpID+"/members", map[string]any{"api_key_id": keyID}), 200, 200)
	if gm := e.one("group.member_add"); !strings.Contains(gm.After, keyID) || strings.Contains(gm.Before, keyID) {
		t.Fatalf("member add: %s | %s", gm.Before, gm.After)
	}
	e.must(e.do("DELETE", "/api/groups/"+grpID+"/members/"+keyID, nil), 200, 200)
	e.one("group.member_remove")

	nid := e.create("discord", "ops", map[string]string{"webhook_url": auHook})
	e.must(e.do("PUT", "/api/notifications/"+nid, map[string]any{"channel_type": "discord", "name": "ops2", "config": `{"webhook_url":"` + auHook + `-2"}`}), 200, 200)
	if nu := e.one("notification.update"); !strings.Contains(nu.After, `"(changed)"`) {
		t.Fatalf("notification: %s", nu.After)
	}

	e.must(e.do("POST", "/api/settings", map[string]string{"gateway_url": "https://gw.example.com", "proxy_port": "18081"}), 200, 200)
	su := e.one("setting.update")
	if !strings.Contains(su.Summary, "gateway_url") || strings.Contains(su.Summary, "approval") {
		t.Fatalf("settings summary lists changed keys only: %q", su.Summary)
	}
	e.must(e.do("POST", "/api/settings", map[string]string{"gateway_url": "https://gw.example.com"}), 200, 200) // no change
	if rows := e.audit("action = 'setting.update'"); strings.Contains(rows[len(rows)-1].After, "gateway_url") {
		t.Fatalf("an unchanged key must not be logged: %s", rows[len(rows)-1].After)
	}

	// Failure recorded as failed; delete records before only.
	if rec := e.do("PUT", "/api/services/nope", map[string]any{"default_acl": "{"}); rec.Code < 400 {
		t.Fatalf("expected an error, got %d", rec.Code)
	}
	if fr := e.audit("action = 'service.update' AND status = 'failed'"); len(fr) != 1 {
		t.Fatalf("failed attempt rows = %d", len(fr))
	}
	e.must(e.do("DELETE", "/api/clients/"+clID, nil), 200, 200)
	if d := e.one("client.delete"); d.Before == "" || d.After != "" {
		t.Fatalf("delete: before=%q after=%q", d.Before, d.After)
	}

	// Dry runs and reads are not recorded; nothing secret is stored anywhere.
	e.do("GET", "/api/services", nil)
	var dump strings.Builder
	for _, r := range e.audit("1=1") {
		fmt.Fprintf(&dump, "%+v\n", r)
	}
	for _, secret := range []string{auSecretKey, "proxypass", "proxyuser", "AUDIT-HOOK-SECRET", "-rotated"} {
		if strings.Contains(dump.String(), secret) {
			t.Fatalf("secret %q leaked into the audit log:\n%s", secret, dump.String())
		}
	}
}

// Approval decisions from the JSON API and from the /admin/approvals form land
// under the phantom token they unlock; a repeated decision records nothing.
func TestAuditApprovalDecisionsFromWeb(t *testing.T) {
	e := newNtEnv(t)
	svc := e.must(e.do("POST", "/api/services", map[string]any{"name": "apv", "upstream_url": "https://api.apv.example"}), 200, 201)
	key := e.must(e.do("POST", "/api/keys", map[string]any{"service_id": svc["id"], "name": "k", "key": "sk-apv-0123456789abcdef"}), 200, 201)
	cl := e.must(e.do("POST", "/api/clients", map[string]any{"name": "c"}), 200, 201)
	ph := e.must(e.do("POST", "/api/placeholders", map[string]any{"env_name": "APV_TOKEN", "service_id": svc["id"], "client_id": cl["id"], "api_key_id": key["id"]}), 200, 201)
	phID := ph["id"].(string)
	ph2 := e.must(e.do("POST", "/api/placeholders", map[string]any{"env_name": "APV_TWO", "service_id": svc["id"], "client_id": cl["id"], "api_key_id": key["id"]}), 200, 201)
	for id, pid := range map[string]string{"apv-1": phID, "apv-2": ph2["id"].(string)} {
		if _, err := e.s.db.Exec(`INSERT INTO approvals (id, placeholder_id, status) VALUES (?, ?, 'pending')`, id, pid); err != nil {
			t.Fatal(err)
		}
	}
	e.must(e.do("POST", "/api/approvals/apv-1/approve", nil), 200, 299)
	form := httptest.NewRequest("POST", "/admin/approvals/apv-2/reject", strings.NewReader(url.Values{}.Encode()))
	form.AddCookie(e.c)
	testSameOrigin(form)
	rec := httptest.NewRecorder()
	e.s.ServeHTTP(rec, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("form reject: %d", rec.Code)
	}
	e.do("POST", "/api/approvals/apv-1/approve", nil) // already decided

	got := e.audit("action LIKE 'approval.%'")
	if len(got) != 2 {
		t.Fatalf("want 2 approval rows, got %d: %+v", len(got), got)
	}
	a, b := got[0], got[1]
	if a.Action != "approval.approve" || a.Type != "placeholder" || a.RID != phID || a.Name != "APV_TOKEN" || !strings.Contains(a.Summary, "apv-1") {
		t.Fatalf("approve row: %+v", a)
	}
	if !strings.Contains(a.Before, `"status":"pending"`) || !strings.Contains(a.After, `"status":"approved"`) {
		t.Fatalf("approve before/after: %s | %s", a.Before, a.After)
	}
	if b.Action != "approval.reject" || !strings.Contains(b.After, `"status":"rejected"`) {
		t.Fatalf("reject row: %+v", b)
	}
}

// The audit write failing must not change the user's action or its response.
func TestAuditWriteFailureDoesNotBreakAction(t *testing.T) {
	e := newNtEnv(t)
	if _, err := e.s.db.Exec(`DROP TABLE admin_audit_log`); err != nil {
		t.Fatal(err)
	}
	e.must(e.do("POST", "/api/settings", map[string]string{"gateway_url": "https://still.works"}), 200, 200)
	if got := e.settings()["gateway_url"]; got != "https://still.works" {
		t.Fatalf("action did not apply: %q", got)
	}
	svc := e.must(e.do("POST", "/api/services", map[string]any{"name": "nolog", "upstream_url": "https://x.example"}), 200, 201)
	e.must(e.do("DELETE", "/api/services/"+svc["id"].(string), nil), 200, 200)
}

func TestAuditAPIFiltersAndPaging(t *testing.T) {
	e := newNtEnv(t)
	for i := 0; i < 5; i++ {
		e.must(e.do("POST", "/api/clients", map[string]any{"name": fmt.Sprintf("host-%d", i)}), 200, 201)
	}
	e.must(e.do("POST", "/api/settings", map[string]string{"proxy_port": "18090"}), 200, 200)

	list := func(q string) (entries []map[string]any, next any) {
		rec := e.do("GET", "/api/audit?"+q, nil)
		m := e.must(rec, 200, 200)
		for _, x := range m["entries"].([]any) {
			entries = append(entries, x.(map[string]any))
		}
		return entries, m["next_cursor"]
	}
	if all, next := list(""); len(all) != 6 || next != nil {
		t.Fatalf("all: %d next=%v", len(all), next)
	}
	page1, next := list("limit=4")
	if len(page1) != 4 || next == nil {
		t.Fatalf("page1: %d next=%v", len(page1), next)
	}
	page2, next2 := list("limit=4&cursor=" + next.(string))
	if len(page2) != 2 || next2 != nil {
		t.Fatalf("page2: %d next=%v", len(page2), next2)
	}
	if page1[3]["id"].(float64) <= page2[0]["id"].(float64) {
		t.Fatal("cursor pages overlap or are out of order")
	}
	if c, _ := list("kind=clients"); len(c) != 5 {
		t.Fatalf("kind=clients: %d", len(c))
	}
	if c, _ := list("kind=settings"); len(c) != 1 {
		t.Fatalf("kind=settings: %d", len(c))
	}
	if c, _ := list("q=host-3"); len(c) != 1 {
		t.Fatalf("q: %d", len(c))
	}
	if c, _ := list("actor=" + url.QueryEscape("admin:duckway") + "&since=" + url.QueryEscape(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))); len(c) != 6 {
		t.Fatalf("actor+since: %d", len(c))
	}
	if c, _ := list("since=" + url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))); len(c) != 0 {
		t.Fatalf("future since: %d", len(c))
	}
	for _, bad := range []string{"kind=nope", "since=yesterday", "cursor=abc", "limit=0"} {
		if rec := e.do("GET", "/api/audit?"+bad, nil); rec.Code != 400 {
			t.Errorf("%s: want 400 got %d", bad, rec.Code)
		}
	}
	id := int64(page1[0]["id"].(float64))
	d := e.must(e.do("GET", fmt.Sprintf("/api/audit/%d", id), nil), 200, 200)
	if d["after"] == nil {
		t.Fatalf("detail should carry after: %v", d)
	}
	if rec := e.do("GET", "/api/audit/999999", nil); rec.Code != 404 {
		t.Fatalf("missing id: %d", rec.Code)
	}
	// read-only: no mutation routes
	if rec := e.do("DELETE", "/api/audit/1", nil); rec.Code != 405 && rec.Code != 404 {
		t.Fatalf("audit must be read-only, DELETE gave %d", rec.Code)
	}
	// anonymous requests get 401
	req := httptest.NewRequest("GET", "/api/audit", nil)
	rec := httptest.NewRecorder()
	e.s.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated audit read: %d", rec.Code)
	}
}

func TestAuditSignInsAndPage(t *testing.T) {
	e := newNtEnv(t)
	if _, err := e.s.db.Exec(`INSERT INTO admin_users (id, username, password_hash) VALUES ('u1', 'root', '$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ012345')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		body, _ := json.Marshal(map[string]string{"username": "root", "password": "wrong-password"})
		req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(string(body)))
		req.RemoteAddr = "203.0.113.7:5555"
		testSameOrigin(req)
		rec := httptest.NewRecorder()
		e.s.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("login status %d", rec.Code)
		}
	}
	rows := e.audit("action = 'signin.failed'")
	if len(rows) != 1 || rows[0].Addr != "203.0.113.7" || rows[0].Name != "root" || !strings.Contains(rows[0].Summary, "4 failed") {
		t.Fatalf("throttled failed sign-ins: %+v", rows)
	}
	if strings.Contains(fmt.Sprintf("%+v", rows), "wrong-password") {
		t.Fatal("password logged")
	}
	rec := e.do("GET", "/admin/audit", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Audit log") || !strings.Contains(rec.Body.String(), `href="/admin/audit"`) {
		t.Fatalf("audit page: %d", rec.Code)
	}
	checkPage(t, rec)
}

func TestAuditRetentionSetting(t *testing.T) {
	e := newNtEnv(t)
	for _, bad := range []string{"29", "3651", "x", "-1"} {
		if rec := e.do("POST", "/api/settings", map[string]string{"audit_retention_days": bad}); rec.Code != 400 {
			t.Errorf("%q: want 400 got %d", bad, rec.Code)
		}
	}
	e.must(e.do("POST", "/api/settings", map[string]string{"audit_retention_days": "60"}), 200, 200)
	aq := queries.NewAuditQueries(e.s.db)
	old := time.Now().AddDate(0, 0, -70).UTC().Format("2006-01-02 15:04:05")
	mid := time.Now().AddDate(0, 0, -50).UTC().Format("2006-01-02 15:04:05")
	_, _ = aq.Insert(queries.AuditRow{CreatedAt: old, Action: "old.row"})
	_, _ = aq.Insert(queries.AuditRow{CreatedAt: mid, Action: "mid.row"})
	n, days, err := pruneAuditLog(queries.NewSettingsQueries(e.s.db), aq)
	if err != nil || days != 60 || n != 1 {
		t.Fatalf("prune: n=%d days=%d err=%v", n, days, err)
	}
	if len(e.audit("action = 'old.row'")) != 0 || len(e.audit("action = 'mid.row'")) != 1 {
		t.Fatal("retention window not honoured")
	}
	// unset -> 365-day default keeps 70-day-old rows
	e.must(e.do("POST", "/api/settings", map[string]string{"audit_retention_days": ""}), 200, 200)
	_, _ = aq.Insert(queries.AuditRow{CreatedAt: old, Action: "old.row"})
	if _, days, _ := pruneAuditLog(queries.NewSettingsQueries(e.s.db), aq); days != DefaultAuditRetentionDays {
		t.Fatalf("default days = %d", days)
	}
	if len(e.audit("action = 'old.row'")) != 1 {
		t.Fatal("default retention must keep 70-day-old rows")
	}
}
