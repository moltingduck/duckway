package database

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
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
		category      TEXT NOT NULL DEFAULT '',
		usage_metering TEXT NOT NULL DEFAULT '',
		is_active     INTEGER NOT NULL DEFAULT 1,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS api_keys (
		id            TEXT PRIMARY KEY,
		service_id    TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		name          TEXT NOT NULL,
		key_encrypted TEXT NOT NULL,
		acl           TEXT NOT NULL DEFAULT '',
		refresh_token TEXT NOT NULL DEFAULT '',
		expires_at    INTEGER NOT NULL DEFAULT 0,
		token_endpoint TEXT NOT NULL DEFAULT '',
		subscription_info TEXT NOT NULL DEFAULT '',
		usage_snapshot TEXT NOT NULL DEFAULT '',
		upstream_proxy_url TEXT NOT NULL DEFAULT '',
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
		update_policy   TEXT NOT NULL DEFAULT 'managed',
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

	`CREATE TABLE IF NOT EXISTS client_runtime_status (
		client_id          TEXT PRIMARY KEY REFERENCES clients(id) ON DELETE CASCADE,
		version            TEXT NOT NULL DEFAULT '',
		os                 TEXT NOT NULL DEFAULT '',
		arch               TEXT NOT NULL DEFAULT '',
		boot_id            TEXT NOT NULL DEFAULT '',
		install_path       TEXT NOT NULL DEFAULT '',
		install_writable   INTEGER NOT NULL DEFAULT 0,
		capabilities       TEXT NOT NULL DEFAULT '[]',
		components         TEXT NOT NULL DEFAULT '{}',
		current_job_id     TEXT NOT NULL DEFAULT '',
		last_heartbeat_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS client_update_rollouts (
		id                       TEXT PRIMARY KEY,
		target_version           TEXT NOT NULL,
		artifacts                TEXT NOT NULL DEFAULT '[]',
		status                   TEXT NOT NULL DEFAULT 'active',
		max_concurrency          INTEGER NOT NULL,
		start_interval_seconds   INTEGER NOT NULL,
		failure_threshold_percent INTEGER NOT NULL DEFAULT 20,
		next_dispatch_at         TEXT NOT NULL DEFAULT (datetime('now')),
		created_at               TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at               TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS client_update_jobs (
		id                TEXT PRIMARY KEY,
		rollout_id        TEXT NOT NULL REFERENCES client_update_rollouts(id) ON DELETE CASCADE,
		client_id         TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
		type              TEXT NOT NULL DEFAULT 'update_restart',
		status            TEXT NOT NULL DEFAULT 'queued',
		lease_token       TEXT NOT NULL DEFAULT '',
		lease_expires_at  TEXT,
		attempts          INTEGER NOT NULL DEFAULT 0,
		error             TEXT NOT NULL DEFAULT '',
		started_at        TEXT,
		finished_at       TEXT,
		created_at        TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at        TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(rollout_id, client_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_client_update_jobs_dispatch ON client_update_jobs(status, rollout_id, client_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_client_update_one_active ON client_update_rollouts((1)) WHERE status IN ('active','paused')`,

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

	// CC v2: client_id + agent_type + placeholder_id moved onto control_channels
	// (1:1 client↔CC). The previous client_cc table is dropped at runtime
	// migration time — see runMigrations below.

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

	`CREATE TABLE IF NOT EXISTS cc_agent_tests (
		id          TEXT PRIMARY KEY,
		cc_id       TEXT NOT NULL REFERENCES control_channels(id) ON DELETE CASCADE,
		client_id   TEXT NOT NULL,
		handle      TEXT NOT NULL,
		agent_type  TEXT NOT NULL,
		status      TEXT NOT NULL DEFAULT 'queued',
		error       TEXT NOT NULL DEFAULT '',
		inbox_id    INTEGER NOT NULL DEFAULT 0,
		created_at  TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_cc_agent_tests_cc ON cc_agent_tests(cc_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_cc_agent_tests_client ON cc_agent_tests(client_id, created_at)`,

	// Migration version tracking
	`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`,

	// Key Groups (usage-aware, score-based key selection with 429 auto-rotation)
	`CREATE TABLE IF NOT EXISTS key_groups (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		description  TEXT NOT NULL DEFAULT '',
		service_name TEXT NOT NULL DEFAULT 'anthropic',
		created_at   TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS key_group_members (
		group_id        TEXT NOT NULL REFERENCES key_groups(id) ON DELETE CASCADE,
		api_key_id      TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
		position        INTEGER NOT NULL DEFAULT 0,
		exhausted_until TEXT,
		PRIMARY KEY (group_id, api_key_id)
	)`,

	// Per-request LLM token usage, parsed from upstream response bodies.
	// conversation_id is claude's X-Claude-Code-Session-Id header (empty
	// for OpenAI / non-claude traffic, which buckets together). Used by
	// the Usage panel for per-key totals and per-conversation drill-down.
	// Pruned to a trailing window by a background sweeper.
	`CREATE TABLE IF NOT EXISTS conversation_usage (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		client_id         TEXT NOT NULL DEFAULT '',
		api_key_id        TEXT NOT NULL DEFAULT '',
		key_group_id      TEXT NOT NULL DEFAULT '',
		service_name      TEXT NOT NULL DEFAULT '',
		provider          TEXT NOT NULL DEFAULT '',
		conversation_id   TEXT NOT NULL DEFAULT '',
		model             TEXT NOT NULL DEFAULT '',
		input_tokens      INTEGER NOT NULL DEFAULT 0,
		output_tokens     INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
		billable_tokens   INTEGER NOT NULL DEFAULT 0,
		cost_usd_micros   INTEGER NOT NULL DEFAULT 0,
		priced            INTEGER NOT NULL DEFAULT 0,
		pricing_version   TEXT NOT NULL DEFAULT '',
		created_at        TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_conv_usage_key ON conversation_usage(api_key_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_conv_usage_conv ON conversation_usage(api_key_id, conversation_id)`,
	`CREATE INDEX IF NOT EXISTS idx_conv_usage_key_day ON conversation_usage(api_key_id, created_at, client_id, model)`,
	// The key_group_id index is created by runMigrations after safeAlters.
	// Existing databases already have conversation_usage without that column.
	`CREATE INDEX IF NOT EXISTS idx_conv_usage_created ON conversation_usage(created_at)`,

	// Immutable model price versions. Rates are USD micros per one million
	// tokens, keeping both storage and cost calculations integer-only.
	`CREATE TABLE IF NOT EXISTS model_pricing (
		id                         TEXT PRIMARY KEY,
		service_id                 TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		model                      TEXT NOT NULL,
		version                    TEXT NOT NULL,
		input_usd_micros_per_mtok  INTEGER NOT NULL DEFAULT 0,
		output_usd_micros_per_mtok INTEGER NOT NULL DEFAULT 0,
		cache_read_usd_micros_per_mtok INTEGER NOT NULL DEFAULT 0,
		cache_creation_usd_micros_per_mtok INTEGER NOT NULL DEFAULT 0,
		reasoning_usd_micros_per_mtok INTEGER NOT NULL DEFAULT 0,
		effective_from             TEXT NOT NULL,
		created_at                 TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(service_id, model, version)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_model_pricing_effective ON model_pricing(service_id, model, effective_from DESC)`,

	// Key Suites: named bundles of different-service keys for bulk client assignment.
	// Editing a suite propagates changes to all clients that received keys from it.
	`CREATE TABLE IF NOT EXISTS key_suites (
		id          TEXT PRIMARY KEY,
		name        TEXT NOT NULL UNIQUE,
		description TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL DEFAULT (datetime('now'))
	)`,

	`CREATE TABLE IF NOT EXISTS key_suite_assignments (
		suite_id   TEXT NOT NULL REFERENCES key_suites(id) ON DELETE CASCADE,
		client_id  TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (suite_id, client_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_key_suite_assignments_client ON key_suite_assignments(client_id)`,

	// One entry per service per suite. Each entry holds either a direct api_key_id
	// or an api_key_group_id (old round-robin groups) — exactly one must be set.
	`CREATE TABLE IF NOT EXISTS key_suite_entries (
		id          TEXT PRIMARY KEY,
		suite_id    TEXT NOT NULL REFERENCES key_suites(id) ON DELETE CASCADE,
		service_id  TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		api_key_id  TEXT REFERENCES api_keys(id) ON DELETE SET NULL,
		group_id    TEXT REFERENCES api_key_groups(id) ON DELETE SET NULL,
		env_name    TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL DEFAULT (datetime('now')),
		UNIQUE(suite_id, service_id)
	)`,
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
		"ALTER TABLE clients ADD COLUMN update_policy TEXT NOT NULL DEFAULT 'managed'",
		"ALTER TABLE client_update_rollouts ADD COLUMN artifacts TEXT NOT NULL DEFAULT '[]'",
		"ALTER TABLE client_update_rollouts ADD COLUMN failure_threshold_percent INTEGER NOT NULL DEFAULT 20",
		"ALTER TABLE client_update_jobs ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE canary_settings ADD COLUMN exclude_clients TEXT NOT NULL DEFAULT '[]'",
		"ALTER TABLE services ADD COLUMN key_directory TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE services ADD COLUMN default_acl TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE services ADD COLUMN delivery_mode TEXT NOT NULL DEFAULT 'proxy'",
		"ALTER TABLE services ADD COLUMN category TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE services ADD COLUMN usage_metering TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN acl TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN refresh_token TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE api_keys ADD COLUMN token_endpoint TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN subscription_info TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN usage_snapshot TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE api_keys ADD COLUMN upstream_proxy_url TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE placeholder_keys ADD COLUMN key_path TEXT NOT NULL DEFAULT ''",
		// CC v2 — collapse the multi-client-per-CC concept into a 1:1
		// client↔CC binding. client_id moves onto control_channels;
		// agent_type + placeholder_id likewise. cc_channels gains the
		// session_id (claude resume id), cwd (per-channel project dir)
		// and kind ("management" | "task") that the new daemon needs.
		"ALTER TABLE control_channels ADD COLUMN client_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE control_channels ADD COLUMN agent_type TEXT NOT NULL DEFAULT 'claude_code'",
		"ALTER TABLE control_channels ADD COLUMN placeholder_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE cc_channels ADD COLUMN session_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE cc_channels ADD COLUMN cwd TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE cc_channels ADD COLUMN kind TEXT NOT NULL DEFAULT 'task'",
		// Key Group v2: score-based selection with 429 auto-rotation
		"ALTER TABLE placeholder_keys ADD COLUMN key_group_id TEXT REFERENCES key_groups(id)",
		// Key Group v3: pluggable rotation strategies + round-robin last_used tracking
		"ALTER TABLE key_groups ADD COLUMN rotation_strategy TEXT NOT NULL DEFAULT 'score'",
		"ALTER TABLE key_group_members ADD COLUMN last_used_at TEXT",
		// Key Suites: track which suite each placeholder came from
		"ALTER TABLE placeholder_keys ADD COLUMN suite_id TEXT REFERENCES key_suites(id) ON DELETE SET NULL",
		"ALTER TABLE conversation_usage ADD COLUMN provider TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE conversation_usage ADD COLUMN key_group_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE conversation_usage ADD COLUMN billable_tokens INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE conversation_usage ADD COLUMN cost_usd_micros INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE conversation_usage ADD COLUMN priced INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE conversation_usage ADD COLUMN pricing_version TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE conversation_usage ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE model_pricing ADD COLUMN reasoning_usd_micros_per_mtok INTEGER NOT NULL DEFAULT 0",
	}
	for _, alt := range safeAlters {
		if _, err := db.Exec(alt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("safe alter: %w", err)
		}
	}
	required := []string{
		`CREATE INDEX IF NOT EXISTS idx_conv_usage_key_day ON conversation_usage(api_key_id, created_at, client_id, model)`,
		`CREATE INDEX IF NOT EXISTS idx_conv_usage_group_day ON conversation_usage(key_group_id, created_at, client_id, model)`,
		`CREATE INDEX IF NOT EXISTS idx_conv_usage_created ON conversation_usage(created_at)`,
		`UPDATE conversation_usage SET provider = service_name WHERE provider = '' AND service_name != ''`,
		`UPDATE conversation_usage
		SET billable_tokens = MAX(input_tokens, 0) + MAX(output_tokens, 0) +
			CASE WHEN lower(provider) = 'openai' THEN 0 ELSE
				MAX(cache_read_tokens, 0) + MAX(cache_creation_tokens, 0) + MAX(reasoning_tokens, 0) END
		WHERE billable_tokens = 0 AND
			(input_tokens != 0 OR output_tokens != 0 OR cache_read_tokens != 0 OR cache_creation_tokens != 0 OR reasoning_tokens != 0)`,
		`
		INSERT OR IGNORE INTO key_suite_assignments (suite_id, client_id)
		SELECT DISTINCT suite_id, client_id
		FROM placeholder_keys
		WHERE suite_id IS NOT NULL AND suite_id != '' AND client_id != ''
	`,
		"DROP TABLE IF EXISTS client_cc",
		"DELETE FROM cc_channels WHERE client_id IS NULL OR client_id = ''",
		"DELETE FROM control_channels WHERE client_id = ''",
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_cc_client ON control_channels(client_id) WHERE client_id != ''`,
		`UPDATE services
		 SET host_pattern = 'discord.com,api.discord.com,gateway.discord.gg,*.discordapp.net', upstream_url = 'https://discord.com/api/v10'
		 WHERE name = 'discord' AND host_pattern = 'discord.com'`,
		`UPDATE services SET key_prefix = 'github_pat_', key_length = 93, delivery_mode = 'proxy'
		 WHERE name = 'github' AND key_prefix = 'ghp_' AND key_length = 40 AND delivery_mode = 'loan_proxy'`,
	}
	for i, statement := range required {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("post migration statement %d: %w", i, err)
		}
	}

	// CC v2: drop the old assignment table (subsumed by control_channels.client_id)
	// and clear out any stale rows that pre-date the redesign — the user agreed
	// to scrap old CC data when planning this change.
	// Enforce 1:1 client↔CC at the index level. Partial index so empty
	// values during the alter window above don't trip the constraint.

	// Widen the existing `discord` service's host_pattern so the duckway-client
	// MITM proxy can also intercept the WSS gateway + CDN hosts that the
	// Control Channel feature needs. Only touches rows that still hold the
	// old narrow pattern — leaves admin-customised values alone.

	// GitHub fine-grained PATs use github_pat_* and the simple default should
	// stay in phantom-token proxy mode. Only touch rows that still match the
	// former shipped default, leaving admin-customized GitHub services alone.

	// Seed the OpenAI service so Codex CLI (and any OpenAI-compatible tool)
	// works without manual admin UI setup. INSERT OR IGNORE keeps it safe on
	// existing databases that already have a user-created openai service.
	if _, err := db.Exec(`INSERT OR IGNORE INTO services
		(id, name, display_name, upstream_url, host_pattern,
		 auth_type, auth_header, auth_prefix,
		 key_prefix, key_length, key_directory, delivery_mode, is_active)
		VALUES
		('svc-openai-default', 'openai', 'OpenAI', 'https://api.openai.com', 'api.openai.com',
		 'bearer', 'Authorization', 'Bearer ',
		 'sk-', 64, '.config/openai/credentials', 'proxy', 1)`); err != nil {
		return fmt.Errorf("seed OpenAI service: %w", err)
	}

	// Seed xAI so Grok Build can use Duckway phantom tokens without manual
	// service setup on existing installations.
	if _, err := db.Exec(`INSERT OR IGNORE INTO services
		(id, name, display_name, upstream_url, host_pattern,
		 auth_type, auth_header, auth_prefix,
		 key_prefix, key_length, key_directory, delivery_mode, is_active)
		VALUES
		('svc-xai-default', 'xai', 'xAI / Grok', 'https://api.x.ai', 'api.x.ai,cli-chat-proxy.grok.com',
		 'bearer', 'Authorization', 'Bearer ',
		 'xai-', 80, '.config/xai/credentials', 'proxy', 1)`); err != nil {
		return fmt.Errorf("seed xAI service: %w", err)
	}

	return nil
}

func isDuplicateColumn(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42701"
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column")
}
