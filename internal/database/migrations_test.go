package database

import "testing"

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
		('svc-gh-old', 'github', 'GitHub API + Git', 'https://api.github.com', 'api.github.com,github.com',
		 'bearer', 'Authorization', 'Bearer ', 'ghp_', 40, '.config/gh/credentials', 'loan_proxy', 1)`); err != nil {
		t.Fatal(err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatal(err)
	}

	var prefix, mode string
	var length int
	if err := db.QueryRow(`SELECT key_prefix, key_length, delivery_mode FROM services WHERE name = 'github'`).Scan(&prefix, &length, &mode); err != nil {
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
