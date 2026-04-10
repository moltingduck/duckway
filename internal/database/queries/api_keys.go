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

func (q *APIKeyQueries) List(serviceID string) ([]models.APIKey, error) {
	query := `SELECT k.id, k.service_id, k.name, k.key_encrypted, k.is_active, k.usage_count, k.last_used_at, k.created_at, s.name
		FROM api_keys k JOIN services s ON k.service_id = s.id`
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
		if err := rows.Scan(&k.ID, &k.ServiceID, &k.Name, &k.KeyEncrypted, &k.IsActive, &k.UsageCount, &k.LastUsedAt, &k.CreatedAt, &k.ServiceName); err != nil {
			return nil, err
		}
		result = append(result, k)
	}
	return result, rows.Err()
}

func (q *APIKeyQueries) GetByID(id string) (*models.APIKey, error) {
	var k models.APIKey
	err := q.db.QueryRow(
		`SELECT k.id, k.service_id, k.name, k.key_encrypted, k.is_active, k.usage_count, k.last_used_at, k.created_at, s.name
		 FROM api_keys k JOIN services s ON k.service_id = s.id WHERE k.id = ?`, id,
	).Scan(&k.ID, &k.ServiceID, &k.Name, &k.KeyEncrypted, &k.IsActive, &k.UsageCount, &k.LastUsedAt, &k.CreatedAt, &k.ServiceName)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (q *APIKeyQueries) Create(k *models.APIKey) error {
	_, err := q.db.Exec(
		"INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES (?, ?, ?, ?)",
		k.ID, k.ServiceID, k.Name, k.KeyEncrypted,
	)
	return err
}

func (q *APIKeyQueries) Update(k *models.APIKey) error {
	_, err := q.db.Exec(
		"UPDATE api_keys SET name=?, key_encrypted=?, is_active=? WHERE id=?",
		k.Name, k.KeyEncrypted, k.IsActive, k.ID,
	)
	return err
}

func (q *APIKeyQueries) Delete(id string) error {
	_, err := q.db.Exec("DELETE FROM api_keys WHERE id = ?", id)
	return err
}

func (q *APIKeyQueries) IncrementUsage(id string) error {
	_, err := q.db.Exec(
		"UPDATE api_keys SET usage_count = usage_count + 1, last_used_at = datetime('now') WHERE id = ?", id,
	)
	return err
}
