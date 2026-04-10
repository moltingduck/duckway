package queries

import (
	"database/sql"

	"github.com/hackerduck/duckway/internal/models"
)

type AdminUserQueries struct {
	db *sql.DB
}

func NewAdminUserQueries(db *sql.DB) *AdminUserQueries {
	return &AdminUserQueries{db: db}
}

func (q *AdminUserQueries) GetByUsername(username string) (*models.AdminUser, error) {
	var u models.AdminUser
	err := q.db.QueryRow(
		"SELECT id, username, password_hash, created_at FROM admin_users WHERE username = ?",
		username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (q *AdminUserQueries) Create(id, username, passwordHash string) error {
	_, err := q.db.Exec(
		"INSERT INTO admin_users (id, username, password_hash) VALUES (?, ?, ?)",
		id, username, passwordHash,
	)
	return err
}

func (q *AdminUserQueries) Count() (int, error) {
	var count int
	err := q.db.QueryRow("SELECT COUNT(*) FROM admin_users").Scan(&count)
	return count, err
}
