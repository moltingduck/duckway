package database

import (
	"testing"
	"time"
)

func TestRunMigrationsAddsUsageGroupAttributionToExistingDatabase(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`DROP TABLE conversation_usage`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE conversation_usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		client_id TEXT NOT NULL DEFAULT '', api_key_id TEXT NOT NULL DEFAULT '',
		service_name TEXT NOT NULL DEFAULT '', conversation_id TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '', input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0, cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatal(err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("upgrade old usage schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_usage (key_group_id, provider, reasoning_tokens) VALUES ('g1', 'openai', 3)`); err != nil {
		t.Fatalf("new usage columns unavailable: %v", err)
	}
	var indexName string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_conv_usage_group_day'`).Scan(&indexName); err != nil {
		t.Fatalf("group usage index unavailable: %v", err)
	}
}

func TestRunMigrationsUpdatesOldGitHubDefaults(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`INSERT INTO services
		(id, name, display_name, upstream_url, host_pattern,
		 auth_type, auth_header, auth_prefix, key_prefix, key_length, key_directory, delivery_mode, is_active)
		VALUES
		('svc-gh-old', 'github', 'GitHub API + Git', 'https://api.github.com', 'api.github.com',
		 'bearer', 'Authorization', 'Bearer ', 'ghp_', 40, '.config/gh/credentials', 'loan_proxy', 1)`); err != nil {
		t.Fatal(err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatal(err)
	}

	var prefix, mode, hostPattern string
	var length int
	if err := db.QueryRow(`SELECT key_prefix, key_length, delivery_mode, host_pattern FROM services WHERE name = 'github'`).Scan(&prefix, &length, &mode, &hostPattern); err != nil {
		t.Fatal(err)
	}
	if prefix != "github_pat_" {
		t.Fatalf("key_prefix = %q, want github_pat_", prefix)
	}
	if length != 93 {
		t.Fatalf("key_length = %d, want 93", length)
	}
	if mode != "proxy" {
		t.Fatalf("delivery_mode = %q, want proxy", mode)
	}
	if hostPattern != "api.github.com,github.com" {
		t.Fatalf("host_pattern = %q, want api.github.com,github.com", hostPattern)
	}
}

func TestRunMigrationsSeedsXAIService(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	var upstream, hostPattern, authHeader, authPrefix, keyPrefix, keyDirectory, deliveryMode string
	var keyLength int
	if err := db.QueryRow(`SELECT upstream_url, host_pattern, auth_header, auth_prefix, key_prefix, key_length, key_directory, delivery_mode
		FROM services WHERE name = 'xai'`).Scan(&upstream, &hostPattern, &authHeader, &authPrefix, &keyPrefix, &keyLength, &keyDirectory, &deliveryMode); err != nil {
		t.Fatal(err)
	}
	if upstream != "https://api.x.ai" || hostPattern != "api.x.ai,cli-chat-proxy.grok.com" {
		t.Fatalf("xai upstream/hosts = %q %q", upstream, hostPattern)
	}
	if authHeader != "Authorization" || authPrefix != "Bearer " {
		t.Fatalf("xai auth = %q %q", authHeader, authPrefix)
	}
	if keyPrefix != "xai-" || keyLength != 80 || keyDirectory != ".config/xai/credentials" || deliveryMode != "proxy" {
		t.Fatalf("xai key/deploy config = prefix:%q len:%d dir:%q mode:%q", keyPrefix, keyLength, keyDirectory, deliveryMode)
	}
}

func TestRunMigrationsCreatesWatcherIndexesIdempotently(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := runMigrations(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, name := range []string{"idx_approvals_watch", "idx_client_update_jobs_watch"} {
		var got string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&got); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
}

// Legacy approvals stored a literal "datetime('now', ...)" string as expires_at,
// which never expired. The migration expires them and is idempotent.
func TestMigrationExpiresLegacyApprovalExpiryStrings(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clients (id, short_id, name, token_hash, is_active) VALUES ('c1','s1','n','h',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placeholder_keys (id, client_id, env_name, placeholder, service_id, api_key_id) VALUES ('p1','c1','E','ph','s','k')`); err != nil {
		t.Fatal(err)
	}
	for id, exp := range map[string]string{"legacy": "datetime('now', '+60 minutes')", "real": "2099-01-01 00:00:00"} {
		if _, err := db.Exec(`INSERT INTO approvals (id, placeholder_id, status, expires_at) VALUES (?, 'p1', 'approved', ?)`, id, exp); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ { // second run proves idempotence
		if err := runMigrations(db); err != nil {
			t.Fatal(err)
		}
	}
	var legacy, real string
	_ = db.QueryRow(`SELECT expires_at FROM approvals WHERE id='legacy'`).Scan(&legacy)
	_ = db.QueryRow(`SELECT expires_at FROM approvals WHERE id='real'`).Scan(&real)
	if len(legacy) != 19 || legacy > time.Now().UTC().Add(time.Minute).Format("2006-01-02 15:04:05") || real != "2099-01-01 00:00:00" {
		t.Fatalf("legacy=%q real=%q", legacy, real)
	}
}
