package queries_test

import (
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func TestPlaceholderDeleteDetachesRequestLogs(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern)
		VALUES ('svc-ph-del', 'ph-delete-test', 'PH Delete Test', 'https://example.com', 'example.com')`); err != nil {
		t.Fatalf("insert service: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id, name, token_hash)
		VALUES ('client-ph-del', 'client ph delete', 'hash-ph-delete')`); err != nil {
		t.Fatalf("insert client: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, refresh_token, expires_at)
		VALUES ('key-ph-del', 'svc-ph-del', 'refreshable key', 'encrypted-access', 'encrypted-refresh', 1)`); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys
		(id, env_name, placeholder, service_id, api_key_id, client_id)
		VALUES ('ph-del', 'OPENAI_API_KEY', 'sk-dw-ph-delete', 'svc-ph-del', 'key-ph-del', 'client-ph-del')`); err != nil {
		t.Fatalf("insert placeholder: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO request_log
		(client_id, placeholder_id, service_name, method, path, status_code)
		VALUES ('client-ph-del', 'ph-del', 'ph-delete-test', 'GET', '/v1/models', 200)`); err != nil {
		t.Fatalf("insert request log: %v", err)
	}

	q := queries.NewPlaceholderQueries(db)
	if err := q.Delete("ph-del"); err != nil {
		t.Fatalf("delete placeholder: %v", err)
	}

	var logRows int
	var nullPlaceholderRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&logRows); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE placeholder_id IS NULL`).Scan(&nullPlaceholderRows); err != nil {
		t.Fatalf("count null placeholder logs: %v", err)
	}
	if logRows != 1 || nullPlaceholderRows != 1 {
		t.Fatalf("request_log not preserved/detached: rows=%d null_placeholder_rows=%d", logRows, nullPlaceholderRows)
	}
}
