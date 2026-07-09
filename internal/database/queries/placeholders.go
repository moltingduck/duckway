package queries

import (
	"database/sql"
	"fmt"

	"github.com/hackerduck/duckway/internal/models"
)

type PlaceholderQueries struct {
	db *sql.DB
}

func NewPlaceholderQueries(db *sql.DB) *PlaceholderQueries {
	return &PlaceholderQueries{db: db}
}

const phSelect = `SELECT p.id, p.env_name, p.placeholder, p.service_id, p.api_key_id, p.group_id,
	p.client_id, p.permission_config, p.requires_approval, p.approval_ttl_minutes,
	p.key_path, p.is_active, p.usage_count, p.last_used_at, p.created_at,
	p.suite_id,
	s.name, c.name, k.name
	FROM placeholder_keys p
	JOIN services s ON p.service_id = s.id
	JOIN clients c ON p.client_id = c.id
	LEFT JOIN api_keys k ON p.api_key_id = k.id`

func scanPH(row interface{ Scan(...interface{}) error }, p *models.PlaceholderKey) error {
	return row.Scan(&p.ID, &p.EnvName, &p.Placeholder, &p.ServiceID, &p.APIKeyID, &p.GroupID,
		&p.ClientID, &p.PermissionConfig, &p.RequiresApproval, &p.ApprovalTTLMinutes,
		&p.KeyPath, &p.IsActive, &p.UsageCount, &p.LastUsedAt, &p.CreatedAt,
		&p.SuiteID,
		&p.ServiceName, &p.ClientName, &p.APIKeyName)
}

func (q *PlaceholderQueries) List(clientID, serviceID string) ([]models.PlaceholderKey, error) {
	query := phSelect + " WHERE 1=1"
	var args []interface{}

	if clientID != "" {
		query += " AND p.client_id = ?"
		args = append(args, clientID)
	}
	if serviceID != "" {
		query += " AND p.service_id = ?"
		args = append(args, serviceID)
	}
	query += " ORDER BY p.created_at DESC"

	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.PlaceholderKey
	for rows.Next() {
		var p models.PlaceholderKey
		if err := scanPH(rows, &p); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (q *PlaceholderQueries) GetByID(id string) (*models.PlaceholderKey, error) {
	var p models.PlaceholderKey
	err := scanPH(q.db.QueryRow(phSelect+" WHERE p.id = ?", id), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (q *PlaceholderQueries) GetByPlaceholder(placeholder string) (*models.PlaceholderKey, error) {
	var p models.PlaceholderKey
	err := scanPH(q.db.QueryRow(phSelect+" WHERE p.placeholder = ? AND p.is_active = 1", placeholder), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (q *PlaceholderQueries) GetByClientAndService(clientID, serviceID string) (*models.PlaceholderKey, error) {
	var p models.PlaceholderKey
	err := scanPH(q.db.QueryRow(phSelect+" WHERE p.client_id = ? AND p.service_id = ? AND p.is_active = 1", clientID, serviceID), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (q *PlaceholderQueries) GetByClientServiceEnv(clientID, serviceID, envName string) (*models.PlaceholderKey, error) {
	var p models.PlaceholderKey
	err := scanPH(q.db.QueryRow(phSelect+" WHERE p.client_id = ? AND p.service_id = ? AND p.env_name = ?", clientID, serviceID, envName), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (q *PlaceholderQueries) GetByClientAPIKey(clientID, apiKeyID string) (*models.PlaceholderKey, error) {
	var p models.PlaceholderKey
	err := scanPH(q.db.QueryRow(phSelect+" WHERE p.client_id = ? AND p.api_key_id = ? ORDER BY p.created_at DESC LIMIT 1", clientID, apiKeyID), &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (q *PlaceholderQueries) Create(p *models.PlaceholderKey) error {
	_, err := q.db.Exec(
		`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, group_id, client_id, permission_config, requires_approval, approval_ttl_minutes, key_path, suite_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.EnvName, p.Placeholder, p.ServiceID, p.APIKeyID, p.GroupID, p.ClientID, p.PermissionConfig, p.RequiresApproval, p.ApprovalTTLMinutes, p.KeyPath, p.SuiteID,
	)
	return err
}

func (q *PlaceholderQueries) Update(p *models.PlaceholderKey) error {
	_, err := q.db.Exec(
		`UPDATE placeholder_keys SET env_name=?, placeholder=?, api_key_id=?, group_id=?, permission_config=?, requires_approval=?, approval_ttl_minutes=?, key_path=?, is_active=? WHERE id=?`,
		p.EnvName, p.Placeholder, p.APIKeyID, p.GroupID, p.PermissionConfig, p.RequiresApproval, p.ApprovalTTLMinutes, p.KeyPath, p.IsActive, p.ID,
	)
	return err
}

func (q *PlaceholderQueries) Delete(id string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// request_log keeps historical rows after a phantom token is removed.
	// Its FK is intentionally nullable, so detach logs before deleting the
	// placeholder row. Approvals use ON DELETE CASCADE and are removed by the
	// placeholder delete below.
	if _, err := tx.Exec("UPDATE request_log SET placeholder_id = NULL WHERE placeholder_id = ?", id); err != nil {
		return fmt.Errorf("detach request logs: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM placeholder_keys WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (q *PlaceholderQueries) IncrementUsage(id string) error {
	_, err := q.db.Exec(
		"UPDATE placeholder_keys SET usage_count = usage_count + 1, last_used_at = datetime('now') WHERE id = ?", id,
	)
	return err
}

func (q *PlaceholderQueries) ListByClient(clientID string) ([]models.PlaceholderKey, error) {
	return q.List(clientID, "")
}

func (q *PlaceholderQueries) UpdatePlaceholder(id, newPlaceholder string) error {
	_, err := q.db.Exec("UPDATE placeholder_keys SET placeholder=? WHERE id=?", newPlaceholder, id)
	return err
}
