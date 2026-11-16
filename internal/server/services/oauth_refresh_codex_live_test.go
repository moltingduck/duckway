package services_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/services"
)

// This probe never refreshes or writes the source credential. Only the access
// session check goes upstream; the expired-refresh response is a local fixture.
func TestCodexOAuthLiveExpiredRefreshPreservesValidAccess(t *testing.T) {
	requireLiveOAuthOptIn(t, liveCodexOptInEnv)
	path, ok := liveCredentialPath(t, liveCodexAuthPathEnv, liveCodexAuthName)
	if !ok {
		// An explicitly enabled regression must not silently skip its live gate.
		t.Fatal("Codex live credential file is required for the access-session regression")
	}
	enforcePrivateLiveCredentialFile(t, path)
	// Unlike optional historical refresh tests, this explicit live gate fails
	// on contention instead of skipping and appearing to pass without a probe.
	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("cannot acquire Codex live credential lock")
	}
	t.Cleanup(func() { _ = os.Remove(lockPath) })
	if err := lock.Close(); err != nil {
		t.Fatal("close Codex live credential lock")
	}
	doc := readLiveCredentialJSON(t, path)
	tokens := liveObject(doc, "tokens")
	if tokens == nil {
		tokens = doc
	}
	access := liveString(tokens, "access_token", "accessToken")
	if access == "" {
		t.Fatal("Codex live credential missing access token")
	}
	expiry := firstPositive(decodeJWTExpirationMillis(access), liveInt(tokens, "expires_at", "expiresAt"))
	if expiry <= time.Now().Add(3*time.Minute).UnixMilli() {
		t.Fatal("Codex access credential expiry must allow three minutes; run credential preflight first")
	}
	metadata, err := json.Marshal(map[string]string{"credential_kind": "codex_oauth", "id_token": liveString(tokens, "id_token", "idToken")})
	if err != nil {
		t.Fatal("marshal fixture metadata")
	}
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal("open fixture database")
	}
	t.Cleanup(func() { _ = db.Close() })
	apiKeys := queries.NewAPIKeyQueries(db)
	svc, err := queries.NewServiceQueries(db).GetByName("openai")
	if err != nil {
		t.Fatal("get fixture service")
	}
	crypto := services.NewCrypto([]byte("0123456789abcdef0123456789abcdef"))
	encAccess, err := crypto.Encrypt(access)
	if err != nil {
		t.Fatal("encrypt fixture access token")
	}
	encRefresh, err := crypto.Encrypt("rt.synthetic-expired-live-regression")
	if err != nil {
		t.Fatal("encrypt fixture refresh token")
	}
	const keyID = "codex-live-expired-refresh"
	if err := apiKeys.Create(&models.APIKey{ID: keyID, ServiceID: svc.ID, Name: "live-access-check", KeyEncrypted: encAccess, RefreshToken: encRefresh, ExpiresAt: expiry, TokenEndpoint: officialCodexRefreshEndpoint, SubscriptionInfo: string(metadata)}); err != nil {
		t.Fatal("create fixture key")
	}
	upstream := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(upstream.CloseIdleConnections)
	checks := 0
	client := &http.Client{Timeout: 30 * time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case officialCodexRefreshEndpoint:
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"refresh_token_expired"}}`)), Request: req}, nil
		case "https://chatgpt.com/backend-api/wham/accounts/check":
			checks++
			return upstream.RoundTrip(req)
		default:
			t.Fatal("unexpected upstream destination in live access-session regression")
			return nil, nil
		}
	})}
	refresher := services.NewTokenRefresher(apiKeys, crypto)
	services.SetTokenRefresherHTTPClientForTest(refresher, client)
	_, err = refresher.RefreshNow(keyID)
	// Do not print raw errors from live HTTP requests or provider response bodies.
	if err == nil || !strings.Contains(err.Error(), "access token test still succeeds") {
		t.Fatal("valid live Codex access session was not confirmed after synthetic expired refresh")
	}
	key, err := apiKeys.GetByID(keyID)
	if err != nil {
		t.Fatal("read fixture result")
	}
	if !key.IsActive || key.KeyEncrypted != encAccess || key.RefreshToken != encRefresh || checks != 1 {
		t.Fatal("live session regression changed fixture credentials or active state")
	}
}
