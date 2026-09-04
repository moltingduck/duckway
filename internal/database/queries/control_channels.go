package queries

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/hackerduck/duckway/internal/models"
)

var ErrDeliveryConflict = errors.New("message delivery key conflicts with different content")

func (q *ControlChannelQueries) BeginMessageDelivery(ccID, handle, key string, digest []byte) (string, error) {
	var existing []byte
	var messageID string
	err := q.db.QueryRow(`SELECT content_digest,message_id FROM cc_message_deliveries WHERE cc_id=? AND delivery_key=?`, ccID, key).Scan(&existing, &messageID)
	if err == nil {
		if !bytes.Equal(existing, digest) {
			return "", ErrDeliveryConflict
		}
		return messageID, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	if _, err = q.db.Exec(`INSERT OR IGNORE INTO cc_message_deliveries(cc_id,channel_handle,delivery_key,content_digest) VALUES(?,?,?,?)`, ccID, handle, key, digest); err != nil {
		return "", err
	}
	var storedHandle string
	if err = q.db.QueryRow(`SELECT channel_handle,content_digest,message_id FROM cc_message_deliveries WHERE cc_id=? AND delivery_key=?`, ccID, key).Scan(&storedHandle, &existing, &messageID); err != nil {
		return "", err
	}
	if storedHandle != handle || !bytes.Equal(existing, digest) {
		return "", ErrDeliveryConflict
	}
	return messageID, nil
}

func (q *ControlChannelQueries) CompleteMessageDelivery(ccID, key, messageID string) error {
	result, err := q.db.Exec(`UPDATE cc_message_deliveries SET message_id=?,updated_at=datetime('now') WHERE cc_id=? AND delivery_key=? AND message_id=''`, messageID, ccID, key)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		var existing string
		if err := q.db.QueryRow(`SELECT message_id FROM cc_message_deliveries WHERE cc_id=? AND delivery_key=?`, ccID, key).Scan(&existing); err != nil || existing != messageID {
			return fmt.Errorf("message delivery completion conflict")
		}
	}
	return nil
}

