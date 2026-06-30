package queries_test

import (
	"database/sql"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func seedSuiteClientAssignment(t *testing.T) (*sql.DB, *queries.KeySuiteQueries, *queries.PlaceholderQueries, func(query string, args ...interface{})) {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	exec := func(query string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern)
		VALUES ('svc-suite-a', 'suite-a', 'Suite A', 'https://a.example', 'a.example')`)
	exec(`INSERT INTO clients (id, name, token_hash)
		VALUES ('client-suite-a', 'suite client a', 'hash-suite-client-a')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted)
		VALUES ('key-suite-a', 'svc-suite-a', 'suite key a', 'encrypted')`)
	exec(`INSERT INTO key_suites (id, name, description)
		VALUES ('suite-a', 'Suite A', '')`)
	exec(`INSERT INTO key_suite_entries (id, suite_id, service_id, api_key_id, env_name)
		VALUES ('entry-suite-a', 'suite-a', 'svc-suite-a', 'key-suite-a', 'SUITE_A_KEY')`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, suite_id)
		VALUES ('ph-suite-a', 'SUITE_A_KEY', 'sk-suite-a', 'svc-suite-a', 'key-suite-a', 'client-suite-a', 'suite-a')`)
	return db, queries.NewKeySuiteQueries(db), queries.NewPlaceholderQueries(db), exec
}

func TestKeySuiteListBoundClients(t *testing.T) {
	_, suites, _, _ := seedSuiteClientAssignment(t)

	clients, err := suites.ListBoundClients("suite-a")
	if err != nil {
		t.Fatalf("ListBoundClients: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("len(clients) = %d, want 1", len(clients))
	}
	if clients[0].ID != "client-suite-a" || clients[0].Name != "suite client a" || clients[0].ServiceCount != 1 {
		t.Fatalf("unexpected client row: %+v", clients[0])
	}
}

func TestKeySuiteDeleteSuiteServicePlaceholdersDetachesLogs(t *testing.T) {
	db, suites, placeholders, exec := seedSuiteClientAssignment(t)
	exec(`INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code)
		VALUES ('client-suite-a', 'ph-suite-a', 'suite-a', 'GET', '/v1/test', 200)`)

	if err := suites.DeleteSuiteServicePlaceholders("suite-a", "svc-suite-a"); err != nil {
		t.Fatalf("DeleteSuiteServicePlaceholders: %v", err)
	}

	if _, err := placeholders.GetByID("ph-suite-a"); err == nil {
		t.Fatal("placeholder still exists after suite service delete")
	}
	clients, err := suites.ListBoundClients("suite-a")
	if err != nil {
		t.Fatalf("ListBoundClients after delete: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("bound clients after delete = %+v, want none", clients)
	}
	var nullLogs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE placeholder_id IS NULL`).Scan(&nullLogs); err != nil {
		t.Fatalf("count detached logs: %v", err)
	}
	if nullLogs != 1 {
		t.Fatalf("detached logs = %d, want 1", nullLogs)
	}
}
