package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func Open(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "duckway.db")
	// _busy_timeout=5000: when another process (split admin/gateway
	// deployment) is mid-write, SQLite returns SQLITE_BUSY immediately by
	// default. With a busy timeout the driver retries internally for up
	// to 5 seconds before surfacing the error — covers normal contention
	// without making the caller deal with retries.
	//
	// _journal_mode=WAL: readers don't block on writers and vice versa,
	// which is what lets the split processes coexist on one file.
	//
	// _txlock=immediate: take the write lock at BEGIN time instead of at
	// the first write within the transaction. Avoids the upgrade-deadlock
	// pattern where two transactions hold read locks then both try to
	// upgrade to write.
	db, err := sql.Open("sqlite",
		dbPath+"?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// One connection per process: SQLite still serializes writers
	// process-wide, but the busy timeout above handles cross-process
	// contention so concurrent admin + gateway readers don't fail.
	db.SetMaxOpenConns(1)

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return db, nil
}
