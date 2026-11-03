package queries

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync/atomic"
	"testing"
	"time"
)

// countingDriver wraps another driver and counts statements that reach the
// database through QueryContext, so tests can prove an aggregate is one query.
type countingDriver struct {
	inner driver.Driver
	n     *atomic.Int64
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: c, n: d.n}, nil
}

type countingConn struct {
	driver.Conn
	n *atomic.Int64
}

func (c *countingConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.n.Add(1)
	if qc, ok := c.Conn.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}

var countingSeq atomic.Int64

func openCountingDB(t *testing.T) (*sql.DB, *atomic.Int64) {
	t.Helper()
	base, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	inner := base.Driver()
	base.Close()
	var n atomic.Int64
	name := "dp-counting-" + time.Now().Format("150405.000000000") + "-" + string(rune('a'+countingSeq.Add(1)%26))
	sql.Register(name, countingDriver{inner: inner, n: &n})
	db, err := sql.Open(name, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db, &n
}

func ts(t time.Time) string { return t.UTC().Format(OverviewTimeFormat) }

func TestTokensSinceAggregatesWindowInOneQuery(t *testing.T) {
	db, n := openCountingDB(t)
	if _, err := db.Exec(`CREATE TABLE conversation_usage (id INTEGER PRIMARY KEY AUTOINCREMENT, api_key_id TEXT NOT NULL DEFAULT '', input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, cache_read_tokens INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []struct {
		key      string
		in, out  int64
		cacheRd  int64
		at       time.Time
		wantSeen bool
	}{
		{"k1", 100, 50, 999, now.Add(-1 * time.Hour), true},
		{"k1", 10, 5, 0, now.Add(-6 * 24 * time.Hour), true},
		{"k1", 7000, 7000, 0, now.Add(-8 * 24 * time.Hour), false},
		{"k2", 1, 2, 0, now.Add(-2 * 24 * time.Hour), true},
		{"k3", 500, 500, 0, now.Add(-30 * 24 * time.Hour), false},
		{"", 40, 40, 0, now.Add(-1 * time.Hour), false},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO conversation_usage (api_key_id, input_tokens, output_tokens, cache_read_tokens, created_at) VALUES (?, ?, ?, ?, ?)`, r.key, r.in, r.out, r.cacheRd, ts(r.at)); err != nil {
			t.Fatal(err)
		}
	}
	before := n.Load()
	got, err := NewAPIKeyQueries(db).TokensSince(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if q := n.Load() - before; q != 1 {
		t.Fatalf("TokensSince issued %d queries, want 1", q)
	}
	if len(got) != 2 || got["k1"] != 165 || got["k2"] != 3 {
		t.Fatalf("got %v, want k1=165 k2=3 only", got)
	}
	if _, ok := got["k3"]; ok {
		t.Fatalf("k3 has no usage in window and must be absent (no token data)")
	}
}

func TestRequestCountsSinceAggregatesWindowInOneQuery(t *testing.T) {
	db, n := openCountingDB(t)
	if _, err := db.Exec(`CREATE TABLE request_log (id INTEGER PRIMARY KEY AUTOINCREMENT, placeholder_id TEXT, client_id TEXT, service_name TEXT NOT NULL DEFAULT '', method TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', status_code INTEGER, decision TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ins := func(ph, client string, at time.Time) {
		var p interface{} = ph
		if ph == "" {
			p = nil
		}
		if _, err := db.Exec(`INSERT INTO request_log (placeholder_id, client_id, created_at) VALUES (?, ?, ?)`, p, client, ts(at)); err != nil {
			t.Fatal(err)
		}
	}
	ins("p1", "c1", now.Add(-time.Hour))
	ins("p1", "c1", now.Add(-6*24*time.Hour))
	ins("p1", "c1", now.Add(-8*24*time.Hour)) // outside window
	ins("p2", "c1", now.Add(-3*24*time.Hour))
	ins("p3", "c2", now.Add(-time.Hour))
	ins("", "c1", now.Add(-time.Hour)) // no phantom token
	// Same predicate as the Overview totals: held, ACL-denied rows and client cancels are not counted.
	for _, x := range []struct {
		decision string
		status   int
	}{{"held", 403}, {"acl_denied", 403}, {"allowed", 0}} {
		if _, err := db.Exec(`INSERT INTO request_log (placeholder_id, client_id, decision, status_code, created_at) VALUES ('p1', 'c1', ?, ?, ?)`, x.decision, x.status, ts(now.Add(-time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	q := NewPlaceholderQueries(db)

	before := n.Load()
	all, err := q.RequestCountsSince(now.Add(-7*24*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if c := n.Load() - before; c != 1 {
		t.Fatalf("RequestCountsSince issued %d queries, want 1", c)
	}
	if len(all) != 3 || all["p1"] != 2 || all["p2"] != 1 || all["p3"] != 1 {
		t.Fatalf("all clients = %v", all)
	}
	one, err := q.RequestCountsSince(now.Add(-7*24*time.Hour), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 2 || one["p1"] != 2 || one["p2"] != 1 {
		t.Fatalf("client c1 = %v", one)
	}
}
