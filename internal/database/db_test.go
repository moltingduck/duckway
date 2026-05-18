package database

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestOpenAppliesPragmas verifies that the DSN actually enables WAL, sets
// busy_timeout, and turns on foreign keys. Earlier versions of db.go
// passed mattn-style query params (`_journal_mode=WAL`) that the
// modernc.org/sqlite driver silently ignored, so WAL was never on.
func TestOpenAppliesPragmas(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}
}

// TestBusyTimeoutRetries proves busy_timeout actually engages: hold a
// write transaction on one connection while another goroutine attempts
// a write — without retry the second write would fail immediately with
// SQLITE_BUSY, but with our busy_timeout the second goroutine waits
// until the first commits.
//
// We force MaxOpenConns(2) for this test only — production runs with
// 1 conn per process, but to demonstrate the contention path we need
// two real connections to the same file.
func TestBusyTimeoutRetries(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	holdReleased := make(chan struct{})
	holdAcquired := make(chan struct{})

	// Goroutine A: BEGIN IMMEDIATE, hold the write lock for 1 second.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Errorf("beginA: %v", err)
			return
		}
		if _, err := tx.Exec("INSERT INTO t(v) VALUES('A')"); err != nil {
			t.Errorf("insertA: %v", err)
			return
		}
		close(holdAcquired)
		time.Sleep(1 * time.Second)
		if err := tx.Commit(); err != nil {
			t.Errorf("commitA: %v", err)
			return
		}
		close(holdReleased)
	}()

	// Wait for A to hold the lock, then have B try to write. Without
	// busy_timeout, B fails immediately; with 5s busy_timeout, B waits
	// for A to commit and then succeeds.
	<-holdAcquired
	start := time.Now()
	_, err = db.ExecContext(ctx, "INSERT INTO t(v) VALUES('B')")
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("B insert (busy_timeout should have retried): %v", err)
	}
	if dur < 500*time.Millisecond {
		t.Errorf("B insert returned in %v — busy_timeout didn't actually wait for A's commit", dur)
	}

	<-holdReleased
	wg.Wait()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("rows after both inserts = %d, want 2", count)
	}
}
