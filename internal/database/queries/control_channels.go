package queries

import (
	"database/sql"
	"fmt"

	"github.com/hackerduck/duckway/internal/models"
)

// ControlChannelQueries owns the CC + cc_channels + discord_inbox tables.
// Schema v2 (post-redesign): control_channels.client_id is unique, no
// separate assignment table.
type ControlChannelQueries struct {
	db *sql.DB
}

func NewControlChannelQueries(db *sql.DB) *ControlChannelQueries {
	return &ControlChannelQueries{db: db}
}

const ccCols = `cc.id, cc.name, cc.service_id, cc.api_key_id, cc.client_id,
                cc.agent_type, cc.placeholder_id, cc.config, cc.is_active, cc.created_at,
                s.name, k.name, c.name`

func scanCC(row interface{ Scan(...interface{}) error }, cc *models.ControlChannel) error {
	return row.Scan(&cc.ID, &cc.Name, &cc.ServiceID, &cc.APIKeyID, &cc.ClientID,
		&cc.AgentType, &cc.PlaceholderID, &cc.Config, &cc.IsActive, &cc.CreatedAt,
		&cc.ServiceName, &cc.APIKeyName, &cc.ClientName)
}

const ccSelect = `SELECT ` + ccCols + ` FROM control_channels cc
                  JOIN services s ON cc.service_id = s.id
                  JOIN api_keys k ON cc.api_key_id = k.id
                  JOIN clients  c ON cc.client_id  = c.id`

func (q *ControlChannelQueries) List() ([]models.ControlChannel, error) {
	rows, err := q.db.Query(ccSelect + ` ORDER BY cc.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.ControlChannel
	for rows.Next() {
		var c models.ControlChannel
		if err := scanCC(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (q *ControlChannelQueries) GetByID(id string) (*models.ControlChannel, error) {
	var c models.ControlChannel
	err := scanCC(q.db.QueryRow(ccSelect+` WHERE cc.id = ?`, id), &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetByClientID looks up the (only) CC bound to a client. The client API
// uses this to resolve "the current CC" without making the agent pass an id.
func (q *ControlChannelQueries) GetByClientID(clientID string) (*models.ControlChannel, error) {
	var c models.ControlChannel
	err := scanCC(q.db.QueryRow(ccSelect+` WHERE cc.client_id = ?`, clientID), &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (q *ControlChannelQueries) Create(c *models.ControlChannel) error {
	_, err := q.db.Exec(
		`INSERT INTO control_channels
		 (id, name, service_id, api_key_id, client_id, agent_type, placeholder_id, config, is_active)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, c.ServiceID, c.APIKeyID, c.ClientID, c.AgentType, c.PlaceholderID,
		c.Config, boolToInt(c.IsActive),
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

const channelCols = `handle, cc_id, client_id, channel_id, name, topic, kind, session_id, cwd, archived, created_at, last_seen_at`

func scanChannel(row interface{ Scan(...interface{}) error }, c *models.CCChannel) error {
	return row.Scan(&c.Handle, &c.CCID, &c.ClientID, &c.ChannelID, &c.Name, &c.Topic,
		&c.Kind, &c.SessionID, &c.Cwd, &c.Archived, &c.CreatedAt, &c.LastSeenAt)
}

func (q *ControlChannelQueries) ListChannels(ccID string) ([]models.CCChannel, error) {
	rows, err := q.db.Query(
		`SELECT `+channelCols+` FROM cc_channels WHERE cc_id = ? ORDER BY kind = 'management' DESC, created_at DESC`, ccID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.CCChannel
	for rows.Next() {
		var c models.CCChannel
		if err := scanChannel(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (q *ControlChannelQueries) GetChannelByHandle(handle string) (*models.CCChannel, error) {
	var c models.CCChannel
	err := scanChannel(
		q.db.QueryRow(`SELECT `+channelCols+` FROM cc_channels WHERE handle = ?`, handle),
		&c,
	)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetChannelByRealID is the gateway's reverse lookup — given a channel_id
// from a Discord event, find which CC + handle it belongs to. Scoped to
// a CC so a bot serving multiple CCs doesn't cross-write events.
func (q *ControlChannelQueries) GetChannelByRealID(ccID, realID string) (*models.CCChannel, error) {
	var c models.CCChannel
	err := scanChannel(
		q.db.QueryRow(`SELECT `+channelCols+` FROM cc_channels WHERE cc_id = ? AND channel_id = ?`, ccID, realID),
		&c,
	)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetManagementChannel returns the kind='management' row for a CC.
func (q *ControlChannelQueries) GetManagementChannel(ccID string) (*models.CCChannel, error) {
	var c models.CCChannel
	err := scanChannel(
		q.db.QueryRow(
			`SELECT `+channelCols+` FROM cc_channels WHERE cc_id = ? AND kind = 'management' LIMIT 1`, ccID),
		&c,
	)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (q *ControlChannelQueries) CreateChannel(c *models.CCChannel) error {
	if c.Kind == "" {
		c.Kind = "task"
	}
	_, err := q.db.Exec(
		`INSERT INTO cc_channels (handle, cc_id, client_id, channel_id, name, topic, kind, session_id, cwd, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Handle, c.CCID, c.ClientID, c.ChannelID, c.Name, c.Topic, c.Kind, c.SessionID, c.Cwd, boolToInt(c.Archived),
	)
	return err
}

func (q *ControlChannelQueries) MarkChannelArchived(handle string) error {
	_, err := q.db.Exec("UPDATE cc_channels SET archived = 1 WHERE handle = ?", handle)
	return err
}

func (q *ControlChannelQueries) DeleteChannel(handle string) error {
	_, err := q.db.Exec("DELETE FROM cc_channels WHERE handle = ?", handle)
	return err
}

func (q *ControlChannelQueries) DeleteChannelByRealID(ccID, realID string) error {
	_, err := q.db.Exec("DELETE FROM cc_channels WHERE cc_id = ? AND channel_id = ?", ccID, realID)
	return err
}

func (q *ControlChannelQueries) UpdateChannelMeta(handle, name, topic string) error {
	_, err := q.db.Exec(
		"UPDATE cc_channels SET name = ?, topic = ?, last_seen_at = datetime('now') WHERE handle = ?",
		name, topic, handle,
	)
	return err
}

// SetChannelSession records the claude session_id and (optionally) the cwd
// the daemon used. Called the first time a channel sees `claude -p`.
func (q *ControlChannelQueries) SetChannelSession(handle, sessionID, cwd string) error {
	_, err := q.db.Exec(
		"UPDATE cc_channels SET session_id = ?, cwd = ?, last_seen_at = datetime('now') WHERE handle = ?",
		sessionID, cwd, handle,
	)
	return err
}

// --- inbox ---------------------------------------------------------------

func (q *ControlChannelQueries) AppendInbox(ccID string, channelHandle *string, eventType, payload string) error {
	_, err := q.db.Exec(
		`INSERT INTO discord_inbox (cc_id, channel_handle, event_type, payload) VALUES (?, ?, ?, ?)`,
		ccID, channelHandle, eventType, payload,
	)
	return err
}

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
