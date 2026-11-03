package handlers_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

type logRow struct {
	status   int
	decision string
	path     string
}

func logRows(t *testing.T, db *sql.DB) []logRow {
	t.Helper()
	rows, err := db.Query(`SELECT COALESCE(status_code, -1), decision, path FROM request_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.status, &r.decision, &r.path); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// An unreachable upstream is logged as a 502 "allowed" row with method and
// path only (no query, no body, no key material).
func TestUpstreamUnreachableIsLogged502(t *testing.T) {
	f := newProxyFixture(t, "http://127.0.0.1:1") // nothing listens here
	h := newProxyHandler(f)
	r := httptest.NewRequest("POST", "/proxy/anthropic/v1/messages?api_key=SECRET-Q", strings.NewReader(`{"secret":"BODYSECRET"}`))
	code, _ := doProxy(h, withClient(r, f.client))
	if code != http.StatusBadGateway {
		t.Fatalf("code=%d, want 502", code)
	}
	rows := logRows(t, f.rawDB)
	if len(rows) != 1 || rows[0].status != 502 || rows[0].decision != "allowed" || rows[0].path != "/v1/messages" {
		t.Fatalf("rows=%+v", rows)
	}
	var dump string
	_ = f.rawDB.QueryRow(`SELECT COALESCE(group_concat(path || method || service_name), '') FROM request_log`).Scan(&dump)
	if strings.Contains(dump, "SECRET") || strings.Contains(dump, "BODY") {
		t.Fatalf("request_log must not hold the query or body: %s", dump)
	}
}

// A forward let through by a valid token-wide approval is labelled approved_forward.
func TestTokenWideApprovedForwardIsLabelled(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(up.Close)
	f := newProxyFixture(t, up.URL)
	ph, _ := f.placeholderQ.GetByID(f.placeholderID)
	ph.RequiresApproval = true
	if err := f.placeholderQ.Update(ph); err != nil {
		t.Fatal(err)
	}
	h := newProxyHandler(f)
	do := func() int {
		code, _ := doProxy(h, withClient(httptest.NewRequest("GET", "/proxy/anthropic/v1/models", nil), f.client))
		return code
	}
	if code := do(); code != http.StatusForbidden {
		t.Fatalf("unapproved: %d", code)
	}
	var id string
	if err := f.rawDB.QueryRow(`SELECT id FROM approvals WHERE placeholder_id = ? AND status = 'pending'`, f.placeholderID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := f.approvalQ.Approve(id, queries.ApprovalExpiry(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code := do(); code != http.StatusOK {
		t.Fatalf("approved: %d", code)
	}
	rows := logRows(t, f.rawDB)
	if len(rows) != 2 || rows[0].decision != "held" || rows[1].decision != "approved_forward" {
		t.Fatalf("rows=%+v, want held then approved_forward", rows)
	}
}

// Ten retries of one held request log one held row; ten identical ACL denials log one row.
func TestHeldRetriesAndDeniedRepeatsDoNotFloodTheLog(t *testing.T) {
	rig := newPathRig(t, approvePostsACL)
	for i := 0; i < 10; i++ {
		rig.doBody("POST", "/v1/messages?x=1", `{"a":1}`)
	}
	held := 0
	for _, r := range logRows(t, rig.f.rawDB) {
		if r.decision == "held" {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("held rows=%d, want 1", held)
	}

	const denyACL = `{"version":"1","provider":"anthropic","rules":[{"name":"no","endpoints":[{"method":"DELETE","path":"/v1/*","allow":false}]}]}`
	rig2 := newPathRig(t, denyACL)
	for i := 0; i < 10; i++ {
		if code := rig2.do("DELETE", "/v1/things"); code != http.StatusForbidden {
			t.Fatalf("denied request: %d", code)
		}
	}
	denied := 0
	for _, r := range logRows(t, rig2.f.rawDB) {
		if r.decision == "acl_denied" {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("acl_denied rows=%d, want 1 (coalesced per minute)", denied)
	}
}
