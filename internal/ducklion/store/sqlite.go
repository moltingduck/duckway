package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/duckwayconfig"
	_ "modernc.org/sqlite"
)

const SchemaVersion = 2

var (
	ErrNotFound            = errors.New("not found")
	ErrIdempotencyConflict = errors.New("idempotency conflict")
	ErrMutationInProgress  = errors.New("mutation in progress")
)

type SQLite struct {
	db   *sql.DB
	path string
}

type MutationKey struct {
	Principal   string
	RequestID   string
	Operation   string
	SessionID   model.SessionID
	Fingerprint [32]byte
}

type MutationResult struct {
	JSON     json.RawMessage
	Replayed bool
}

func (s *SQLite) ReplayMutation(ctx context.Context, key MutationKey) (MutationResult, bool, error) {
	var operation, sessionID, status string
	var fingerprint, response []byte
	err := s.db.QueryRowContext(ctx, `SELECT operation,session_id,fingerprint,status,response_json FROM mutation_requests WHERE principal=? AND request_id=?`, key.Principal, key.RequestID).
		Scan(&operation, &sessionID, &fingerprint, &status, &response)
	if errors.Is(err, sql.ErrNoRows) { return MutationResult{}, false, nil }
	if err != nil { return MutationResult{}, false, err }
	if operation != key.Operation || sessionID != string(key.SessionID) || !equalBytes(fingerprint, key.Fingerprint[:]) { return MutationResult{}, true, ErrIdempotencyConflict }
	if status != "completed" { return MutationResult{}, true, ErrMutationInProgress }
	return MutationResult{JSON: append(json.RawMessage(nil), response...), Replayed: true}, true, nil
}

func Fingerprint(operation string, sessionID model.SessionID, canonicalPayload []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte(operation))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(sessionID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(canonicalPayload)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func DefaultPath() string {
	return filepath.Join(duckwayconfig.DefaultConfigDir(), "ducklion", "ducklion.db")
}

func Open(ctx context.Context, path string) (*SQLite, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve ducklion database path: %w", err)
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("ducklion database path is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect ducklion database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open ducklion database: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &SQLite{db: db, path: path}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping ducklion database: %w", err)
	}
	if err := chmodDatabaseFiles(path); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create ducklion data directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect ducklion data directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("ducklion data path is not a real directory")
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("secure ducklion data directory: %w", err)
	}
	return nil
}

