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
	res, err := q.db.Exec(
		"INSERT INTO request_log (client_id, placeholder_id, service_name, method, path, status_code) VALUES (?, ?, ?, ?, ?, ?)",
		clientID, phID, serviceName, method, path, statusCode,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
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
		`INSERT OR REPLACE INTO request_log_detail
		 (log_id, request_headers, request_body, request_size,
		  response_headers, response_body, response_size, duration_ms, truncated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
	CreatedAt     string  `json:"created_at"`
}

func (q *RequestLogQueries) Recent(limit int) ([]RequestLogEntry, error) {
	rows, err := q.db.Query(
		`SELECT r.id, r.service_name, r.method, r.path, r.status_code,
		 r.client_id, COALESCE(c.name, ''), r.placeholder_id, COALESCE(p.env_name, ''), r.created_at
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
			&l.ClientID, &l.ClientName, &l.PlaceholderID, &l.EnvName, &l.CreatedAt); err != nil {
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
