package queries

import (
	"context"
	"testing"
)

func openWatchDB(t *testing.T) *WatchQueries {
	t.Helper()
	db := openTestDB(t)
	for _, s := range []string{
		`CREATE TABLE request_log (id INTEGER PRIMARY KEY AUTOINCREMENT, service_name TEXT, method TEXT, path TEXT, created_at TEXT DEFAULT (datetime('now')))`,
		`CREATE TABLE approvals (id TEXT PRIMARY KEY, placeholder_id TEXT, status TEXT NOT NULL DEFAULT 'pending', approved_at TEXT, expires_at TEXT, request_info TEXT, created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE TABLE client_update_rollouts (id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'active', updated_at TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE TABLE client_update_jobs (id TEXT PRIMARY KEY, rollout_id TEXT, status TEXT NOT NULL DEFAULT 'queued', updated_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return NewWatchQueries(db)
}

func TestWatchSnapshotChangesOnlyOnRelevantWrites(t *testing.T) {
	q := openWatchDB(t)
	db := q.db
	ctx := context.Background()
	snap := func() WatchSnapshot {
		s, err := q.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	base := snap()
	if base != snap() {
		t.Fatal("snapshot not stable when nothing changed")
	}

	db.Exec(`INSERT INTO request_log (service_name, method, path) VALUES ('s','GET','/')`)
	s := snap()
	if s.LogMaxID != 1 || s.ApprovalsSig != base.ApprovalsSig || s.RolloutsSig != base.RolloutsSig {
		t.Fatalf("log insert: %+v", s)
	}

	db.Exec(`INSERT INTO approvals (id, placeholder_id) VALUES ('a','p')`)
	s2 := snap()
	if s2.ApprovalsPend != 1 || s2.ApprovalsSig == s.ApprovalsSig || s2.LogMaxID != 1 {
		t.Fatalf("approval insert: %+v", s2)
	}
	db.Exec(`UPDATE approvals SET status='approved', approved_at='2026-01-01 00:00:00' WHERE id='a'`)
	s3 := snap()
	if s3.ApprovalsPend != 0 || s3.ApprovalsSig == s2.ApprovalsSig {
		t.Fatalf("approval decide: %+v", s3)
	}

	db.Exec(`INSERT INTO client_update_rollouts (id) VALUES ('r')`)
	s4 := snap()
	if s4.RolloutsSig == s3.RolloutsSig || s4.ApprovalsSig != s3.ApprovalsSig {
		t.Fatalf("rollout insert: %+v", s4)
	}
	db.Exec(`INSERT INTO client_update_jobs (id, rollout_id) VALUES ('j','r')`)
	s5 := snap()
	if s5.RolloutsSig == s4.RolloutsSig {
		t.Fatal("job insert not detected")
	}
	db.Exec(`UPDATE client_update_jobs SET status='succeeded' WHERE id='j'`)
	if snap().RolloutsSig == s5.RolloutsSig {
		t.Fatal("job status change not detected")
	}
}
