package queries_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func logDB(t *testing.T) (*sql.DB, *queries.RequestLogQueries) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, id := range []string{"c1", "c2"} {
		if _, err := db.Exec(`INSERT INTO clients (id, name, token_hash) VALUES (?, ?, ?)`, id, id, "h"+id); err != nil {
			t.Fatal(err)
		}
	}
	return db, queries.NewRequestLogQueries(db)
}

func countWhere(t *testing.T, db *sql.DB, where string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStoredPathIsTruncatedForEveryRow(t *testing.T) {
	db, q := logDB(t)
	long := "/" + strings.Repeat("a", 5000)
	multi := "/" + strings.Repeat("é", 2000) // 2 bytes per rune: the cut must not split a rune
	if err := q.Log("c1", "", "s", "GET", long, 200); err != nil {
		t.Fatal(err)
	}
	if _, err := q.LogWithReturn("c1", "", "s", "GET", multi, 200); err != nil {
		t.Fatal(err)
	}
	if err := q.LogACLDenied("c1", "", "s", "GET", long); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT path FROM request_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		n++
		if len(p) > queries.MaxLoggedPathBytes {
			t.Errorf("stored path has %d bytes, want <= %d", len(p), queries.MaxLoggedPathBytes)
		}
		if p != strings.ToValidUTF8(p, "") {
			t.Error("a multi-byte rune was cut in half")
		}
	}
	if n != 3 {
		t.Fatalf("rows=%d", n)
	}
}

