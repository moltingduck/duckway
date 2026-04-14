package database

import (
	"database/sql"
	"fmt"
)

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS admin_users (
		id            TEXT PRIMARY KEY,
		username      TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS services (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL UNIQUE,
		display_name  TEXT NOT NULL,
		upstream_url  TEXT NOT NULL,
		host_pattern  TEXT NOT NULL,
		auth_type     TEXT NOT NULL DEFAULT 'bearer',
		auth_header   TEXT NOT NULL DEFAULT 'Authorization',
		auth_prefix   TEXT NOT NULL DEFAULT 'Bearer ',
		key_prefix    TEXT NOT NULL DEFAULT '',
		key_length    INTEGER NOT NULL DEFAULT 64,
		is_active     INTEGER NOT NULL DEFAULT 1,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS api_keys (
		id            TEXT PRIMARY KEY,
		service_id    TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		name          TEXT NOT NULL,
		key_encrypted TEXT NOT NULL,
		is_active     INTEGER NOT NULL DEFAULT 1,
		usage_count   INTEGER NOT NULL DEFAULT 0,
		last_used_at  TEXT,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS api_key_groups (
		id            TEXT PRIMARY KEY,
		service_id    TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		name          TEXT NOT NULL,
		strategy      TEXT NOT NULL DEFAULT 'round-robin',
		last_index    INTEGER NOT NULL DEFAULT 0,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS api_key_group_members (
		group_id      TEXT NOT NULL REFERENCES api_key_groups(id) ON DELETE CASCADE,
		api_key_id    TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
		priority      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (group_id, api_key_id)
	)`,

	`CREATE TABLE IF NOT EXISTS clients (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL,
		token_hash    TEXT NOT NULL UNIQUE,
		is_active     INTEGER NOT NULL DEFAULT 1,
		last_seen_at  TEXT,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS placeholder_keys (
		id                  TEXT PRIMARY KEY,
		env_name            TEXT NOT NULL,
		placeholder         TEXT NOT NULL UNIQUE,
		service_id          TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		api_key_id          TEXT REFERENCES api_keys(id) ON DELETE SET NULL,
		group_id            TEXT REFERENCES api_key_groups(id) ON DELETE SET NULL,
		client_id           TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
		permission_config   TEXT,
		requires_approval   INTEGER NOT NULL DEFAULT 1,
		approval_ttl_minutes INTEGER NOT NULL DEFAULT 1440,
		is_active           INTEGER NOT NULL DEFAULT 1,
		usage_count         INTEGER NOT NULL DEFAULT 0,
		last_used_at        TEXT,
		created_at          TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(client_id, service_id, env_name),
		CHECK (
			(api_key_id IS NOT NULL AND group_id IS NULL) OR
			(api_key_id IS NULL AND group_id IS NOT NULL)
		)
	)`,

	`CREATE TABLE IF NOT EXISTS approvals (
		id              TEXT PRIMARY KEY,
		placeholder_id  TEXT NOT NULL REFERENCES placeholder_keys(id) ON DELETE CASCADE,
		status          TEXT NOT NULL DEFAULT 'pending',
		approved_at     TEXT,
		expires_at      TEXT,
		request_info    TEXT,
		created_at      TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS request_log (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		placeholder_id  TEXT REFERENCES placeholder_keys(id),
		client_id       TEXT REFERENCES clients(id),
		service_name    TEXT NOT NULL,
		method          TEXT NOT NULL,
		path            TEXT NOT NULL,
		status_code     INTEGER,
		created_at      TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE INDEX IF NOT EXISTS idx_approvals_lookup ON approvals(placeholder_id, status)`,
	`CREATE INDEX IF NOT EXISTS idx_request_log_time ON request_log(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_placeholder_client ON placeholder_keys(client_id, service_id)`,

	`CREATE TABLE IF NOT EXISTS notification_channels (
		id            TEXT PRIMARY KEY,
		channel_type  TEXT NOT NULL,
		name          TEXT NOT NULL,
		config        TEXT NOT NULL,
		is_active     INTEGER NOT NULL DEFAULT 1,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS canary_settings (
		id              TEXT PRIMARY KEY DEFAULT 'default',
		email           TEXT NOT NULL DEFAULT '',
		enabled_types   TEXT NOT NULL DEFAULT '[]',
		exclude_clients TEXT NOT NULL DEFAULT '[]',
		created_at      TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS canary_tokens (
		id              TEXT PRIMARY KEY,
		client_id       TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
		token_type      TEXT NOT NULL,
		canary_token    TEXT NOT NULL,
		auth_token      TEXT NOT NULL,
		token_value     TEXT NOT NULL,
		secret_value    TEXT,
		memo            TEXT NOT NULL,
		deploy_path     TEXT NOT NULL,
		deploy_content  TEXT NOT NULL,
		created_at      TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_canary_client ON canary_tokens(client_id)`,

	// Migration version tracking
	`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`,
}

func runMigrations(db *sql.DB) error {
	for i, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}
	return nil
}
