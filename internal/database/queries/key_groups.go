package queries

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/models"
)

func CreateKeyGroup(db *sql.DB, name, description, serviceName string) (*models.KeyGroup, error) {
	id := uuid.New().String()
	_, err := db.Exec(
		`INSERT INTO key_groups (id, name, description, service_name) VALUES (?, ?, ?, ?)`,
		id, name, description, serviceName,
	)
	if err != nil {
		return nil, err
	}
	return &models.KeyGroup{
		ID:          id,
		Name:        name,
		Description: description,
		ServiceName: serviceName,
	}, nil
}

func ListKeyGroups(db *sql.DB) ([]models.KeyGroup, error) {
	rows, err := db.Query(`SELECT id, name, description, service_name, created_at FROM key_groups ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.KeyGroup
	for rows.Next() {
		var g models.KeyGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.ServiceName, &g.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, g)
	}
	return result, rows.Err()
}

func GetKeyGroup(db *sql.DB, id string) (*models.KeyGroup, error) {
	var g models.KeyGroup
	err := db.QueryRow(
		`SELECT id, name, description, service_name, created_at FROM key_groups WHERE id = ?`, id,
	).Scan(&g.ID, &g.Name, &g.Description, &g.ServiceName, &g.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func DeleteKeyGroup(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM key_groups WHERE id = ?`, id)
	return err
}

func AddKeyToGroup(db *sql.DB, groupID, apiKeyID string, position int) error {
	_, err := db.Exec(
		`INSERT OR REPLACE INTO key_group_members (group_id, api_key_id, position) VALUES (?, ?, ?)`,
		groupID, apiKeyID, position,
	)
	return err
}

func RemoveKeyFromGroup(db *sql.DB, groupID, apiKeyID string) error {
	_, err := db.Exec(
		`DELETE FROM key_group_members WHERE group_id = ? AND api_key_id = ?`,
		groupID, apiKeyID,
	)
	return err
}

func GetKeyGroupWithMembers(db *sql.DB, groupID string) (*models.KeyGroupWithMembers, error) {
	g, err := GetKeyGroup(db, groupID)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
		SELECT m.api_key_id, k.name, m.position, m.exhausted_until,
		       json_extract(k.usage_snapshot, '$.tokens_remaining'),
		       json_extract(k.usage_snapshot, '$.tokens_limit'),
		       json_extract(k.usage_snapshot, '$.reset_at'),
		       (SELECT COUNT(*) FROM placeholder_keys p WHERE p.key_group_id = m.group_id AND p.api_key_id = m.api_key_id),
		       COALESCE(CAST(json_extract(k.usage_snapshot, '$.tokens_remaining') AS REAL), 9999999999.0)
		         / MAX(CAST((SELECT COUNT(*) FROM placeholder_keys p WHERE p.key_group_id = m.group_id AND p.api_key_id = m.api_key_id) AS REAL), 1.0) AS score
		FROM key_group_members m
		JOIN api_keys k ON k.id = m.api_key_id
		WHERE m.group_id = ?
		ORDER BY m.position ASC`, groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := &models.KeyGroupWithMembers{KeyGroup: *g}
	for rows.Next() {
		var d models.KeyGroupMemberDetail
		var tokensRemaining, tokensLimit *float64
		if err := rows.Scan(
			&d.APIKeyID, &d.KeyName, &d.Position, &d.ExhaustedUntil,
			&tokensRemaining, &tokensLimit, &d.ResetAt,
			&d.BoundClients, &d.Score,
		); err != nil {
			return nil, err
		}
		d.GroupID = groupID
		if tokensRemaining != nil {
			v := int64(*tokensRemaining)
			d.TokensRemaining = &v
		}
		if tokensLimit != nil {
			v := int64(*tokensLimit)
			d.TokensLimit = &v
		}
		result.Members = append(result.Members, d)
	}
	return result, rows.Err()
}

// SelectKeyForGroup implements the score-based selection algorithm.
// Returns the api_key_id to use. excludeKeyID may be empty string.
func SelectKeyForGroup(db *sql.DB, groupID, excludeKeyID string) (string, error) {
	row := db.QueryRow(`
		SELECT m.api_key_id,
		       COALESCE(CAST(json_extract(k.usage_snapshot, '$.tokens_remaining') AS REAL), 9999999999.0)
		         / MAX(CAST(COUNT(p.id) AS REAL), 1.0) AS score,
		       CASE WHEN json_extract(k.usage_snapshot, '$.tokens_remaining') IS NULL THEN 0 ELSE 1 END AS has_data,
		       m.position
		FROM key_group_members m
		JOIN api_keys k ON k.id = m.api_key_id
		LEFT JOIN placeholder_keys p ON p.api_key_id = k.id AND p.key_group_id = m.group_id
		WHERE m.group_id = ?
		  AND (m.exhausted_until IS NULL OR m.exhausted_until < datetime('now'))
		  AND (? = '' OR m.api_key_id != ?)
		GROUP BY m.api_key_id, m.position, k.usage_snapshot
		ORDER BY has_data ASC, score DESC, m.position ASC
		LIMIT 1`,
		groupID, excludeKeyID, excludeKeyID,
	)

	var apiKeyID string
	var score float64
	var hasData int
	var position int
	if err := row.Scan(&apiKeyID, &score, &hasData, &position); err != nil {
		return "", err
	}
	return apiKeyID, nil
}

// MarkKeyExhausted sets exhausted_until on a group member.
func MarkKeyExhausted(db *sql.DB, groupID, apiKeyID, resetAt string) error {
	_, err := db.Exec(
		`UPDATE key_group_members SET exhausted_until = ? WHERE group_id = ? AND api_key_id = ?`,
		resetAt, groupID, apiKeyID,
	)
	return err
}

// ClearExpiredExhausted clears exhausted_until for keys past their reset time.
func ClearExpiredExhausted(db *sql.DB) error {
	_, err := db.Exec(`UPDATE key_group_members SET exhausted_until = NULL WHERE exhausted_until < datetime('now')`)
	return err
}

// UpdateAnthropicUsage parses Anthropic rate-limit headers and stores them in api_keys.usage_snapshot.
func UpdateAnthropicUsage(db *sql.DB, apiKeyID string, headers map[string]string) error {
	snapshot := map[string]interface{}{
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}

	parseIntHeader := func(key, snapKey string) {
		if v, ok := headers[key]; ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				snapshot[snapKey] = n
			} else {
				snapshot[snapKey] = v
			}
		}
	}
	parseIntHeader("x-ratelimit-limit-requests", "requests_limit")
	parseIntHeader("x-ratelimit-remaining-requests", "requests_remaining")
	parseIntHeader("x-ratelimit-limit-tokens", "tokens_limit")
	parseIntHeader("x-ratelimit-remaining-tokens", "tokens_remaining")
	if v, ok := headers["x-ratelimit-reset-tokens"]; ok {
		snapshot["reset_at"] = v
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE api_keys SET usage_snapshot = ? WHERE id = ?`, string(data), apiKeyID)
	return err
}
