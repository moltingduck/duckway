package queries

import "database/sql"

type ConversationUsageQueries struct {
	db *sql.DB
}

func NewConversationUsageQueries(db *sql.DB) *ConversationUsageQueries {
	return &ConversationUsageQueries{db: db}
}

// ConversationUsageRecord is one captured per-request token row.
type ConversationUsageRecord struct {
	ClientID            string
	APIKeyID            string
	ServiceName         string
	ConversationID      string // claude X-Claude-Code-Session-Id; "" for non-claude
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// Insert records one request's token usage. Best-effort: callers log
// and continue on error rather than failing the proxied request.
func (q *ConversationUsageQueries) Insert(r *ConversationUsageRecord) error {
	_, err := q.db.Exec(
		`INSERT INTO conversation_usage
		 (client_id, api_key_id, service_name, conversation_id, model,
		  input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ClientID, r.APIKeyID, r.ServiceName, r.ConversationID, r.Model,
		r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens,
	)
	return err
}

// KeyTokenTotals is the per-key token rollup shown on the usage panel.
type KeyTokenTotals struct {
	APIKeyID            string `json:"api_key_id"`
	Requests            int64  `json:"requests"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	Conversations       int64  `json:"conversations"`
	LastSeen            string `json:"last_seen"`
}

// TotalsByKey returns token rollups keyed by api_key_id over the
// trailing sinceHours hours (0 = all time). Map for O(1) lookup when
// decorating the per-key usage rows.
func (q *ConversationUsageQueries) TotalsByKey(sinceHours int) (map[string]KeyTokenTotals, error) {
	where := ""
	var args []interface{}
	if sinceHours > 0 {
		where = "WHERE created_at >= datetime('now', ?)"
		args = append(args, "-"+itoa(sinceHours)+" hours")
	}
	rows, err := q.db.Query(`
		SELECT api_key_id,
		       COUNT(*),
		       COALESCE(SUM(input_tokens),0),
		       COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cache_read_tokens),0),
		       COALESCE(SUM(cache_creation_tokens),0),
		       COUNT(DISTINCT conversation_id),
		       MAX(created_at)
		FROM conversation_usage `+where+`
		GROUP BY api_key_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]KeyTokenTotals{}
	for rows.Next() {
		var t KeyTokenTotals
		var last *string
		if err := rows.Scan(&t.APIKeyID, &t.Requests, &t.InputTokens, &t.OutputTokens,
			&t.CacheReadTokens, &t.CacheCreationTokens, &t.Conversations, &last); err != nil {
			return nil, err
		}
		if last != nil {
			t.LastSeen = *last
		}
		out[t.APIKeyID] = t
	}
	return out, rows.Err()
}

// ConversationUsageRow is one conversation's rollup for the drill-down.
type ConversationUsageRow struct {
	ConversationID      string `json:"conversation_id"`
	Model               string `json:"model"`
	Requests            int64  `json:"requests"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	FirstSeen           string `json:"first_seen"`
	LastSeen            string `json:"last_seen"`
	// ChannelName is the CC channel this claude session belongs to, when
	// the conversation_id matches a cc_channels.session_id. Empty for
	// non-CC traffic.
	ChannelName string `json:"channel_name"`
}

// ByKey returns per-conversation rollups for one api key over the
// trailing sinceHours hours (0 = all time), busiest first. Joins
// cc_channels to surface a human-readable channel name when the
// conversation id is a claude session bound to a CC channel.
func (q *ConversationUsageQueries) ByKey(apiKeyID string, sinceHours int) ([]ConversationUsageRow, error) {
	where := "WHERE u.api_key_id = ?"
	args := []interface{}{apiKeyID}
	if sinceHours > 0 {
		where += " AND u.created_at >= datetime('now', ?)"
		args = append(args, "-"+itoa(sinceHours)+" hours")
	}
	rows, err := q.db.Query(`
		SELECT u.conversation_id,
		       COALESCE(MAX(u.model),''),
		       COUNT(*),
		       COALESCE(SUM(u.input_tokens),0),
		       COALESCE(SUM(u.output_tokens),0),
		       COALESCE(SUM(u.cache_read_tokens),0),
		       COALESCE(SUM(u.cache_creation_tokens),0),
		       MIN(u.created_at),
		       MAX(u.created_at),
		       COALESCE(MAX(c.name),'')
		FROM conversation_usage u
		LEFT JOIN cc_channels c ON c.session_id = u.conversation_id AND u.conversation_id != ''
		`+where+`
		GROUP BY u.conversation_id
		ORDER BY SUM(u.input_tokens + u.output_tokens) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConversationUsageRow
	for rows.Next() {
		var r ConversationUsageRow
		var first, last *string
		if err := rows.Scan(&r.ConversationID, &r.Model, &r.Requests,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
			&first, &last, &r.ChannelName); err != nil {
			return nil, err
		}
		if first != nil {
			r.FirstSeen = *first
		}
		if last != nil {
			r.LastSeen = *last
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneOlderThan deletes usage rows older than the given number of days.
// Returns rows affected. Called periodically by the retention sweeper.
func (q *ConversationUsageQueries) PruneOlderThan(days int) (int64, error) {
	res, err := q.db.Exec(
		"DELETE FROM conversation_usage WHERE created_at < datetime('now', ?)",
		"-"+itoa(days)+" days",
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
