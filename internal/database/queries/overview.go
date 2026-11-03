package queries

import (
	"database/sql"
	"time"
)

// OverviewQueries answers the cheap aggregate questions behind GET
// /api/overview. All time comparisons use Go-computed UTC cutoffs bound as
// parameters ("2006-01-02 15:04:05", the datetime('now') text format) so the
// same SQL runs on SQLite and PostgreSQL.
type OverviewQueries struct {
	db *sql.DB
}

func NewOverviewQueries(db *sql.DB) *OverviewQueries { return &OverviewQueries{db: db} }

// OverviewTimeFormat is the stored text timestamp layout.
const OverviewTimeFormat = "2006-01-02 15:04:05"

func ovCutoff(t time.Time) string { return t.UTC().Format(OverviewTimeFormat) }

// ParseStoredTime parses the timestamp layouts the schema produces.
func ParseStoredTime(s string) (time.Time, bool) {
	for _, l := range []string{OverviewTimeFormat, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05Z07:00"} {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// OverviewPending summarises pending approvals.
type OverviewPending struct {
	Count         int
	OldestCreated string
	OldestClient  string
	OldestService string
}

func (q *OverviewQueries) Pending() (OverviewPending, error) {
	var p OverviewPending
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM approvals WHERE status = 'pending'`).Scan(&p.Count); err != nil {
		return p, err
	}
	if p.Count == 0 {
		return p, nil
	}
	err := q.db.QueryRow(`SELECT a.created_at, COALESCE(c.name, ''), COALESCE(s.name, '')
		FROM approvals a
		JOIN placeholder_keys p ON a.placeholder_id = p.id
		LEFT JOIN clients c ON p.client_id = c.id
		LEFT JOIN services s ON p.service_id = s.id
		WHERE a.status = 'pending'
		ORDER BY a.created_at ASC LIMIT 1`).Scan(&p.OldestCreated, &p.OldestClient, &p.OldestService)
	if err == sql.ErrNoRows {
		return p, nil
	}
	return p, err
}

// OverviewNames is a count plus up to a few names.
type OverviewNames struct {
	Count int
	Names []string
}

// StaleClients lists active clients not seen since cutoff (never-seen clients
// fall back to their creation time).
func (q *OverviewQueries) StaleClients(cutoff time.Time, limit int) (OverviewNames, error) {
	var out OverviewNames
	c := ovCutoff(cutoff)
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM clients WHERE is_active = 1 AND COALESCE(last_seen_at, created_at) < ?`, c).Scan(&out.Count); err != nil {
		return out, err
	}
	rows, err := q.db.Query(`SELECT name FROM clients WHERE is_active = 1 AND COALESCE(last_seen_at, created_at) < ?
		ORDER BY COALESCE(last_seen_at, created_at) ASC, name ASC LIMIT ?`, c, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return out, err
		}
		out.Names = append(out.Names, n)
	}
	return out, rows.Err()
}

// OverviewRollout is the current (active/paused) client rollout, if it needs
// attention.
type OverviewRollout struct {
	ID            string
	TargetVersion string
	Status        string
	Failed        int
	Total         int
}

// AttentionRollouts returns active/paused rollouts that are paused or have
// failed jobs.
func (q *OverviewQueries) AttentionRollouts() ([]OverviewRollout, error) {
	rows, err := q.db.Query(`SELECT r.id, r.target_version, r.status,
		COALESCE(SUM(CASE WHEN j.status = 'failed' THEN 1 ELSE 0 END), 0), COUNT(j.id)
		FROM client_update_rollouts r LEFT JOIN client_update_jobs j ON j.rollout_id = r.id
		WHERE r.status IN ('active', 'paused')
		GROUP BY r.id, r.target_version, r.status, r.created_at
		ORDER BY r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OverviewRollout
	for rows.Next() {
		var r OverviewRollout
		if err := rows.Scan(&r.ID, &r.TargetVersion, &r.Status, &r.Failed, &r.Total); err != nil {
			return nil, err
		}
		if r.Status == "paused" || r.Failed > 0 {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// OverviewCredentials counts credentials needing attention (api_keys).
type OverviewCredentials struct {
	Total int
	// Expiring: active non-refreshable keys with an expiry at or before the
	// horizon (already-expired included).
	Expiring OverviewNames
	// RefreshOverdue: active refreshable keys whose access token expired more
	// than the grace period ago (the refresher is not renewing them).
	RefreshOverdue OverviewNames
	// Deactivated: refreshable keys that are disabled (inactive but still
	// holding a refresh token). No failure marker is stored, so this may be an
	// admin action or a refresher failure.
	Deactivated OverviewNames
}

func (q *OverviewQueries) names(query string, args ...interface{}) (OverviewNames, error) {
	var out OverviewNames
	rows, err := q.db.Query(query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return out, err
		}
		out.Names = append(out.Names, n)
	}
	return out, rows.Err()
}

func (q *OverviewQueries) Credentials(now time.Time, horizon, grace time.Duration, limit int) (OverviewCredentials, error) {
	var out OverviewCredentials
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE key_encrypted <> ''`).Scan(&out.Total); err != nil {
		return out, err
	}
	hz := now.Add(horizon).UnixMilli()
	gr := now.Add(-grace).UnixMilli()
	type spec struct {
		dst  *OverviewNames
		cond string
		arg  []interface{}
	}
	for _, s := range []spec{
		{&out.Expiring, `is_active = 1 AND refresh_token = '' AND expires_at > 0 AND expires_at <= ?`, []interface{}{hz}},
		{&out.RefreshOverdue, `is_active = 1 AND refresh_token <> '' AND expires_at > 0 AND expires_at < ?`, []interface{}{gr}},
		{&out.Deactivated, `is_active = 0 AND refresh_token <> ''`, nil},
	} {
		if err := q.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE `+s.cond, s.arg...).Scan(&s.dst.Count); err != nil {
			return out, err
		}
		if s.dst.Count == 0 {
			continue
		}
		n, err := q.names(`SELECT name FROM api_keys WHERE `+s.cond+` ORDER BY name ASC LIMIT ?`, append(append([]interface{}{}, s.arg...), limit)...)
		if err != nil {
			return out, err
		}
		s.dst.Names = n.Names
	}
	return out, nil
}

// OverviewStats are the stat-card counters.
type OverviewStats struct {
	ClientsTotal  int
	ClientsOnline int
	PhantomTokens int
	Requests24h   int
	CanaryTokens  int
	CanaryAlertTo string
}

func (q *OverviewQueries) Stats(now time.Time, online time.Duration) (OverviewStats, error) {
	var s OverviewStats
	if err := q.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN last_seen_at >= ? THEN 1 ELSE 0 END), 0)
		FROM clients WHERE is_active = 1`, ovCutoff(now.Add(-online))).Scan(&s.ClientsTotal, &s.ClientsOnline); err != nil {
		return s, err
	}
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM placeholder_keys`).Scan(&s.PhantomTokens); err != nil {
		return s, err
	}
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE created_at >= ?`, ovCutoff(now.Add(-24*time.Hour))).Scan(&s.Requests24h); err != nil {
		return s, err
	}
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM canary_tokens`).Scan(&s.CanaryTokens); err != nil {
		return s, err
	}
	err := q.db.QueryRow(`SELECT email FROM canary_settings WHERE id = 'default'`).Scan(&s.CanaryAlertTo)
	if err == sql.ErrNoRows {
		err = nil
	}
	return s, err
}
