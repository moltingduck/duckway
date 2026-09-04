package queries

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openTestDB spins up a sqlite database with the minimal schema the inbox
// tests need. We don't run the full migrations.go — just the tables that
// are touched, so the test stays focused.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_foreign_keys=off")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE control_channels (id TEXT PRIMARY KEY, name TEXT, service_id TEXT, api_key_id TEXT, client_id TEXT, agent_type TEXT, placeholder_id TEXT, config TEXT, is_active INT, created_at TEXT)`,
		`CREATE TABLE cc_channels (handle TEXT PRIMARY KEY, cc_id TEXT, client_id TEXT, channel_id TEXT, name TEXT, topic TEXT, kind TEXT, session_id TEXT, cwd TEXT, archived INT, created_at TEXT, last_seen_at TEXT)`,
		`CREATE TABLE discord_inbox (id INTEGER PRIMARY KEY AUTOINCREMENT, cc_id TEXT, channel_handle TEXT, event_type TEXT, payload TEXT,
		 event_key TEXT NOT NULL DEFAULT '', lane_key TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'admitted',
		 claim_token TEXT NOT NULL DEFAULT '', lease_expires_at TEXT, attempt_count INTEGER NOT NULL DEFAULT 0,
		 last_error TEXT NOT NULL DEFAULT '', completed_at TEXT, created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE UNIQUE INDEX idx_inbox_event_key ON discord_inbox(cc_id,event_key) WHERE event_key != ''`,
		`CREATE TABLE cc_message_deliveries (cc_id TEXT, channel_handle TEXT, delivery_key TEXT, content_digest BLOB, message_id TEXT NOT NULL DEFAULT '', created_at TEXT DEFAULT CURRENT_TIMESTAMP, updated_at TEXT DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY(cc_id,delivery_key))`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO control_channels VALUES ('cc1','x','svc','key','client1','claude_code','ph1','{}',1,datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cc_channels VALUES ('h1','cc1','c1','D1','home1','','management','','',0,datetime('now'),null)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestInboxAppendAndPull(t *testing.T) {
	db := openTestDB(t)
	q := NewControlChannelQueries(db)

	handle := "h1"
	var lastID int64
	for i := 0; i < 5; i++ {
		id, err := q.AppendInbox("cc1", &handle, "MESSAGE_CREATE", `{"i":1}`)
		if err != nil {
			t.Fatal(err)
		}
		lastID = id
	}

	got, err := q.PullInbox("cc1", 0, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("expected 5, got %d", len(got))
	}
	// Ascending order
	for i := 1; i < len(got); i++ {
		if got[i].ID <= got[i-1].ID {
			t.Errorf("ids not ascending at %d", i)
		}
	}

	// Cursor advances
	got2, err := q.PullInbox("cc1", got[2].ID, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 2 {
		t.Errorf("after cursor=%d, got %d (want 2)", got[2].ID, len(got2))
	}

	latest, err := q.LatestInboxID("cc1")
	if err != nil {
		t.Fatal(err)
	}
	if latest != lastID {
		t.Errorf("latest inbox id = %d, want %d", latest, lastID)
	}
}

func TestInboxFilterByHandle(t *testing.T) {
	db := openTestDB(t)
	q := NewControlChannelQueries(db)
	if _, err := db.Exec(`INSERT INTO cc_channels VALUES ('h2','cc1','c2','D2','home2','','task','','',0,datetime('now'),null)`); err != nil {
		t.Fatal(err)
	}
	h1, h2 := "h1", "h2"
	_, _ = q.AppendInbox("cc1", &h1, "MESSAGE_CREATE", `{}`)
	_, _ = q.AppendInbox("cc1", &h2, "MESSAGE_CREATE", `{}`)
	_, _ = q.AppendInbox("cc1", &h1, "MESSAGE_CREATE", `{}`)

	only := func(events []interface{}) {}
	_ = only
	got, err := q.PullInbox("cc1", 0, []string{"h1"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 h1 events, got %d", len(got))
	}
	for _, e := range got {
		if e.ChannelHandle == nil || *e.ChannelHandle != "h1" {
			t.Errorf("filter leak: got %v", e.ChannelHandle)
		}
	}
}

func TestInboxCleanupRetention(t *testing.T) {
	db := openTestDB(t)
	q := NewControlChannelQueries(db)

	// Insert one row with a backdated created_at and one row with now.
	if _, err := db.Exec(`INSERT INTO discord_inbox (cc_id, channel_handle, event_type, payload, created_at)
	  VALUES ('cc1', 'h1', 'old', '{}', datetime('now', '-48 hours'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.AppendInbox("cc1", strPtr("h1"), "fresh", "{}"); err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(`UPDATE discord_inbox SET status='completed'`)

	// Retention 24h should drop the old one but keep fresh.
	if err := q.CleanupInbox(24, 1000); err != nil {
		t.Fatal(err)
	}
	got, _ := q.PullInbox("cc1", 0, nil, 100)
	if len(got) != 1 || got[0].EventType != "fresh" {
		t.Errorf("retention failed: got %+v", got)
	}
}

func TestInboxCleanupPerChannelCap(t *testing.T) {
	db := openTestDB(t)
	q := NewControlChannelQueries(db)
	for i := 0; i < 5; i++ {
		_, _ = q.AppendInbox("cc1", strPtr("h1"), "x", `{}`)
		// Tiny sleep so created_at differs (sqlite second resolution); also
		// the id ordering already guarantees newest-N selection.
		time.Sleep(time.Millisecond)
	}
	_, _ = db.Exec(`UPDATE discord_inbox SET status='completed'`)

	// Cap at 2: should keep newest 2.
	if err := q.CleanupInbox(0, 2); err != nil {
		t.Fatal(err)
	}
	got, _ := q.PullInbox("cc1", 0, nil, 100)
	if len(got) != 2 {
		t.Errorf("cap failed: got %d, want 2", len(got))
	}
}

func TestInboxAdmissionDeduplicatesEventKey(t *testing.T) {
	db := openTestDB(t)
	q := NewControlChannelQueries(db)
	id1, inserted, err := q.AdmitInboxDetailed("cc1", strPtr("h1"), "MESSAGE_CREATE", "MESSAGE_CREATE:123", "h1", `{}`)
	if err != nil || !inserted {
		t.Fatalf("first admission id=%d inserted=%v err=%v", id1, inserted, err)
	}
	id2, inserted, err := q.AdmitInboxDetailed("cc1", strPtr("h1"), "MESSAGE_CREATE", "MESSAGE_CREATE:123", "h1", `{}`)
	if err != nil || inserted || id2 != id1 {
		t.Fatalf("duplicate id=%d inserted=%v err=%v", id2, inserted, err)
	}
}

func TestInboxClaimEnforcesLaneFIFOAndToken(t *testing.T) {
	db := openTestDB(t)
	q := NewControlChannelQueries(db)
	first, _ := q.AdmitInbox("cc1", strPtr("h1"), "MESSAGE_CREATE", "MESSAGE_CREATE:1", "h1", `{}`)
	second, _ := q.AdmitInbox("cc1", strPtr("h1"), "MESSAGE_CREATE", "MESSAGE_CREATE:2", "h1", `{}`)
	other, _ := q.AdmitInbox("cc1", strPtr("h2"), "MESSAGE_CREATE", "MESSAGE_CREATE:3", "h2", `{}`)

	a, err := q.ClaimInbox("cc1", 120)
	if err != nil || a.ID != first || a.ClaimToken == "" {
		t.Fatalf("first claim=%+v err=%v", a, err)
	}
	b, err := q.ClaimInbox("cc1", 120)
	if err != nil || b.ID != other {
		t.Fatalf("parallel lane claim=%+v err=%v", b, err)
	}
	if err := q.FinishInbox("cc1", first, "wrong", "completed", ""); err != sql.ErrNoRows {
		t.Fatalf("wrong token err=%v", err)
	}
	if err := q.FinishInbox("cc1", first, a.ClaimToken, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if err := q.FinishInbox("cc1", other, b.ClaimToken, "completed", ""); err != nil {
		t.Fatal(err)
	}
	c, err := q.ClaimInbox("cc1", 120)
	if err != nil || c.ID != second {
		t.Fatalf("second lane head=%+v err=%v", c, err)
	}
}

func TestInboxExpiredLeaseIsReclaimedBeforeLaterLaneItem(t *testing.T) {
	db := openTestDB(t)
	q := NewControlChannelQueries(db)
	first, _ := q.AdmitInbox("cc1", strPtr("h1"), "MESSAGE_CREATE", "MESSAGE_CREATE:1", "h1", `{}`)
	_, _ = q.AdmitInbox("cc1", strPtr("h1"), "MESSAGE_CREATE", "MESSAGE_CREATE:2", "h1", `{}`)
	a, _ := q.ClaimInbox("cc1", 120)
	_, _ = db.Exec(`UPDATE discord_inbox SET lease_expires_at=datetime('now','-1 second') WHERE id=?`, first)
	b, err := q.ClaimInbox("cc1", 120)
	if err != nil || b.ID != first || b.ClaimToken == a.ClaimToken || b.AttemptCount != 2 {
		t.Fatalf("reclaim=%+v first=%+v err=%v", b, a, err)
	}
}

func strPtr(s string) *string { return &s }

func TestMessageDeliveryIsDurableAndRejectsKeyReuse(t *testing.T) {
	db := openTestDB(t)
	q := NewControlChannelQueries(db)
	digest := bytes.Repeat([]byte{1}, 32)
	if id, err := q.BeginMessageDelivery("cc1", "h1", "task:0", digest); err != nil || id != "" {
		t.Fatalf("begin id=%q err=%v", id, err)
	}
	if err := q.CompleteMessageDelivery("cc1", "task:0", "M1"); err != nil {
		t.Fatal(err)
	}
	if id, err := q.BeginMessageDelivery("cc1", "h1", "task:0", digest); err != nil || id != "M1" {
		t.Fatalf("replay id=%q err=%v", id, err)
	}
	if _, err := q.BeginMessageDelivery("cc1", "h1", "task:0", bytes.Repeat([]byte{2}, 32)); !errors.Is(err, ErrDeliveryConflict) {
		t.Fatalf("conflicting replay err=%v", err)
	}
	if _, err := q.BeginMessageDelivery("cc1", "h1", "pending:0", digest); err != nil {
		t.Fatal(err)
	}
	if safe, err := q.MessageDeliveryRetrySafe("cc1", "pending:0"); err != nil || !safe {
		t.Fatalf("new pending safe=%v err=%v", safe, err)
	}
	_, _ = db.Exec(`UPDATE cc_message_deliveries SET created_at=datetime('now','-11 minutes') WHERE delivery_key='pending:0'`)
	if safe, err := q.MessageDeliveryRetrySafe("cc1", "pending:0"); err != nil || safe {
		t.Fatalf("old pending safe=%v err=%v", safe, err)
	}
}
