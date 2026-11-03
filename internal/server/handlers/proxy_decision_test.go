package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

type decisionRow struct {
	Decision, ApprovalID, Path, Method string
	Status                             int
}

func decisionRows(t *testing.T, rig *approveRig) []decisionRow {
	t.Helper()
	rows, err := rig.f.rawDB.Query(`SELECT decision, approval_id, path, method, status_code FROM request_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []decisionRow
	for rows.Next() {
		var r decisionRow
		if err := rows.Scan(&r.Decision, &r.ApprovalID, &r.Path, &r.Method, &r.Status); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

const decisionACL = `{"version":"1","provider":"anthropic","rules":[{"name":"r","deny_all_other":true,"endpoints":[
	{"method":"GET","path":"/v1/*","allow":true},
	{"method":"POST","path":"/v1/messages","approve":true}]}]}`

func TestProxyLogsDecisions(t *testing.T) {
	rig := newApproveRig(t, decisionACL)

	if code, _ := rig.do("GET", "/v1/models"); code != http.StatusOK {
		t.Fatalf("allowed: %d", code)
	}
	if code, _ := rig.do("DELETE", "/v1/models"); code != http.StatusForbidden {
		t.Fatalf("acl denial: %d", code)
	}
	code, held := rig.do("POST", "/v1/messages")
	if code != http.StatusForbidden || held["approval_id"] == "" {
		t.Fatalf("held: %d %v", code, held)
	}
	if err := rig.f.approvalQ.Approve(held["approval_id"], queries.ApprovalExpiry(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code, _ := rig.do("POST", "/v1/messages"); code != http.StatusOK {
		t.Fatalf("approved forward: %d", code)
	}

	got := decisionRows(t, rig)
	want := []decisionRow{
		{"allowed", "", "/v1/models", "GET", 200},
		{"acl_denied", "", "/v1/models", "DELETE", 403},
		{"held", held["approval_id"], "/v1/messages", "POST", 403},
		{"approved_forward", "", "/v1/messages", "POST", 200},
	}
	if len(got) != len(want) {
		t.Fatalf("rows: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}
