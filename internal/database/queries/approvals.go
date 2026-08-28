package queries

import (
	"database/sql"
	"strconv"

	"github.com/hackerduck/duckway/internal/models"
)

type ApprovalQueries struct {
	db *sql.DB
}

func NewApprovalQueries(db *sql.DB) *ApprovalQueries {
	return &ApprovalQueries{db: db}
}

func (q *ApprovalQueries) GetValidApproval(placeholderID string) (*models.Approval, error) {
	var a models.Approval
	err := q.db.QueryRow(
		`SELECT id, placeholder_id, status, approved_at, expires_at, request_info, created_at
		 FROM approvals
		 WHERE placeholder_id = ? AND status = 'approved' AND (expires_at IS NULL OR expires_at > datetime('now'))
		 ORDER BY approved_at DESC LIMIT 1`,
		placeholderID,
	).Scan(&a.ID, &a.PlaceholderID, &a.Status, &a.ApprovedAt, &a.ExpiresAt, &a.RequestInfo, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (q *ApprovalQueries) GetPending(placeholderID string) (*models.Approval, error) {
	var a models.Approval
	err := q.db.QueryRow(
		`SELECT id, placeholder_id, status, approved_at, expires_at, request_info, created_at
		 FROM approvals WHERE placeholder_id = ? AND status = 'pending' ORDER BY created_at DESC LIMIT 1`,
		placeholderID,
	).Scan(&a.ID, &a.PlaceholderID, &a.Status, &a.ApprovedAt, &a.ExpiresAt, &a.RequestInfo, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (q *ApprovalQueries) GetByID(id string) (*models.Approval, error) {
	var a models.Approval
	err := q.db.QueryRow(
		`SELECT id, placeholder_id, status, approved_at, expires_at, request_info, created_at
		 FROM approvals WHERE id = ?`, id,
	).Scan(&a.ID, &a.PlaceholderID, &a.Status, &a.ApprovedAt, &a.ExpiresAt, &a.RequestInfo, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (q *ApprovalQueries) ListPending() ([]models.Approval, error) {
	rows, err := q.db.Query(
		`SELECT id, placeholder_id, status, approved_at, expires_at, request_info, created_at
		 FROM approvals WHERE status = 'pending' ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.Approval
	for rows.Next() {
		var a models.Approval
		if err := rows.Scan(&a.ID, &a.PlaceholderID, &a.Status, &a.ApprovedAt, &a.ExpiresAt, &a.RequestInfo, &a.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (q *ApprovalQueries) Create(a *models.Approval) error {
	_, err := q.db.Exec(
		"INSERT INTO approvals (id, placeholder_id, status, request_info) VALUES (?, ?, ?, ?)",
		a.ID, a.PlaceholderID, a.Status, a.RequestInfo,
	)
	return err
}

func (q *ApprovalQueries) Approve(id string, expiresAt string) error {
	_, err := q.db.Exec(
		"UPDATE approvals SET status = 'approved', approved_at = datetime('now'), expires_at = ? WHERE id = ?",
		expiresAt, id,
	)
	return err
}

func (q *ApprovalQueries) Reject(id string) error {
	_, err := q.db.Exec("UPDATE approvals SET status = 'rejected' WHERE id = ?", id)
	return err
}

// MarkExpiredAsIgnored finds all pending approvals older than ttlMinutes and
// transitions them to "ignored". Returns the number of rows affected.
//
// Distinct from "rejected" — rejected = admin actively said no, ignored = no
// reply within the timeout. Useful audit signal: lots of ignored = admin is
// overwhelmed or the channel is dead.
func (q *ApprovalQueries) MarkExpiredAsIgnored(ttlMinutes int) (int64, error) {
	if ttlMinutes <= 0 {
		return 0, nil
	}
	res, err := q.db.Exec(
		`UPDATE approvals
		 SET status = 'ignored'
		 WHERE status = 'pending'
		   AND created_at < datetime('now', '-' || ? || ' minutes')`,
		strconv.Itoa(ttlMinutes),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ApprovalListItem is the enriched row used by the admin page — joins through
// placeholder_keys to the client and service so the UI can show names instead
// of opaque IDs and filter by them.
type ApprovalListItem struct {
	ID            string  `json:"id"`
	PlaceholderID string  `json:"placeholder_id"`
	EnvName       string  `json:"env_name"`
	ClientID      string  `json:"client_id"`
	ClientName    string  `json:"client_name"`
	ServiceID     string  `json:"service_id"`
	ServiceName   string  `json:"service_name"`
	Status        string  `json:"status"`
	RequestInfo   *string `json:"request_info"`
	CreatedAt     string  `json:"created_at"`
	ApprovedAt    *string `json:"approved_at"`
	ExpiresAt     *string `json:"expires_at"`
}

// ListEnriched returns approvals filtered by status (any of the given values)
// joined with client and service info. Pass empty statuses for "all".
func (q *ApprovalQueries) ListEnriched(statuses []string, limit int) ([]ApprovalListItem, error) {
	query := `SELECT a.id, a.placeholder_id, p.env_name,
	          p.client_id, COALESCE(c.name, ''),
	          p.service_id, COALESCE(s.name, ''),
	          a.status, a.request_info, a.created_at, a.approved_at, a.expires_at
	          FROM approvals a
	          JOIN placeholder_keys p ON a.placeholder_id = p.id
	          LEFT JOIN clients c ON p.client_id = c.id
	          LEFT JOIN services s ON p.service_id = s.id`

	args := []interface{}{}
	if len(statuses) > 0 {
		placeholders := ""
		for i, st := range statuses {
			if i > 0 {
				placeholders += ", "
			}
			placeholders += "?"
			args = append(args, st)
		}
		query += " WHERE a.status IN (" + placeholders + ")"
	}
	query += " ORDER BY a.created_at DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ApprovalListItem
	for rows.Next() {
		var a ApprovalListItem
		if err := rows.Scan(&a.ID, &a.PlaceholderID, &a.EnvName,
			&a.ClientID, &a.ClientName, &a.ServiceID, &a.ServiceName,
			&a.Status, &a.RequestInfo, &a.CreatedAt, &a.ApprovedAt, &a.ExpiresAt); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// LatestByPlaceholder returns the most recent approval for a placeholder (any status).
func (q *ApprovalQueries) LatestByPlaceholder(placeholderID string) (*models.Approval, error) {
	var a models.Approval
	err := q.db.QueryRow(
		`SELECT id, placeholder_id, status, approved_at, expires_at, request_info, created_at
		 FROM approvals WHERE placeholder_id = ? ORDER BY created_at DESC LIMIT 1`,
		placeholderID,
	).Scan(&a.ID, &a.PlaceholderID, &a.Status, &a.ApprovedAt, &a.ExpiresAt, &a.RequestInfo, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}
