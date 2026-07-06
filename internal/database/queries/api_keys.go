package queries

import (
	"database/sql"
	"fmt"

	"github.com/hackerduck/duckway/internal/models"
)

type APIKeyQueries struct {
	db *sql.DB
}

type RefreshableDeleteImpact struct {
	KeyID           string                       `json:"key_id"`
	KeyName         string                       `json:"key_name"`
	ServiceName     string                       `json:"service_name"`
	KeySuites       []RefreshableKeySuiteImpact  `json:"key_suites"`
	Clients         []RefreshableClientImpact    `json:"clients"`
	ControlChannels []RefreshableControlCCImpact `json:"control_channels"`
}

type RefreshableKeySuiteImpact struct {
	EntryID     string `json:"entry_id"`
	SuiteID     string `json:"suite_id"`
	SuiteName   string `json:"suite_name"`
	ServiceName string `json:"service_name"`
	EnvName     string `json:"env_name"`
}

type RefreshableClientImpact struct {
	PlaceholderID string  `json:"placeholder_id"`
	ClientID      string  `json:"client_id"`
	ClientName    string  `json:"client_name"`
	ServiceName   string  `json:"service_name"`
	EnvName       string  `json:"env_name"`
	SuiteID       *string `json:"suite_id,omitempty"`
}

type RefreshableControlCCImpact struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ClientID    string `json:"client_id"`
	ClientName  string `json:"client_name"`
	AgentType   string `json:"agent_type"`
	ServiceName string `json:"service_name"`
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

func (q *APIKeyQueries) DeleteWithControlChannelCleanup(id string) error {
	key, err := q.GetByID(id)
	if err != nil {
		return err
	}
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var ccCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM control_channels WHERE api_key_id = ?`, id).Scan(&ccCount); err != nil {
		return err
	}
	if ccCount == 0 {
		if _, err := tx.Exec("DELETE FROM api_keys WHERE id = ?", id); err != nil {
			return err
		}
		return tx.Commit()
	}

	if _, err := tx.Exec(`UPDATE control_channels SET is_active = 0 WHERE api_key_id = ?`, id); err != nil {
		return fmt.Errorf("disable control channels: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE api_keys
		SET name = ?, key_encrypted = '', refresh_token = '', expires_at = 0,
		    token_endpoint = '', subscription_info = '', usage_snapshot = '',
		    is_active = 0
		WHERE id = ?`, "Deleted API key: "+key.Name, id); err != nil {
		return fmt.Errorf("disable retained key reference: %w", err)
	}
	return tx.Commit()
}

func (q *APIKeyQueries) RefreshableDeleteImpact(id string) (*RefreshableDeleteImpact, error) {
	key, err := q.GetByID(id)
	if err != nil {
		return nil, err
	}
	if !key.IsRefreshable {
		return nil, fmt.Errorf("not a refreshable key")
	}
	impact := &RefreshableDeleteImpact{
		KeyID:       key.ID,
		KeyName:     key.Name,
		ServiceName: key.ServiceName,
	}

	suiteRows, err := q.db.Query(`
		SELECT e.id, ks.id, ks.name, s.name, e.env_name
		FROM key_suite_entries e
		JOIN key_suites ks ON ks.id = e.suite_id
		JOIN services s ON s.id = e.service_id
		WHERE e.api_key_id = ?
		ORDER BY ks.name, s.name`, id)
	if err != nil {
		return nil, err
	}
	defer suiteRows.Close()
	for suiteRows.Next() {
		var row RefreshableKeySuiteImpact
		if err := suiteRows.Scan(&row.EntryID, &row.SuiteID, &row.SuiteName, &row.ServiceName, &row.EnvName); err != nil {
			return nil, err
		}
		impact.KeySuites = append(impact.KeySuites, row)
	}
	if err := suiteRows.Err(); err != nil {
		return nil, err
	}

	clientRows, err := q.db.Query(`
		SELECT p.id, c.id, c.name, s.name, p.env_name, p.suite_id
		FROM placeholder_keys p
		JOIN clients c ON c.id = p.client_id
		JOIN services s ON s.id = p.service_id
		WHERE p.api_key_id = ?
		ORDER BY c.name, s.name, p.env_name`, id)
	if err != nil {
		return nil, err
	}
	defer clientRows.Close()
	for clientRows.Next() {
		var row RefreshableClientImpact
		if err := clientRows.Scan(&row.PlaceholderID, &row.ClientID, &row.ClientName, &row.ServiceName, &row.EnvName, &row.SuiteID); err != nil {
			return nil, err
		}
		impact.Clients = append(impact.Clients, row)
	}
	if err := clientRows.Err(); err != nil {
		return nil, err
	}

	ccRows, err := q.db.Query(`
		SELECT cc.id, cc.name, cc.client_id, c.name, cc.agent_type, s.name
		FROM control_channels cc
		JOIN clients c ON c.id = cc.client_id
		JOIN services s ON s.id = cc.service_id
		WHERE cc.api_key_id = ?
		ORDER BY cc.name`, id)
	if err != nil {
		return nil, err
	}
	defer ccRows.Close()
	for ccRows.Next() {
		var row RefreshableControlCCImpact
		if err := ccRows.Scan(&row.ID, &row.Name, &row.ClientID, &row.ClientName, &row.AgentType, &row.ServiceName); err != nil {
			return nil, err
		}
		impact.ControlChannels = append(impact.ControlChannels, row)
	}
	if err := ccRows.Err(); err != nil {
		return nil, err
	}

	return impact, nil
}

