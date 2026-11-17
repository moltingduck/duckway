package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

type decAuditRow struct{ actor, via, action, rtype, rid, summary, before, after, status string }

func (e *approverEnv) auditRows(t *testing.T, where string) []decAuditRow {
	t.Helper()
	rows, err := e.db.Query(`SELECT actor, via, action, resource_type, resource_id, summary, before_json, after_json, status
		FROM admin_audit_log WHERE ` + where + ` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []decAuditRow
	for rows.Next() {
		var r decAuditRow
		if err := rows.Scan(&r.actor, &r.via, &r.action, &r.rtype, &r.rid, &r.summary, &r.before, &r.after, &r.status); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// Every decision path records exactly one audit row with the same action and
// resource convention; web decisions are not recorded twice.
func TestApprovalDecisionsAreAuditedOncePerPath(t *testing.T) {
	e := newApproverEnv(t)
	for _, id := range []string{"a-web", "a-tui", "a-discord", "a-telegram", "a-sweep"} {
		e.addApproval(t, id)
	}
	if rec := e.admin("alice", "POST", "/api/approvals/a-web/approve", ""); rec.Code != 200 {
		t.Fatalf("web approve: %d %s", rec.Code, rec.Body.String())
	}
	_, tok := e.newToken(t, "ci bot")
	if rec := e.call(bearer(tok), "POST", "/approver/approvals/a-tui/reject", ""); rec.Code != 200 {
		t.Fatalf("approver reject: %d %s", rec.Code, rec.Body.String())
	}
	if err := e.ss.Decider.Approve(t.Context(), "a-discord", 5, "discord:123", "discord"); err != nil {
		t.Fatal(err)
	}
	if err := e.ss.Decider.Reject(t.Context(), "a-telegram", "telegram:456", "telegram"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`UPDATE approvals SET created_at = datetime('now','-3 days') WHERE id = 'a-sweep'`); err != nil {
		t.Fatal(err)
	}
	if n, err := e.ss.Decider.Expire(t.Context(), 1); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	// A lost race records nothing extra.
	_ = e.ss.Decider.Reject(t.Context(), "a-web", "discord:9", "discord")

	rows := e.auditRows(t, "action LIKE 'approval.%'")
	want := []struct{ actor, via, action, word string }{
		{"admin:alice", "web", "approval.approve", "approved request a-web"},
		{"approver:ci bot", "tui", "approval.reject", "rejected request a-tui"},
		{"discord:123", "discord", "approval.approve", "approved request a-discord"},
		{"telegram:456", "telegram", "approval.reject", "rejected request a-telegram"},
		{"system:sweeper", "sweeper", "approval.expire", "expired request a-sweep"},
	}
	if len(rows) != len(want) {
		t.Fatalf("want %d rows, got %d: %+v", len(want), len(rows), rows)
	}
	for i, w := range want {
		r := rows[i]
		if r.actor != w.actor || r.via != w.via || r.action != w.action || r.rtype != "placeholder" || r.rid != "p1" || !strings.Contains(r.summary, w.word) || r.status != "ok" && r.status != "" {
			t.Errorf("row %d = %+v, want %+v", i, r, w)
		}
	}
}

// Approver token admin routes are audited without the token or hash, and the
// preview-impact dry runs are not audited at all.
func TestApproverAdminAuditHasNoSecretsAndPreviewIsSkipped(t *testing.T) {
	e := newApproverEnv(t)
	id, tok := e.newToken(t, "tui-laptop")
	if rec := e.admin("alice", "DELETE", "/api/approvers/"+id, ""); rec.Code >= 300 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	rows := e.auditRows(t, "resource_type = 'approver_token'")
	if len(rows) != 2 || rows[0].action != "approver_token.create" || rows[1].action != "approver_token.revoke" || rows[0].rid != id {
		t.Fatalf("approver rows: %+v", rows)
	}
	all := e.auditRows(t, "1=1")
	for _, r := range all {
		blob := strings.Join([]string{r.summary, r.before, r.after}, " ")
		if strings.Contains(blob, tok) || strings.Contains(strings.ToLower(blob), "token_hash") || strings.Contains(blob, "dwa_") {
			t.Fatalf("secret in audit row: %+v", r)
		}
	}
	if !strings.Contains(rows[1].after, "revoked_at") {
		t.Fatalf("revoke after-snapshot lacks revoked_at: %+v", rows[1])
	}
	before := len(e.auditRows(t, "1=1"))
	e.admin("alice", "POST", "/api/placeholders/p1/preview-impact", `{"permission_config":""}`)
	e.admin("alice", "POST", "/api/keys/k1/preview-impact", `{"acl":""}`)
	e.admin("alice", "POST", "/api/services/s1/preview-impact", `{"default_acl":""}`)
	if after := len(e.auditRows(t, "1=1")); after != before {
		t.Fatalf("preview-impact was audited: %d -> %d", before, after)
	}
}

// A chat channel's approver list shows in the notification audit before/after.
func TestNotificationApproverListShowsInAudit(t *testing.T) {
	e := newNtEnv(t)
	id := e.create("discord_bot", "approvals-bot", map[string]string{"bot_token": "BOT-SECRET-TOKEN-123", "channel_id": "42", "approver_user_ids": "111"})
	cfg, _ := json.Marshal(map[string]string{"bot_token": "BOT-SECRET-TOKEN-123", "channel_id": "42", "approver_user_ids": "111,222", "approver_role_ids": "900"})
	e.must(e.do("PUT", "/api/notifications/"+id, map[string]string{"name": "approvals-bot", "config": string(cfg)}), 200, 299)
	r := e.one("notification.update")
	if !strings.Contains(r.Before, `"approver_user_ids":"111"`) || !strings.Contains(r.After, "111,222") || !strings.Contains(r.After, "900") {
		t.Fatalf("approver list missing: before=%s after=%s", r.Before, r.After)
	}
	if strings.Contains(r.Before+r.After, "BOT-SECRET-TOKEN-123") {
		t.Fatal("bot token leaked into audit")
	}
}

// A web decision that loses the race (the redirect still says 303) records
// nothing, and a panicking hook never reaches the deciding goroutine.
func TestWebLostRaceAuditsNothingAndHookRecovers(t *testing.T) {
	e := newApproverEnv(t)
	e.addApproval(t, "race-1")
	if rec := e.admin("alice", "POST", "/api/approvals/race-1/approve", ""); rec.Code != 200 {
		t.Fatalf("approve: %d", rec.Code)
	}
	_ = e.admin("bob", "POST", "/api/approvals/race-1/reject", "") // lost: already decided
	rows := e.auditRows(t, "action LIKE 'approval.%'")
	if len(rows) != 1 || rows[0].actor != "admin:alice" || rows[0].via != "web" {
		t.Fatalf("rows: %+v", rows)
	}
	hook := auditDecisionHook(nil, nil) // nil recorder and snapshots would panic
	hook(t.Context(), "x", "discord:1", "discord", "approved")
}

// Failed sign-ins only store the username when it names a real admin.
func TestFailedSignInStoresOnlyKnownUsernames(t *testing.T) {
	e := newNtEnv(t)
	if _, err := e.s.db.Exec(`INSERT INTO admin_users (id, username, password_hash) VALUES ('u1', 'root', '$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ012345')`); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"hunter2-typed-password", "root"} {
		body, _ := json.Marshal(map[string]string{"username": name, "password": "x"})
		req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(string(body)))
		req.RemoteAddr = "198.51.100." + string(rune('1'+i)) + ":1"
		testSameOrigin(req)
		e.s.ServeHTTP(httptest.NewRecorder(), req)
	}
	rows := e.audit("action = 'signin.failed'")
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	blob := fmt.Sprintf("%+v", rows)
	if strings.Contains(blob, "hunter2") {
		t.Fatalf("typed unknown username stored: %s", blob)
	}
	if !strings.Contains(blob, "(unknown user)") || !strings.Contains(blob, "root") {
		t.Fatalf("expected placeholder and real name: %s", blob)
	}
}

func TestGenericRouteVerbs(t *testing.T) {
	if verbOf("POST", false) != "create" || verbOf("POST", true) != "action" || verbOf("PUT", true) != "update" {
		t.Fatal("verbs")
	}
}
