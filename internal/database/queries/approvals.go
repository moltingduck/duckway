package queries

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
		 WHERE placeholder_id = ? AND scope = '' AND status = 'approved' AND (expires_at IS NULL OR expires_at > datetime('now'))
		 ORDER BY approved_at DESC LIMIT 1`,
		placeholderID,
	).Scan(&a.ID, &a.PlaceholderID, &a.Status, &a.ApprovedAt, &a.ExpiresAt, &a.RequestInfo, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetValidScopedApproval returns an unexpired approval granted for one specific
// per-rule request signature (see CreateScoped). Token-wide approvals have an
// empty scope and never satisfy a scoped lookup, and vice versa.
func (q *ApprovalQueries) GetValidScopedApproval(placeholderID, scope string) (*models.Approval, error) {
	var a models.Approval
	err := q.db.QueryRow(
		`SELECT id, placeholder_id, status, approved_at, expires_at, request_info, created_at
		 FROM approvals
		 WHERE placeholder_id = ? AND scope = ? AND scope <> '' AND status = 'approved' AND (expires_at IS NULL OR expires_at > datetime('now'))
		 ORDER BY approved_at DESC LIMIT 1`,
		placeholderID, scope,
	).Scan(&a.ID, &a.PlaceholderID, &a.Status, &a.ApprovedAt, &a.ExpiresAt, &a.RequestInfo, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetPendingScoped returns the newest pending approval for a per-rule request
// signature, so a client retrying while the admin decides reuses it instead of
// queueing (and notifying) again.
func (q *ApprovalQueries) GetPendingScoped(placeholderID, scope string) (*models.Approval, error) {
	var a models.Approval
	err := q.db.QueryRow(
		`SELECT id, placeholder_id, status, approved_at, expires_at, request_info, created_at
		 FROM approvals WHERE placeholder_id = ? AND scope = ? AND scope <> '' AND status = 'pending' ORDER BY created_at DESC LIMIT 1`,
		placeholderID, scope,
	).Scan(&a.ID, &a.PlaceholderID, &a.Status, &a.ApprovedAt, &a.ExpiresAt, &a.RequestInfo, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetPendingTokenWide returns the newest pending token-wide approval (empty
// scope) for a phantom token, so retries reuse it instead of queueing another.
func (q *ApprovalQueries) GetPendingTokenWide(placeholderID string) (*models.Approval, error) {
	var a models.Approval
	err := q.db.QueryRow(
		`SELECT id, placeholder_id, status, approved_at, expires_at, request_info, created_at
		 FROM approvals WHERE placeholder_id = ? AND scope = '' AND status = 'pending' ORDER BY created_at DESC LIMIT 1`,
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

// CountPending returns the number of approvals awaiting a decision.
func (q *ApprovalQueries) CountPending() (int, error) {
	var n int
	err := q.db.QueryRow(`SELECT COUNT(*) FROM approvals WHERE status = 'pending'`).Scan(&n)
	return n, err
}

func (q *ApprovalQueries) Create(a *models.Approval) error {
	_, err := q.db.Exec(
		"INSERT INTO approvals (id, placeholder_id, status, request_info) VALUES (?, ?, ?, ?)",
		a.ID, a.PlaceholderID, a.Status, a.RequestInfo,
	)
	return err
}

// CreateScoped inserts a pending approval limited to one request signature
// (an approve rule matched). scope must be non-empty.
func (q *ApprovalQueries) CreateScoped(a *models.Approval, scope string) error {
	_, err := q.db.Exec(
		"INSERT INTO approvals (id, placeholder_id, status, request_info, scope) VALUES (?, ?, ?, ?, ?)",
		a.ID, a.PlaceholderID, a.Status, a.RequestInfo, scope,
	)
	return err
}

// ApprovalTimeLayout is the stored timestamp format (UTC), identical to what
// SQLite's datetime('now') returns, so stored values compare correctly in SQL.
const ApprovalTimeLayout = "2006-01-02 15:04:05"

// MaxApprovalMinutes caps an approval's lifetime at one year.
const MaxApprovalMinutes = 60 * 24 * 365

// ClampApprovalMinutes limits a duration in minutes to 1..MaxApprovalMinutes.
func ClampApprovalMinutes(n int) int {
	if n < 1 {
		return 1
	}
	if n > MaxApprovalMinutes {
		return MaxApprovalMinutes
	}
	return n
}

// DefaultApprovalMinutes is the lifetime of an approval when neither the
// request nor the phantom token specifies one (24 hours).
const DefaultApprovalMinutes = 1440

// PlaceholderApprovalMinutes returns the phantom token's configured approval
// lifetime (approval_ttl_minutes), or DefaultApprovalMinutes when it is unset or
// the token cannot be read. This is the lifetime an approval gets when the
// approver does not choose one.
func (q *ApprovalQueries) PlaceholderApprovalMinutes(placeholderID string) int {
	var n int
	if err := q.db.QueryRow(`SELECT approval_ttl_minutes FROM placeholder_keys WHERE id = ?`, placeholderID).Scan(&n); err != nil || n <= 0 {
		return DefaultApprovalMinutes
	}
	return n
}

// ApprovalExpiry returns the UTC expiry timestamp string for an approval
// granted now that lasts d. Computed in Go so it is portable across SQLite and
// PostgreSQL.
func ApprovalExpiry(d time.Duration) string {
	return time.Now().UTC().Add(d).Format(ApprovalTimeLayout)
}

// Approve marks a PENDING approval granted until expiresAt (anything already
// decided is left untouched and ErrApprovalNotPending is returned), a UTC timestamp in
// ApprovalTimeLayout (see ApprovalExpiry). Anything else is an error: the
// value is stored as text and compared with datetime('now'), so an
// unrecognised string would sort wrongly and might never expire.
func (q *ApprovalQueries) Approve(id string, expiresAt string) error {
	if _, err := time.Parse(ApprovalTimeLayout, expiresAt); err != nil {
		return fmt.Errorf("approval expiry %q must be a UTC timestamp %q", expiresAt, ApprovalTimeLayout)
	}
	res, err := q.db.Exec(
		"UPDATE approvals SET status = 'approved', approved_at = datetime('now'), expires_at = ? WHERE id = ? AND status = 'pending'",
		expiresAt, id,
	)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// Reject marks a PENDING approval rejected. Anything already decided
// (approved, rejected, ignored) is left untouched and ErrApprovalNotPending is
// returned, so a stale form or chat button cannot overturn a decision.
func (q *ApprovalQueries) Reject(id string) error {
	res, err := q.db.Exec("UPDATE approvals SET status = 'rejected' WHERE id = ? AND status = 'pending'", id)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// ErrApprovalNotPending means the approval was already decided (approved,
// rejected or ignored), or does not exist: the decision was not changed.
var ErrApprovalNotPending = errors.New("approval already decided (approved, rejected or expired)")

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrApprovalNotPending
	}
	return nil
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
	// Additive detail fields for the inbox view.
	CredentialName   string  `json:"credential_name"`   // API key or key group name ("" if unknown)
	RequiresApproval bool    `json:"requires_approval"` // phantom token flag that caused the hold
	ClientLastSeenAt *string `json:"client_last_seen_at"`
	ClientVersion    string  `json:"client_version"` // "" when the client never reported one
	// ApprovalTTLMinutes is how long an approval granted from the console lasts
	// (the token's approval_ttl_minutes).
	ApprovalTTLMinutes int `json:"approval_ttl_minutes"`
}

// ListEnriched returns approvals filtered by status (any of the given values)
// joined with client and service info. Pass empty statuses for "all".
func (q *ApprovalQueries) ListEnriched(statuses []string, limit int) ([]ApprovalListItem, error) {
	return q.ListEnrichedFiltered(statuses, "", "", limit)
}

// ListEnrichedFiltered is ListEnriched with optional client and service
// filters applied in SQL before the limit, newest first (created_at, then id
// as a deterministic tie-breaker).
func (q *ApprovalQueries) ListEnrichedFiltered(statuses []string, clientID, serviceID string, limit int) ([]ApprovalListItem, error) {
	query := `SELECT a.id, a.placeholder_id, p.env_name,
	          p.client_id, COALESCE(c.name, ''),
	          p.service_id, COALESCE(s.name, ''),
	          a.status, a.request_info, a.created_at, a.approved_at, a.expires_at,
	          COALESCE(k.name, g.name, ''), p.requires_approval, c.last_seen_at, COALESCE(rs.version, ''), p.approval_ttl_minutes
	          FROM approvals a
	          JOIN placeholder_keys p ON a.placeholder_id = p.id
	          LEFT JOIN clients c ON p.client_id = c.id
	          LEFT JOIN services s ON p.service_id = s.id
	          LEFT JOIN api_keys k ON p.api_key_id = k.id
	          LEFT JOIN api_key_groups g ON p.group_id = g.id
	          LEFT JOIN client_runtime_status rs ON rs.client_id = p.client_id`

	args := []interface{}{}
	conds := []string{}
	if len(statuses) > 0 {
		placeholders := ""
		for i, st := range statuses {
			if i > 0 {
				placeholders += ", "
			}
			placeholders += "?"
			args = append(args, st)
		}
		conds = append(conds, "a.status IN ("+placeholders+")")
	}
	if clientID != "" {
		conds = append(conds, "p.client_id = ?")
		args = append(args, clientID)
	}
	if serviceID != "" {
		conds = append(conds, "p.service_id = ?")
		args = append(args, serviceID)
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY a.created_at DESC, a.id DESC"
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
		var requires int
		if err := rows.Scan(&a.ID, &a.PlaceholderID, &a.EnvName,
			&a.ClientID, &a.ClientName, &a.ServiceID, &a.ServiceName,
			&a.Status, &a.RequestInfo, &a.CreatedAt, &a.ApprovedAt, &a.ExpiresAt,
			&a.CredentialName, &requires, &a.ClientLastSeenAt, &a.ClientVersion, &a.ApprovalTTLMinutes); err != nil {
			return nil, err
		}
		a.RequiresApproval = requires != 0
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
