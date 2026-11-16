package queries

import (
	"database/sql"

	"github.com/hackerduck/duckway/internal/models"
)

type GroupQueries struct {
	db *sql.DB
}

func NewGroupQueries(db *sql.DB) *GroupQueries {
	return &GroupQueries{db: db}
}

func (q *GroupQueries) List(serviceID string) ([]models.APIKeyGroup, error) {
	query := `SELECT g.id, g.service_id, g.name, g.strategy, g.last_index, g.created_at, s.name
		FROM api_key_groups g JOIN services s ON g.service_id = s.id`
	var args []interface{}
	if serviceID != "" {
		query += " WHERE g.service_id = ?"
		args = append(args, serviceID)
	}
	query += " ORDER BY g.created_at DESC"

	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.APIKeyGroup
	for rows.Next() {
		var g models.APIKeyGroup
		if err := rows.Scan(&g.ID, &g.ServiceID, &g.Name, &g.Strategy, &g.LastIndex, &g.CreatedAt, &g.ServiceName); err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	return result, rows.Err()
}

func (q *GroupQueries) GetByID(id string) (*models.APIKeyGroup, error) {
	var g models.APIKeyGroup
	err := q.db.QueryRow(
		`SELECT g.id, g.service_id, g.name, g.strategy, g.last_index, g.created_at, s.name
		 FROM api_key_groups g JOIN services s ON g.service_id = s.id WHERE g.id = ?`, id,
	).Scan(&g.ID, &g.ServiceID, &g.Name, &g.Strategy, &g.LastIndex, &g.CreatedAt, &g.ServiceName)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (q *GroupQueries) Create(g *models.APIKeyGroup) error {
	_, err := q.db.Exec(
		"INSERT INTO api_key_groups (id, service_id, name, strategy) VALUES (?, ?, ?, ?)",
		g.ID, g.ServiceID, g.Name, g.Strategy,
	)
	return err
}

func (q *GroupQueries) Delete(id string) error {
	_, err := q.db.Exec("DELETE FROM api_key_groups WHERE id = ?", id)
	return err
}

func (q *GroupQueries) GetMembers(groupID string) ([]models.APIKey, error) {
	// Uses the shared apiKeyCols/scanAPIKey so every field the proxy relies on
	// (acl, upstream_proxy_url, ...) is loaded; a narrower list silently dropped
	// the member key's own ACL layer.
	rows, err := q.db.Query(
		`SELECT `+apiKeyCols+`
		 FROM api_keys k
		 JOIN api_key_group_members m ON k.id = m.api_key_id
		 JOIN services s ON k.service_id = s.id
		 WHERE m.group_id = ? AND k.is_active = 1
		 ORDER BY m.priority ASC`, groupID,
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

func (q *GroupQueries) AddMember(groupID, apiKeyID string, priority int) error {
	_, err := q.db.Exec(
		`INSERT INTO api_key_group_members (group_id, api_key_id, priority) VALUES (?, ?, ?)
		 ON CONFLICT(group_id, api_key_id) DO UPDATE SET priority=excluded.priority`,
		groupID, apiKeyID, priority,
	)
	return err
}

func (q *GroupQueries) RemoveMember(groupID, apiKeyID string) error {
	_, err := q.db.Exec(
		"DELETE FROM api_key_group_members WHERE group_id = ? AND api_key_id = ?",
		groupID, apiKeyID,
	)
	return err
}

func (q *GroupQueries) Update(g *models.APIKeyGroup) error {
	_, err := q.db.Exec("UPDATE api_key_groups SET name = ?, strategy = ? WHERE id = ?", g.Name, g.Strategy, g.ID)
	return err
}

func (q *GroupQueries) UpdateLastIndex(groupID string, index int) error {
	_, err := q.db.Exec("UPDATE api_key_groups SET last_index = ? WHERE id = ?", index, groupID)
	return err
}

// IncrementLastIndex atomically increments last_index and returns the old
// value, giving the caller a collision-free slot in the round-robin sequence.
func (q *GroupQueries) IncrementLastIndex(groupID string, memberCount int) (int, error) {
	var old int
	err := q.db.QueryRow(`
		UPDATE api_key_groups
		SET last_index = last_index + 1
		WHERE id = ?
		RETURNING last_index - 1`, groupID).Scan(&old)
	if err != nil {
		return 0, err
	}
	return old % memberCount, nil
}

// KeyServiceID returns the service id of an API key (sql.ErrNoRows if absent).
func (q *GroupQueries) KeyServiceID(apiKeyID string) (string, error) {
	var id string
	err := q.db.QueryRow(`SELECT service_id FROM api_keys WHERE id = ?`, apiKeyID).Scan(&id)
	return id, err
}
