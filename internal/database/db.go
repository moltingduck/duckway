package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

func Open(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "duckway.db")
	// modernc.org/sqlite expects PRAGMAs as `_pragma=name(value)` — the
	// shorter `_journal_mode=WAL` / `_busy_timeout=5000` shorthand that
	// mattn/go-sqlite3 accepts is silently ignored by this driver, so
	// earlier versions of this file weren't actually enabling WAL at
	// all. Each `_pragma` is run as `PRAGMA <expr>` against the new
	// connection.
	//
	//   journal_mode(DELETE) — avoids modernc SQLite WAL shared-memory
	//     SIGBUS faults observed on Linux ARM64 when split admin + gateway
	//     processes share a Docker volume. Rollback journals still support
	//     multiple local processes; busy_timeout handles lock contention.
	//   busy_timeout(5000) — when another process holds the write lock,
	//     retry internally for up to 5 seconds before surfacing
	//     SQLITE_BUSY. Covers normal cross-process contention without
	//     making every caller wrap queries in retry loops.
	//   foreign_keys(on) — enforce ON DELETE CASCADE / REFERENCES.
	//
	// _txlock=immediate (NOT a pragma — a driver-level option) takes
	// the write lock at BEGIN time instead of at the first write inside
	// the transaction. Avoids the read-then-upgrade deadlock where two
	// transactions both hold read locks and both try to promote.
	journalMode := strings.ToUpper(strings.TrimSpace(os.Getenv("DUCKWAY_SQLITE_JOURNAL_MODE")))
	if journalMode == "" {
		journalMode = "DELETE"
	}
	if journalMode != "DELETE" && journalMode != "WAL" {
		return nil, fmt.Errorf("invalid DUCKWAY_SQLITE_JOURNAL_MODE %q: must be DELETE or WAL", journalMode)
	}
	dsn := dbPath +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(" + journalMode + ")" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(on)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// One connection per process keeps writers serialized in-process so
	// we never see SQLITE_BUSY from our own concurrent writes; the busy
	// timeout above handles the cross-process case.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	var actualJournalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&actualJournalMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("verify journal mode: %w", err)
	}
	if !strings.EqualFold(actualJournalMode, journalMode) {
		db.Close()
		return nil, fmt.Errorf("journal mode is %q, want %q", actualJournalMode, strings.ToLower(journalMode))
	}

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return db, nil
}
