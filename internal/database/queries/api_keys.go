package queries

import (
	"database/sql"

	"github.com/hackerduck/duckway/internal/models"
)

type APIKeyQueries struct {
	db *sql.DB
}

func NewAPIKeyQueries(db *sql.DB) *APIKeyQueries {
	return &APIKeyQueries{db: db}
}

const apiKeyCols = "k.id, k.service_id, k.name, k.key_encrypted, k.acl, k.refresh_token, k.expires_at, k.token_endpoint, k.subscription_info, k.usage_snapshot, k.is_active, k.usage_count, k.last_used_at, k.created_at, s.name"

func scanAPIKey(row interface{ Scan(...interface{}) error }, k *models.APIKey) error {
	err := row.Scan(&k.ID, &k.ServiceID, &k.Name, &k.KeyEncrypted, &k.ACL, &k.RefreshToken, &k.ExpiresAt, &k.TokenEndpoint, &k.SubscriptionInfo, &k.UsageSnapshot, &k.IsActive, &k.UsageCount, &k.LastUsedAt, &k.CreatedAt, &k.ServiceName)
	if err == nil {
		k.IsRefreshable = k.RefreshToken != ""
	}
	return err
}

func (q *APIKeyQueries) List(serviceID string) ([]models.APIKey, error) {
	query := `SELECT ` + apiKeyCols + ` FROM api_keys k JOIN services s ON k.service_id = s.id`
	var args []interface{}

	if serviceID != "" {
		query += " WHERE k.service_id = ?"
		args = append(args, serviceID)
	}
	query += " ORDER BY k.created_at DESC"

	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.APIKey
	for rows.Next() {
		var k models.APIKey
		if err := scanAPIKey(rows, &k); err != nil {
			return nil, err
		}
		result = append(result, k)
	}
	return result, rows.Err()
}

func (q *APIKeyQueries) GetByID(id string) (*models.APIKey, error) {
	var k models.APIKey
	err := scanAPIKey(q.db.QueryRow(
		`SELECT `+apiKeyCols+` FROM api_keys k JOIN services s ON k.service_id = s.id WHERE k.id = ?`, id,
	), &k)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (q *APIKeyQueries) Create(k *models.APIKey) error {
	_, err := q.db.Exec(
		`INSERT INTO api_keys (id, service_id, name, key_encrypted, acl, refresh_token, expires_at, token_endpoint, subscription_info)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.ServiceID, k.Name, k.KeyEncrypted, k.ACL, k.RefreshToken, k.ExpiresAt, k.TokenEndpoint, k.SubscriptionInfo,
	)
	return err
}

func (q *APIKeyQueries) Update(k *models.APIKey) error {
	_, err := q.db.Exec(
		"UPDATE api_keys SET name=?, key_encrypted=?, acl=?, is_active=? WHERE id=?",
		k.Name, k.KeyEncrypted, k.ACL, k.IsActive, k.ID,
	)
	return err
}

func (q *APIKeyQueries) UpdateACL(id, acl string) error {
	_, err := q.db.Exec("UPDATE api_keys SET acl = ? WHERE id = ?", acl, id)
	return err
}

func (q *APIKeyQueries) Delete(id string) error {
	_, err := q.db.Exec("DELETE FROM api_keys WHERE id = ?", id)
	return err
}

func (q *APIKeyQueries) UpdateTokens(id, keyEncrypted string, expiresAt int64) error {
	_, err := q.db.Exec(
		"UPDATE api_keys SET key_encrypted = ?, expires_at = ?, last_used_at = datetime('now') WHERE id = ?",
		keyEncrypted, expiresAt, id,
	)
	return err
}

func (q *APIKeyQueries) UpdateRefreshToken(id, refreshToken string) error {
	_, err := q.db.Exec("UPDATE api_keys SET refresh_token = ? WHERE id = ?", refreshToken, id)
	return err
}

func (q *APIKeyQueries) UpdateRefreshable(id, name, keyEncrypted, refreshToken, tokenEndpoint, subscriptionInfo string) error {
	query := "UPDATE api_keys SET name = ?, token_endpoint = ?, subscription_info = ?"
	args := []interface{}{name, tokenEndpoint, subscriptionInfo}
	if keyEncrypted != "" {
		query += ", key_encrypted = ?"
		args = append(args, keyEncrypted)
	}
	if refreshToken != "" {
		query += ", refresh_token = ?"
		args = append(args, refreshToken)
	}
	query += " WHERE id = ?"
	args = append(args, id)
	_, err := q.db.Exec(query, args...)
	return err
}

func (q *APIKeyQueries) ListExpiring(withinMinutes int) ([]models.APIKey, error) {
	rows, err := q.db.Query(
		`SELECT `+apiKeyCols+` FROM api_keys k JOIN services s ON k.service_id = s.id
		 WHERE k.is_active = 1 AND k.refresh_token != '' AND k.expires_at > 0
		 AND k.expires_at < (strftime('%s','now') * 1000 + ? * 60 * 1000)`,
		withinMinutes,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.APIKey
	for rows.Next() {
		var k models.APIKey
		if err := scanAPIKey(rows, &k); err != nil {
			return nil, err
		}
		result = append(result, k)
	}
	return result, rows.Err()
}

func (q *APIKeyQueries) IncrementUsage(id string) error {
	_, err := q.db.Exec(
		"UPDATE api_keys SET usage_count = usage_count + 1, last_used_at = datetime('now') WHERE id = ?", id,
	)
	return err
}

// UpdateUsageSnapshot stores the JSON-encoded rate-limit snapshot from the
// most recent upstream response. snapshot must be a JSON object string —
// pass "" to clear.
func (q *APIKeyQueries) UpdateUsageSnapshot(id, snapshot string) error {
	_, err := q.db.Exec("UPDATE api_keys SET usage_snapshot = ? WHERE id = ?", snapshot, id)
	return err
}
