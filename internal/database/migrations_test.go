package database

import "testing"

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
