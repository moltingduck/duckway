package queries

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func approverDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	for _, ddl := range []string{
		`CREATE TABLE approver_tokens (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, token_hash TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL DEFAULT (datetime('now')), last_used_at TEXT, revoked_at TEXT)`,
		`CREATE TABLE approvals (id TEXT PRIMARY KEY, placeholder_id TEXT, status TEXT NOT NULL DEFAULT 'pending', approved_at TEXT, expires_at TEXT, request_info TEXT, scope TEXT NOT NULL DEFAULT '', decided_by TEXT NOT NULL DEFAULT '', decided_via TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestApproverTokensStoreOnlyTheHash(t *testing.T) {
	db := approverDB(t)
	q := NewApproverTokenQueries(db)
	const token, hash = "dwa_plaintext-token-value", "hash-of-it"
	if err := q.Create("a1", "alice", hash); err != nil {
		t.Fatal(err)
	}
	var dump string
	rows, err := db.Query(`SELECT id || '|' || name || '|' || token_hash || '|' || created_at FROM approver_tokens`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		dump += s
	}
	rows.Close()
	if strings.Contains(dump, token) {
		t.Fatalf("plaintext token stored: %s", dump)
	}
	tok, stored, err := q.GetActiveByHash(hash)
	if err != nil || tok.Name != "alice" || stored != hash {
		t.Fatalf("lookup: %v %v %q", tok, err, stored)
	}
	list, _ := q.List()
	if len(list) != 1 || list[0].RevokedAt != nil || list[0].LastUsedAt != nil {
		t.Fatalf("list: %+v", list)
	}
}

func TestApproverTokenNamesAreUniqueAndRevokeSticks(t *testing.T) {
	q := NewApproverTokenQueries(approverDB(t))
	if err := q.Create("a1", "alice", "h1"); err != nil {
		t.Fatal(err)
	}
	if err := q.Create("a2", "alice", "h2"); !errors.Is(err, ErrApproverNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := q.Revoke("a1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.GetActiveByHash("h1"); !errors.Is(err, ErrApproverNotFound) {
		t.Fatalf("revoked token must not authenticate: %v", err)
	}
	if err := q.Create("a3", "alice", "h3"); !errors.Is(err, ErrApproverNameTaken) {
		t.Fatalf("a revoked token keeps its name (decided_by stays unambiguous): %v", err)
	}
	if err := q.Revoke("a1"); err != nil {
		t.Fatalf("revoking twice is fine: %v", err)
	}
	if err := q.Revoke("nope"); !errors.Is(err, ErrApproverNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestApproverTouchLastUsedIsThrottled(t *testing.T) {
	db := approverDB(t)
	q := NewApproverTokenQueries(db)
	_ = q.Create("a1", "alice", "h1")
	q.TouchLastUsed("a1")
	var first string
	if err := db.QueryRow(`SELECT last_used_at FROM approver_tokens WHERE id='a1'`).Scan(&first); err != nil || first == "" {
		t.Fatalf("last_used_at not set: %q %v", first, err)
	}
	old := time.Now().UTC().Add(-time.Hour).Format(ApprovalTimeLayout)
	_, _ = db.Exec(`UPDATE approver_tokens SET last_used_at = ? WHERE id='a1'`, old)
	q.TouchLastUsed("a1")
	var got string
	_ = db.QueryRow(`SELECT last_used_at FROM approver_tokens WHERE id='a1'`).Scan(&got)
	if got == old {
		t.Fatal("a stale last_used_at must be refreshed")
	}
	q.TouchLastUsed("a1") // within the minute: unchanged
	var again string
	_ = db.QueryRow(`SELECT last_used_at FROM approver_tokens WHERE id='a1'`).Scan(&again)
	if again != got {
		t.Fatal("last_used_at must not be rewritten within a minute")
	}
}

func TestDecisionAttributionAndFirstDeciderWins(t *testing.T) {
	db := approverDB(t)
	q := NewApprovalQueries(db)
	for _, id := range []string{"a", "b", "c", "old"} {
		_, _ = db.Exec(`INSERT INTO approvals (id, placeholder_id) VALUES (?, 'p')`, id)
	}
	if err := q.ApproveBy("a", ApprovalExpiry(time.Hour), "admin:alice", "web"); err != nil {
		t.Fatal(err)
	}
	if err := q.RejectBy("a", "discord:9", "discord"); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("second decision must lose: %v", err)
	}
	d, err := q.GetDecision("a")
	if err != nil || d.Status != "approved" || d.DecidedBy != "admin:alice" || d.DecidedVia != "web" {
		t.Fatalf("decision: %+v %v", d, err)
	}
	if err := q.RejectBy("b", "approver:ci", "tui"); err != nil {
		t.Fatal(err)
	}
	if d, _ := q.GetDecision("b"); d.Status != "rejected" || d.DecidedBy != "approver:ci" || d.DecidedVia != "tui" {
		t.Fatalf("reject decision: %+v", d)
	}
	if d, _ := q.GetDecision("c"); d.Status != "pending" || d.DecidedBy != "" {
		t.Fatalf("pending decision: %+v", d)
	}
	if _, err := q.GetDecision("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing: %v", err)
	}
	_, _ = db.Exec(`UPDATE approvals SET created_at = datetime('now','-3 hours') WHERE id IN ('old')`)
	ids, err := q.ExpirePending(60)
	if err != nil || len(ids) != 1 || ids[0] != "old" {
		t.Fatalf("expire: %v %v", ids, err)
	}
	if d, _ := q.GetDecision("old"); d.Status != "ignored" || d.DecidedBy != SweeperDecidedBy || d.DecidedVia != SweeperDecidedVia {
		t.Fatalf("sweeper decision: %+v", d)
	}
}

func TestApproverCreateRaceMapsToNameTaken(t *testing.T) {
	q := NewApproverTokenQueries(approverDB(t))
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = q.Create(fmt.Sprintf("r%d", i), "racer", fmt.Sprintf("hash%d", i))
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrApproverNameTaken):
			t.Fatalf("raw error leaked: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("want exactly one winner, got %d", ok)
	}
}
