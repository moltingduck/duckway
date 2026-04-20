package queries

import "database/sql"

type CanarySettings struct {
	Email          string `json:"email"`
	EnabledTypes   string `json:"enabled_types"`   // JSON array: ["aws_keys","github",...]
	ExcludeClients string `json:"exclude_clients"` // JSON array of client names to skip
}

type CanaryToken struct {
	ID            string  `json:"id"`
	ClientID      string  `json:"client_id"`
	TokenType     string  `json:"token_type"`
	CanaryToken   string  `json:"canary_token"` // canarytokens.org token ID
	AuthToken     string  `json:"auth_token"`    // canarytokens.org auth token (needed for management)
	TokenValue    string  `json:"token_value"`   // the fake credential value
	SecretValue   *string `json:"secret_value"`  // secondary value (e.g., AWS secret key)
	Memo          string  `json:"memo"`
	DeployPath    string  `json:"deploy_path"`    // where client should place it
	DeployContent string  `json:"deploy_content"` // file content to write
	CreatedAt     string  `json:"created_at"`
}

type CanaryQueries struct {
	db *sql.DB
}

func NewCanaryQueries(db *sql.DB) *CanaryQueries {
	return &CanaryQueries{db: db}
}

func (q *CanaryQueries) GetSettings() (*CanarySettings, error) {
	var s CanarySettings
	err := q.db.QueryRow("SELECT email, enabled_types, exclude_clients FROM canary_settings WHERE id = 'default'").Scan(&s.Email, &s.EnabledTypes, &s.ExcludeClients)
	if err == sql.ErrNoRows {
		return &CanarySettings{EnabledTypes: "[]", ExcludeClients: "[]"}, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (q *CanaryQueries) SaveSettings(s *CanarySettings) error {
	if s.ExcludeClients == "" {
		s.ExcludeClients = "[]"
	}
	_, err := q.db.Exec(
		`INSERT INTO canary_settings (id, email, enabled_types, exclude_clients) VALUES ('default', ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET email = ?, enabled_types = ?, exclude_clients = ?`,
		s.Email, s.EnabledTypes, s.ExcludeClients, s.Email, s.EnabledTypes, s.ExcludeClients,
	)
	return err
}

func (q *CanaryQueries) ListByClient(clientID string) ([]CanaryToken, error) {
	rows, err := q.db.Query(
		"SELECT id, client_id, token_type, canary_token, auth_token, token_value, secret_value, memo, deploy_path, deploy_content, created_at FROM canary_tokens WHERE client_id = ?",
		clientID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CanaryToken
	for rows.Next() {
		var t CanaryToken
		if err := rows.Scan(&t.ID, &t.ClientID, &t.TokenType, &t.CanaryToken, &t.AuthToken, &t.TokenValue, &t.SecretValue, &t.Memo, &t.DeployPath, &t.DeployContent, &t.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

func (q *CanaryQueries) Create(t *CanaryToken) error {
	_, err := q.db.Exec(
		`INSERT INTO canary_tokens (id, client_id, token_type, canary_token, auth_token, token_value, secret_value, memo, deploy_path, deploy_content) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ClientID, t.TokenType, t.CanaryToken, t.AuthToken, t.TokenValue, t.SecretValue, t.Memo, t.DeployPath, t.DeployContent,
	)
	return err
}

func (q *CanaryQueries) GetByID(id string) (*CanaryToken, error) {
	var t CanaryToken
	err := q.db.QueryRow(
		"SELECT id, client_id, token_type, canary_token, auth_token, token_value, secret_value, memo, deploy_path, deploy_content, created_at FROM canary_tokens WHERE id = ?", id,
	).Scan(&t.ID, &t.ClientID, &t.TokenType, &t.CanaryToken, &t.AuthToken, &t.TokenValue, &t.SecretValue, &t.Memo, &t.DeployPath, &t.DeployContent, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (q *CanaryQueries) DeleteByID(id string) error {
	_, err := q.db.Exec("DELETE FROM canary_tokens WHERE id = ?", id)
	return err
}

func (q *CanaryQueries) DeleteByClient(clientID string) error {
	_, err := q.db.Exec("DELETE FROM canary_tokens WHERE client_id = ?", clientID)
	return err
}
