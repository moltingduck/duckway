package queries

import "database/sql"

type OAuthCredential struct {
	ID               string `json:"id"`
	ServiceID        string `json:"service_id"`
	Name             string `json:"name"`
	AccessToken      string `json:"-"`      // encrypted
	RefreshToken     string `json:"-"`      // encrypted
	ExpiresAt        int64  `json:"expires_at"` // Unix ms
	TokenEndpoint    string `json:"token_endpoint"`
	ClientIDOAuth    string `json:"client_id_oauth"`
	Scopes           string `json:"scopes"`
	SubscriptionType string `json:"subscription_type"`
	RateLimitTier    string `json:"rate_limit_tier"`
	IsActive         bool   `json:"is_active"`
	LastRefreshed    *string `json:"last_refreshed"`
	CreatedAt        string `json:"created_at"`
}

type OAuthQueries struct {
	db *sql.DB
}

func NewOAuthQueries(db *sql.DB) *OAuthQueries {
	return &OAuthQueries{db: db}
}

func (q *OAuthQueries) List() ([]OAuthCredential, error) {
	rows, err := q.db.Query(
		`SELECT id, service_id, name, access_token, refresh_token, expires_at,
		 token_endpoint, client_id_oauth, scopes, subscription_type, rate_limit_tier,
		 is_active, last_refreshed, created_at FROM oauth_credentials ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []OAuthCredential
	for rows.Next() {
		var c OAuthCredential
		if err := rows.Scan(&c.ID, &c.ServiceID, &c.Name, &c.AccessToken, &c.RefreshToken,
			&c.ExpiresAt, &c.TokenEndpoint, &c.ClientIDOAuth, &c.Scopes,
			&c.SubscriptionType, &c.RateLimitTier, &c.IsActive, &c.LastRefreshed, &c.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (q *OAuthQueries) GetByID(id string) (*OAuthCredential, error) {
	var c OAuthCredential
	err := q.db.QueryRow(
		`SELECT id, service_id, name, access_token, refresh_token, expires_at,
		 token_endpoint, client_id_oauth, scopes, subscription_type, rate_limit_tier,
		 is_active, last_refreshed, created_at FROM oauth_credentials WHERE id = ?`, id,
	).Scan(&c.ID, &c.ServiceID, &c.Name, &c.AccessToken, &c.RefreshToken,
		&c.ExpiresAt, &c.TokenEndpoint, &c.ClientIDOAuth, &c.Scopes,
		&c.SubscriptionType, &c.RateLimitTier, &c.IsActive, &c.LastRefreshed, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (q *OAuthQueries) GetByServiceID(serviceID string) (*OAuthCredential, error) {
	var c OAuthCredential
	err := q.db.QueryRow(
		`SELECT id, service_id, name, access_token, refresh_token, expires_at,
		 token_endpoint, client_id_oauth, scopes, subscription_type, rate_limit_tier,
		 is_active, last_refreshed, created_at FROM oauth_credentials WHERE service_id = ? AND is_active = 1`, serviceID,
	).Scan(&c.ID, &c.ServiceID, &c.Name, &c.AccessToken, &c.RefreshToken,
		&c.ExpiresAt, &c.TokenEndpoint, &c.ClientIDOAuth, &c.Scopes,
		&c.SubscriptionType, &c.RateLimitTier, &c.IsActive, &c.LastRefreshed, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (q *OAuthQueries) Create(c *OAuthCredential) error {
	_, err := q.db.Exec(
		`INSERT INTO oauth_credentials (id, service_id, name, access_token, refresh_token, expires_at,
		 token_endpoint, client_id_oauth, scopes, subscription_type, rate_limit_tier)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ServiceID, c.Name, c.AccessToken, c.RefreshToken, c.ExpiresAt,
		c.TokenEndpoint, c.ClientIDOAuth, c.Scopes, c.SubscriptionType, c.RateLimitTier,
	)
	return err
}

func (q *OAuthQueries) UpdateTokens(id, accessToken string, expiresAt int64) error {
	_, err := q.db.Exec(
		"UPDATE oauth_credentials SET access_token = ?, expires_at = ?, last_refreshed = datetime('now') WHERE id = ?",
		accessToken, expiresAt, id,
	)
	return err
}

func (q *OAuthQueries) UpdateRefreshToken(id, refreshToken string) error {
	_, err := q.db.Exec("UPDATE oauth_credentials SET refresh_token = ? WHERE id = ?", refreshToken, id)
	return err
}

func (q *OAuthQueries) Delete(id string) error {
	_, err := q.db.Exec("DELETE FROM oauth_credentials WHERE id = ?", id)
	return err
}

// ListExpiring returns OAuth credentials expiring within the given minutes.
func (q *OAuthQueries) ListExpiring(withinMinutes int) ([]OAuthCredential, error) {
	// expires_at is Unix ms, compare with current time + margin
	rows, err := q.db.Query(
		`SELECT id, service_id, name, access_token, refresh_token, expires_at,
		 token_endpoint, client_id_oauth, scopes, subscription_type, rate_limit_tier,
		 is_active, last_refreshed, created_at
		 FROM oauth_credentials
		 WHERE is_active = 1 AND expires_at > 0
		 AND expires_at < (strftime('%s','now') * 1000 + ? * 60 * 1000)`,
		withinMinutes,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []OAuthCredential
	for rows.Next() {
		var c OAuthCredential
		if err := rows.Scan(&c.ID, &c.ServiceID, &c.Name, &c.AccessToken, &c.RefreshToken,
			&c.ExpiresAt, &c.TokenEndpoint, &c.ClientIDOAuth, &c.Scopes,
			&c.SubscriptionType, &c.RateLimitTier, &c.IsActive, &c.LastRefreshed, &c.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
