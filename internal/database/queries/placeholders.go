package queries

import (
	"database/sql"

	"github.com/hackerduck/duckway/internal/models"
)

type PlaceholderQueries struct {
	db *sql.DB
}

func NewPlaceholderQueries(db *sql.DB) *PlaceholderQueries {
	return &PlaceholderQueries{db: db}
}

func (q *PlaceholderQueries) List(clientID, serviceID string) ([]models.PlaceholderKey, error) {
	query := `SELECT p.id, p.env_name, p.placeholder, p.service_id, p.api_key_id, p.group_id,
		p.client_id, p.permission_config, p.requires_approval, p.approval_ttl_minutes,
		p.is_active, p.usage_count, p.last_used_at, p.created_at,
		s.name, c.name
		FROM placeholder_keys p
		JOIN services s ON p.service_id = s.id
		JOIN clients c ON p.client_id = c.id
		WHERE 1=1`
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
		if err := rows.Scan(&p.ID, &p.EnvName, &p.Placeholder, &p.ServiceID, &p.APIKeyID, &p.GroupID,
			&p.ClientID, &p.PermissionConfig, &p.RequiresApproval, &p.ApprovalTTLMinutes,
			&p.IsActive, &p.UsageCount, &p.LastUsedAt, &p.CreatedAt,
			&p.ServiceName, &p.ClientName); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (q *PlaceholderQueries) GetByID(id string) (*models.PlaceholderKey, error) {
	var p models.PlaceholderKey
	err := q.db.QueryRow(
		`SELECT p.id, p.env_name, p.placeholder, p.service_id, p.api_key_id, p.group_id,
		 p.client_id, p.permission_config, p.requires_approval, p.approval_ttl_minutes,
		 p.is_active, p.usage_count, p.last_used_at, p.created_at,
		 s.name, c.name
		 FROM placeholder_keys p
		 JOIN services s ON p.service_id = s.id
		 JOIN clients c ON p.client_id = c.id
		 WHERE p.id = ?`, id,
	).Scan(&p.ID, &p.EnvName, &p.Placeholder, &p.ServiceID, &p.APIKeyID, &p.GroupID,
		&p.ClientID, &p.PermissionConfig, &p.RequiresApproval, &p.ApprovalTTLMinutes,
		&p.IsActive, &p.UsageCount, &p.LastUsedAt, &p.CreatedAt,
		&p.ServiceName, &p.ClientName)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (q *PlaceholderQueries) GetByPlaceholder(placeholder string) (*models.PlaceholderKey, error) {
	var p models.PlaceholderKey
	err := q.db.QueryRow(
		`SELECT p.id, p.env_name, p.placeholder, p.service_id, p.api_key_id, p.group_id,
		 p.client_id, p.permission_config, p.requires_approval, p.approval_ttl_minutes,
		 p.is_active, p.usage_count, p.last_used_at, p.created_at,
		 s.name, c.name
		 FROM placeholder_keys p
		 JOIN services s ON p.service_id = s.id
		 JOIN clients c ON p.client_id = c.id
		 WHERE p.placeholder = ? AND p.is_active = 1`, placeholder,
	).Scan(&p.ID, &p.EnvName, &p.Placeholder, &p.ServiceID, &p.APIKeyID, &p.GroupID,
		&p.ClientID, &p.PermissionConfig, &p.RequiresApproval, &p.ApprovalTTLMinutes,
		&p.IsActive, &p.UsageCount, &p.LastUsedAt, &p.CreatedAt,
		&p.ServiceName, &p.ClientName)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (q *PlaceholderQueries) GetByClientAndService(clientID, serviceID string) (*models.PlaceholderKey, error) {
	var p models.PlaceholderKey
	err := q.db.QueryRow(
		`SELECT p.id, p.env_name, p.placeholder, p.service_id, p.api_key_id, p.group_id,
		 p.client_id, p.permission_config, p.requires_approval, p.approval_ttl_minutes,
		 p.is_active, p.usage_count, p.last_used_at, p.created_at,
		 s.name, c.name
		 FROM placeholder_keys p
		 JOIN services s ON p.service_id = s.id
		 JOIN clients c ON p.client_id = c.id
		 WHERE p.client_id = ? AND p.service_id = ? AND p.is_active = 1`, clientID, serviceID,
	).Scan(&p.ID, &p.EnvName, &p.Placeholder, &p.ServiceID, &p.APIKeyID, &p.GroupID,
		&p.ClientID, &p.PermissionConfig, &p.RequiresApproval, &p.ApprovalTTLMinutes,
		&p.IsActive, &p.UsageCount, &p.LastUsedAt, &p.CreatedAt,
		&p.ServiceName, &p.ClientName)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (q *PlaceholderQueries) Create(p *models.PlaceholderKey) error {
	_, err := q.db.Exec(
		`INSERT INTO placeholder_keys (id, env_name, placeholder, service_id, api_key_id, group_id, client_id, permission_config, requires_approval, approval_ttl_minutes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.EnvName, p.Placeholder, p.ServiceID, p.APIKeyID, p.GroupID, p.ClientID, p.PermissionConfig, p.RequiresApproval, p.ApprovalTTLMinutes,
	)
	return err
}

func (q *PlaceholderQueries) Update(p *models.PlaceholderKey) error {
	_, err := q.db.Exec(
		`UPDATE placeholder_keys SET env_name=?, api_key_id=?, group_id=?, permission_config=?, requires_approval=?, approval_ttl_minutes=?, is_active=? WHERE id=?`,
		p.EnvName, p.APIKeyID, p.GroupID, p.PermissionConfig, p.RequiresApproval, p.ApprovalTTLMinutes, p.IsActive, p.ID,
	)
	return err
}

func (q *PlaceholderQueries) Delete(id string) error {
	_, err := q.db.Exec("DELETE FROM placeholder_keys WHERE id = ?", id)
	return err
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
