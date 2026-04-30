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
		key_directory TEXT NOT NULL DEFAULT '',
		default_acl   TEXT NOT NULL DEFAULT '',
		is_active     INTEGER NOT NULL DEFAULT 1,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS api_keys (
		id            TEXT PRIMARY KEY,
		service_id    TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		name          TEXT NOT NULL,
		key_encrypted TEXT NOT NULL,
		acl           TEXT NOT NULL DEFAULT '',
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
		id              TEXT PRIMARY KEY,
		short_id        TEXT NOT NULL DEFAULT '',
		name            TEXT NOT NULL UNIQUE,
		token_hash      TEXT NOT NULL UNIQUE,
		is_active       INTEGER NOT NULL DEFAULT 1,
		canary_enabled  INTEGER NOT NULL DEFAULT 1,
		last_seen_at    TEXT,
		created_at      TEXT NOT NULL DEFAULT (datetime('now'))
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
		key_path            TEXT NOT NULL DEFAULT '',
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

	// Optional per-request detail capture. Only populated when the admin
	// toggles record-details on (request_log_capture_enabled setting).
	// Not part of request_log itself so the list view stays cheap.
	`CREATE TABLE IF NOT EXISTS request_log_detail (
		log_id            INTEGER PRIMARY KEY REFERENCES request_log(id) ON DELETE CASCADE,
		request_headers   TEXT NOT NULL DEFAULT '',
		request_body      TEXT NOT NULL DEFAULT '',
		request_size      INTEGER NOT NULL DEFAULT 0,
		response_headers  TEXT NOT NULL DEFAULT '',
		response_body     TEXT NOT NULL DEFAULT '',
		response_size     INTEGER NOT NULL DEFAULT 0,
		duration_ms       INTEGER NOT NULL DEFAULT 0,
		truncated         INTEGER NOT NULL DEFAULT 0
	)`,

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


	`CREATE TABLE IF NOT EXISTS oauth_credentials (
		id                TEXT PRIMARY KEY,
		service_id        TEXT NOT NULL REFERENCES services(id),
		name              TEXT NOT NULL,
		access_token      TEXT NOT NULL,
		refresh_token     TEXT NOT NULL,
		expires_at        INTEGER NOT NULL DEFAULT 0,
		token_endpoint    TEXT NOT NULL DEFAULT 'https://console.anthropic.com/v1/oauth/token',
		client_id_oauth   TEXT NOT NULL DEFAULT '',
		scopes            TEXT NOT NULL DEFAULT '[]',
		subscription_type TEXT NOT NULL DEFAULT '',
		rate_limit_tier   TEXT NOT NULL DEFAULT '',
		is_active         INTEGER NOT NULL DEFAULT 1,
		last_refreshed    TEXT,
		created_at        TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	)`,

	// Control Channels (CC) — per-bot, per-category logical group. One CC ≈
	// "this bot, this category" — clients assigned to a CC each get a home
	// channel under that category and can create more via the MCP server.
	`CREATE TABLE IF NOT EXISTS control_channels (
		id          TEXT PRIMARY KEY,
		name        TEXT NOT NULL,
		service_id  TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		api_key_id  TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
		config      TEXT NOT NULL DEFAULT '{}',
		is_active   INTEGER NOT NULL DEFAULT 1,
		created_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	// cc_channels caches every channel under a CC's category. handle is the
	// opaque ID exposed to agents (real Discord channel_id never leaks).
	// is_home=1 = the per-client home channel auto-created on assign.
	`CREATE TABLE IF NOT EXISTS cc_channels (
		handle        TEXT PRIMARY KEY,
		cc_id         TEXT NOT NULL REFERENCES control_channels(id) ON DELETE CASCADE,
		client_id     TEXT REFERENCES clients(id) ON DELETE SET NULL,
		channel_id    TEXT NOT NULL DEFAULT '',
		name          TEXT NOT NULL,
		topic         TEXT NOT NULL DEFAULT '',
		is_home       INTEGER NOT NULL DEFAULT 0,
		archived      INTEGER NOT NULL DEFAULT 0,
		created_at    TEXT NOT NULL DEFAULT (datetime('now')),
		last_seen_at  TEXT
	)`,
	// channel_id is the real Discord ID. Phase A leaves it empty until
	// Phase B provisions the channel and writes it back. Once non-empty,
	// it must be unique within a CC. We enforce that with a partial unique
	// index instead of a table-level UNIQUE, so multiple Phase-A stubs
	// (channel_id='') don't collide.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_cc_channels_unique ON cc_channels(cc_id, channel_id) WHERE channel_id != ''`,
	`CREATE INDEX IF NOT EXISTS idx_cc_channels_cc ON cc_channels(cc_id)`,
	`CREATE INDEX IF NOT EXISTS idx_cc_channels_client ON cc_channels(client_id)`,

	// client_cc — assignment row. agent_type drives what duckway sync writes.
	`CREATE TABLE IF NOT EXISTS client_cc (
		client_id       TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
		cc_id           TEXT NOT NULL REFERENCES control_channels(id) ON DELETE CASCADE,
		agent_type      TEXT NOT NULL DEFAULT 'claude_code',
		home_handle     TEXT NOT NULL REFERENCES cc_channels(handle) ON DELETE CASCADE,
		placeholder_id  TEXT NOT NULL REFERENCES placeholder_keys(id) ON DELETE CASCADE,
		created_at      TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (client_id, cc_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_client_cc_cc ON client_cc(cc_id)`,

	// discord_inbox — gateway events ingested server-side, polled by clients.
	// payload is the (filtered) JSON from Discord's gateway dispatch.
	`CREATE TABLE IF NOT EXISTS discord_inbox (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		cc_id       TEXT NOT NULL REFERENCES control_channels(id) ON DELETE CASCADE,
		channel_handle TEXT REFERENCES cc_channels(handle) ON DELETE CASCADE,
		event_type  TEXT NOT NULL,
		payload     TEXT NOT NULL DEFAULT '{}',
		created_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_inbox_cc_id ON discord_inbox(cc_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_inbox_created ON discord_inbox(created_at)`,

	// Migration version tracking
	`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`,
}

