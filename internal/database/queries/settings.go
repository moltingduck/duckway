package queries

import (
	"database/sql"
	"sort"
)

type SettingsQueries struct {
	db *sql.DB
}

func NewSettingsQueries(db *sql.DB) *SettingsQueries {
	return &SettingsQueries{db: db}
}

func (q *SettingsQueries) Get(key string) string {
	var val string
	q.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&val)
	return val
}

func (q *SettingsQueries) Set(key, value string) error {
	_, err := q.db.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?",
		key, value, value,
	)
	return err
}

// SetMany writes every pair in one transaction: all of them or none.
func (q *SettingsQueries) SetMany(kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, k := range keys {
		v := kv[k]
		if _, err := tx.Exec(
			"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = ?",
			k, v, v,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (q *SettingsQueries) GetAll() map[string]string {
	rows, err := q.db.Query("SELECT key, value FROM settings")
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		result[k] = v
	}
	return result
}

// Well-known setting keys
const (
	SettingGatewayURL               = "gateway_url"                  // e.g., http://duckway-gw.tailnet:8080
	SettingProxyPort                = "proxy_port"                   // suggested client proxy port, default 18080
	SettingRequestLogCaptureOn      = "request_log_capture_enabled"  // "1" / "0"
	SettingRequestLogCaptureClients = "request_log_capture_clients"  // JSON array of client IDs; empty = all
	SettingApprovalRequestTTL       = "approval_request_ttl_minutes" // pending approvals auto-marked "ignored" after this; default 60
	SettingSessionEpoch             = "session_epoch"                // admin session generation; bumped on password change to sign out other sessions
	SettingAgentStatuslineScript    = "agent_statusline_script"      // shell script body served via GET /client/statusline; agents `duckway sync` writes it to ~/.duckway/statusline.sh
)
