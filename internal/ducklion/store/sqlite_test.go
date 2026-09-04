package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func TestOpenCreatesPrivateDatabaseAndStableInstance(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ducklion", "ducklion.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	id1, err := s.InstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id2, err := s.InstanceID(ctx)
	if err != nil || id1 != id2 {
		t.Fatalf("instance IDs: %q %q err=%v", id1, id2, err)
	}
	for _, target := range []string{filepath.Dir(path), path} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0700)
		if target == path {
			want = 0600
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode %s = %o, want %o", target, info.Mode().Perm(), want)
		}
	}
}

func TestOpenRejectsSymlinkDatabase(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ducklion.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), link); err == nil {
		t.Fatal("symlink database accepted")
	}
}

func TestOpenRejectsNewerSchemaWithoutChangingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ducklion.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=99`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("newer schema accepted")
	}
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 99 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestOpenMigratesV1DatabaseAndCreatesBackup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ducklion.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV1(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	columns := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if !columns["exit_success"] || !columns["exit_reason"] {
		t.Fatalf("columns=%v", columns)
	}
	backups, err := filepath.Glob(path + ".bak-v1-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups=%v err=%v", backups, err)
	}
	info, err := os.Stat(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("backup mode=%v", info.Mode())
	}
}

func TestMutationIsAtomicReplayableAndPayloadBound(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key := MutationKey{Principal: "terminal:laptop", RequestID: "req-1", Operation: "yield", SessionID: model.SessionID("ABC123")}
	key.Fingerprint = Fingerprint(key.Operation, key.SessionID, []byte(`{"wait":true}`))
	var calls atomic.Int32
	run := func() (MutationResult, error) {
		return s.RunMutation(ctx, key, func(_tx *sql.Tx) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`{"decision":"waiting"}`), nil
		})
	}
	first, err := run()
	if err != nil || first.Replayed {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := run()
	if err != nil || !second.Replayed || string(second.JSON) != string(first.JSON) || calls.Load() != 1 {
		t.Fatalf("second=%+v calls=%d err=%v", second, calls.Load(), err)
	}
	conflict := key
	conflict.Fingerprint = Fingerprint(key.Operation, key.SessionID, []byte(`{"wait":false}`))
	if _, err := s.RunMutation(ctx, conflict, func(_ *sql.Tx) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict error=%v", err)
	}
}
