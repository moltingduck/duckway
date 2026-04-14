package queries

import (
	"database/sql"

	"github.com/hackerduck/duckway/internal/models"
)

type ClientQueries struct {
	db *sql.DB
}

func NewClientQueries(db *sql.DB) *ClientQueries {
	return &ClientQueries{db: db}
}

const clientCols = "id, name, token_hash, is_active, canary_enabled, last_seen_at, created_at"

func scanClient(row interface{ Scan(...interface{}) error }, c *models.Client) error {
	return row.Scan(&c.ID, &c.Name, &c.TokenHash, &c.IsActive, &c.CanaryEnabled, &c.LastSeenAt, &c.CreatedAt)
}

func (q *ClientQueries) List() ([]models.Client, error) {
	rows, err := q.db.Query("SELECT " + clientCols + " FROM clients ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.Client
	for rows.Next() {
		var c models.Client
		if err := scanClient(rows, &c); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (q *ClientQueries) GetByID(id string) (*models.Client, error) {
	var c models.Client
	err := scanClient(q.db.QueryRow("SELECT "+clientCols+" FROM clients WHERE id = ?", id), &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (q *ClientQueries) GetByTokenHash(hash string) (*models.Client, error) {
	var c models.Client
	err := scanClient(q.db.QueryRow("SELECT "+clientCols+" FROM clients WHERE token_hash = ? AND is_active = 1", hash), &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (q *ClientQueries) Create(c *models.Client) error {
	_, err := q.db.Exec(
		"INSERT INTO clients (id, name, token_hash, canary_enabled) VALUES (?, ?, ?, ?)",
		c.ID, c.Name, c.TokenHash, c.CanaryEnabled,
	)
	return err
}

func (q *ClientQueries) UpdateLastSeen(id string) error {
	_, err := q.db.Exec("UPDATE clients SET last_seen_at = datetime('now') WHERE id = ?", id)
	return err
}

func (q *ClientQueries) UpdateCanaryEnabled(id string, enabled bool) error {
	_, err := q.db.Exec("UPDATE clients SET canary_enabled = ? WHERE id = ?", enabled, id)
	return err
}

func (q *ClientQueries) Delete(id string) error {
	_, err := q.db.Exec("DELETE FROM clients WHERE id = ?", id)
	return err
}
