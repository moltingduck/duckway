package database

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

func Open(dataDir string) (*sql.DB, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("DUCKWAY_DATABASE_DRIVER")), "postgres") {
		return openPostgres()
	}
	return openSQLite(dataDir)
}

// OpenSQLite opens the on-disk database regardless of the configured runtime
// backend. It is used by the offline PostgreSQL migration command.
func OpenSQLite(dataDir string) (*sql.DB, error) { return openSQLite(dataDir) }

// OpenPostgresFromEnv opens PostgreSQL regardless of the configured runtime
// backend. It is used by the offline migration command.
func OpenPostgresFromEnv() (*sql.DB, error) { return openPostgres() }

func openSQLite(dataDir string) (*sql.DB, error) {
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

func openPostgres() (*sql.DB, error) {
	dsn, err := postgresDSNFromEnv()
	if err != nil {
		return nil, err
	}
	if dsn == "" {
		return nil, fmt.Errorf("PostgreSQL connection settings are required")
	}
	ensurePostgresDriver()
	db, err := sql.Open(postgresDriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL database: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to PostgreSQL database: %w", redactDatabaseError(err, dsn))
	}
	if err := runPostgresMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run PostgreSQL migrations: %w", err)
	}
	return db, nil
}

func postgresDSNFromEnv() (string, error) {
	if dsn := strings.TrimSpace(os.Getenv("DUCKWAY_DATABASE_URL")); dsn != "" {
		return dsn, nil
	}
	passwordFile := strings.TrimSpace(os.Getenv("DUCKWAY_POSTGRES_PASSWORD_FILE"))
	if passwordFile == "" {
		return "", nil
	}
	password, err := os.ReadFile(passwordFile)
	if err != nil {
		return "", fmt.Errorf("read PostgreSQL password file: %w", err)
	}
	passwordText := strings.TrimSpace(string(password))
	if passwordText == "" || strings.ContainsAny(passwordText, "\r\n") {
		return "", fmt.Errorf("PostgreSQL password file must contain one non-empty line")
	}
	u := &url.URL{
		Scheme: "postgres",
		Host:   strings.TrimSpace(os.Getenv("DUCKWAY_POSTGRES_HOST")),
		Path:   "/" + envDefault("DUCKWAY_POSTGRES_DB", "duckway"),
		User:   url.UserPassword(envDefault("DUCKWAY_POSTGRES_USER", "duckway"), passwordText),
	}
	if u.Host == "" {
		u.Host = "postgres:5432"
	} else if !strings.Contains(u.Host, ":") {
		u.Host += ":5432"
	}
	q := u.Query()
	q.Set("sslmode", envDefault("DUCKWAY_POSTGRES_SSLMODE", "disable"))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func redactDatabaseError(err error, secret string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "<redacted>")
	}
	return fmt.Errorf("%s", msg)
}
