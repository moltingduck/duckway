package queries

import (
	"database/sql"
	"fmt"

	"github.com/hackerduck/duckway/internal/models"
)

type ClientQueries struct {
	db *sql.DB
}

func NewClientQueries(db *sql.DB) *ClientQueries {
	return &ClientQueries{db: db}
}

const clientCols = "id, short_id, name, token_hash, is_active, canary_enabled, last_seen_at, created_at"

func scanClient(row interface{ Scan(...interface{}) error }, c *models.Client) error {
	return row.Scan(&c.ID, &c.ShortID, &c.Name, &c.TokenHash, &c.IsActive, &c.CanaryEnabled, &c.LastSeenAt, &c.CreatedAt)
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
		"INSERT INTO clients (id, short_id, name, token_hash, canary_enabled) VALUES (?, ?, ?, ?, ?)",
		c.ID, c.ShortID, c.Name, c.TokenHash, c.CanaryEnabled,
	)
	return err
}

func (q *ClientQueries) GetByName(name string) (*models.Client, error) {
	var c models.Client
	err := scanClient(q.db.QueryRow("SELECT "+clientCols+" FROM clients WHERE name = ?", name), &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (q *ClientQueries) UpdateLastSeen(id string) error {
	_, err := q.db.Exec("UPDATE clients SET last_seen_at = datetime('now') WHERE id = ?", id)
	return err
}

func (q *ClientQueries) Update(c *models.Client) error {
	_, err := q.db.Exec("UPDATE clients SET name = ?, is_active = ? WHERE id = ?", c.Name, c.IsActive, c.ID)
	return err
}

func (q *ClientQueries) UpdateCanaryEnabled(id string, enabled bool) error {
	_, err := q.db.Exec("UPDATE clients SET canary_enabled = ? WHERE id = ?", enabled, id)
	return err
}

func (q *ClientQueries) Delete(id string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// request_log keeps historical rows after a client is removed. Its
	// client_id and placeholder_id FKs are nullable but do not cascade, so
	// detach them before deleting the client and its placeholders.
	if _, err := tx.Exec("UPDATE request_log SET client_id = NULL WHERE client_id = ?", id); err != nil {
		return fmt.Errorf("detach client request logs: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE request_log
		SET placeholder_id = NULL
		WHERE placeholder_id IN (
			SELECT id FROM placeholder_keys WHERE client_id = ?
		)`, id); err != nil {
		return fmt.Errorf("detach placeholder request logs: %w", err)
	}

	// control_channels.client_id was added by migration and has no FK on
	// existing databases. Remove its dependent rows explicitly to avoid
	// orphaned channels/inbox events when deleting the bound client.
	if _, err := tx.Exec(`
		DELETE FROM discord_inbox
		WHERE cc_id IN (
			SELECT id FROM control_channels WHERE client_id = ?
		)`, id); err != nil {
		return fmt.Errorf("delete control channel inbox: %w", err)
	}
	if _, err := tx.Exec(`
		DELETE FROM cc_channels
		WHERE cc_id IN (
			SELECT id FROM control_channels WHERE client_id = ?
		)`, id); err != nil {
		return fmt.Errorf("delete control channel channels: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM control_channels WHERE client_id = ?", id); err != nil {
		return fmt.Errorf("delete control channels: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM canary_tokens WHERE client_id = ?", id); err != nil {
		return fmt.Errorf("delete canary tokens: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM key_suite_assignments WHERE client_id = ?", id); err != nil {
		return fmt.Errorf("delete key suite assignments: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM placeholder_keys WHERE client_id = ?", id); err != nil {
		return fmt.Errorf("delete placeholder keys: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM clients WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete client: %w", err)
	}
	return tx.Commit()
}
