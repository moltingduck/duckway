package queries

import (
	"database/sql"
	"fmt"

	"github.com/hackerduck/duckway/internal/models"
)

type KeySuiteQueries struct {
	db *sql.DB
}

func NewKeySuiteQueries(db *sql.DB) *KeySuiteQueries {
	return &KeySuiteQueries{db: db}
}

const suiteEntrySelect = `SELECT e.id, e.suite_id, e.service_id, e.api_key_id, e.group_id, e.env_name, e.created_at,
	s.name, k.name, g.name
	FROM key_suite_entries e
	JOIN services s ON e.service_id = s.id
	LEFT JOIN api_keys k ON e.api_key_id = k.id
	LEFT JOIN api_key_groups g ON e.group_id = g.id`

func scanEntry(row interface{ Scan(...interface{}) error }, e *models.KeySuiteEntry) error {
	return row.Scan(
		&e.ID, &e.SuiteID, &e.ServiceID, &e.APIKeyID, &e.GroupID, &e.EnvName, &e.CreatedAt,
		&e.ServiceName, &e.APIKeyName, &e.GroupName,
	)
}

func (q *KeySuiteQueries) List() ([]models.KeySuite, error) {
	rows, err := q.db.Query(`SELECT id, name, description, created_at FROM key_suites ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var suites []models.KeySuite
	for rows.Next() {
		var s models.KeySuite
		if err := rows.Scan(&s.ID, &s.Name, &s.Description, &s.CreatedAt); err != nil {
			return nil, err
		}
		suites = append(suites, s)
	}
	return suites, rows.Err()
}

func (q *KeySuiteQueries) GetByID(id string) (*models.KeySuite, error) {
	var s models.KeySuite
	err := q.db.QueryRow(`SELECT id, name, description, created_at FROM key_suites WHERE id = ?`, id).
		Scan(&s.ID, &s.Name, &s.Description, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	entries, err := q.ListEntries(id)
	if err != nil {
		return nil, err
	}
	s.Entries = entries
	return &s, nil
}

func (q *KeySuiteQueries) Create(s *models.KeySuite) error {
	_, err := q.db.Exec(
		`INSERT INTO key_suites (id, name, description) VALUES (?, ?, ?)`,
		s.ID, s.Name, s.Description,
	)
	return err
}

func (q *KeySuiteQueries) Update(s *models.KeySuite) error {
	_, err := q.db.Exec(
		`UPDATE key_suites SET name=?, description=? WHERE id=?`,
		s.Name, s.Description, s.ID,
	)
	return err
}

func (q *KeySuiteQueries) Delete(id string) error {
	_, err := q.db.Exec(`DELETE FROM key_suites WHERE id = ?`, id)
	return err
}

func (q *KeySuiteQueries) ListEntries(suiteID string) ([]models.KeySuiteEntry, error) {
	rows, err := q.db.Query(suiteEntrySelect+` WHERE e.suite_id = ? ORDER BY s.name`, suiteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []models.KeySuiteEntry
	for rows.Next() {
		var e models.KeySuiteEntry
		if err := scanEntry(rows, &e); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (q *KeySuiteQueries) GetEntry(id string) (*models.KeySuiteEntry, error) {
	var e models.KeySuiteEntry
	err := scanEntry(q.db.QueryRow(suiteEntrySelect+` WHERE e.id = ?`, id), &e)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (q *KeySuiteQueries) AddEntry(e *models.KeySuiteEntry) error {
	_, err := q.db.Exec(
		`INSERT INTO key_suite_entries (id, suite_id, service_id, api_key_id, group_id, env_name) VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, e.SuiteID, e.ServiceID, e.APIKeyID, e.GroupID, e.EnvName,
	)
	return err
}

func (q *KeySuiteQueries) UpdateEntry(e *models.KeySuiteEntry) error {
	_, err := q.db.Exec(
		`UPDATE key_suite_entries SET api_key_id=?, group_id=?, env_name=? WHERE id=?`,
		e.APIKeyID, e.GroupID, e.EnvName, e.ID,
	)
	return err
}

func (q *KeySuiteQueries) RemoveEntry(id string) error {
	_, err := q.db.Exec(`DELETE FROM key_suite_entries WHERE id = ?`, id)
	return err
}

func (q *KeySuiteQueries) DeleteSuiteServicePlaceholders(suiteID, serviceID string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		UPDATE request_log
		SET placeholder_id = NULL
		WHERE placeholder_id IN (
			SELECT id FROM placeholder_keys WHERE suite_id = ? AND service_id = ?
		)`, suiteID, serviceID); err != nil {
		return fmt.Errorf("detach request logs: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM placeholder_keys WHERE suite_id = ? AND service_id = ?`, suiteID, serviceID); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckConflicts returns service IDs in the suite that the given client already
// has an individual (non-suite) placeholder for. These are the conflict cases.
func (q *KeySuiteQueries) CheckConflicts(suiteID, clientID string) ([]string, error) {
	rows, err := q.db.Query(`
		SELECT e.service_id
		FROM key_suite_entries e
		JOIN placeholder_keys p ON p.service_id = e.service_id
		WHERE e.suite_id = ?
		  AND p.client_id = ?
		  AND p.is_active = 1
		  AND (p.suite_id IS NULL OR p.suite_id != ?)
	`, suiteID, clientID, suiteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PropagateEntryUpdate updates all placeholder_keys that came from the given
// suite entry (suite_id + service_id) to use the new api_key_id / group_id.
// Returns the IDs of updated placeholders.
func (q *KeySuiteQueries) PropagateEntryUpdate(suiteID, serviceID string, apiKeyID, groupID *string) ([]string, error) {
	rows, err := q.db.Query(
		`SELECT id FROM placeholder_keys WHERE suite_id = ? AND service_id = ? AND is_active = 1`,
		suiteID, serviceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	_, err = q.db.Exec(
		`UPDATE placeholder_keys SET api_key_id=?, group_id=? WHERE suite_id=? AND service_id=? AND is_active=1`,
		apiKeyID, groupID, suiteID, serviceID,
	)
	return ids, err
}

// CountBoundClients returns how many distinct clients have active placeholders
// originating from the given suite.
func (q *KeySuiteQueries) CountBoundClients(suiteID string) (int, error) {
	var n int
	err := q.db.QueryRow(
		`SELECT COUNT(DISTINCT client_id) FROM placeholder_keys WHERE suite_id = ? AND is_active = 1`,
		suiteID,
	).Scan(&n)
	return n, err
}

func (q *KeySuiteQueries) ListBoundClients(suiteID string) ([]models.KeySuiteClient, error) {
	rows, err := q.db.Query(`
		SELECT c.id, c.name, COUNT(DISTINCT p.service_id) AS service_count
		FROM placeholder_keys p
		JOIN clients c ON c.id = p.client_id
		WHERE p.suite_id = ? AND p.is_active = 1
		GROUP BY c.id, c.name
		ORDER BY c.name`, suiteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var clients []models.KeySuiteClient
	for rows.Next() {
		var c models.KeySuiteClient
		if err := rows.Scan(&c.ID, &c.Name, &c.ServiceCount); err != nil {
			return nil, err
		}
		clients = append(clients, c)
	}
	return clients, rows.Err()
}
