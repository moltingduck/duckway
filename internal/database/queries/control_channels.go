package queries

import (
	"database/sql"
	"fmt"

	"github.com/hackerduck/duckway/internal/models"
)

type ControlChannelQueries struct {
	db *sql.DB
}

func NewControlChannelQueries(db *sql.DB) *ControlChannelQueries {
	return &ControlChannelQueries{db: db}
}

// List returns all CCs joined with the underlying service + bot key name.
func (q *ControlChannelQueries) List() ([]models.ControlChannel, error) {
	rows, err := q.db.Query(
		`SELECT cc.id, cc.name, cc.service_id, cc.api_key_id, cc.config, cc.is_active, cc.created_at,
		        s.name, k.name
		 FROM control_channels cc
		 JOIN services s ON cc.service_id = s.id
		 JOIN api_keys k ON cc.api_key_id = k.id
		 ORDER BY cc.created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ControlChannel
	for rows.Next() {
		var c models.ControlChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.ServiceID, &c.APIKeyID, &c.Config, &c.IsActive, &c.CreatedAt,
			&c.ServiceName, &c.APIKeyName); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (q *ControlChannelQueries) GetByID(id string) (*models.ControlChannel, error) {
	var c models.ControlChannel
	err := q.db.QueryRow(
		`SELECT cc.id, cc.name, cc.service_id, cc.api_key_id, cc.config, cc.is_active, cc.created_at,
		        s.name, k.name
		 FROM control_channels cc
		 JOIN services s ON cc.service_id = s.id
		 JOIN api_keys k ON cc.api_key_id = k.id
		 WHERE cc.id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.ServiceID, &c.APIKeyID, &c.Config, &c.IsActive, &c.CreatedAt,
		&c.ServiceName, &c.APIKeyName)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (q *ControlChannelQueries) Create(c *models.ControlChannel) error {
	_, err := q.db.Exec(
		`INSERT INTO control_channels (id, name, service_id, api_key_id, config, is_active)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, c.ServiceID, c.APIKeyID, c.Config, boolToInt(c.IsActive),
	)
	return err
}

func (q *ControlChannelQueries) Update(id, name, config string, isActive bool) error {
	_, err := q.db.Exec(
		"UPDATE control_channels SET name = ?, config = ?, is_active = ? WHERE id = ?",
		name, config, boolToInt(isActive), id,
	)
	return err
}

func (q *ControlChannelQueries) Delete(id string) error {
	_, err := q.db.Exec("DELETE FROM control_channels WHERE id = ?", id)
	return err
}

// --- channels under a CC -------------------------------------------------

func (q *ControlChannelQueries) ListChannels(ccID string) ([]models.CCChannel, error) {
	rows, err := q.db.Query(
		`SELECT handle, cc_id, client_id, channel_id, name, topic, is_home, archived, created_at, last_seen_at
		 FROM cc_channels WHERE cc_id = ? ORDER BY created_at DESC`, ccID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.CCChannel
	for rows.Next() {
		var c models.CCChannel
		if err := rows.Scan(&c.Handle, &c.CCID, &c.ClientID, &c.ChannelID, &c.Name, &c.Topic,
			&c.IsHome, &c.Archived, &c.CreatedAt, &c.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (q *ControlChannelQueries) GetChannelByHandle(handle string) (*models.CCChannel, error) {
	var c models.CCChannel
	err := q.db.QueryRow(
		`SELECT handle, cc_id, client_id, channel_id, name, topic, is_home, archived, created_at, last_seen_at
		 FROM cc_channels WHERE handle = ?`, handle,
	).Scan(&c.Handle, &c.CCID, &c.ClientID, &c.ChannelID, &c.Name, &c.Topic,
		&c.IsHome, &c.Archived, &c.CreatedAt, &c.LastSeenAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetChannelByRealID looks up a channel by its real Discord channel_id (used
// when the gateway delivers an event and we need to map back to a handle).
func (q *ControlChannelQueries) GetChannelByRealID(ccID, realID string) (*models.CCChannel, error) {
	var c models.CCChannel
	err := q.db.QueryRow(
		`SELECT handle, cc_id, client_id, channel_id, name, topic, is_home, archived, created_at, last_seen_at
		 FROM cc_channels WHERE cc_id = ? AND channel_id = ?`, ccID, realID,
	).Scan(&c.Handle, &c.CCID, &c.ClientID, &c.ChannelID, &c.Name, &c.Topic,
		&c.IsHome, &c.Archived, &c.CreatedAt, &c.LastSeenAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (q *ControlChannelQueries) CreateChannel(c *models.CCChannel) error {
	_, err := q.db.Exec(
		`INSERT INTO cc_channels (handle, cc_id, client_id, channel_id, name, topic, is_home, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Handle, c.CCID, c.ClientID, c.ChannelID, c.Name, c.Topic, boolToInt(c.IsHome), boolToInt(c.Archived),
	)
	return err
}

func (q *ControlChannelQueries) MarkChannelArchived(handle string) error {
	_, err := q.db.Exec("UPDATE cc_channels SET archived = 1 WHERE handle = ?", handle)
	return err
}

func (q *ControlChannelQueries) UpdateChannelMeta(handle, name, topic string) error {
	_, err := q.db.Exec("UPDATE cc_channels SET name = ?, topic = ?, last_seen_at = datetime('now') WHERE handle = ?",
		name, topic, handle)
	return err
}

// --- assignments ---------------------------------------------------------

func (q *ControlChannelQueries) Assign(a *models.ClientCC) error {
	_, err := q.db.Exec(
		`INSERT INTO client_cc (client_id, cc_id, agent_type, home_handle, placeholder_id)
		 VALUES (?, ?, ?, ?, ?)`,
		a.ClientID, a.CCID, a.AgentType, a.HomeHandle, a.PlaceholderID,
	)
	return err
}

func (q *ControlChannelQueries) Unassign(clientID, ccID string) error {
	_, err := q.db.Exec("DELETE FROM client_cc WHERE client_id = ? AND cc_id = ?", clientID, ccID)
	return err
}

func (q *ControlChannelQueries) GetAssignment(clientID, ccID string) (*models.ClientCC, error) {
	var a models.ClientCC
	err := q.db.QueryRow(
		`SELECT client_id, cc_id, agent_type, home_handle, placeholder_id, created_at
		 FROM client_cc WHERE client_id = ? AND cc_id = ?`, clientID, ccID,
	).Scan(&a.ClientID, &a.CCID, &a.AgentType, &a.HomeHandle, &a.PlaceholderID, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// AssignmentsForClient lists CCs the client is currently assigned to.
func (q *ControlChannelQueries) AssignmentsForClient(clientID string) ([]models.ClientCCDetail, error) {
	rows, err := q.db.Query(
		`SELECT a.client_id, a.cc_id, a.agent_type, a.home_handle, a.placeholder_id, a.created_at,
		        cc.name, ch.name, ch.channel_id
		 FROM client_cc a
		 JOIN control_channels cc ON a.cc_id = cc.id
		 JOIN cc_channels ch ON a.home_handle = ch.handle
		 WHERE a.client_id = ?
		 ORDER BY a.created_at DESC`, clientID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ClientCCDetail
	for rows.Next() {
		var d models.ClientCCDetail
		if err := rows.Scan(&d.ClientID, &d.CCID, &d.AgentType, &d.HomeHandle, &d.PlaceholderID, &d.CreatedAt,
			&d.CCName, &d.HomeChannelName, &d.HomeChannelID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (q *ControlChannelQueries) AssignmentsForCC(ccID string) ([]models.ClientCCDetail, error) {
	rows, err := q.db.Query(
		`SELECT a.client_id, a.cc_id, a.agent_type, a.home_handle, a.placeholder_id, a.created_at,
		        cc.name, ch.name, ch.channel_id, c.name
		 FROM client_cc a
		 JOIN control_channels cc ON a.cc_id = cc.id
		 JOIN cc_channels ch ON a.home_handle = ch.handle
		 JOIN clients c ON a.client_id = c.id
		 WHERE a.cc_id = ?
		 ORDER BY a.created_at DESC`, ccID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ClientCCDetail
	for rows.Next() {
		var d models.ClientCCDetail
		if err := rows.Scan(&d.ClientID, &d.CCID, &d.AgentType, &d.HomeHandle, &d.PlaceholderID, &d.CreatedAt,
			&d.CCName, &d.HomeChannelName, &d.HomeChannelID, &d.ClientName); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- inbox ---------------------------------------------------------------

func (q *ControlChannelQueries) AppendInbox(ccID string, channelHandle *string, eventType, payload string) error {
	_, err := q.db.Exec(
		`INSERT INTO discord_inbox (cc_id, channel_handle, event_type, payload) VALUES (?, ?, ?, ?)`,
		ccID, channelHandle, eventType, payload,
	)
	return err
}

// PullInbox returns events with id > sinceID for the given CC, optionally
// filtered to a set of channel handles. Cap by limit.
func (q *ControlChannelQueries) PullInbox(ccID string, sinceID int64, channelHandles []string, limit int) ([]models.InboxEvent, error) {
	args := []interface{}{ccID, sinceID}
	q1 := `SELECT id, cc_id, channel_handle, event_type, payload, created_at
	       FROM discord_inbox WHERE cc_id = ? AND id > ?`
	if len(channelHandles) > 0 {
		q1 += " AND channel_handle IN ("
		for i, h := range channelHandles {
			if i > 0 {
				q1 += ","
			}
			q1 += "?"
			args = append(args, h)
		}
		q1 += ")"
	}
	q1 += " ORDER BY id ASC LIMIT ?"
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	args = append(args, limit)

	rows, err := q.db.Query(q1, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.InboxEvent
	for rows.Next() {
		var e models.InboxEvent
		if err := rows.Scan(&e.ID, &e.CCID, &e.ChannelHandle, &e.EventType, &e.Payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CleanupInbox enforces the retention window + per-channel cap.
func (q *ControlChannelQueries) CleanupInbox(retentionHours, perChannelMax int) error {
	if retentionHours > 0 {
		if _, err := q.db.Exec(
			"DELETE FROM discord_inbox WHERE created_at < datetime('now', ?)",
			fmt.Sprintf("-%d hours", retentionHours),
		); err != nil {
			return err
		}
	}
	if perChannelMax > 0 {
		// For each channel_handle, keep only the newest perChannelMax rows.
		_, err := q.db.Exec(
			`DELETE FROM discord_inbox WHERE id NOT IN (
				SELECT id FROM (
					SELECT id, ROW_NUMBER() OVER (PARTITION BY cc_id, channel_handle ORDER BY id DESC) AS rn
					FROM discord_inbox
				) WHERE rn <= ?
			)`, perChannelMax,
		)
		return err
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