func (q *ControlChannelQueries) MessageDeliveryRetrySafe(ccID, key string) (bool, error) {
	var safe int
	err := q.db.QueryRow(`SELECT CASE WHEN created_at >= datetime('now','-10 minutes') THEN 1 ELSE 0 END FROM cc_message_deliveries WHERE cc_id=? AND delivery_key=?`, ccID, key).Scan(&safe)
	return safe == 1, err
}

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
// ListByAPIKeyID returns all active CCs that use the given API key. Used by
// the gateway to route events without scanning the full control_channels table.
func (q *ControlChannelQueries) ListByAPIKeyID(apiKeyID string) ([]models.ControlChannel, error) {
	rows, err := q.db.Query(ccSelect+` WHERE cc.api_key_id = ? ORDER BY cc.created_at DESC`, apiKeyID)
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

func (q *ControlChannelQueries) Update(id, name, apiKeyID, agentType, config string, isActive bool) error {
	_, err := q.db.Exec(
		"UPDATE control_channels SET name = ?, api_key_id = ?, agent_type = ?, config = ?, is_active = ? WHERE id = ?",
		name, apiKeyID, agentType, config, boolToInt(isActive), id,
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

func (q *ControlChannelQueries) AppendInbox(ccID string, channelHandle *string, eventType, payload string) (int64, error) {
	lane := ""
	if channelHandle != nil {
		lane = *channelHandle
	}
	return q.AdmitInbox(ccID, channelHandle, eventType, "", lane, payload)
}

// AdmitInbox durably admits a Discord dispatch. eventKey is a namespaced
// Discord identity (for example MESSAGE_CREATE:<snowflake>); duplicate gateway
// replay returns the existing row rather than publishing a second job.
func (q *ControlChannelQueries) AdmitInbox(ccID string, channelHandle *string, eventType, eventKey, laneKey, payload string) (int64, error) {
	id, _, err := q.AdmitInboxDetailed(ccID, channelHandle, eventType, eventKey, laneKey, payload)
	return id, err
}

func (q *ControlChannelQueries) AdmitInboxDetailed(ccID string, channelHandle *string, eventType, eventKey, laneKey, payload string) (int64, bool, error) {
	var id int64
	err := q.db.QueryRow(
		`INSERT INTO discord_inbox (cc_id, channel_handle, event_type, event_key, lane_key, payload)
		 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING RETURNING id`,
		ccID, channelHandle, eventType, eventKey, laneKey, payload,
	).Scan(&id)
	inserted := err == nil
	if err == sql.ErrNoRows && eventKey != "" {
		err = q.db.QueryRow(`SELECT id FROM discord_inbox WHERE cc_id = ? AND event_key = ?`, ccID, eventKey).Scan(&id)
	}
	if err != nil {
		return 0, false, err
	}
	return id, inserted, nil
}

func newInboxClaimToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ClaimInbox atomically leases one lane-head event. A later row in the same
// lane cannot be claimed while an earlier row is admitted or actively leased.
func (q *ControlChannelQueries) ClaimInbox(ccID string, leaseSeconds int) (*models.InboxEvent, error) {
	if leaseSeconds < 10 || leaseSeconds > 3600 {
		leaseSeconds = 120
	}
	token, err := newInboxClaimToken()
	if err != nil {
		return nil, err
	}
	tx, err := q.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow(`SELECT i.id FROM discord_inbox i
		WHERE i.cc_id = ?
		  AND (i.status = 'admitted' OR (i.status = 'claimed' AND i.lease_expires_at <= datetime('now')))
		  AND NOT EXISTS (
		    SELECT 1 FROM discord_inbox p
		    WHERE p.cc_id = i.cc_id AND p.lane_key = i.lane_key AND p.id < i.id
		      AND (p.status = 'admitted' OR (p.status = 'claimed' AND (p.lease_expires_at IS NULL OR p.lease_expires_at > datetime('now'))))
		  )
		ORDER BY i.id LIMIT 1`, ccID).Scan(&id)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`UPDATE discord_inbox SET status='claimed', claim_token=?,
		lease_expires_at=datetime('now', ?), attempt_count=attempt_count+1
		WHERE id=? AND cc_id=? AND (status='admitted' OR (status='claimed' AND lease_expires_at <= datetime('now')))`,
		token, fmt.Sprintf("+%d seconds", leaseSeconds), id, ccID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, sql.ErrNoRows
	}
	var e models.InboxEvent
	err = tx.QueryRow(`SELECT id, cc_id, channel_handle, event_type, payload, event_key, lane_key,
		status, claim_token, attempt_count, last_error, created_at FROM discord_inbox WHERE id=?`, id).
		Scan(&e.ID, &e.CCID, &e.ChannelHandle, &e.EventType, &e.Payload, &e.EventKey, &e.LaneKey,
			&e.Status, &e.ClaimToken, &e.AttemptCount, &e.LastError, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &e, nil
}

func (q *ControlChannelQueries) FinishInbox(ccID string, id int64, claimToken, status, lastError string) error {
	if status != "completed" && status != "admitted" && status != "dead_letter" {
		return fmt.Errorf("invalid inbox terminal status %q", status)
	}
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	completed := "NULL"
	if status == "completed" || status == "dead_letter" {
		completed = "datetime('now')"
	}
	res, err := q.db.Exec(`UPDATE discord_inbox SET status=?, claim_token='', lease_expires_at=NULL,
		last_error=?, completed_at=`+completed+` WHERE cc_id=? AND id=? AND status='claimed' AND claim_token=?`,
		status, lastError, ccID, id, claimToken)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (q *ControlChannelQueries) RenewInbox(ccID string, id int64, claimToken string, leaseSeconds int) error {
	if leaseSeconds < 10 || leaseSeconds > 3600 {
		leaseSeconds = 120
	}
	res, err := q.db.Exec(`UPDATE discord_inbox SET lease_expires_at=datetime('now', ?)
		WHERE cc_id=? AND id=? AND status='claimed' AND claim_token=?`,
		fmt.Sprintf("+%d seconds", leaseSeconds), ccID, id, claimToken)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (q *ControlChannelQueries) LatestInboxID(ccID string) (int64, error) {
	var id int64
	err := q.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM discord_inbox WHERE cc_id = ?`, ccID).Scan(&id)
	return id, err
}

func (q *ControlChannelQueries) CreateAgentTest(t *models.CCAgentTest) error {
	_, err := q.db.Exec(
		`INSERT INTO cc_agent_tests (id, cc_id, client_id, handle, agent_type, status, error, inbox_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.CCID, t.ClientID, t.Handle, t.AgentType, t.Status, t.Error, t.InboxID,
	)
	return err
}

func (q *ControlChannelQueries) GetAgentTest(ccID, id string) (*models.CCAgentTest, error) {
	var t models.CCAgentTest
	err := q.db.QueryRow(
		`SELECT id, cc_id, client_id, handle, agent_type, status, error, inbox_id, created_at, updated_at
		 FROM cc_agent_tests WHERE cc_id = ? AND id = ?`,
		ccID, id,
	).Scan(&t.ID, &t.CCID, &t.ClientID, &t.Handle, &t.AgentType, &t.Status, &t.Error, &t.InboxID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (q *ControlChannelQueries) UpdateAgentTestStatusForClient(id, clientID, status, errText string) error {
	res, err := q.db.Exec(
		`UPDATE cc_agent_tests
		 SET status = ?, error = ?, updated_at = datetime('now')
		 WHERE id = ? AND client_id = ?`,
		status, errText, id, clientID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (q *ControlChannelQueries) PullInbox(ccID string, sinceID int64, channelHandles []string, limit int) ([]models.InboxEvent, error) {
	args := []interface{}{ccID, sinceID}
	q1 := `SELECT id, cc_id, channel_handle, event_type, payload, event_key, lane_key, status,
	              claim_token, attempt_count, last_error, created_at
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
		if err := rows.Scan(&e.ID, &e.CCID, &e.ChannelHandle, &e.EventType, &e.Payload, &e.EventKey,
			&e.LaneKey, &e.Status, &e.ClaimToken, &e.AttemptCount, &e.LastError, &e.CreatedAt); err != nil {
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
			"DELETE FROM discord_inbox WHERE status IN ('completed','dead_letter') AND created_at < datetime('now', ?)",
			fmt.Sprintf("-%d hours", retentionHours),
		); err != nil {
			return err
		}
	}
	if perChannelMax > 0 {
		_, err := q.db.Exec(
			`DELETE FROM discord_inbox WHERE status IN ('completed','dead_letter') AND id NOT IN (
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
