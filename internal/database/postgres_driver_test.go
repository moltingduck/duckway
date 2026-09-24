package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresQueryRebind(t *testing.T) {
	in := `SELECT '?', value FROM t WHERE a = ? AND note = 'it''s ?' AND b = ?`
	want := `SELECT '?', value FROM t WHERE a = $1 AND note = 'it''s ?' AND b = $2`
	if got := postgresQuery(in); got != want {
		t.Fatalf("postgresQuery() = %q, want %q", got, want)
	}
}

func TestPostgresQueryUsesBigintForSQLiteIntegerDDL(t *testing.T) {
	got := postgresQuery(`CREATE TABLE sample (expires_at INTEGER NOT NULL, id INTEGER PRIMARY KEY AUTOINCREMENT)`)
	want := `CREATE TABLE sample (expires_at BIGINT NOT NULL, id BIGSERIAL PRIMARY KEY)`
	if got != want {
		t.Fatalf("postgresQuery() = %q, want %q", got, want)
	}
}

func TestPostgresQueryUsesByteaForSQLiteBlobDDL(t *testing.T) {
	ddl := `CREATE TABLE cc_message_deliveries (content_digest BLOB NOT NULL, BLOB_data TEXT, "BLOB" TEXT, note TEXT DEFAULT ' BLOB ')`
	if got, want := postgresQuery(ddl), `CREATE TABLE cc_message_deliveries (content_digest BYTEA NOT NULL, BLOB_data TEXT, "BLOB" TEXT, note TEXT DEFAULT ' BLOB ')`; got != want {
		t.Fatalf("postgresQuery() = %q, want %q", got, want)
	}
	if got := postgresQuery(`SELECT BLOB FROM example`); got != `SELECT BLOB FROM example` {
		t.Fatalf("non-DDL query changed unexpectedly: %q", got)
	}
}

