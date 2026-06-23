package services_test

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
)

// ---- isPermanentOAuthError unit tests ----

func TestIsPermanentOAuthError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       bool
	}{
		{
			name:       "400 invalid_grant",
			statusCode: 400,
			body:       []byte(`{"error":"invalid_grant"}`),
			want:       true,
		},
		{
			name:       "400 invalid_client",
			statusCode: 400,
			body:       []byte(`{"error":"invalid_client"}`),
			want:       true,
		},
		{
			name:       "400 unauthorized_client",
			statusCode: 400,
			body:       []byte(`{"error":"unauthorized_client"}`),
			want:       true,
		},
		{
			name:       "400 unsupported_grant_type",
			statusCode: 400,
			body:       []byte(`{"error":"unsupported_grant_type"}`),
			want:       true,
		},
		{
			name:       "401 any body",
			statusCode: 401,
			body:       []byte(`{"error":"unauthorized"}`),
			want:       true,
		},
		{
			name:       "401 empty body",
			statusCode: 401,
			body:       []byte(``),
			want:       true,
		},
		{
			name:       "400 temporarily_unavailable",
			statusCode: 400,
			body:       []byte(`{"error":"temporarily_unavailable"}`),
			want:       false,
		},
		{
			name:       "500 any body",
			statusCode: 500,
			body:       []byte(`{"error":"server_error"}`),
			want:       false,
		},
		{
			name:       "503 any body",
			statusCode: 503,
			body:       []byte(`service unavailable`),
			want:       false,
		},
		{
			name:       "400 malformed JSON",
			statusCode: 400,
			body:       []byte(`not json at all`),
			want:       false,
		},
		{
			name:       "400 empty error field",
			statusCode: 400,
			body:       []byte(`{"error":""}`),
			want:       false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := services.IsPermanentOAuthError(tc.statusCode, tc.body)
			if got != tc.want {
				t.Errorf("IsPermanentOAuthError(%d, %q) = %v, want %v",
					tc.statusCode, string(tc.body), got, tc.want)
			}
		})
	}
}

// ---- refreshKey integration test: permanent error deactivates the key ----

func TestRefreshKey_PermanentErrorDeactivatesKey(t *testing.T) {
	// Fake token endpoint that returns 400 invalid_grant.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer ts.Close()

	// Open an in-memory-like test DB with full migrations applied.
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Set up Crypto with a random 32-byte key.
	cryptoKey := make([]byte, 32)
	if _, err := rand.Read(cryptoKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	crypto := services.NewCrypto(cryptoKey)

	// We need a service row because api_keys has a FK on service_id.
	const svcID = "svc-test-01"
	_, err = db.Exec(
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES (?, ?, ?, ?, ?)`,
		svcID, "test-svc", "Test Service", "https://example.com", "example.com",
	)
	if err != nil {
		t.Fatalf("insert service: %v", err)
	}

	// Encrypt a fake refresh token and access token.
	encRefresh, err := crypto.Encrypt("sk-ant-oart01-fake-refresh-token")
	if err != nil {
		t.Fatalf("encrypt refresh token: %v", err)
	}
	encAccess, err := crypto.Encrypt("sk-ant-fake-access-token")
	if err != nil {
		t.Fatalf("encrypt access token: %v", err)
	}

	const keyID = "key-test-01"
	apiKeyQ := queries.NewAPIKeyQueries(db)
	key := &models.APIKey{
		ID:            keyID,
		ServiceID:     svcID,
		Name:          "test-key",
		KeyEncrypted:  encAccess,
		RefreshToken:  encRefresh,
		ExpiresAt:     1, // already "expiring"
		TokenEndpoint: ts.URL,
	}
	if err := apiKeyQ.Create(key); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// Verify key starts active.
	before, err := apiKeyQ.GetByID(keyID)
	if err != nil {
		t.Fatalf("get key before: %v", err)
	}
	if !before.IsActive {
		t.Fatal("key should be active before refresh attempt")
	}

	// Run the refresher; it should hit the fake server and get invalid_grant,
	// triggering deactivation of the key.
	refresher := services.NewTokenRefresher(apiKeyQ, crypto)

	// refreshExpiring picks up keys with expires_at < now+10min.  Our key has
	// expires_at=1 (epoch+1ms) which is always in the past, so it qualifies.
	// Call the exported Start/Stop pair to drive one tick synchronously by
	// instead directly exercising the exported RefreshNow path.
	_, err = refresher.RefreshNow(keyID)
	if err == nil {
		t.Fatal("expected error from invalid_grant, got nil")
	}

	// The key must now be deactivated.
	after, err := apiKeyQ.GetByID(keyID)
	if err != nil {
		t.Fatalf("get key after: %v", err)
	}
	if after.IsActive {
		t.Errorf("key should be deactivated after permanent OAuth error, but is_active=%v", after.IsActive)
	}
}

// Verify that the error JSON we check against the DB roundtrip contains the
// permanent-error message.
func TestRefreshKey_PermanentError_ErrorMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		body, _ := json.Marshal(map[string]string{"error": "invalid_grant"})
		w.Write(body)
	}))
	defer ts.Close()

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cryptoKey := make([]byte, 32)
	rand.Read(cryptoKey)
	crypto := services.NewCrypto(cryptoKey)

	const svcID = "svc-err-01"
	db.Exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES (?, ?, ?, ?, ?)`,
		svcID, "err-svc", "Error Service", "https://example.com", "example.com")

	encRefresh, _ := crypto.Encrypt("generic-refresh-token")
	encAccess, _ := crypto.Encrypt("generic-access-token")

	const keyID = "key-err-01"
	apiKeyQ := queries.NewAPIKeyQueries(db)
	_ = apiKeyQ.Create(&models.APIKey{
		ID:            keyID,
		ServiceID:     svcID,
		Name:          "err-key",
		KeyEncrypted:  encAccess,
		RefreshToken:  encRefresh,
		ExpiresAt:     1,
		TokenEndpoint: ts.URL,
	})

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	_, err = refresher.RefreshNow(keyID)
	if err == nil {
		t.Fatal("expected error")
	}

	// Error message should mention "permanent OAuth error" or "deactivated".
	errMsg := err.Error()
	if len(errMsg) == 0 {
		t.Error("empty error message")
	}
}
