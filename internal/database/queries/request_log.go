package queries

import (
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"
)

type RequestLogQueries struct {
	db *sql.DB
}

func NewRequestLogQueries(db *sql.DB) *RequestLogQueries {
	return &RequestLogQueries{db: db}
}

// Request decisions recorded in request_log.decision. Legacy rows are ”.
const (
	DecisionAllowed         = "allowed"
	DecisionApprovedForward = "approved_forward"
	DecisionACLDenied       = "acl_denied"
	DecisionHeld            = "held"
)

func (q *RequestLogQueries) Log(clientID, placeholderID, serviceName, method, path string, statusCode int) error {
	return q.LogDecision(clientID, placeholderID, serviceName, method, path, statusCode, DecisionAllowed, "")
}

// LogDecision inserts a row with an explicit gateway decision. approvalID is
// only meaningful for held rows ("" otherwise).
func (q *RequestLogQueries) LogDecision(clientID, placeholderID, serviceName, method, path string, statusCode int, decision, approvalID string) error {
	path = TruncateLogPath(path)
	var phID interface{}
	if placeholderID != "" {
		phID = placeholderID
	}
	_, err := q.db.Exec(
		"INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code, decision, approval_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		clientID, phID, serviceName, method, path, statusCode, decision, approvalID,
	)
	return err
}

// SuggestedRequestLogRetentionDays is the value the Settings page suggests. Nothing
// is pruned unless request_log_retention_days is explicitly set (7-3650), so an
// upgrade never drops history silently.
const SuggestedRequestLogRetentionDays = 90

// MaxACLDeniedRowsPerMinute caps acl_denied rows per client per UTC minute.
const MaxACLDeniedRowsPerMinute = 30

// MaxLoggedPathBytes bounds the stored path of every request_log row so a
// client cannot bloat the table with huge URLs.
const MaxLoggedPathBytes = 2048

// TruncateLogPath cuts a path to MaxLoggedPathBytes on a UTF-8 boundary.
func TruncateLogPath(p string) string {
	// Invalid UTF-8 would be rejected by PostgreSQL (and shown as garbage):
	// replace it before anything is stored, whatever the length.
	p = strings.ToValidUTF8(p, "\uFFFD")
	if len(p) <= MaxLoggedPathBytes {
		return p
	}
	p = p[:MaxLoggedPathBytes]
	for len(p) > 0 && !utf8.ValidString(p) {
		p = p[:len(p)-1] // drop a partial trailing rune
	}
	return p
}

