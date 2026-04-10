package queries

import (
	"database/sql"

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