// Opt in with DUCKWAY_POSTGRES_TEST_URL=postgres://... to exercise migrations
// against a real PostgreSQL server. The test creates and drops its own schema.
func TestPostgresMigrationsCreateAndRecoverMessageDeliveryTable(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DUCKWAY_POSTGRES_TEST_URL"))
	if dsn == "" {
		t.Skip("set DUCKWAY_POSTGRES_TEST_URL to run PostgreSQL migration integration")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("DUCKWAY_POSTGRES_TEST_URL must be a postgres URL")
	}
	adminDB, err := openPostgresTestDB(dsn)
	if err != nil {
		t.Fatalf("connect to PostgreSQL test server: %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })

	schema := fmt.Sprintf("duckway_migration32_%d", time.Now().UnixNano())
	if _, err := adminDB.Exec(`CREATE SCHEMA ` + quotePostgresIdentifier(schema)); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.Exec(`DROP SCHEMA ` + quotePostgresIdentifier(schema) + ` CASCADE`)
	})

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := openPostgresTestDB(u.String())
	if err != nil {
		t.Fatalf("connect to isolated schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := runPostgresMigrations(db); err != nil {
		t.Fatalf("fresh PostgreSQL migrations: %v", err)
	}
	assertMessageDigestRoundTrip(t, db)

	// Re-running against a partially upgraded installation must be safe and
	// preserve the existing binary digest row.
	if err := runPostgresMigrations(db); err != nil {
		t.Fatalf("migration recovery retry: %v", err)
	}
	var got []byte
	if err := db.QueryRow(`SELECT content_digest FROM cc_message_deliveries WHERE cc_id = 'pg-blob-test' AND delivery_key = 'pg-blob-key'`).Scan(&got); err != nil {
		t.Fatalf("read preserved bytea digest after retry: %v", err)
	}
	if string(got) != string([]byte{0, 1, 2, 127, 128, 255}) {
		t.Fatalf("retry changed digest to %v", got)
	}

	partialSchema := schema + "_partial"
	if _, err := adminDB.Exec(`CREATE SCHEMA ` + quotePostgresIdentifier(partialSchema)); err != nil {
		t.Fatalf("create partial schema: %v", err)
	}
	t.Cleanup(func() { _, _ = adminDB.Exec(`DROP SCHEMA ` + quotePostgresIdentifier(partialSchema) + ` CASCADE`) })
	partialURL := *u
	partialQuery := partialURL.Query()
	partialQuery.Set("search_path", partialSchema)
	partialURL.RawQuery = partialQuery.Encode()
	partialDB, err := openPostgresTestDB(partialURL.String())
	if err != nil {
		t.Fatalf("connect to partial schema: %v", err)
	}
	t.Cleanup(func() { _ = partialDB.Close() })
	for _, statement := range postgresCompatibilityFunctions {
		if _, err := partialDB.Exec(statement); err != nil {
			t.Fatalf("install compatibility function for partial schema: %v", err)
		}
	}
	targetMigration := -1
	for i, statement := range migrations {
		if strings.Contains(statement, "CREATE TABLE IF NOT EXISTS cc_message_deliveries") {
			targetMigration = i
			break
		}
	}
	if targetMigration < 0 {
		t.Fatal("cc_message_deliveries creation migration not found")
	}
	for i, statement := range migrations[:targetMigration] {
		if _, err := partialDB.Exec(statement); err != nil {
			t.Fatalf("prepare pre-32 migration %d: %v", i, err)
		}
	}
	if _, err := partialDB.Exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('pre32-sentinel', 'pre32-sentinel', 'sentinel', '', '')`); err != nil {
		t.Fatalf("seed pre-32 existing row: %v", err)
	}
	if err := runPostgresMigrations(partialDB); err != nil {
		t.Fatalf("recover pre-32 partial migration: %v", err)
	}
	var preserved string
	if err := partialDB.QueryRow(`SELECT display_name FROM services WHERE id = 'pre32-sentinel'`).Scan(&preserved); err != nil {
		t.Fatalf("read pre-32 sentinel row: %v", err)
	}
	if preserved != "sentinel" {
		t.Fatalf("pre-32 migration changed sentinel to %q", preserved)
	}
	assertMessageDigestRoundTrip(t, partialDB)
	if err := partialDB.QueryRow(`SELECT content_digest FROM cc_message_deliveries WHERE cc_id = 'pg-blob-test' AND delivery_key = 'pg-blob-key'`).Scan(&got); err != nil {
		t.Fatalf("read preserved pre-32 digest: %v", err)
	}
	if string(got) != string([]byte{0, 1, 2, 127, 128, 255}) {
		t.Fatalf("partial migration recovery changed digest to %v", got)
	}
}

func openPostgresTestDB(dsn string) (*sql.DB, error) {
	ensurePostgresDriver()
	db, err := sql.Open(postgresDriverName, dsn)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func assertMessageDigestRoundTrip(t *testing.T, db *sql.DB) {
	t.Helper()
	digest := []byte{0, 1, 2, 127, 128, 255}
	if _, err := db.Exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES ('pg-blob-service', 'pg-blob-service', 'test', '', '') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert service: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES ('pg-blob-key-id', 'pg-blob-service', 'test', '') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert API key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO control_channels (id, name, service_id, api_key_id, client_id) VALUES ('pg-blob-test', 'test', 'pg-blob-service', 'pg-blob-key-id', 'pg-blob-client') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert control channel: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO cc_channels (handle, cc_id, name) VALUES ('pg-blob-handle', 'pg-blob-test', 'test') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert CC channel: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO cc_message_deliveries (cc_id, channel_handle, delivery_key, content_digest) VALUES ('pg-blob-test', 'pg-blob-handle', 'pg-blob-key', ?)
		ON CONFLICT (cc_id, delivery_key) DO UPDATE SET content_digest = EXCLUDED.content_digest`, digest); err != nil {
		t.Fatalf("write bytea digest: %v", err)
	}
	var got []byte
	if err := db.QueryRow(`SELECT content_digest FROM cc_message_deliveries WHERE cc_id = 'pg-blob-test' AND delivery_key = 'pg-blob-key'`).Scan(&got); err != nil {
		t.Fatalf("read bytea digest: %v", err)
	}
	if string(got) != string(digest) {
		t.Fatalf("digest round trip = %v, want %v", got, digest)
	}
}

func quotePostgresIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
