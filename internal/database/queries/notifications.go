package queries

import "database/sql"

type NotificationChannel struct {
	ID          string `json:"id"`
	ChannelType string `json:"channel_type"` // telegram, discord, webhook
	Name        string `json:"name"`
	Config      string `json:"config"` // JSON config
	IsActive    bool   `json:"is_active"`
	CreatedAt   string `json:"created_at"`
}

type NotificationQueries struct {
	db *sql.DB
}

func NewNotificationQueries(db *sql.DB) *NotificationQueries {
	return &NotificationQueries{db: db}
}

func (q *NotificationQueries) List() ([]NotificationChannel, error) {
	rows, err := q.db.Query(
		"SELECT id, channel_type, name, config, is_active, created_at FROM notification_channels ORDER BY created_at",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []NotificationChannel
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.ChannelType, &c.Name, &c.Config, &c.IsActive, &c.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (q *NotificationQueries) ListActive() ([]NotificationChannel, error) {
	rows, err := q.db.Query(
		"SELECT id, channel_type, name, config, is_active, created_at FROM notification_channels WHERE is_active = 1",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []NotificationChannel
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.ChannelType, &c.Name, &c.Config, &c.IsActive, &c.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (q *NotificationQueries) Create(c *NotificationChannel) error {
	_, err := q.db.Exec(
		"INSERT INTO notification_channels (id, channel_type, name, config) VALUES (?, ?, ?, ?)",
		c.ID, c.ChannelType, c.Name, c.Config,
	)
	return err
}

func (q *NotificationQueries) Delete(id string) error {
	_, err := q.db.Exec("DELETE FROM notification_channels WHERE id = ?", id)
	return err
}

func (q *NotificationQueries) Update(id string, isActive bool) error {
	_, err := q.db.Exec("UPDATE notification_channels SET is_active = ? WHERE id = ?", isActive, id)
	return err
}
