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
