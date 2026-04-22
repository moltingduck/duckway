package queries

import "database/sql"

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
	SettingGatewayURL = "gateway_url"  // e.g., http://duckway-gw.tailnet:8080
	SettingProxyPort  = "proxy_port"   // suggested client proxy port, default 18080
)
