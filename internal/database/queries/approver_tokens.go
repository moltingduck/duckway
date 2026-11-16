package queries

import (
	"database/sql"
	"errors"
	"time"
)

// ApproverToken is the stored, non-secret view of an approver token. The token
// itself is never stored, only its SHA-256 (token_hash), and that hash is never
// returned by List.
type ApproverToken struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	RevokedAt  *string `json:"revoked_at"`
}

// ErrApproverNotFound means no active approver token has that ID or hash.
var ErrApproverNotFound = errors.New("approver token not found")

// ErrApproverNameTaken means another approver token (active or revoked) already
// uses the name.
var ErrApproverNameTaken = errors.New("an approver token with this name already exists")

type ApproverTokenQueries struct{ db *sql.DB }

func NewApproverTokenQueries(db *sql.DB) *ApproverTokenQueries { return &ApproverTokenQueries{db: db} }

// Create stores a new token by hash.
func (q *ApproverTokenQueries) Create(id, name, tokenHash string) error {
	var n int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM approver_tokens WHERE name = ?`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrApproverNameTaken
	}
	_, err := q.db.Exec(`INSERT INTO approver_tokens (id, name, token_hash) VALUES (?, ?, ?)`, id, name, tokenHash)
	return err
}

// List returns every token, newest first, revoked ones included.
func (q *ApproverTokenQueries) List() ([]ApproverToken, error) {
	rows, err := q.db.Query(`SELECT id, name, created_at, last_used_at, revoked_at FROM approver_tokens ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApproverToken{}
	for rows.Next() {
		var t ApproverToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetActiveByHash returns the non-revoked token whose hash matches, and the
// stored hash so the caller can compare in constant time.
func (q *ApproverTokenQueries) GetActiveByHash(hash string) (*ApproverToken, string, error) {
	var t ApproverToken
	var stored string
	err := q.db.QueryRow(`SELECT id, name, created_at, last_used_at, revoked_at, token_hash FROM approver_tokens WHERE token_hash = ? AND revoked_at IS NULL`, hash).
		Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrApproverNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return &t, stored, nil
}

// GetByID returns a token (revoked or not).
func (q *ApproverTokenQueries) GetByID(id string) (*ApproverToken, error) {
	var t ApproverToken
	err := q.db.QueryRow(`SELECT id, name, created_at, last_used_at, revoked_at FROM approver_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrApproverNotFound
	}
	return &t, err
}

// Revoke disables a token. Revoking an already revoked token keeps its first
// revocation time and succeeds; an unknown ID is ErrApproverNotFound.
func (q *ApproverTokenQueries) Revoke(id string) error {
	if _, err := q.GetByID(id); err != nil {
		return err
	}
	_, err := q.db.Exec(`UPDATE approver_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, time.Now().UTC().Format(ApprovalTimeLayout), id)
	return err
}

// TouchLastUsed records a use, at most once per minute per token so a busy
// stream or poller does not write on every request.
func (q *ApproverTokenQueries) TouchLastUsed(id string) {
	now := time.Now().UTC()
	_, _ = q.db.Exec(`UPDATE approver_tokens SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		now.Format(ApprovalTimeLayout), id, now.Add(-time.Minute).Format(ApprovalTimeLayout))
}
