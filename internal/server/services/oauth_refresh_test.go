package services_test

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
)

const officialCodexRefreshEndpoint = "https://auth.openai.com/oauth/token"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

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
			name:       "400 openai nested invalid_refresh_token",
			statusCode: 400,
			body:       []byte(`{"error":{"message":"Invalid refresh token.","type":"invalid_request_error","param":null,"code":"invalid_refresh_token"}}`),
			want:       true,
		},
		{
			name:       "400 openai nested invalid refresh token message",
			statusCode: 400,
			body:       []byte(`{"error":{"message":"Invalid refresh token.","type":"invalid_request_error"}}`),
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

// ---- refreshKey integration tests: permanent refresh errors only deactivate
// when the current access token also fails a provider auth check. ----

func TestRefreshNowPermanentErrorPreservesActiveWhenCurrentAccessTokenWorks(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cryptoKey := make([]byte, 32)
	if _, err := rand.Read(cryptoKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	crypto := services.NewCrypto(cryptoKey)
	apiKeyQ := queries.NewAPIKeyQueries(db)

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatalf("get openai service: %v", err)
	}

	encRefresh, err := crypto.Encrypt("rt.invalid-refresh")
	if err != nil {
		t.Fatalf("encrypt refresh token: %v", err)
	}
	encAccess, err := crypto.Encrypt("working-access-token")
	if err != nil {
		t.Fatalf("encrypt access token: %v", err)
	}

	const keyID = "key-refresh-fails-access-works"
	key := &models.APIKey{
		ID:               keyID,
		ServiceID:        openaiSvc.ID,
		Name:             "codex-auth",
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        1,
		TokenEndpoint:    officialCodexRefreshEndpoint,
		SubscriptionInfo: `{"credential_kind":"codex_oauth","client_id":"app_codex_test"}`,
	}
	if err := apiKeyQ.Create(key); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	services.SetTokenRefresherHTTPClientForTest(refresher, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case officialCodexRefreshEndpoint:
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"refresh_token_invalidated","message":"invalidated"}}`)),
				Request:    r,
			}, nil
		case "https://api.openai.com/v1/models":
			if got := r.Header.Get("Authorization"); got != "Bearer working-access-token" {
				t.Fatalf("Authorization = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
				Request:    r,
			}, nil
		default:
			t.Fatalf("unexpected request URL: %s", r.URL.String())
			return nil, nil
		}
	})})

	_, err = refresher.RefreshNow(keyID)
	if err == nil {
		t.Fatal("expected refresh error, got nil")
	}
	if !strings.Contains(err.Error(), "access token test still succeeds") {
		t.Fatalf("unexpected error: %v", err)
	}

	after, err := apiKeyQ.GetByID(keyID)
	if err != nil {
		t.Fatalf("get key after: %v", err)
	}
	if !after.IsActive {
		t.Errorf("key should remain active when access token still works, but is_active=%v", after.IsActive)
	}
}

func TestRefreshKey_PermanentErrorDeactivatesKeyOnlyWhenAccessTokenTestFails(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cryptoKey := make([]byte, 32)
	if _, err := rand.Read(cryptoKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	crypto := services.NewCrypto(cryptoKey)
	apiKeyQ := queries.NewAPIKeyQueries(db)

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatalf("get openai service: %v", err)
	}

	encRefresh, err := crypto.Encrypt("rt.invalid-refresh")
	if err != nil {
		t.Fatalf("encrypt refresh token: %v", err)
	}
	encAccess, err := crypto.Encrypt("bad-access-token")
	if err != nil {
		t.Fatalf("encrypt access token: %v", err)
	}

	const keyID = "key-refresh-fails-access-fails"
	key := &models.APIKey{
		ID:               keyID,
		ServiceID:        openaiSvc.ID,
		Name:             "codex-auth",
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        1,
		TokenEndpoint:    officialCodexRefreshEndpoint,
		SubscriptionInfo: `{"credential_kind":"codex_oauth","client_id":"app_codex_test"}`,
	}
	if err := apiKeyQ.Create(key); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	services.SetTokenRefresherHTTPClientForTest(refresher, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case officialCodexRefreshEndpoint:
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"refresh_token_invalidated","message":"invalidated"}}`)),
				Request:    r,
			}, nil
		case "https://api.openai.com/v1/models":
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_api_key"}`)),
				Request:    r,
			}, nil
		default:
			t.Fatalf("unexpected request URL: %s", r.URL.String())
			return nil, nil
		}
	})})

	_, err = refresher.RefreshNow(keyID)
	if err == nil {
		t.Fatal("expected refresh error, got nil")
	}
	if !strings.Contains(err.Error(), "deactivated after access token test failed") {
		t.Fatalf("unexpected error: %v", err)
	}

	after, err := apiKeyQ.GetByID(keyID)
	if err != nil {
		t.Fatalf("get key after: %v", err)
	}
	if after.IsActive {
		t.Errorf("key should be deactivated after refresh and access token test both fail, but is_active=%v", after.IsActive)
	}
}