func (q *APIKeyQueries) DeleteRefreshableWithCleanup(id string) (*RefreshableDeleteImpact, error) {
	impact, err := q.RefreshableDeleteImpact(id)
	if err != nil {
		return nil, err
	}

	tx, err := q.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var refreshToken string
	if err := tx.QueryRow(`SELECT refresh_token FROM api_keys WHERE id = ?`, id).Scan(&refreshToken); err != nil {
		return nil, err
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("not a refreshable key")
	}

	if _, err := tx.Exec(`DELETE FROM key_suite_entries WHERE api_key_id = ?`, id); err != nil {
		return nil, fmt.Errorf("remove key suite entries: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM api_key_group_members WHERE api_key_id = ?`, id); err != nil {
		return nil, fmt.Errorf("remove legacy key group members: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM key_group_members WHERE api_key_id = ?`, id); err != nil {
		return nil, fmt.Errorf("remove key group members: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE request_log
		SET placeholder_id = NULL
		WHERE placeholder_id IN (
			SELECT id FROM placeholder_keys WHERE api_key_id = ?
		)`, id); err != nil {
		return nil, fmt.Errorf("detach request logs: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM placeholder_keys WHERE api_key_id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete client placeholders: %w", err)
	}

	if len(impact.ControlChannels) > 0 {
		if _, err := tx.Exec(`UPDATE control_channels SET is_active = 0 WHERE api_key_id = ?`, id); err != nil {
			return nil, fmt.Errorf("disable control channels: %w", err)
		}
		if _, err := tx.Exec(`
			UPDATE api_keys
			SET name = ?, key_encrypted = '', refresh_token = '', expires_at = 0,
			    token_endpoint = '', subscription_info = '', usage_snapshot = '',
			    is_active = 0
			WHERE id = ?`, "Deleted refreshable token: "+impact.KeyName, id); err != nil {
			return nil, fmt.Errorf("disable retained key reference: %w", err)
		}
	} else {
		if _, err := tx.Exec(`DELETE FROM api_keys WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("delete api key: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return impact, nil
}

func (q *APIKeyQueries) Deactivate(id string) error {
	_, err := q.db.Exec("UPDATE api_keys SET is_active = 0 WHERE id = ?", id)
	return err
}

func (q *APIKeyQueries) UpdateTokens(id, keyEncrypted string, expiresAt int64) error {
	_, err := q.db.Exec(
		"UPDATE api_keys SET key_encrypted = ?, expires_at = ?, last_used_at = datetime('now'), is_active = 1 WHERE id = ?",
		keyEncrypted, expiresAt, id,
	)
	return err
}

func (q *APIKeyQueries) UpdateRefreshToken(id, refreshToken string) error {
	_, err := q.db.Exec("UPDATE api_keys SET refresh_token = ? WHERE id = ?", refreshToken, id)
	return err
}

func (q *APIKeyQueries) UpdateRefreshable(id, name, keyEncrypted, refreshToken, tokenEndpoint, subscriptionInfo string, expiresAt int64) error {
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
	if expiresAt > 0 {
		query += ", expires_at = ?"
		args = append(args, expiresAt)
	}
	if keyEncrypted != "" && refreshToken != "" {
		query += ", is_active = 1"
	}
	query += " WHERE id = ?"
	args = append(args, id)
	_, err := q.db.Exec(query, args...)
	return err
}

func (q *APIKeyQueries) SetActive(id string, active bool) error {
	_, err := q.db.Exec("UPDATE api_keys SET is_active = ? WHERE id = ?", boolToInt(active), id)
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
