package queries

import (
	"database/sql"
	"errors"

	"github.com/hackerduck/duckway/internal/models"
)

// ErrNotDraft is returned when an operation requires an unfinished client draft.
var ErrNotDraft = errors.New("client is not a draft")

// ClientDraft is an unfinished Add-client wizard run. The backing client row is
// inactive and holds an unusable token hash until IssueToken completes it.
type ClientDraft struct {
	ClientID     string `json:"id"`
	ShortID      string `json:"short_id"`
	Name         string `json:"name"`
	UpdatePolicy string `json:"update_policy"`
	Step         int    `json:"step"`
	SuiteID      string `json:"suite_id"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type ClientDraftQueries struct {
	db *sql.DB
}

func NewClientDraftQueries(db *sql.DB) *ClientDraftQueries {
	return &ClientDraftQueries{db: db}
}

const clientDraftCols = `d.client_id, c.short_id, c.name, c.update_policy, d.step, d.suite_id, d.created_at, d.updated_at`
const clientDraftFrom = ` FROM client_drafts d JOIN clients c ON c.id = d.client_id`

func scanClientDraft(row interface{ Scan(...interface{}) error }, d *ClientDraft) error {
	return row.Scan(&d.ClientID, &d.ShortID, &d.Name, &d.UpdatePolicy, &d.Step, &d.SuiteID, &d.CreatedAt, &d.UpdatedAt)
}

// Create inserts an inactive client row (placeholder token hash, never
// presentable) together with its draft marker in one transaction.
func (q *ClientDraftQueries) Create(c *models.Client) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		"INSERT INTO clients (id, short_id, name, token_hash, is_active, canary_enabled, update_policy) VALUES (?, ?, ?, ?, 0, ?, ?)",
		c.ID, c.ShortID, c.Name, c.TokenHash, c.CanaryEnabled, defaultUpdatePolicy(c.UpdatePolicy),
	); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO client_drafts (client_id) VALUES (?)", c.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (q *ClientDraftQueries) List() ([]ClientDraft, error) {
	rows, err := q.db.Query("SELECT " + clientDraftCols + clientDraftFrom + " ORDER BY d.updated_at DESC, d.client_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClientDraft{}
	for rows.Next() {
		var d ClientDraft
		if err := scanClientDraft(rows, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (q *ClientDraftQueries) Get(id string) (*ClientDraft, error) {
	var d ClientDraft
	if err := scanClientDraft(q.db.QueryRow("SELECT "+clientDraftCols+clientDraftFrom+" WHERE d.client_id = ?", id), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Update changes the draft's wizard state. Nil fields are left unchanged.
func (q *ClientDraftQueries) Update(id string, name, policy *string, step *int, suiteID *string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM client_drafts WHERE client_id = ?", id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotDraft
	}
	if name != nil {
		if _, err := tx.Exec("UPDATE clients SET name = ? WHERE id = ?", *name, id); err != nil {
			return err
		}
	}
	if policy != nil {
		if _, err := tx.Exec("UPDATE clients SET update_policy = ? WHERE id = ?", defaultUpdatePolicy(*policy), id); err != nil {
			return err
		}
	}
	if step != nil {
		if _, err := tx.Exec("UPDATE client_drafts SET step = ? WHERE client_id = ?", *step, id); err != nil {
			return err
		}
	}
	if suiteID != nil {
		if _, err := tx.Exec("UPDATE client_drafts SET suite_id = ? WHERE client_id = ?", *suiteID, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE client_drafts SET updated_at = datetime('now') WHERE client_id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// IsDraft reports whether the client is an unfinished draft.
func (q *ClientDraftQueries) IsDraft(id string) bool {
	var n int
	return q.db.QueryRow("SELECT COUNT(*) FROM client_drafts WHERE client_id = ?", id).Scan(&n) == nil && n > 0
}

// IssueToken finishes a draft: it installs the real token hash, activates the
// client and removes the draft marker atomically. Only this call makes a
// draft client able to authenticate.
func (q *ClientDraftQueries) IssueToken(id, tokenHash string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("DELETE FROM client_drafts WHERE client_id = ?", id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrNotDraft
	}
	if _, err := tx.Exec("UPDATE clients SET token_hash = ?, is_active = 1 WHERE id = ?", tokenHash, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Discard removes an unfinished draft and its client row atomically. The
// marker delete must affect exactly one row, so a draft that was issued (or
// discarded) concurrently is never deleted as a live client: ErrNotDraft.
func (q *ClientDraftQueries) Discard(id string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("DELETE FROM client_drafts WHERE client_id = ?", id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrNotDraft
	}
	if err := deleteClientTx(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}