func TestRefreshNowPermanentErrorDoesNotDeactivateWhenCredentialChangesBeforeAccessTestFailure(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cryptoKey := make([]byte, 32)
	if _, err := rand.Read(cryptoKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	crypto := services.NewCrypto(cryptoKey)
	apiKeyQ := queries.NewAPIKeyQueries(db)

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatalf("get openai service: %v", err)
	}

	encRefresh, err := crypto.Encrypt("rt.invalid-refresh")
	if err != nil {
		t.Fatalf("encrypt refresh token: %v", err)
	}
	encAccess, err := crypto.Encrypt("bad-access-token")
	if err != nil {
		t.Fatalf("encrypt access token: %v", err)
	}

	const keyID = "key-refresh-fails-credential-changes"
	if err := apiKeyQ.Create(&models.APIKey{
		ID:               keyID,
		ServiceID:        openaiSvc.ID,
		Name:             "codex-auth",
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        1,
		TokenEndpoint:    officialCodexRefreshEndpoint,
		SubscriptionInfo: `{"credential_kind":"codex_oauth","client_id":"app_codex_test"}`,
	}); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	services.SetTokenRefresherHTTPClientForTest(refresher, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case officialCodexRefreshEndpoint:
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"refresh_token_invalidated","message":"invalidated"}}`)),
				Request:    r,
			}, nil
		case "https://api.openai.com/v1/models":
			repairedAccess, err := crypto.Encrypt("repaired-access-token")
			if err != nil {
				t.Fatalf("encrypt repaired access: %v", err)
			}
			if err := apiKeyQ.UpdateTokens(keyID, repairedAccess, 1783222051000); err != nil {
				t.Fatalf("store repaired access: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_api_key"}`)),
				Request:    r,
			}, nil
		default:
			t.Fatalf("unexpected request URL: %s", r.URL.String())
			return nil, nil
		}
	})})

	_, err = refresher.RefreshNow(keyID)
	if err == nil {
		t.Fatal("expected refresh error, got nil")
	}
	if !strings.Contains(err.Error(), "credentials changed before deactivate") {
		t.Fatalf("unexpected error: %v", err)
	}

	after, err := apiKeyQ.GetByID(keyID)
	if err != nil {
		t.Fatalf("get key after: %v", err)
	}
	if !after.IsActive {
		t.Fatalf("key should remain active after concurrent credential repair: %+v", after)
	}
}

