package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
)

func TestSnapshotSQLiteMakesReadOnlySourceWritable(t *testing.T) {
	sourceDir := t.TempDir()
	db, err := database.OpenSQLite(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(sourceDir, "duckway.db"), 0400); err != nil {
		t.Fatal(err)
	}

	snapshotDir, err := snapshotSQLite(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(snapshotDir)
	snapshot, err := database.OpenSQLite(snapshotDir)
	if err != nil {
		t.Fatalf("open writable snapshot: %v", err)
	}
	defer snapshot.Close()
}

func TestTableHashUsesExplicitColumnOrder(t *testing.T) {
	left, err := database.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := database.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	if _, err := left.Exec(`CREATE TABLE reordered (first TEXT, second TEXT); INSERT INTO reordered VALUES ('a', 'b')`); err != nil {
		t.Fatal(err)
	}
	if _, err := right.Exec(`CREATE TABLE reordered (second TEXT, first TEXT); INSERT INTO reordered VALUES ('b', 'a')`); err != nil {
		t.Fatal(err)
	}
	columns := []string{"first", "second"}
	leftHash, err := tableHash(context.Background(), left, "reordered", columns)
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := tableHash(context.Background(), right, "reordered", columns)
	if err != nil {
		t.Fatal(err)
	}
	if leftHash != rightHash {
		t.Fatal("equivalent rows with different physical column order hashed differently")
	}
}

func TestRemoveOrphanRowsHandlesMultipleViolationsAndCascades(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys
		(id, env_name, placeholder, service_id, group_id, client_id)
		VALUES ('orphan-placeholder', 'TOKEN', 'phantom', 'svc-openai-default', 'missing-group', 'missing-client')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO approvals (id, placeholder_id) VALUES ('dependent-approval', 'orphan-placeholder')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}

	report, err := removeOrphanRows(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if report.Removed["placeholder_keys"] != 1 || report.Removed["approvals"] != 1 {
		t.Fatalf("repair report = %#v", report)
	}
	for _, table := range []string{"placeholder_keys", "approvals"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s still has %d rows", table, count)
		}
	}
	var violations int
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		violations++
	}
	if violations != 0 {
		t.Fatalf("foreign key check still has %d violations", violations)
	}
}

func TestRemoveOrphanRowsPreservesNullableRelationships(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF;
		INSERT INTO request_log (client_id, service_name, method, path)
		VALUES ('missing-client', 'openai', 'POST', '/responses');
		PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	report, err := removeOrphanRows(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if report.Nullified["request_log"] != 1 || report.Removed["request_log"] != 0 {
		t.Fatalf("repair report = %#v", report)
	}
	var count int
	var clientID sql.NullString
	if err := db.QueryRow(`SELECT COUNT(*), client_id FROM request_log`).Scan(&count, &clientID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || clientID.Valid {
		t.Fatalf("request log count=%d client_id=%#v", count, clientID)
	}
}

func TestClearRequestLogsOnlyRemovesRequestHistory(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		INSERT INTO clients (id, short_id, name, token_hash) VALUES ('keep-client', 'keep', 'Keep Client', 'hash');
		INSERT INTO request_log (id, client_id, service_name, method, path) VALUES (101, 'keep-client', 'openai', 'POST', '/responses');
		INSERT INTO request_log_detail (log_id, request_body, response_body) VALUES (101, 'request', 'response');
	`); err != nil {
		t.Fatal(err)
	}

	logs, details, err := clearRequestLogs(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if logs != 1 || details != 1 {
		t.Fatalf("removed request logs=%d details=%d", logs, details)
	}
	for _, table := range []string{"request_log", "request_log_detail"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s still has %d rows", table, count)
		}
	}
	var clients int
	if err := db.QueryRow(`SELECT COUNT(*) FROM clients WHERE id = 'keep-client'`).Scan(&clients); err != nil {
		t.Fatal(err)
	}
	if clients != 1 {
		t.Fatalf("unrelated client row count = %d", clients)
	}
}

func TestMigrationTableInventoryMatchesSQLiteSchema(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	want := slices.Clone(tableOrder)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("migration table inventory = %v, schema tables = %v", want, got)
	}
}

func TestLivePostgresMigrationAndQueries(t *testing.T) {
	dsn := os.Getenv("DUCKWAY_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("DUCKWAY_TEST_POSTGRES_URL is not set")
	}
	t.Setenv("DUCKWAY_DATABASE_URL", dsn)
	source, err := database.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	clientQ := queries.NewClientQueries(source)
	if err := clientQ.Create(&models.Client{ID: "pg-client", ShortID: "pgtest", Name: "Postgres Test", TokenHash: "pg-hash", CanaryEnabled: true}); err != nil {
		t.Fatal(err)
	}
	keyQ := queries.NewAPIKeyQueries(source)
	if err := keyQ.Create(&models.APIKey{ID: "pg-key", ServiceID: "svc-openai-default", Name: "Postgres Key", KeyEncrypted: "ciphertext", RefreshToken: "refresh-ciphertext", ExpiresAt: 1788598308000}); err != nil {
		t.Fatal(err)
	}
	logQ := queries.NewRequestLogQueries(source)
	logID, err := logQ.LogWithReturn("pg-client", "", "openai", "POST", "/responses", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := logQ.StoreDetail(&queries.RequestLogDetail{LogID: logID, RequestBody: "request", ResponseBody: "response", Truncated: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`UPDATE request_log_detail SET response_body = CAST(X'62B563' AS TEXT) WHERE log_id = ?`, logID); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`INSERT INTO placeholder_keys
		(id, env_name, placeholder, service_id, group_id, client_id)
		VALUES ('pg-orphan', 'TOKEN', 'pg-phantom', 'svc-openai-default', 'missing-group', 'missing-client')`); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	report, err := removeOrphanRows(context.Background(), source)
	if err != nil || report.Removed["placeholder_keys"] != 1 {
		t.Fatalf("remove migration orphans = %#v, err=%v", report, err)
	}

	target, err := database.OpenPostgresFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := migrate(context.Background(), source, target); err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), source, target); err == nil {
		t.Fatal("second migration unexpectedly replaced a non-empty PostgreSQL target")
	}

	gotClient, err := queries.NewClientQueries(target).GetByID("pg-client")
	if err != nil || !gotClient.CanaryEnabled {
		t.Fatalf("migrated client = %#v, err=%v", gotClient, err)
	}
	gotKey, err := queries.NewAPIKeyQueries(target).GetByID("pg-key")
	if err != nil || gotKey.KeyEncrypted != "ciphertext" || gotKey.RefreshToken != "refresh-ciphertext" || gotKey.ExpiresAt != 1788598308000 {
		t.Fatalf("migrated key = %#v, err=%v", gotKey, err)
	}
	if _, err := queries.NewRequestLogQueries(target).LogWithReturn("pg-client", "", "openai", "POST", "/responses", 201); err != nil {
		t.Fatalf("PostgreSQL RETURNING query: %v", err)
	}
	if _, err := queries.NewApprovalQueries(target).MarkExpiredAsIgnored(60); err != nil {
		t.Fatalf("PostgreSQL datetime modifier query: %v", err)
	}
	var responseBody string
	if err := target.QueryRow(`SELECT response_body FROM request_log_detail WHERE log_id = ?`, logID).Scan(&responseBody); err != nil {
		t.Fatal(err)
	}
	if responseBody != "b\uFFFDc" {
		t.Fatalf("normalized response body = %q", responseBody)
	}
}