// Repeated denials of the same request in the same minute store one row.
func TestACLDeniedRepeatsAreCoalescedPerMinute(t *testing.T) {
	db, q := logDB(t)
	for i := 0; i < 20; i++ {
		if err := q.LogACLDenied("c1", "", "s", "POST", "/v1/admin"); err != nil {
			t.Fatal(err)
		}
	}
	if n := countWhere(t, db, `decision = 'acl_denied'`); n != 1 {
		t.Fatalf("20 identical denials stored %d rows, want 1", n)
	}
	// A different client, method or path is a different denial.
	_ = q.LogACLDenied("c2", "", "s", "POST", "/v1/admin")
	_ = q.LogACLDenied("c1", "", "s", "GET", "/v1/admin")
	_ = q.LogACLDenied("c1", "", "s", "POST", "/v1/other")
	if n := countWhere(t, db, `decision = 'acl_denied'`); n != 4 {
		t.Fatalf("distinct denials stored %d rows, want 4", n)
	}
	// An older denial (previous minutes) does not suppress a new one.
	if _, err := db.Exec(`UPDATE request_log SET created_at = ? WHERE client_id = 'c1' AND method = 'POST' AND path = '/v1/admin'`,
		time.Now().UTC().Add(-3*time.Minute).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	_ = q.LogACLDenied("c1", "", "s", "POST", "/v1/admin")
	if n := countWhere(t, db, `decision = 'acl_denied' AND client_id = 'c1' AND path = '/v1/admin' AND method = 'POST'`); n != 2 {
		t.Fatalf("denial after the minute boundary: %d rows, want 2", n)
	}
}

func TestHeldRowOnlyForNewApprovalOrAfter60s(t *testing.T) {
	db, q := logDB(t)
	// A new approval always gets its row.
	if err := q.LogHeldOnce("c1", "", "s", "POST", "/x", "ap1", true); err != nil {
		t.Fatal(err)
	}
	// Retries of a pending approval within 60s add nothing.
	for i := 0; i < 10; i++ {
		if err := q.LogHeldOnce("c1", "", "s", "POST", "/x", "ap1", false); err != nil {
			t.Fatal(err)
		}
	}
	if n := countWhere(t, db, `decision = 'held' AND approval_id = 'ap1'`); n != 1 {
		t.Fatalf("held rows=%d, want 1", n)
	}
	// Once the last held row is older than 60s, a retry is recorded again.
	if _, err := db.Exec(`UPDATE request_log SET created_at = ? WHERE approval_id = 'ap1'`, time.Now().UTC().Add(-2*time.Minute).Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	_ = q.LogHeldOnce("c1", "", "s", "POST", "/x", "ap1", false)
	if n := countWhere(t, db, `decision = 'held' AND approval_id = 'ap1'`); n != 2 {
		t.Fatalf("held rows after 60s=%d, want 2", n)
	}
	// Another approval is independent.
	_ = q.LogHeldOnce("c1", "", "s", "POST", "/x", "ap2", false)
	if n := countWhere(t, db, `decision = 'held' AND approval_id = 'ap2'`); n != 1 {
		t.Fatalf("a different approval with no row yet must log: %d", n)
	}
}

func TestRequestLogRetentionPrunesInBatches(t *testing.T) {
	db, q := logDB(t)
	old := time.Now().UTC().AddDate(0, 0, -100).Format("2006-01-02 15:04:05")
	recent := time.Now().UTC().AddDate(0, 0, -5).Format("2006-01-02 15:04:05")
	for i := 0; i < 25; i++ {
		if _, err := db.Exec(`INSERT INTO request_log (client_id, service_name, method, path, status_code, created_at) VALUES ('c1','s','GET','/o',200,?)`, old); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := db.Exec(`INSERT INTO request_log (client_id, service_name, method, path, status_code, created_at) VALUES ('c1','s','GET','/r',200,?)`, recent); err != nil {
			t.Fatal(err)
		}
	}
	var oldID int64
	_ = db.QueryRow(`SELECT id FROM request_log WHERE path = '/o' LIMIT 1`).Scan(&oldID)
	if err := q.StoreDetail(&queries.RequestLogDetail{LogID: oldID}); err != nil {
		t.Fatal(err)
	}
	// Batch of 10, at most 1 batch: deletes exactly 10 and stops.
	if n, err := q.PruneOlderThan(90, 10, 1); err != nil || n != 10 {
		t.Fatalf("limited sweep removed %d (err=%v), want 10", n, err)
	}
	// Unlimited batches: the rest of the old rows go; recent rows stay.
	n, err := q.PruneOlderThan(90, 10, 0)
	if err != nil || n != 15 {
		t.Fatalf("full sweep removed %d (err=%v), want 15", n, err)
	}
	if countWhere(t, db, `path = '/r'`) != 3 || countWhere(t, db, `path = '/o'`) != 0 {
		t.Fatal("only rows older than the retention window may be removed")
	}
	var d int
	_ = db.QueryRow(`SELECT COUNT(*) FROM request_log_detail`).Scan(&d)
	if d != 0 {
		t.Fatalf("captured details of pruned rows must go too: %d", d)
	}
	if n, _ := q.PruneOlderThan(0, 10, 0); n != 0 {
		t.Fatal("an invalid retention must never delete anything")
	}
}

// One client probing many different paths cannot write more than 30 denial rows a minute.
func TestACLDeniedPerClientPerMinuteCap(t *testing.T) {
	db, q := logDB(t)
	for i := 0; i < 100; i++ {
		if err := q.LogACLDenied("c1", "", "s", "GET", "/probe/"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('A'+i/26))); err != nil {
			t.Fatal(err)
		}
	}
	if n := countWhere(t, db, `decision = 'acl_denied' AND client_id = 'c1'`); n != queries.MaxACLDeniedRowsPerMinute {
		t.Fatalf("c1 wrote %d denial rows, want the cap %d", n, queries.MaxACLDeniedRowsPerMinute)
	}
	// The cap is per client: another client still logs.
	if err := q.LogACLDenied("c2", "", "s", "GET", "/probe/other"); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, db, `decision = 'acl_denied' AND client_id = 'c2'`); n != 1 {
		t.Fatalf("c2 rows=%d", n)
	}
	// Identical repeats are still coalesced under the cap.
	for i := 0; i < 5; i++ {
		_ = q.LogACLDenied("c2", "", "s", "GET", "/probe/other")
	}
	if n := countWhere(t, db, `decision = 'acl_denied' AND client_id = 'c2'`); n != 1 {
		t.Fatalf("c2 identical repeats stored %d rows", n)
	}
}

// Invalid UTF-8 is replaced before storing (PostgreSQL would reject it), at any length.
func TestLogPathInvalidUTF8IsReplaced(t *testing.T) {
	db, q := logDB(t)
	bad := "/v1/\xff\xfe/ok\xc3"
	if err := q.Log("c1", "", "s", "GET", bad, 200); err != nil {
		t.Fatal(err)
	}
	long := "/" + strings.Repeat("a", 3000) + "\xff"
	if err := q.Log("c1", "", "s", "GET", long, 200); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT path FROM request_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		got = append(got, p)
	}
	if got[0] != "/v1/�/ok�" {
		t.Fatalf("path=%q", got[0])
	}
	for _, p := range got {
		if p != strings.ToValidUTF8(p, "") || len(p) > queries.MaxLoggedPathBytes {
			t.Fatalf("stored path must be valid UTF-8 and capped: %d bytes", len(p))
		}
	}
}
