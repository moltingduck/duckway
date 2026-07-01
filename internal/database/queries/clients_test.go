package queries_test

import (
	"database/sql"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
)

func seedClientDeleteImpact(t *testing.T) (*sql.DB, *queries.ClientQueries) {
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
		VALUES ('svc-client-del', 'client-del', 'Client Delete', 'https://client.example', 'client.example')`)
	exec(`INSERT INTO clients (id, name, token_hash)
		VALUES ('client-del', 'delete client', 'hash-client-del')`)
	exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted)
		VALUES ('key-client-del', 'svc-client-del', 'delete key', 'encrypted')`)
	exec(`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, client_id)
		VALUES ('ph-client-del', 'CLIENT_DEL_KEY', 'sk-client-del', 'svc-client-del', 'key-client-del', 'client-del')`)
	exec(`INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code)
		VALUES ('client-del', 'ph-client-del', 'client-del', 'GET', '/v1/test', 200)`)
	exec(`INSERT INTO canary_tokens
		(id, client_id, token_type, canary_token, auth_token, token_value, secret_value, memo, deploy_path, deploy_content)
		VALUES ('canary-client-del', 'client-del', 'env', 'canary', 'auth', 'value', '', 'memo', '/tmp/token', 'content')`)
	exec(`INSERT INTO control_channels (id, name, service_id, api_key_id, client_id, agent_type, placeholder_id, config, is_active)
		VALUES ('cc-client-del', 'Delete CC', 'svc-client-del', 'key-client-del', 'client-del', 'codex', 'ph-client-del', '{}', 1)`)
	exec(`INSERT INTO cc_channels (handle, cc_id, client_id, channel_id, name, topic, kind)
		VALUES ('chan-client-del', 'cc-client-del', 'client-del', 'discord-channel', 'home', '', 'management')`)
	exec(`INSERT INTO discord_inbox (cc_id, channel_handle, event_type, payload)
		VALUES ('cc-client-del', 'chan-client-del', 'MESSAGE_CREATE', '{}')`)

	return db, queries.NewClientQueries(db)
}

func TestClientDeleteCleansReferencesAndPreservesRequestLog(t *testing.T) {
	db, clients := seedClientDeleteImpact(t)

	if err := clients.Delete("client-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"clients", `SELECT COUNT(*) FROM clients WHERE id = 'client-del'`},
		{"placeholders", `SELECT COUNT(*) FROM placeholder_keys WHERE id = 'ph-client-del'`},
		{"canary tokens", `SELECT COUNT(*) FROM canary_tokens WHERE id = 'canary-client-del'`},
		{"control channels", `SELECT COUNT(*) FROM control_channels WHERE id = 'cc-client-del'`},
		{"cc channels", `SELECT COUNT(*) FROM cc_channels WHERE handle = 'chan-client-del'`},
		{"discord inbox", `SELECT COUNT(*) FROM discord_inbox WHERE cc_id = 'cc-client-del'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var count int
			if err := db.QueryRow(tc.query).Scan(&count); err != nil {
				t.Fatalf("count %s: %v", tc.name, err)
			}
			if count != 0 {
				t.Fatalf("%s count = %d, want 0", tc.name, count)
			}
		})
	}

	var logs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE client_id IS NULL AND placeholder_id IS NULL`).Scan(&logs); err != nil {
		t.Fatalf("count detached logs: %v", err)
	}
	if logs != 1 {
		t.Fatalf("detached logs = %d, want 1", logs)
	}
}
