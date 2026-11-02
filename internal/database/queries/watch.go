package queries

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// WatchQueries runs the cheap change-detection queries behind the admin
// live-update stream. Every query is an aggregate over an indexed or tiny
// table and uses no bind parameters or backend-specific SQL, so it behaves
// identically on SQLite and PostgreSQL. Snapshots never carry row data.
type WatchQueries struct{ db *sql.DB }

func NewWatchQueries(db *sql.DB) *WatchQueries { return &WatchQueries{db: db} }

// WatchSnapshot is a comparable fingerprint of the watched tables.
type WatchSnapshot struct {
	LogMaxID       int64  // MAX(request_log.id)
	ApprovalsPend  int64  // COUNT of pending approvals
	ApprovalsSig   string // per-status count + latest decision time
	RolloutsSig    string // per-status count + latest updated_at (rollouts and jobs)
	ApprovalsTotal int64
}

// Snapshot reads one fingerprint. It stops at the first failing query.
func (q *WatchQueries) Snapshot(ctx context.Context) (WatchSnapshot, error) {
	var s WatchSnapshot
	if err := q.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM request_log`).Scan(&s.LogMaxID); err != nil {
		return s, fmt.Errorf("watch request_log: %w", err)
	}
	var err error
	if s.ApprovalsSig, s.ApprovalsPend, s.ApprovalsTotal, err = q.approvalsSig(ctx); err != nil {
		return s, err
	}
	var rollouts, jobs string
	if rollouts, err = q.groupSig(ctx, "client_update_rollouts"); err != nil {
		return s, err
	}
	if jobs, err = q.groupSig(ctx, "client_update_jobs"); err != nil {
		return s, err
	}
	s.RolloutsSig = rollouts + "|" + jobs
	return s, nil
}

func (q *WatchQueries) approvalsSig(ctx context.Context) (string, int64, int64, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT status, COUNT(*), COALESCE(MAX(approved_at), ''), COALESCE(MAX(created_at), '') FROM approvals GROUP BY status ORDER BY status`)
	if err != nil {
		return "", 0, 0, fmt.Errorf("watch approvals: %w", err)
	}
	defer rows.Close()
	var parts []string
	var pending, total int64
	for rows.Next() {
		var st, decided, created string
		var n int64
		if err := rows.Scan(&st, &n, &decided, &created); err != nil {
			return "", 0, 0, fmt.Errorf("watch approvals scan: %w", err)
		}
		total += n
		if st == "pending" {
			pending = n
		}
		parts = append(parts, fmt.Sprintf("%s:%d:%s:%s", st, n, decided, created))
	}
	return strings.Join(parts, ";"), pending, total, rows.Err()
}

func (q *WatchQueries) groupSig(ctx context.Context, table string) (string, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT status, COUNT(*), COALESCE(MAX(updated_at), '') FROM `+table+` GROUP BY status ORDER BY status`)
	if err != nil {
		return "", fmt.Errorf("watch %s: %w", table, err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var st, upd string
		var n int64
		if err := rows.Scan(&st, &n, &upd); err != nil {
			return "", fmt.Errorf("watch %s scan: %w", table, err)
		}
		parts = append(parts, fmt.Sprintf("%s:%d:%s", st, n, upd))
	}
	return strings.Join(parts, ";"), rows.Err()
}