func runMigrations(db *sql.DB) error {
	for i, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}

	// Safe column additions for existing databases
	safeAlters := []string{
		"ALTER TABLE clients ADD COLUMN canary_enabled INTEGER NOT NULL DEFAULT 1",
		"ALTER TABLE clients ADD COLUMN short_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE canary_settings ADD COLUMN exclude_clients TEXT NOT NULL DEFAULT '[]'",
		"ALTER TABLE services ADD COLUMN key_directory TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE services ADD COLUMN default_acl TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE services ADD COLUMN delivery_mode TEXT NOT NULL DEFAULT 'proxy'",
		"ALTER TABLE api_keys ADD COLUMN acl TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN refresh_token TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE api_keys ADD COLUMN token_endpoint TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN subscription_info TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN usage_snapshot TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE placeholder_keys ADD COLUMN key_path TEXT NOT NULL DEFAULT ''",
	}
	for _, alt := range safeAlters {
		db.Exec(alt) // ignore "duplicate column" errors
	}

	// Widen the existing `discord` service's host_pattern so the duckway-client
	// MITM proxy can also intercept the WSS gateway + CDN hosts that the
	// Control Channel feature needs. Only touches rows that still hold the
	// old narrow pattern — leaves admin-customised values alone.
	db.Exec(`UPDATE services
		SET host_pattern = 'discord.com,api.discord.com,gateway.discord.gg,*.discordapp.net',
		    upstream_url = 'https://discord.com/api/v10'
		WHERE name = 'discord' AND host_pattern = 'discord.com'`)

	return nil
}
