package queries

import (
	"database/sql"

	"github.com/hackerduck/duckway/internal/models"
)

type RequestLogQueries struct {
	db *sql.DB
}

func NewRequestLogQueries(db *sql.DB) *RequestLogQueries {
	return &RequestLogQueries{db: db}
}

func (q *RequestLogQueries) Log(clientID, serviceName, method, path string, statusCode int) error {
	_, err := q.db.Exec(
		"INSERT INTO request_log (client_id, service_name, method, path, status_code) VALUES (?, ?, ?, ?, ?)",
		clientID, serviceName, method, path, statusCode,
	)
	return err
}

func (q *RequestLogQueries) Recent(limit int) ([]models.RequestLog, error) {
	rows, err := q.db.Query(
		"SELECT id, placeholder_id, client_id, service_name, method, path, status_code, created_at FROM request_log ORDER BY created_at DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.RequestLog
	for rows.Next() {
		var l models.RequestLog
		if err := rows.Scan(&l.ID, &l.PlaceholderID, &l.ClientID, &l.ServiceName, &l.Method, &l.Path, &l.StatusCode, &l.CreatedAt); err != nil {
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