func TestRefreshNowPermanentErrorAccessTokenWorksPreservesInactiveState(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cryptoKey := make([]byte, 32)
	if _, err := rand.Read(cryptoKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	crypto := services.NewCrypto(cryptoKey)
	apiKeyQ := queries.NewAPIKeyQueries(db)

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatalf("get openai service: %v", err)
	}

	encRefresh, err := crypto.Encrypt("rt.invalid-refresh")
	if err != nil {
		t.Fatalf("encrypt refresh token: %v", err)
	}
	encAccess, err := crypto.Encrypt("working-access-token")
	if err != nil {
		t.Fatalf("encrypt access token: %v", err)
	}

	const keyID = "key-refresh-fails-preserve-inactive"
	if err := apiKeyQ.Create(&models.APIKey{
		ID:               keyID,
		ServiceID:        openaiSvc.ID,
		Name:             "codex-auth",
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        1,
		TokenEndpoint:    officialCodexRefreshEndpoint,
		SubscriptionInfo: `{"credential_kind":"codex_oauth","client_id":"app_codex_test"}`,
	}); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	if err := apiKeyQ.Deactivate(keyID); err != nil {
		t.Fatalf("deactivate key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	services.SetTokenRefresherHTTPClientForTest(refresher, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case officialCodexRefreshEndpoint:
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"refresh_token_invalidated","message":"invalidated"}}`)),
				Request:    r,
			}, nil
		case "https://api.openai.com/v1/models":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
				Request:    r,
			}, nil
		default:
			t.Fatalf("unexpected request URL: %s", r.URL.String())
			return nil, nil
		}
	})})

	_, err = refresher.RefreshNow(keyID)
	if err == nil {
		t.Fatal("expected refresh error, got nil")
	}

	after, err := apiKeyQ.GetByID(keyID)
	if err != nil {
		t.Fatalf("get key after: %v", err)
	}
	if after.IsActive {
		t.Fatalf("key should preserve inactive state when refresh fails but access token works: %+v", after)
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

func TestRefreshKey_ErrorRedactsEchoedTokens(t *testing.T) {
	const refreshToken = "rt.secret-refresh-token"
	const echoedJWT = "header.payload.signature"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"code":"invalid_grant","message":"bad %s %s"}}`, refreshToken, echoedJWT)
	}))
	defer ts.Close()

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	const svcID = "svc-redact-refresh-error"
	if _, err := db.Exec(`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES (?, ?, ?, ?, ?)`,
		svcID, "redact-refresh-error", "Redact Refresh Error", "https://example.com", "example.com"); err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt(refreshToken)
	if err != nil {
		t.Fatal(err)
	}
	encAccess, err := crypto.Encrypt("old-access-token")
	if err != nil {
		t.Fatal(err)
	}
	apiKeyQ := queries.NewAPIKeyQueries(db)
	const keyID = "key-redact-refresh-error"
	if err := apiKeyQ.Create(&models.APIKey{
		ID:            keyID,
		ServiceID:     svcID,
		Name:          "redact-key",
		KeyEncrypted:  encAccess,
		RefreshToken:  encRefresh,
		ExpiresAt:     1,
		TokenEndpoint: ts.URL,
	}); err != nil {
		t.Fatal(err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	_, err = refresher.RefreshNow(keyID)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), refreshToken) || strings.Contains(err.Error(), echoedJWT) {
		t.Fatalf("error leaked token: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "[REDACTED_REFRESH_TOKEN]") || !strings.Contains(err.Error(), "[REDACTED_JWT]") {
		t.Fatalf("error did not include redaction markers: %s", err.Error())
	}
}

