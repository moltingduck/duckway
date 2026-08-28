package queries

import "database/sql"

type RequestLogQueries struct {
	db *sql.DB
}

func NewRequestLogQueries(db *sql.DB) *RequestLogQueries {
	return &RequestLogQueries{db: db}
}

func (q *RequestLogQueries) Log(clientID, placeholderID, serviceName, method, path string, statusCode int) error {
	var phID interface{}
	if placeholderID != "" {
		phID = placeholderID
	}
	_, err := q.db.Exec(
		"INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code) VALUES (?, ?, ?, ?, ?, ?)",
		clientID, phID, serviceName, method, path, statusCode,
	)
	return err
}

// LogWithReturn inserts a row and returns its auto-incremented id, so callers
// that capture details can attach them to the same log entry.
func (q *RequestLogQueries) LogWithReturn(clientID, placeholderID, serviceName, method, path string, statusCode int) (int64, error) {
	var phID interface{}
	if placeholderID != "" {
		phID = placeholderID
	}
	var id int64
	err := q.db.QueryRow(
		"INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code) VALUES (?, ?, ?, ?, ?, ?) RETURNING id",
		clientID, phID, serviceName, method, path, statusCode,
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
}

func (q *RequestLogQueries) Recent(limit int) ([]RequestLogEntry, error) {
	rows, err := q.db.Query(
		`SELECT r.id, r.service_name, r.method, r.path, r.status_code,
		 r.client_id, COALESCE(c.name, ''),
		 r.placeholder_id, COALESCE(p.env_name, ''), COALESCE(p.placeholder, ''),
		 r.created_at
		 FROM request_log r
		 LEFT JOIN clients c ON r.client_id = c.id
		 LEFT JOIN placeholder_keys p ON r.placeholder_id = p.id
		 ORDER BY r.created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RequestLogEntry
	for rows.Next() {
		var l RequestLogEntry
		if err := rows.Scan(&l.ID, &l.ServiceName, &l.Method, &l.Path, &l.StatusCode,
			&l.ClientID, &l.ClientName, &l.PlaceholderID, &l.EnvName, &l.Placeholder,
			&l.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, l)
	}
	return result, rows.Err()
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
	where := ""
	var args []interface{}
	if sinceHours > 0 {
		where = "WHERE r.created_at >= datetime('now', ?)"
		args = append(args, "-"+itoa(sinceHours)+" hours")
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
