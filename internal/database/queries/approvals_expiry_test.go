package queries

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestApproveStoresGoComputedUTCExpiry(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE approvals (id TEXT PRIMARY KEY, placeholder_id TEXT, status TEXT NOT NULL DEFAULT 'pending', approved_at TEXT, expires_at TEXT, request_info TEXT, scope TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatal(err)
	}
	q := NewApprovalQueries(db)
	for _, id := range []string{"future", "past", "scoped"} {
		if _, err := db.Exec(`INSERT INTO approvals (id, placeholder_id) VALUES (?, 'p')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE approvals SET scope='rule:POST /x' WHERE id='scoped'`); err != nil {
		t.Fatal(err)
	}
	if err := q.Approve("future", ApprovalExpiry(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var exp string
	if err := db.QueryRow(`SELECT expires_at FROM approvals WHERE id='future'`).Scan(&exp); err != nil || len(exp) != 19 {
		t.Fatalf("expires_at must be a concrete timestamp, got %q (%v)", exp, err)
	}
	if got, err := q.GetValidApproval("p"); err != nil || got.ID != "future" {
		t.Fatalf("valid approval: %v %v", got, err)
	}
	if err := q.Approve("past", "2000-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	if err := q.Approve("scoped", ApprovalExpiry(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := q.GetValidScopedApproval("p", "rule:POST /x"); err != nil || got.ID != "scoped" {
		t.Fatalf("scoped lookup: %v %v", got, err)
	}
	if _, err := q.GetValidScopedApproval("p", "rule:POST /y"); err != sql.ErrNoRows {
		t.Fatalf("different signature must not match: %v", err)
	}
	if _, err := q.GetValidScopedApproval("p", ""); err != sql.ErrNoRows {
		t.Fatalf("empty scope must never match: %v", err)
	}
	// Anything that is not a UTC timestamp is an error (no SQL expressions, no
	// relative strings, no other layouts) and changes nothing.
	for _, bad := range []string{"datetime('now', '+60 minutes')", "datetime('now', '+1 minutes'); DROP TABLE approvals", "", "2099-01-01", "2099-01-01T00:00:00Z", "tomorrow"} {
		if err := q.Approve("future", bad); err == nil {
			t.Errorf("Approve(%q) must be rejected", bad)
		}
	}
	if _, err := db.Exec(`SELECT 1 FROM approvals`); err != nil {
		t.Fatal(err)
	}
}

func TestClampApprovalMinutes(t *testing.T) {
	for in, want := range map[int]int{-5: 1, 0: 1, 1: 1, 90: 90, MaxApprovalMinutes: MaxApprovalMinutes, MaxApprovalMinutes + 1: MaxApprovalMinutes, 1 << 40: MaxApprovalMinutes} {
		if got := ClampApprovalMinutes(in); got != want {
			t.Errorf("Clamp(%d)=%d want %d", in, got, want)
		}
	}
	if ApprovalExpiry(time.Duration(ClampApprovalMinutes(1<<40))*time.Minute) > time.Now().UTC().AddDate(1, 0, 2).Format(ApprovalTimeLayout) {
		t.Fatal("clamped expiry must be at most about one year out")
	}
}
