package queries

import (
	"database/sql"
	"strings"
	"time"
)

// AuditRow is one admin_audit_log row. Before/After are redacted JSON.
type AuditRow struct {
	ID           int64  `json:"id"`
	CreatedAt    string `json:"created_at"`
	Actor        string `json:"actor"`
	Via          string `json:"via"`
	RemoteAddr   string `json:"remote_addr"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	ResourceName string `json:"resource_name"`
	Summary      string `json:"summary"`
	Before       string `json:"-"`
	After        string `json:"-"`
	HasDiff      bool   `json:"has_diff"`
	Status       string `json:"status"`
}

// AuditFilter selects rows for List. Zero values mean "no filter".
type AuditFilter struct {
	Actor        string
	Kind         string // rules | approvals | credentials | clients | signins | settings
	Since        string // UTC "2006-01-02 15:04:05"
	Query        string
	ResourceType string
	ResourceID   string
	BeforeID     int64 // cursor: only rows with id < BeforeID
	Limit        int
}

// AuditKinds maps a UI kind to the resource types it covers. "approvals" is
// matched by action (decisions are filed under the phantom token they unlock).
var AuditKinds = map[string][]string{
	"rules":       {"service", "placeholder", "group", "key_group", "key_suite"},
	"credentials": {"api_key"},
	"clients":     {"client"},
	"signins":     {"signin"},
	"settings":    {"setting", "notification", "canary", "supply_chain", "cc", "plugin"},
}

type AuditQueries struct{ db *sql.DB }

func NewAuditQueries(db *sql.DB) *AuditQueries { return &AuditQueries{db: db} }

// Insert stores a row and returns its id.
func (q *AuditQueries) Insert(r AuditRow) (int64, error) {
	if r.Status == "" {
		r.Status = "ok"
	}
	created := r.CreatedAt
	if created == "" {
		created = utcFormat(time.Now())
	}
	var id int64
	err := q.db.QueryRow(
		`INSERT INTO admin_audit_log (created_at, actor, via, remote_addr, action, resource_type, resource_id, resource_name, summary, before_json, after_json, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		created, r.Actor, r.Via, r.RemoteAddr, r.Action, r.ResourceType, r.ResourceID, r.ResourceName, r.Summary, r.Before, r.After, r.Status,
	).Scan(&id)
	return id, err
}

// UpdateSummary rewrites the summary of one row (sign-in failure throttling).
func (q *AuditQueries) UpdateSummary(id int64, summary string) error {
	_, err := q.db.Exec(`UPDATE admin_audit_log SET summary = ? WHERE id = ?`, summary, id)
	return err
}

// List returns newest-first rows (without before/after bodies) and the id to
// pass as BeforeID for the next page (0 when there is none).
func (q *AuditQueries) List(f AuditFilter) ([]AuditRow, int64, error) {
	limit := f.Limit
	if limit < 1 || limit > 200 {
		limit = 50
	}
	var where []string
	var args []interface{}
	if f.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, f.Actor)
	}
	if f.Kind == "approvals" {
		where = append(where, "action LIKE 'approval.%'")
	} else if types, ok := AuditKinds[f.Kind]; ok {
		ph := strings.TrimRight(strings.Repeat("?,", len(types)), ",")
		cond := "resource_type IN (" + ph + ")"
		for _, t := range types {
			args = append(args, t)
		}
		if f.Kind == "rules" {
			cond = "((" + cond + " AND action NOT LIKE 'approval.%') OR action LIKE 'api\\_key.acl%' ESCAPE '\\')"
		}
		where = append(where, cond)
	}
	if f.Since != "" {
		where = append(where, "created_at >= ?")
		args = append(args, f.Since)
	}
	if f.ResourceType != "" {
		where = append(where, "resource_type = ?")
		args = append(args, f.ResourceType)
	}
	if f.ResourceID != "" {
		where = append(where, "resource_id = ?")
		args = append(args, f.ResourceID)
	}
	if f.Query != "" {
		like := "%" + escapeLike(strings.ToLower(f.Query)) + "%"
		where = append(where, `(LOWER(summary) LIKE ? ESCAPE '\' OR LOWER(resource_name) LIKE ? ESCAPE '\' OR LOWER(action) LIKE ? ESCAPE '\' OR LOWER(actor) LIKE ? ESCAPE '\' OR LOWER(resource_id) LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like, like)
	}
	if f.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, f.BeforeID)
	}
	sqlText := `SELECT id, created_at, actor, via, remote_addr, action, resource_type, resource_id, resource_name, summary,
		(CASE WHEN before_json <> '' OR after_json <> '' THEN 1 ELSE 0 END), status FROM admin_audit_log`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := q.db.Query(sqlText, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var diff int
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.Actor, &r.Via, &r.RemoteAddr, &r.Action, &r.ResourceType, &r.ResourceID, &r.ResourceName, &r.Summary, &diff, &r.Status); err != nil {
			return nil, 0, err
		}
		r.HasDiff = diff == 1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// Get returns one full row including before/after.
func (q *AuditQueries) Get(id int64) (*AuditRow, error) {
	var r AuditRow
	err := q.db.QueryRow(
		`SELECT id, created_at, actor, via, remote_addr, action, resource_type, resource_id, resource_name, summary, before_json, after_json, status
		 FROM admin_audit_log WHERE id = ?`, id,
	).Scan(&r.ID, &r.CreatedAt, &r.Actor, &r.Via, &r.RemoteAddr, &r.Action, &r.ResourceType, &r.ResourceID, &r.ResourceName, &r.Summary, &r.Before, &r.After, &r.Status)
	if err != nil {
		return nil, err
	}
	r.HasDiff = r.Before != "" || r.After != ""
	return &r, nil
}

// PruneOlderThan deletes rows older than days, in bounded batches.
func (q *AuditQueries) PruneOlderThan(days, batch, maxBatches int) (int64, error) {
	if days < 1 {
		return 0, nil
	}
	if batch < 1 {
		batch = 1000
	}
	cutoff := utcFormat(time.Now().AddDate(0, 0, -days))
	var total int64
	for i := 0; maxBatches <= 0 || i < maxBatches; i++ {
		res, err := q.db.Exec(`DELETE FROM admin_audit_log WHERE id IN (SELECT id FROM admin_audit_log WHERE created_at < ? ORDER BY id LIMIT ?)`, cutoff, batch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batch) {
			break
		}
	}
	return total, nil
}

// UTCNow formats t the way every timestamp column stores it.
func UTCNow(t time.Time) string { return utcFormat(t) }