func chmodDatabaseFiles(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Lstat(p); err == nil {
			if err := os.Chmod(p, 0600); err != nil {
				return fmt.Errorf("secure database file %s: %w", filepath.Base(p), err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *SQLite) migrate(ctx context.Context) error {
	var userVersion int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return fmt.Errorf("read ducklion schema version: %w", err)
	}
	if userVersion > SchemaVersion {
		return fmt.Errorf("ducklion database schema %d is newer than supported schema %d", userVersion, SchemaVersion)
	}
	if userVersion == SchemaVersion {
		return nil
	}
	if info, err := os.Stat(s.path); err == nil && info.Size() > 0 && userVersion > 0 {
		if err := s.backup(ctx, userVersion); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if userVersion < 1 {
		if err := migrateV1(ctx, tx); err != nil {
			return fmt.Errorf("migrate ducklion schema to v1: %w", err)
		}
	}
	if userVersion < 2 {
		if err := migrateV2(ctx, tx); err != nil {
			return fmt.Errorf("migrate ducklion schema to v2: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ducklion migration: %w", err)
	}
	return chmodDatabaseFiles(s.path)
}

func (s *SQLite) backup(ctx context.Context, version int) error {
	backup := fmt.Sprintf("%s.bak-v%d-%d", s.path, version, time.Now().UTC().UnixMilli())
	if strings.ContainsRune(backup, '\x00') {
		return fmt.Errorf("invalid backup path")
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", backup); err != nil {
		return fmt.Errorf("backup ducklion database: %w", err)
	}
	if err := os.Chmod(backup, 0600); err != nil {
		return fmt.Errorf("secure ducklion backup: %w", err)
	}
	return nil
}

func migrateV1(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE instance (singleton INTEGER PRIMARY KEY CHECK(singleton=1), instance_id TEXT NOT NULL UNIQUE, created_at_ms INTEGER NOT NULL)`,
		`CREATE TABLE sessions (
            session_id TEXT PRIMARY KEY CHECK(length(session_id)=6), handle TEXT NOT NULL,
            kind TEXT NOT NULL CHECK(kind IN ('agent','shell')), agent_type TEXT NOT NULL DEFAULT '',
            cwd TEXT NOT NULL, shell TEXT NOT NULL DEFAULT '', status TEXT NOT NULL CHECK(status IN ('provisioning','running','stopped','recovering','destroying')),
            writer_kind TEXT CHECK(writer_kind IS NULL OR writer_kind IN ('cc','terminal')), writer_id TEXT,
            ownership_epoch INTEGER NOT NULL CHECK(ownership_epoch>=0), runtime_generation INTEGER NOT NULL CHECK(runtime_generation>=0),
            task_state TEXT NOT NULL CHECK(task_state IN ('idle','running','replying')),
            adapter_state TEXT NOT NULL CHECK(adapter_state IN ('unavailable','healthy','unhealthy','recovering')),
			recovery_public_key BLOB, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL,
            CHECK((kind='agent' AND agent_type<>'' AND writer_kind IS NOT NULL AND writer_id IS NOT NULL) OR
                  (kind='shell' AND agent_type='' AND writer_kind IS NULL AND writer_id IS NULL)))`,
		`CREATE TABLE pending_yields (
            session_id TEXT PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
            requester_kind TEXT NOT NULL CHECK(requester_kind IN ('cc','terminal')), requester_id TEXT NOT NULL,
            source_epoch INTEGER NOT NULL CHECK(source_epoch>=0), request_id TEXT NOT NULL, created_at_ms INTEGER NOT NULL)`,
		`CREATE TABLE mutation_requests (
            principal TEXT NOT NULL, request_id TEXT NOT NULL, operation TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '',
            fingerprint BLOB NOT NULL CHECK(length(fingerprint)=32), status TEXT NOT NULL CHECK(status IN ('in_progress','completed')),
            response_json BLOB, created_at_ms INTEGER NOT NULL, completed_at_ms INTEGER,
            PRIMARY KEY(principal, request_id))`,
		`CREATE TABLE audit_events (
            id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, principal TEXT NOT NULL, event_type TEXT NOT NULL,
            ownership_epoch INTEGER, runtime_generation INTEGER, outcome_code TEXT NOT NULL, created_at_ms INTEGER NOT NULL)`,
		`CREATE INDEX audit_events_created_idx ON audit_events(created_at_ms)`,
		`CREATE INDEX mutation_requests_completed_idx ON mutation_requests(completed_at_ms)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func migrateV2(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`ALTER TABLE sessions ADD COLUMN exit_success INTEGER`,
		`ALTER TABLE sessions ADD COLUMN exit_reason TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLite) InstanceID(ctx context.Context) (model.InstanceID, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT instance_id FROM instance WHERE singleton=1`).Scan(&raw)
	if err == nil {
		return model.ParseInstanceID(raw)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id := model.NewInstanceID()
	_, err = s.db.ExecContext(ctx, `INSERT INTO instance(singleton,instance_id,created_at_ms) VALUES(1,?,?)`, id, time.Now().UTC().UnixMilli())
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *SQLite) RunMutation(ctx context.Context, key MutationKey, fn func(*sql.Tx) (json.RawMessage, error)) (MutationResult, error) {
	if strings.TrimSpace(key.Principal) == "" || strings.TrimSpace(key.RequestID) == "" || strings.TrimSpace(key.Operation) == "" {
		return MutationResult{}, fmt.Errorf("mutation principal, request id, and operation are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO mutation_requests
        (principal,request_id,operation,session_id,fingerprint,status,created_at_ms)
        VALUES(?,?,?,?,?,'in_progress',?) ON CONFLICT(principal,request_id) DO NOTHING`,
		key.Principal, key.RequestID, key.Operation, key.SessionID, key.Fingerprint[:], now)
	if err != nil {
		return MutationResult{}, err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		var operation, sessionID, status string
		var fingerprint, response []byte
		if err := tx.QueryRowContext(ctx, `SELECT operation,session_id,fingerprint,status,response_json FROM mutation_requests WHERE principal=? AND request_id=?`, key.Principal, key.RequestID).
			Scan(&operation, &sessionID, &fingerprint, &status, &response); err != nil {
			return MutationResult{}, err
		}
		if operation != key.Operation || sessionID != string(key.SessionID) || !equalBytes(fingerprint, key.Fingerprint[:]) {
			return MutationResult{}, ErrIdempotencyConflict
		}
		if status != "completed" {
			return MutationResult{}, ErrMutationInProgress
		}
		if err := tx.Commit(); err != nil {
			return MutationResult{}, err
		}
		return MutationResult{JSON: append(json.RawMessage(nil), response...), Replayed: true}, nil
	}
	response, err := fn(tx)
	if err != nil {
		return MutationResult{}, err
	}
	if !json.Valid(response) {
		return MutationResult{}, fmt.Errorf("mutation response is not valid JSON")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mutation_requests SET status='completed',response_json=?,completed_at_ms=? WHERE principal=? AND request_id=?`, response, now, key.Principal, key.RequestID); err != nil {
		return MutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{JSON: append(json.RawMessage(nil), response...)}, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var different byte
	for i := range a {
		different |= a[i] ^ b[i]
	}
	return different == 0
}
