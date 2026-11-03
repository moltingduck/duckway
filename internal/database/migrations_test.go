package database

import (
	"strings"
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

// allow_encoded_slash is additive: an existing services table gains it via the
// shared safeAlters path (SQLite and PostgreSQL both run it), default off.
func TestMigrationAddsAllowEncodedSlashDefaultOff(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`ALTER TABLE services DROP COLUMN allow_encoded_slash`); err != nil {
		t.Fatalf("simulate an older schema: %v", err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := runMigrations(db); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM services WHERE allow_encoded_slash <> 0`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("column missing or not default-off: n=%d err=%v", n, err)
	}
}

// Duplicate pending scoped approvals (possible before the unique index existed)
// must not block startup: the oldest stays pending, the rest are ignored.
func TestMigrationCollapsesDuplicatePendingScopedApprovals(t *testing.T) {
	scopeX, scopeY := "rule:"+strings.Repeat("a1", 32), "rule:"+strings.Repeat("c3", 32)
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	for _, idx := range []string{"idx_approvals_pending_scope", "idx_approvals_pending_wide"} {
		if _, err := db.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
			t.Fatal(err)
		}
	}
	ins := func(id, ph, status, scope, created string) {
		if _, err := db.Exec(`INSERT INTO approvals (id, placeholder_id, status, scope, created_at) VALUES (?,?,?,?,?)`, id, ph, status, scope, created); err != nil {
			t.Fatal(err)
		}
	}
	ins("old", "p1", "pending", scopeX, "2026-01-01 00:00:00")
	ins("dup1", "p1", "pending", scopeX, "2026-01-01 00:00:05")
	ins("dup2", "p1", "pending", scopeX, "2026-01-01 00:00:09")
	ins("other-ph", "p2", "pending", scopeX, "2026-01-01 00:00:07") // different token: kept
	ins("other-scope", "p1", "pending", scopeY, "2026-01-01 00:00:08")
	ins("wide1", "p1", "pending", "", "2026-01-01 00:00:01") // duplicate token-wide pending rows: oldest stays pending
	ins("wide2", "p1", "pending", "", "2026-01-01 00:00:02")
	ins("done", "p1", "approved", scopeX, "2026-01-01 00:00:03")
	for i := 0; i < 2; i++ { // second run: idempotent
		if err := runMigrations(db); err != nil {
			t.Fatalf("migration must not fail on duplicates: %v", err)
		}
	}
	status := func(id string) string {
		var s string
		_ = db.QueryRow(`SELECT status FROM approvals WHERE id = ?`, id).Scan(&s)
		return s
	}
	for id, want := range map[string]string{"old": "pending", "dup1": "ignored", "dup2": "ignored", "other-ph": "pending", "other-scope": "pending", "wide1": "pending", "wide2": "ignored", "done": "approved"} {
		if got := status(id); got != want {
			t.Errorf("%s: status %q, want %q", id, got, want)
		}
	}
	if _, err := db.Exec(`INSERT INTO approvals (id, placeholder_id, status, scope) VALUES ('x','p1','pending',?)`, scopeX); err == nil {
		t.Fatal("the unique index must be back")
	}
}

// Pre-HMAC scoped approvals hold raw "rule:METHOD path?query#sha256" text. The
// migration retires them (an approved one must not become token-wide), clears
// the text, leaves current "rule:<64 hex>" scopes alone, and is idempotent.
func TestMigrationRetiresLegacyRawScopeApprovals(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_approvals_pending_scope`); err != nil { // an older database has no such index yet
		t.Fatal(err)
	}
	current := "rule:" + strings.Repeat("a1", 32)
	ins := func(id, status, scope, expires string) {
		if _, err := db.Exec(`INSERT INTO approvals (id, placeholder_id, status, scope, expires_at) VALUES (?,?,?,?,?)`, id, "p1", status, scope, expires); err != nil {
			t.Fatal(err)
		}
	}
	legacy := "rule:POST /v1/pay?api_key=SECRET#" + strings.Repeat("b2", 32)
	ins("lp", "pending", legacy, "")
	ins("lp2", "pending", legacy, "") // duplicate legacy pending: must not trip the unique index
	ins("la", "approved", legacy, "2099-01-01 00:00:00")
	ins("lr", "rejected", legacy, "")
	ins("cur", "pending", current, "")
	ins("curA", "approved", current, "2099-01-01 00:00:00")
	ins("wide", "approved", "", "2099-01-01 00:00:00")
	for i := 0; i < 2; i++ {
		if err := runMigrations(db); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	get := func(id string) (status, scope, expires string) {
		_ = db.QueryRow(`SELECT status, scope, COALESCE(expires_at,'') FROM approvals WHERE id = ?`, id).Scan(&status, &scope, &expires)
		return
	}
	for _, id := range []string{"lp", "lp2", "la"} {
		st, sc, _ := get(id)
		if st != "ignored" || sc != "" {
			t.Errorf("%s: status=%q scope=%q, want ignored and cleared", id, st, sc)
		}
	}
	if _, _, exp := get("la"); exp > time.Now().UTC().Add(time.Minute).Format("2006-01-02 15:04:05") {
		t.Errorf("approved legacy row must expire now, expires=%q", exp)
	}
	if st, sc, _ := get("lr"); st != "rejected" || sc != "" {
		t.Errorf("rejected legacy: %q %q", st, sc)
	}
	if st, sc, _ := get("cur"); st != "pending" || sc != current {
		t.Errorf("current pending scope must be untouched: %q %q", st, sc)
	}
	if st, sc, exp := get("curA"); st != "approved" || sc != current || exp != "2099-01-01 00:00:00" {
		t.Errorf("current approved scope must be untouched: %q %q %q", st, sc, exp)
	}
	if st, sc, _ := get("wide"); st != "approved" || sc != "" {
		t.Errorf("token-wide approval must be untouched: %q %q", st, sc)
	}
	var raw int
	_ = db.QueryRow(`SELECT COUNT(*) FROM approvals WHERE scope LIKE '%SECRET%'`).Scan(&raw)
	if raw != 0 {
		t.Fatal("no raw query may remain stored")
	}
}

func TestMigrationAddsRequestLogDecisionIdempotently(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO request_log (service_name, method, path, status_code) VALUES ('s', 'GET', '/legacy', 200)`); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"approval_id", "decision"} {
		if _, err := db.Exec(`ALTER TABLE request_log DROP COLUMN ` + col); err != nil {
			t.Fatalf("simulate an older schema: %v", err)
		}
	}
	for i := 0; i < 3; i++ { // idempotent
		if err := runMigrations(db); err != nil {
			t.Fatal(err)
		}
	}
	var decision, approval string
	if err := db.QueryRow(`SELECT decision, approval_id FROM request_log WHERE path = '/legacy'`).Scan(&decision, &approval); err != nil || decision != "" || approval != "" {
		t.Fatalf("legacy row must default to empty decision: %q %q %v", decision, approval, err)
	}
}
