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
