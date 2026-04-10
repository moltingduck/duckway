package queries

import (
	"database/sql"

	"github.com/hackerduck/duckway/internal/models"
)

type ServiceQueries struct {
	db *sql.DB
}

func NewServiceQueries(db *sql.DB) *ServiceQueries {
	return &ServiceQueries{db: db}
}

func (q *ServiceQueries) List() ([]models.Service, error) {
	rows, err := q.db.Query(
		"SELECT id, name, display_name, upstream_url, host_pattern, auth_type, auth_header, auth_prefix, key_prefix, key_length, is_active, created_at FROM services ORDER BY name",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.Service
	for rows.Next() {
		var s models.Service
		if err := rows.Scan(&s.ID, &s.Name, &s.DisplayName, &s.UpstreamURL, &s.HostPattern, &s.AuthType, &s.AuthHeader, &s.AuthPrefix, &s.KeyPrefix, &s.KeyLength, &s.IsActive, &s.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func (q *ServiceQueries) GetByID(id string) (*models.Service, error) {
	var s models.Service
	err := q.db.QueryRow(
		"SELECT id, name, display_name, upstream_url, host_pattern, auth_type, auth_header, auth_prefix, key_prefix, key_length, is_active, created_at FROM services WHERE id = ?",
		id,
	).Scan(&s.ID, &s.Name, &s.DisplayName, &s.UpstreamURL, &s.HostPattern, &s.AuthType, &s.AuthHeader, &s.AuthPrefix, &s.KeyPrefix, &s.KeyLength, &s.IsActive, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (q *ServiceQueries) GetByName(name string) (*models.Service, error) {
	var s models.Service
	err := q.db.QueryRow(
		"SELECT id, name, display_name, upstream_url, host_pattern, auth_type, auth_header, auth_prefix, key_prefix, key_length, is_active, created_at FROM services WHERE name = ? AND is_active = 1",
		name,
	).Scan(&s.ID, &s.Name, &s.DisplayName, &s.UpstreamURL, &s.HostPattern, &s.AuthType, &s.AuthHeader, &s.AuthPrefix, &s.KeyPrefix, &s.KeyLength, &s.IsActive, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (q *ServiceQueries) Create(s *models.Service) error {
	_, err := q.db.Exec(
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern, auth_type, auth_header, auth_prefix, key_prefix, key_length)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Name, s.DisplayName, s.UpstreamURL, s.HostPattern, s.AuthType, s.AuthHeader, s.AuthPrefix, s.KeyPrefix, s.KeyLength,
	)
	return err
}

func (q *ServiceQueries) Update(s *models.Service) error {
	_, err := q.db.Exec(
		`UPDATE services SET name=?, display_name=?, upstream_url=?, host_pattern=?, auth_type=?, auth_header=?, auth_prefix=?, key_prefix=?, key_length=?, is_active=? WHERE id=?`,
		s.Name, s.DisplayName, s.UpstreamURL, s.HostPattern, s.AuthType, s.AuthHeader, s.AuthPrefix, s.KeyPrefix, s.KeyLength, s.IsActive, s.ID,
	)
	return err
}

func (q *ServiceQueries) Delete(id string) error {
	_, err := q.db.Exec("DELETE FROM services WHERE id = ?", id)
	return err
}