func TestJWTExpiresAtMillis(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1783222051}`))
	token := "header." + payload + ".sig"
	if got, want := services.JWTExpiresAtMillis(token), int64(1783222051000); got != want {
		t.Fatalf("JWTExpiresAtMillis = %d, want %d", got, want)
	}
	if got := services.JWTExpiresAtMillis("not-a-jwt"); got != 0 {
		t.Fatalf("invalid token expiry = %d, want 0", got)
	}
}

func TestRefreshKey_GenericOAuthSendsClientIDAndUsesJWTExpiry(t *testing.T) {
	var formSeen url.Values
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1783222051}`))
	nextAccess := "header." + payload + ".sig"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		formSeen = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q}`, nextAccess)
	}))
	defer ts.Close()

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cryptoKey := make([]byte, 32)
	if _, err := rand.Read(cryptoKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	crypto := services.NewCrypto(cryptoKey)

	const svcID = "svc-openai-refresh"
	if _, err := db.Exec(
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES (?, ?, ?, ?, ?)`,
		svcID, "openai-refresh-test", "OpenAI Refresh Test", "https://api.openai.com", "api.openai.com",
	); err != nil {
		t.Fatalf("insert service: %v", err)
	}

	encRefresh, err := crypto.Encrypt("rt.fake-refresh")
	if err != nil {
		t.Fatalf("encrypt refresh: %v", err)
	}
	encAccess, err := crypto.Encrypt("old-access")
	if err != nil {
		t.Fatalf("encrypt access: %v", err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	const keyID = "key-openai-refresh"
	if err := apiKeyQ.Create(&models.APIKey{
		ID:               keyID,
		ServiceID:        svcID,
		Name:             "codex-auth",
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        1,
		TokenEndpoint:    ts.URL,
		SubscriptionInfo: `{"client_id":"app_codex_test"}`,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	expiresAt, err := refresher.RefreshNow(keyID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got, want := formSeen.Get("client_id"), "app_codex_test"; got != want {
		t.Fatalf("client_id form value = %q, want %q", got, want)
	}
	if got, want := formSeen.Get("grant_type"), "refresh_token"; got != want {
		t.Fatalf("grant_type = %q, want %q", got, want)
	}
	if got, want := expiresAt, int64(1783222051000); got != want {
		t.Fatalf("expiresAt = %d, want %d", got, want)
	}
}

func TestRefreshKey_CodexOAuthSendsJSONRefreshRequest(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1783222051}`))
	nextAccess := "header." + payload + ".sig"
	nextID := "id." + payload + ".sig"
	var contentType string
	var bodySeen map[string]string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != officialCodexRefreshEndpoint {
			t.Fatalf("upstream URL = %s", r.URL.String())
		}
		contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&bodySeen); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"access_token":%q,"id_token":%q}`, nextAccess, nextID))),
			Request:    r,
		}, nil
	})

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cryptoKey := make([]byte, 32)
	if _, err := rand.Read(cryptoKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	crypto := services.NewCrypto(cryptoKey)

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatalf("get openai service: %v", err)
	}

	encRefresh, err := crypto.Encrypt("rt.fake-refresh")
	if err != nil {
		t.Fatalf("encrypt refresh: %v", err)
	}
	encAccess, err := crypto.Encrypt("old-access")
	if err != nil {
		t.Fatalf("encrypt access: %v", err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	const keyID = "key-codex-refresh-json"
	if err := apiKeyQ.Create(&models.APIKey{
		ID:               keyID,
		ServiceID:        openaiSvc.ID,
		Name:             "codex-auth",
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        1,
		TokenEndpoint:    officialCodexRefreshEndpoint,
		SubscriptionInfo: `{"credential_kind":"codex_oauth","client_id":"app_codex_test"}`,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	services.SetTokenRefresherHTTPClientForTest(refresher, &http.Client{Transport: transport})
	if _, err := refresher.RefreshNow(keyID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !strings.Contains(contentType, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	if got, want := bodySeen["client_id"], "app_codex_test"; got != want {
		t.Fatalf("client_id = %q, want %q; body=%#v", got, want, bodySeen)
	}
	if got, want := bodySeen["grant_type"], "refresh_token"; got != want {
		t.Fatalf("grant_type = %q, want %q", got, want)
	}
	if got, want := bodySeen["refresh_token"], "rt.fake-refresh"; got != want {
		t.Fatalf("refresh_token = %q, want %q", got, want)
	}
	key, err := apiKeyQ.GetByID(keyID)
	if err != nil {
		t.Fatal(err)
	}
	var subInfo map[string]interface{}
	if err := json.Unmarshal([]byte(key.SubscriptionInfo), &subInfo); err != nil {
		t.Fatal(err)
	}
	if subInfo["id_token"] != nextID {
		t.Fatalf("id_token metadata = %#v, want %q", subInfo["id_token"], nextID)
	}
	if subInfo["last_refresh"] == "" {
		t.Fatalf("last_refresh metadata missing: %#v", subInfo)
	}
}

func TestRefreshNowSerializesConcurrentRefreshesForSameKey(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1783222051}`))
	var inFlight int32
	var maxInFlight int32
	var mu sync.Mutex
	var seenRefreshTokens []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != officialCodexRefreshEndpoint {
			t.Fatalf("upstream URL = %s", r.URL.String())
		}
		current := atomic.AddInt32(&inFlight, 1)
		for {
			max := atomic.LoadInt32(&maxInFlight)
			if current <= max || atomic.CompareAndSwapInt32(&maxInFlight, max, current) {
				break
			}
		}
		defer atomic.AddInt32(&inFlight, -1)

		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		refreshToken := body["refresh_token"]
		mu.Lock()
		seenRefreshTokens = append(seenRefreshTokens, refreshToken)
		call := len(seenRefreshTokens)
		mu.Unlock()

		nextRefresh := "rt.second"
		if call == 2 {
			nextRefresh = "rt.third"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"access_token":%q,"refresh_token":%q}`, "header."+payload+".sig", nextRefresh))),
			Request:    r,
		}, nil
	})

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	openaiSvc, err := queries.NewServiceQueries(db).GetByName("openai")
	if err != nil {
		t.Fatalf("get openai service: %v", err)
	}
	encAccess, err := crypto.Encrypt("old-access")
	if err != nil {
		t.Fatalf("encrypt access: %v", err)
	}
	encRefresh, err := crypto.Encrypt("rt.first")
	if err != nil {
		t.Fatalf("encrypt refresh: %v", err)
	}
	apiKeyQ := queries.NewAPIKeyQueries(db)
	const keyID = "key-concurrent-refresh"
	if err := apiKeyQ.Create(&models.APIKey{
		ID:               keyID,
		ServiceID:        openaiSvc.ID,
		Name:             "codex-auth",
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        1,
		TokenEndpoint:    officialCodexRefreshEndpoint,
		SubscriptionInfo: `{"credential_kind":"codex_oauth","client_id":"app_codex_test"}`,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	services.SetTokenRefresherHTTPClientForTest(refresher, &http.Client{Transport: transport})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := refresher.RefreshNow(keyID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("refresh error: %v", err)
		}
	}
	if atomic.LoadInt32(&maxInFlight) > 1 {
		t.Fatalf("refreshes were not serialized; max in-flight = %d", maxInFlight)
	}
	if len(seenRefreshTokens) != 2 {
		t.Fatalf("seen refresh tokens = %#v, want 2 calls", seenRefreshTokens)
	}
	if seenRefreshTokens[0] != "rt.first" || seenRefreshTokens[1] != "rt.second" {
		t.Fatalf("refresh tokens sent upstream = %#v, want [rt.first rt.second]", seenRefreshTokens)
	}
}

