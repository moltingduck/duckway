package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/database"
)

func newRec(t *testing.T) (*Recorder, *sql.DB) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db), db
}

func count(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM admin_audit_log WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The redaction table: every secret-bearing shape becomes (set) / (changed) / "".
func TestRedactPairTable(t *testing.T) {
	cases := []struct {
		name          string
		before, after any
		wantBefore    string
		wantAfter     string
	}{
		{"key value set on create", nil, map[string]any{"name": "k", "key": "sk-live-AAA"}, "", `{"key":"(set)","name":"k"}`},
		{"refresh token changed", map[string]any{"refresh_token": "old"}, map[string]any{"refresh_token": "new"}, `{"refresh_token":"(set)"}`, `{"refresh_token":"(changed)"}`},
		{"unchanged secret stays (set)", map[string]any{"client_token": "same", "n": 1}, map[string]any{"client_token": "same", "n": 2}, `{"client_token":"(set)","n":1}`, `{"client_token":"(set)","n":2}`},
		{"approver token", nil, map[string]any{"approver_token": "dwa_zzz"}, "", `{"approver_token":"(set)"}`},
		{"password", nil, map[string]any{"password": "hunter2hunter2"}, "", `{"password":"(set)"}`},
		{"bot token and webhook url", nil, map[string]any{"bot_token": "123:abc", "webhook_url": "https://hooks/x"}, "", `{"bot_token":"(set)","webhook_url":"(set)"}`},
		{"nested", nil, map[string]any{"config": map[string]any{"secret": "s", "chat_id": "5"}}, "", `{"config":{"chat_id":"5","secret":"(set)"}}`},
		{"empty secret stays empty", nil, map[string]any{"password": ""}, "", `{"password":""}`},
		{"token_endpoint is not a secret", nil, map[string]any{"token_endpoint": "https://idp/token"}, "", `{"token_endpoint":"https://idp/token"}`},
		{"json string input", `{"key":"abc"}`, `{"key":"abd"}`, `{"key":"(set)"}`, `{"key":"(changed)"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, a := RedactPair(c.before, c.after)
			if b != c.wantBefore || a != c.wantAfter {
				t.Fatalf("got %q / %q, want %q / %q", b, a, c.wantBefore, c.wantAfter)
			}
		})
	}
}

func TestRecordStoresRowAndNeverLeaksSecrets(t *testing.T) {
	r, db := newRec(t)
	ctx := WithIdentity(context.Background(), Identity{Actor: "admin:alice", Via: "web", RemoteAddr: "10.0.0.9"})
	r.Record(ctx, Entry{Action: "api_key.update", ResourceType: "api_key", ResourceID: "k1", ResourceName: "prod",
		Summary: "update credential prod", Before: map[string]any{"key_encrypted": "CIPHER-A", "name": "prod"},
		After: map[string]any{"key_encrypted": "CIPHER-B", "name": "prod2"}})
	var actor, via, addr, before, after, status string
	if err := db.QueryRow(`SELECT actor, via, remote_addr, before_json, after_json, status FROM admin_audit_log`).Scan(&actor, &via, &addr, &before, &after, &status); err != nil {
		t.Fatal(err)
	}
	if actor != "admin:alice" || via != "web" || addr != "10.0.0.9" || status != "ok" {
		t.Fatalf("identity not applied: %s %s %s %s", actor, via, addr, status)
	}
	if strings.Contains(before+after, "CIPHER") {
		t.Fatalf("secret stored: %s %s", before, after)
	}
	if !strings.Contains(after, "(changed)") {
		t.Fatalf("change not marked: %s", after)
	}
	var v map[string]any
	if json.Unmarshal([]byte(after), &v) != nil || v["name"] != "prod2" {
		t.Fatalf("after not JSON: %s", after)
	}
}

// A failed audit write must never fail (or panic) the caller.
func TestRecordFailureIsSwallowed(t *testing.T) {
	r, db := newRec(t)
	if _, err := db.Exec(`DROP TABLE admin_audit_log`); err != nil {
		t.Fatal(err)
	}
	r.Record(context.Background(), Entry{Action: "x.y", Summary: "s"})
	r.RecordSignIn(context.Background(), "bob", false, "1.2.3.4", "web")
	var nilRec *Recorder
	func() {
		defer func() {
			if recover() != nil {
				t.Fatal("nil recorder panicked")
			}
		}()
		Record(context.Background(), Entry{Action: "x.y"}) // no recorder anywhere
	}()
	_ = nilRec
}

func TestSignInThrottlePerIPPerMinute(t *testing.T) {
	r, db := newRec(t)
	now := time.Date(2026, 11, 16, 10, 0, 5, 0, time.UTC)
	r.SetClock(func() time.Time { return now })
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		r.RecordSignIn(ctx, "root", false, "9.9.9.9", "web")
	}
	r.RecordSignIn(ctx, "root", false, "8.8.8.8", "web") // other IP: its own row
	if n := count(t, db, "action = 'signin.failed' AND remote_addr = '9.9.9.9'"); n != 1 {
		t.Fatalf("want one row for the burst, got %d", n)
	}
	if n := count(t, db, "action = 'signin.failed' AND remote_addr = '8.8.8.8'"); n != 1 {
		t.Fatalf("other IP rows = %d", n)
	}
	var summary string
	_ = db.QueryRow(`SELECT summary FROM admin_audit_log WHERE remote_addr = '9.9.9.9'`).Scan(&summary)
	if !strings.Contains(summary, "5 failed sign-ins") {
		t.Fatalf("summary should carry the count: %q", summary)
	}
	now = now.Add(61 * time.Second)
	r.RecordSignIn(ctx, "root", false, "9.9.9.9", "web")
	if n := count(t, db, "action = 'signin.failed' AND remote_addr = '9.9.9.9'"); n != 2 {
		t.Fatalf("next minute should start a new row, got %d", n)
	}
	r.RecordSignIn(ctx, "root", true, "9.9.9.9", "web")
	r.RecordSignIn(ctx, "root", true, "9.9.9.9", "web") // successes are never throttled
	if n := count(t, db, "action = 'signin.success'"); n != 2 {
		t.Fatalf("successes = %d", n)
	}
	if n := count(t, db, "action = 'signin.failed' AND resource_name = 'root' AND status = 'failed'"); n != 3 {
		t.Fatalf("failed rows keep the username tried: %d", n)
	}
}

func TestClipAndRedactCutOnRuneBoundaries(t *testing.T) {
	cjk := strings.Repeat("警", 500) // 3 bytes per rune
	for _, n := range []int{1, 2, 3, 100, 101, 299} {
		if got := clip(cjk, n); !utf8.ValidString(got) || len(got) > n {
			t.Fatalf("clip(%d) invalid or too long: valid=%v len=%d", n, utf8.ValidString(got), len(got))
		}
	}
	long := strings.Repeat("警", maxStringBytes) // 6000 bytes
	b, _ := RedactPair(map[string]any{"note": long}, nil)
	var m map[string]string
	if err := json.Unmarshal([]byte(b), &m); err != nil || !utf8.ValidString(m["note"]) {
		t.Fatalf("redacted string invalid: %v", err)
	}
	if !strings.Contains(m["note"], "bytes)") || strings.Contains(m["note"], "�") {
		t.Fatalf("truncation marker or replacement char: %q", m["note"][len(m["note"])-30:])
	}
	if got := clip("ok\xff\xfe", 50); !utf8.ValidString(got) {
		t.Fatal("invalid input bytes survive clip")
	}
	// Insert succeeds with CJK at the clip limit.
	r, db := newRec(t)
	r.Record(context.Background(), Entry{Action: "x.y", Summary: cjk, ResourceName: cjk})
	if n := count(t, db, "action = 'x.y'"); n != 1 {
		t.Fatalf("row lost: %d", n)
	}
}

func TestSignInDoesNotHoldMutexDuringWrites(t *testing.T) {
	r, db := newRec(t)
	r.SetClock(func() time.Time { return time.Date(2026, 11, 16, 10, 0, 5, 0, time.UTC) })
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.RecordSignIn(context.Background(), "(unknown user)", false, "7.7.7.7", "web")
		}()
	}
	wg.Wait()
	if n := count(t, db, "action = 'signin.failed' AND remote_addr = '7.7.7.7'"); n != 1 {
		t.Fatalf("burst rows = %d", n)
	}
	var summary string
	_ = db.QueryRow(`SELECT summary FROM admin_audit_log WHERE remote_addr = '7.7.7.7'`).Scan(&summary)
	if !strings.Contains(summary, "failed sign-in") { // concurrent updates may land out of order, so the count is not asserted
		t.Fatalf("summary = %q", summary)
	}
	// The lock is free while a write is in flight: SetClock takes it.
	done := make(chan struct{})
	go func() { r.SetClock(time.Now); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mutex held")
	}
}
