package queries_test

import (
	"database/sql"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func seedRefreshableDeleteImpact(t *testing.T) (*sql.DB, *queries.APIKeyQueries, func(query string, args ...interface{})) {
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
		VALUES ('svc-refresh-del', 'refresh-del', 'Refresh Delete', 'https://refresh.example', 'refresh.example')`)
	exec(`INSERT INTO clients (id, name, token_hash)
		VALUES ('client-refresh-del', 'refresh client', 'hash-refresh-del')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted, refresh_token)
		VALUES ('key-refresh-del', 'svc-refresh-del', 'refresh key', 'encrypted-access', 'encrypted-refresh')`)
	exec(`INSERT INTO key_suites (id, name, description)
		VALUES ('suite-refresh-del', 'Refresh Suite', '')`)
	exec(`INSERT INTO key_suite_entries (id, suite_id, service_id, api_key_id, env_name)
		VALUES ('entry-refresh-del', 'suite-refresh-del', 'svc-refresh-del', 'key-refresh-del', 'REFRESH_KEY')`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id, suite_id)
		VALUES ('ph-refresh-del', 'REFRESH_KEY', 'sk-refresh-del', 'svc-refresh-del', 'key-refresh-del', 'client-refresh-del', 'suite-refresh-del')`)
	exec(`INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code)
		VALUES ('client-refresh-del', 'ph-refresh-del', 'refresh-del', 'GET', '/v1/test', 200)`)
	exec(`INSERT INTO control_channels (id, name, service_id, api_key_id, client_id, agent_type, placeholder_id, config, is_active)
		VALUES ('cc-refresh-del', 'Refresh CC', 'svc-refresh-del', 'key-refresh-del', 'client-refresh-del', 'codex', 'ph-refresh-del', '{}', 1)`)

	return db, queries.NewAPIKeyQueries(db), exec
}

func TestRefreshableDeleteImpactListsReferences(t *testing.T) {
	_, keys, _ := seedRefreshableDeleteImpact(t)

	impact, err := keys.RefreshableDeleteImpact("key-refresh-del")
	if err != nil {
		t.Fatalf("RefreshableDeleteImpact: %v", err)
	}
	if len(impact.KeySuites) != 1 || impact.KeySuites[0].SuiteName != "Refresh Suite" {
		t.Fatalf("unexpected suites impact: %+v", impact.KeySuites)
	}
	if len(impact.Clients) != 1 || impact.Clients[0].ClientName != "refresh client" {
		t.Fatalf("unexpected clients impact: %+v", impact.Clients)
	}
	if len(impact.ControlChannels) != 1 || impact.ControlChannels[0].Name != "Refresh CC" {
		t.Fatalf("unexpected cc impact: %+v", impact.ControlChannels)
	}
}

func TestDeleteRefreshableWithCleanupDisablesCCAndCleansReferences(t *testing.T) {
	db, keys, _ := seedRefreshableDeleteImpact(t)

	impact, err := keys.DeleteRefreshableWithCleanup("key-refresh-del")
	if err != nil {
		t.Fatalf("DeleteRefreshableWithCleanup: %v", err)
	}
	if len(impact.ControlChannels) != 1 {
		t.Fatalf("control channel impact = %+v", impact.ControlChannels)
	}

	var entryKey sql.NullString
	if err := db.QueryRow(`SELECT api_key_id FROM key_suite_entries WHERE id = 'entry-refresh-del'`).Scan(&entryKey); err != nil {
		t.Fatalf("entry lookup: %v", err)
	}
	if entryKey.Valid {
		t.Fatalf("key suite entry api_key_id still set to %q", entryKey.String)
	}

	var placeholders int
	if err := db.QueryRow(`SELECT COUNT(*) FROM placeholder_keys WHERE id = 'ph-refresh-del'`).Scan(&placeholders); err != nil {
		t.Fatalf("count placeholders: %v", err)
	}
	if placeholders != 0 {
		t.Fatalf("placeholder count = %d, want 0", placeholders)
	}

	var nullLogs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE placeholder_id IS NULL`).Scan(&nullLogs); err != nil {
		t.Fatalf("count detached logs: %v", err)
	}
	if nullLogs != 1 {
		t.Fatalf("detached logs = %d, want 1", nullLogs)
	}

	var ccActive int
	if err := db.QueryRow(`SELECT is_active FROM control_channels WHERE id = 'cc-refresh-del'`).Scan(&ccActive); err != nil {
		t.Fatalf("cc lookup: %v", err)
	}
	if ccActive != 0 {
		t.Fatalf("cc is_active = %d, want 0", ccActive)
	}

	key, err := keys.GetByID("key-refresh-del")
	if err != nil {
		t.Fatalf("retained key lookup: %v", err)
	}
	if key.IsRefreshable || key.IsActive {
		t.Fatalf("retained key should be inactive and non-refreshable: %+v", key)
	}
}