func utcFormat(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

// LogACLDenied records an ACL denial, coalescing repeats: the first denial for
// the same client, phantom token, method and path within a UTC minute is
// stored, later identical denials in that minute are dropped (a retry loop
// would otherwise write one row per attempt). Portable plain SQL: one lookup
// bounded by the minute start, then an insert; a rare race may store two rows.
func (q *RequestLogQueries) LogACLDenied(clientID, placeholderID, serviceName, method, path string) error {
	path = TruncateLogPath(path)
	minuteStart := utcFormat(time.Now().Truncate(time.Minute))
	var existing int64
	var err error
	if placeholderID == "" {
		err = q.db.QueryRow(`SELECT id FROM request_log WHERE decision = ? AND client_id = ? AND placeholder_id IS NULL AND method = ? AND path = ? AND created_at >= ? LIMIT 1`,
			DecisionACLDenied, clientID, method, path, minuteStart).Scan(&existing)
	} else {
		err = q.db.QueryRow(`SELECT id FROM request_log WHERE decision = ? AND client_id = ? AND placeholder_id = ? AND method = ? AND path = ? AND created_at >= ? LIMIT 1`,
			DecisionACLDenied, clientID, placeholderID, method, path, minuteStart).Scan(&existing)
	}
	if err == nil {
		return nil // already recorded this minute
	}
	if err != sql.ErrNoRows {
		return err
	}
	// Per-client cap: a client probing many different paths cannot write more
	// than MaxACLDeniedRowsPerMinute denial rows in one UTC minute.
	var inMinute int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE decision = ? AND client_id = ? AND created_at >= ?`,
		DecisionACLDenied, clientID, minuteStart).Scan(&inMinute); err != nil {
		return err
	}
	if inMinute >= MaxACLDeniedRowsPerMinute {
		return nil
	}
	return q.LogDecision(clientID, placeholderID, serviceName, method, path, 403, DecisionACLDenied, "")
}

// LogHeldOnce records an approval hold. A newly created approval always gets
// its row (created=true); a retry of an already-pending approval is recorded
// only if no held row for that approval exists in the last 60 seconds.
func (q *RequestLogQueries) LogHeldOnce(clientID, placeholderID, serviceName, method, path, approvalID string, created bool) error {
	if !created && approvalID != "" {
		var existing int64
		err := q.db.QueryRow(`SELECT id FROM request_log WHERE decision = ? AND approval_id = ? AND created_at >= ? LIMIT 1`,
			DecisionHeld, approvalID, utcFormat(time.Now().Add(-60*time.Second))).Scan(&existing)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
	}
	return q.LogDecision(clientID, placeholderID, serviceName, method, path, 403, DecisionHeld, approvalID)
}

// PruneOlderThan deletes request_log rows (and their captured details) older
// than the given number of days, in batches so a large backlog never holds a
// long write lock. It returns the total rows removed. Cutoff is computed in Go
// (UTC string) and the SQL is portable.
func (q *RequestLogQueries) PruneOlderThan(days, batch, maxBatches int) (int64, error) {
	if days < 1 {
		return 0, nil
	}
	if batch < 1 {
		batch = 1000
	}
	cutoff := utcFormat(time.Now().AddDate(0, 0, -days))
	var total int64
	for i := 0; maxBatches <= 0 || i < maxBatches; i++ {
		if _, err := q.db.Exec(`DELETE FROM request_log_detail WHERE log_id IN (SELECT id FROM request_log WHERE created_at < ? ORDER BY id LIMIT ?)`, cutoff, batch); err != nil {
			return total, err
		}
		res, err := q.db.Exec(`DELETE FROM request_log WHERE id IN (SELECT id FROM request_log WHERE created_at < ? ORDER BY id LIMIT ?)`, cutoff, batch)
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

// LogWithReturn inserts a row and returns its auto-incremented id, so callers
// that capture details can attach them to the same log entry.
func (q *RequestLogQueries) LogWithReturn(clientID, placeholderID, serviceName, method, path string, statusCode int) (int64, error) {
	return q.LogWithReturnDecision(clientID, placeholderID, serviceName, method, path, statusCode, DecisionAllowed)
}

// LogWithReturnDecision is LogWithReturn with an explicit decision.
func (q *RequestLogQueries) LogWithReturnDecision(clientID, placeholderID, serviceName, method, path string, statusCode int, decision string) (int64, error) {
	path = TruncateLogPath(path)
	var phID interface{}
	if placeholderID != "" {
		phID = placeholderID
	}
	var id int64
	err := q.db.QueryRow(
		"INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code, decision) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id",
		clientID, phID, serviceName, method, path, statusCode, decision,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// RequestLogDetail holds the per-request payload captured when the admin has
// toggled detail recording on. Bodies may be truncated.
type RequestLogDetail struct {
	LogID           int64  `json:"log_id"`
	RequestHeaders  string `json:"request_headers"`
	RequestBody     string `json:"request_body"`
	RequestSize     int64  `json:"request_size"`
	ResponseHeaders string `json:"response_headers"`
	ResponseBody    string `json:"response_body"`
	ResponseSize    int64  `json:"response_size"`
	DurationMs      int64  `json:"duration_ms"`
	Truncated       bool   `json:"truncated"`
}

func (q *RequestLogQueries) StoreDetail(d *RequestLogDetail) error {
	tr := 0
	if d.Truncated {
		tr = 1
	}
	_, err := q.db.Exec(
		`INSERT INTO request_log_detail
		 (log_id, request_headers, request_body, request_size,
		  response_headers, response_body, response_size, duration_ms, truncated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(log_id) DO UPDATE SET
		 request_headers=excluded.request_headers, request_body=excluded.request_body,
		 request_size=excluded.request_size, response_headers=excluded.response_headers,
		 response_body=excluded.response_body, response_size=excluded.response_size,
		 duration_ms=excluded.duration_ms, truncated=excluded.truncated`,
		d.LogID, d.RequestHeaders, d.RequestBody, d.RequestSize,
		d.ResponseHeaders, d.ResponseBody, d.ResponseSize, d.DurationMs, tr,
	)
	return err
}

func (q *RequestLogQueries) GetDetail(logID int64) (*RequestLogDetail, error) {
	var d RequestLogDetail
	var tr int
	err := q.db.QueryRow(
		`SELECT log_id, request_headers, request_body, request_size,
		        response_headers, response_body, response_size, duration_ms, truncated
		 FROM request_log_detail WHERE log_id = ?`, logID,
	).Scan(&d.LogID, &d.RequestHeaders, &d.RequestBody, &d.RequestSize,
		&d.ResponseHeaders, &d.ResponseBody, &d.ResponseSize, &d.DurationMs, &tr)
	if err != nil {
		return nil, err
	}
	d.Truncated = tr == 1
	return &d, nil
}

// DropAllDetails clears the detail table. Called when the admin disables
// detail capture — the previously stored bodies are sensitive, so we don't
// keep them around.
func (q *RequestLogQueries) DropAllDetails() error {
	_, err := q.db.Exec("DELETE FROM request_log_detail")
	return err
}

// SetCaptureDisabledAndDrop flips the capture toggle to "0" AND drops all
// existing detail rows in a single transaction. This closes the race where
// a request that read the old toggle (= ON) at start-of-handler could write
// a detail row AFTER a separate DELETE has already cleaned up. SQLite
// serialises writers, so any in-flight INSERT has either already committed
// (its row is wiped by our DELETE) or hasn't started (it'll see the new
// toggle when it does its own re-check before writing).
//
// Pass clientsJSON to also persist the client filter in the same TX.
// If clientsJSON is empty string, the existing filter is left as-is.
func (q *RequestLogQueries) SetCaptureDisabledAndDrop(clientsJSON string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	upsert := `INSERT INTO settings (key, value) VALUES (?, ?)
	           ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := tx.Exec(upsert, "request_log_capture_enabled", "0"); err != nil {
		return err
	}
	if clientsJSON != "" {
		if _, err := tx.Exec(upsert, "request_log_capture_clients", clientsJSON); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM request_log_detail"); err != nil {
		return err
	}
	return tx.Commit()
}

// SetCaptureEnabled flips the toggle to "1". If clientsJSON is non-empty it
// also updates the client filter; otherwise the existing filter is preserved.
func (q *RequestLogQueries) SetCaptureEnabled(clientsJSON string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	upsert := `INSERT INTO settings (key, value) VALUES (?, ?)
	           ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := tx.Exec(upsert, "request_log_capture_enabled", "1"); err != nil {
		return err
	}
	if clientsJSON != "" {
		if _, err := tx.Exec(upsert, "request_log_capture_clients", clientsJSON); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type RequestLogEntry struct {
	ID            int64   `json:"id"`
	ServiceName   string  `json:"service_name"`
	Method        string  `json:"method"`
	Path          string  `json:"path"`
	StatusCode    *int    `json:"status_code"`
	ClientID      *string `json:"client_id"`
	ClientName    string  `json:"client_name"`
	PlaceholderID *string `json:"placeholder_id"`
	EnvName       string  `json:"env_name"`
	// Placeholder is the actual phantom-token string (sk-ant-dw_..., ghp_dw_..., etc.)
	// surfaced here so the logs UI can show it next to the request and link
	// back to the phantom-token detail view.
	Placeholder string `json:"placeholder"`
	CreatedAt   string `json:"created_at"`
	// HasDetail reports whether captured headers/bodies exist, so the UI can
	// skip the detail request (a 404) for rows logged with capture off.
	HasDetail bool `json:"has_detail"`
	// Credential is the name of the API key behind the phantom token ("" when
	// the token is bound to a key group or no longer exists).
	Credential string `json:"credential"`
	// RequiresApproval reports whether the phantom token that served the
	// request is approval-gated (the request log itself does not record
	// individual hold decisions).
	RequiresApproval bool `json:"requires_approval"`
	// DurationMs is only known when detail capture recorded this request.
	DurationMs *int64 `json:"duration_ms"`
	// Decision is the gateway decision: "" (legacy), allowed, approved_forward,
	// acl_denied or held. ApprovalID links held rows to their approval.
	Decision   string `json:"decision"`
	ApprovalID string `json:"approval_id"`
}

const requestLogSelect = `SELECT r.id, r.service_name, r.method, r.path, r.status_code,
		 r.client_id, COALESCE(c.name, ''),
		 r.placeholder_id, COALESCE(p.env_name, ''), COALESCE(p.placeholder, ''),
		 r.created_at,
		 EXISTS(SELECT 1 FROM request_log_detail d WHERE d.log_id = r.id),
		 COALESCE(k.name, ''), COALESCE(p.requires_approval, 0),
		 (SELECT d2.duration_ms FROM request_log_detail d2 WHERE d2.log_id = r.id),
		 r.decision, r.approval_id
		 FROM request_log r
		 LEFT JOIN clients c ON r.client_id = c.id
		 LEFT JOIN placeholder_keys p ON r.placeholder_id = p.id
		 LEFT JOIN api_keys k ON p.api_key_id = k.id`

func scanRequestLogEntries(rows *sql.Rows) ([]RequestLogEntry, error) {
	var result []RequestLogEntry
	for rows.Next() {
		var l RequestLogEntry
		var req int
		var dur sql.NullInt64
		if err := rows.Scan(&l.ID, &l.ServiceName, &l.Method, &l.Path, &l.StatusCode,
			&l.ClientID, &l.ClientName, &l.PlaceholderID, &l.EnvName, &l.Placeholder,
			&l.CreatedAt, &l.HasDetail, &l.Credential, &req, &dur, &l.Decision, &l.ApprovalID); err != nil {
			return nil, err
		}
		l.RequiresApproval = req != 0
		if dur.Valid {
			v := dur.Int64
			l.DurationMs = &v
		}
		result = append(result, l)
	}
	return result, rows.Err()
}

func (q *RequestLogQueries) Recent(limit int) ([]RequestLogEntry, error) {
	rows, err := q.db.Query(requestLogSelect+` ORDER BY r.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequestLogEntries(rows)
}

// RequestLogCountCap bounds the COUNT work for filtered listings.
const RequestLogCountCap = 10000

// RequestLogFilter selects rows for Search. Zero values mean "no filter".
// Since/Until are UTC "2006-01-02 15:04:05" strings (the stored format).
type RequestLogFilter struct {
	Since, Until  string
	Q             string
	ClientID      string
	Service       string
	Credential    string
	StatusClasses []string // subset of 2xx,3xx,4xx,5xx
	Held          bool     // phantom token requires approval
	ACLDenied     bool     // status 403 (includes upstream 403 and gateway denials)
	Decision      string   // exact request_log.decision match (allowed, approved_forward, acl_denied, held)
	BeforeID      int64
	AfterID       int64
	Limit         int
}

// RequestLogPage is one keyset page of Search results.
type RequestLogPage struct {
	Items        []RequestLogEntry
	NextBeforeID int64 // 0 when there are no older rows
	Total        int   // matching rows ignoring the cursor, capped
	TotalCapped  bool
	Offset       int // matching rows newer than the first item, capped
	HasNewer     bool
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// where builds the parameterized WHERE clause. cursor adds the id bounds.
func (f RequestLogFilter) where(cursor bool) (string, []interface{}) {
	var conds []string
	var args []interface{}
	if f.Since != "" {
		conds = append(conds, "r.created_at >= ?")
		args = append(args, f.Since)
	}
	if f.Until != "" {
		conds = append(conds, "r.created_at < ?")
		args = append(args, f.Until)
	}
	if f.Q != "" {
		like := "%" + escapeLike(strings.ToLower(f.Q)) + "%"
		conds = append(conds, `(LOWER(r.path) LIKE ? ESCAPE '\' OR LOWER(COALESCE(c.name, '')) LIKE ? ESCAPE '\' OR LOWER(r.service_name) LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if f.ClientID != "" {
		conds = append(conds, "r.client_id = ?")
		args = append(args, f.ClientID)
	}
	if f.Service != "" {
		conds = append(conds, "r.service_name = ?")
		args = append(args, f.Service)
	}
	if f.Credential != "" {
		conds = append(conds, "k.name = ?")
		args = append(args, f.Credential)
	}
	if len(f.StatusClasses) > 0 {
		var ors []string
		for _, c := range f.StatusClasses {
			if len(c) != 3 || c[1:] != "xx" || c[0] < '2' || c[0] > '5' {
				continue
			}
			lo := int(c[0]-'0') * 100
			ors = append(ors, "(r.status_code >= ? AND r.status_code < ?)")
			args = append(args, lo, lo+100)
		}
		if len(ors) > 0 {
			conds = append(conds, "("+strings.Join(ors, " OR ")+")")
		}
	}
	if f.Held {
		conds = append(conds, "COALESCE(p.requires_approval, 0) <> 0")
	}
	if f.ACLDenied {
		conds = append(conds, "r.status_code = 403")
	}
	if f.Decision != "" {
		conds = append(conds, "r.decision = ?")
		args = append(args, f.Decision)
	}
	if cursor {
		if f.BeforeID > 0 {
			conds = append(conds, "r.id < ?")
			args = append(args, f.BeforeID)
		}
		if f.AfterID > 0 {
			conds = append(conds, "r.id > ?")
			args = append(args, f.AfterID)
		}
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

const requestLogJoins = ` FROM request_log r
		 LEFT JOIN clients c ON r.client_id = c.id
		 LEFT JOIN placeholder_keys p ON r.placeholder_id = p.id
		 LEFT JOIN api_keys k ON p.api_key_id = k.id`

func (q *RequestLogQueries) countCapped(where string, args []interface{}) (int, bool, error) {
	var n int
	err := q.db.QueryRow(`SELECT COUNT(*) FROM (SELECT 1`+requestLogJoins+where+` LIMIT ?) t`,
		append(append([]interface{}{}, args...), RequestLogCountCap+1)...).Scan(&n)
	if err != nil {
		return 0, false, err
	}
	if n > RequestLogCountCap {
		return RequestLogCountCap, true, nil
	}
	return n, false, nil
}

// Search returns one keyset page (newest first) of rows matching f. Limit must
// already be validated (1..200). BeforeID pages toward older rows, AfterID
// toward newer rows; they are mutually exclusive.
func (q *RequestLogQueries) Search(f RequestLogFilter) (*RequestLogPage, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	cw, cargs := f.where(true)
	order := "DESC"
	if f.AfterID > 0 {
		order = "ASC"
	}
	rows, err := q.db.Query(requestLogSelect+cw+" ORDER BY r.id "+order+" LIMIT ?",
		append(append([]interface{}{}, cargs...), f.Limit+1)...)
	if err != nil {
		return nil, err
	}
	items, err := scanRequestLogEntries(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	hasOlder := false
	if f.AfterID > 0 {
		// Fetched oldest-first from the cursor; the extra row only says there
		// are even newer rows. Reverse into newest-first.
		if len(items) > f.Limit {
			items = items[:f.Limit]
		}
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
		if len(items) > 0 {
			g := f
			g.AfterID, g.BeforeID = 0, items[len(items)-1].ID
			w, a := g.where(true)
			var one int
			err := q.db.QueryRow(`SELECT 1`+requestLogJoins+w+` LIMIT 1`, a...).Scan(&one)
			if err == nil {
				hasOlder = true
			} else if err != sql.ErrNoRows {
				return nil, err
			}
		}
	} else if len(items) > f.Limit {
		items = items[:f.Limit]
		hasOlder = true
	}
	page := &RequestLogPage{Items: items}
	if hasOlder && len(items) > 0 {
		page.NextBeforeID = items[len(items)-1].ID
	}
	tw, targs := f.where(false)
	if page.Total, page.TotalCapped, err = q.countCapped(tw, targs); err != nil {
		return nil, err
	}
	if f.BeforeID > 0 || f.AfterID > 0 {
		if len(items) > 0 {
			g := f
			g.BeforeID, g.AfterID = 0, 0
			w, a := g.where(false)
			cond := "r.id > ?"
			if w == "" {
				w = " WHERE " + cond
			} else {
				w += " AND " + cond
			}
			n, _, err := q.countCapped(w, append(a, items[0].ID))
			if err != nil {
				return nil, err
			}
			page.Offset = n
			page.HasNewer = n > 0
		} else {
			// Past the newest end an after_id page is simply empty; only a
			// before_id page that ran off the end has newer rows to go back to.
			page.HasNewer = f.AfterID == 0
		}
	}
	return page, nil
}

// LogFacets lists the values offered by the request-log filter selects.
type LogFacetClient struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type LogFacets struct {
	Clients     []LogFacetClient `json:"clients"`
	Services    []string         `json:"services"`
	Credentials []string         `json:"credentials"`
	// Decisions counts rows per decision over the last 7 days.
	Decisions map[string]int `json:"decisions"`
}

func (q *RequestLogQueries) Facets() (*LogFacets, error) {
	out := &LogFacets{Clients: []LogFacetClient{}, Services: []string{}, Credentials: []string{}, Decisions: map[string]int{}}
	rows, err := q.db.Query(`SELECT id, name FROM clients WHERE id NOT IN (SELECT client_id FROM client_drafts) ORDER BY name LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c LogFacetClient
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			rows.Close()
			return nil, err
		}
		out.Clients = append(out.Clients, c)
	}
	rows.Close()
	for _, spec := range []struct {
		sql string
		dst *[]string
	}{
		{`SELECT DISTINCT name FROM services ORDER BY name LIMIT 1000`, &out.Services},
		{`SELECT DISTINCT name FROM api_keys WHERE key_encrypted <> '' ORDER BY name LIMIT 1000`, &out.Credentials},
	} {
		r, err := q.db.Query(spec.sql)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var n string
			if err := r.Scan(&n); err != nil {
				r.Close()
				return nil, err
			}
			*spec.dst = append(*spec.dst, n)
		}
		r.Close()
	}
	since := time.Now().Add(-7 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	dr, err := q.db.Query(`SELECT decision, COUNT(*) FROM request_log WHERE created_at >= ? AND decision <> '' GROUP BY decision`, since)
	if err != nil {
		return nil, err
	}
	defer dr.Close()
	for dr.Next() {
		var d string
		var n int
		if err := dr.Scan(&d, &n); err != nil {
			return nil, err
		}
		out.Decisions[d] = n
	}
	if err := dr.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (q *RequestLogQueries) Count() (int, error) {
	var count int
	err := q.db.QueryRow("SELECT COUNT(*) FROM request_log").Scan(&count)
	return count, err
}

// SessionUsageRow aggregates request activity for one client (= one
// agent "session" from the operator's point of view) against one
// service. request_log doesn't store token counts, so this is a
// request-volume view: total calls, error calls, and last activity.
type SessionUsageRow struct {
	ClientID    string `json:"client_id"`
	ClientName  string `json:"client_name"`
	ServiceName string `json:"service_name"`
	Requests    int64  `json:"requests"`
	Errors      int64  `json:"errors"`    // status_code >= 400
	LastSeen    string `json:"last_seen"` // max(created_at)
}

// SessionUsage returns per-client, per-service request aggregates over
// the trailing `sinceHours` hours (0 = all time). Ordered by request
// volume descending so the busiest agents surface first.
func (q *RequestLogQueries) SessionUsage(sinceHours int) ([]SessionUsageRow, error) {
	return q.sessionUsage(sinceHours, nil)
}

// SessionUsageWindow is SessionUsage over an explicit [Since, Until) window.
func (q *RequestLogQueries) SessionUsageWindow(w UsageWindow) ([]SessionUsageRow, error) {
	return q.sessionUsage(0, &w)
}

func (q *RequestLogQueries) sessionUsage(sinceHours int, w *UsageWindow) ([]SessionUsageRow, error) {
	where := ""
	cond, args := usageCutoffs("r.created_at", sinceHours, w)
	if cond != "" {
		where = "WHERE " + cond
	}
	query := `
		SELECT r.client_id,
		       COALESCE(c.name, ''),
		       r.service_name,
		       COUNT(*),
		       SUM(CASE WHEN r.status_code >= 400 THEN 1 ELSE 0 END),
		       MAX(r.created_at)
		FROM request_log r
		LEFT JOIN clients c ON r.client_id = c.id
		` + where + `
		GROUP BY r.client_id, r.service_name
		ORDER BY COUNT(*) DESC`
	rows, err := q.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionUsageRow
	for rows.Next() {
		var s SessionUsageRow
		var clientID, lastSeen *string
		var errs *int64
		if err := rows.Scan(&clientID, &s.ClientName, &s.ServiceName, &s.Requests, &errs, &lastSeen); err != nil {
			return nil, err
		}
		if clientID != nil {
			s.ClientID = *clientID
		}
		if errs != nil {
			s.Errors = *errs
		}
		if lastSeen != nil {
			s.LastSeen = *lastSeen
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// itoa is a tiny strconv.Itoa to avoid importing strconv just for the
// "-N hours" SQLite modifier string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