func TestRefreshNowRejectsCodexOAuthSpoofedEndpoint(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	openaiSvc, err := queries.NewServiceQueries(db).GetByName("openai")
	if err != nil {
		t.Fatalf("get openai service: %v", err)
	}
	encAccess, err := crypto.Encrypt("old-access")
	if err != nil {
		t.Fatal(err)
	}
	encRefresh, err := crypto.Encrypt("rt.first")
	if err != nil {
		t.Fatal(err)
	}
	apiKeyQ := queries.NewAPIKeyQueries(db)
	const keyID = "key-codex-spoofed-endpoint"
	if err := apiKeyQ.Create(&models.APIKey{
		ID:               keyID,
		ServiceID:        openaiSvc.ID,
		Name:             "codex-auth",
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        1,
		TokenEndpoint:    "https://auth.openai.com.evil.test/oauth/token",
		SubscriptionInfo: `{"credential_kind":"codex_oauth","client_id":"app_codex_test"}`,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	services.SetTokenRefresherHTTPClientForTest(refresher, &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected upstream request to %s", req.URL.String())
		return nil, nil
	})})
	_, err = refresher.RefreshNow(keyID)
	if err == nil {
		t.Fatal("expected spoofed endpoint error")
	}
	if !strings.Contains(err.Error(), "codex oauth token_endpoint must be https://auth.openai.com/oauth/token") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRefreshNowSuccessfulRefreshReactivatesInactiveKey(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1783222051}`))
	nextAccess := "header." + payload + ".sig"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q}`, nextAccess)
	}))
	defer ts.Close()

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cryptoKey := make([]byte, 32)
	if _, err := rand.Read(cryptoKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	crypto := services.NewCrypto(cryptoKey)

	const svcID = "svc-reactivate-refresh"
	if _, err := db.Exec(
		`INSERT INTO services (id, name, display_name, upstream_url, host_pattern) VALUES (?, ?, ?, ?, ?)`,
		svcID, "reactivate-refresh-test", "Reactivate Refresh Test", "https://api.openai.com", "api.openai.com",
	); err != nil {
		t.Fatalf("insert service: %v", err)
	}

	encRefresh, err := crypto.Encrypt("rt.fake-refresh")
	if err != nil {
		t.Fatalf("encrypt refresh: %v", err)
	}
	encAccess, err := crypto.Encrypt("old-access")
	if err != nil {
		t.Fatalf("encrypt access: %v", err)
	}

	apiKeyQ := queries.NewAPIKeyQueries(db)
	const keyID = "key-reactivate-refresh"
	if err := apiKeyQ.Create(&models.APIKey{
		ID:            keyID,
		ServiceID:     svcID,
		Name:          "inactive oauth",
		KeyEncrypted:  encAccess,
		RefreshToken:  encRefresh,
		ExpiresAt:     1,
		TokenEndpoint: ts.URL,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := apiKeyQ.Deactivate(keyID); err != nil {
		t.Fatalf("deactivate key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	if _, err := refresher.RefreshNow(keyID); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	key, err := apiKeyQ.GetByID(keyID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if !key.IsActive {
		t.Fatalf("key should be active after successful manual refresh: %+v", key)
	}
}

func TestRefreshNowPermanentErrorDoesNotDeactivateWhenRefreshTokenAlreadyRotated(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cryptoKey := make([]byte, 32)
	if _, err := rand.Read(cryptoKey); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	crypto := services.NewCrypto(cryptoKey)
	apiKeyQ := queries.NewAPIKeyQueries(db)

	svcQ := queries.NewServiceQueries(db)
	openaiSvc, err := svcQ.GetByName("openai")
	if err != nil {
		t.Fatalf("get openai service: %v", err)
	}
	encRefresh, err := crypto.Encrypt("rt.old-refresh")
	if err != nil {
		t.Fatalf("encrypt refresh: %v", err)
	}
	encAccess, err := crypto.Encrypt("old-access")
	if err != nil {
		t.Fatalf("encrypt access: %v", err)
	}

	const keyID = "key-race-refresh"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != officialCodexRefreshEndpoint {
			t.Fatalf("upstream URL = %s", r.URL.String())
		}
		rotatedRefresh, err := crypto.Encrypt("rt.new-refresh")
		if err != nil {
			t.Fatalf("encrypt rotated refresh: %v", err)
		}
		if err := apiKeyQ.UpdateRefreshToken(keyID, rotatedRefresh); err != nil {
			t.Fatalf("rotate refresh token: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"refresh_token_invalidated","message":"invalidated"}}`)),
			Request:    r,
		}, nil
	})

	if err := apiKeyQ.Create(&models.APIKey{
		ID:               keyID,
		ServiceID:        openaiSvc.ID,
		Name:             "codex-auth",
		KeyEncrypted:     encAccess,
		RefreshToken:     encRefresh,
		ExpiresAt:        1,
		TokenEndpoint:    officialCodexRefreshEndpoint,
		SubscriptionInfo: `{"credential_kind":"codex_oauth","client_id":"app_codex_test"}`,
		IsActive:         true,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	refresher := services.NewTokenRefresher(apiKeyQ, crypto)
	services.SetTokenRefresherHTTPClientForTest(refresher, &http.Client{Transport: transport})
	if _, err := refresher.RefreshNow(keyID); err != nil {
		t.Fatalf("refresh should ignore stale invalidation after rotation: %v", err)
	}
	key, err := apiKeyQ.GetByID(keyID)
	if err != nil {
		t.Fatal(err)
	}
	if !key.IsActive {
		t.Fatalf("key was deactivated despite refresh token rotation: %+v", key)
	}
	refreshToken, err := crypto.Decrypt(key.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if refreshToken != "rt.new-refresh" {
		t.Fatalf("refresh token = %q, want rotated token", refreshToken)
	}
}
