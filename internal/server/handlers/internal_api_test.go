package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/services"
)

// newTestResolver creates a KeyResolver backed by a real SQLite database with
// all migrations applied. The database is stored in a temp dir and closed when
// the test ends.
func newTestResolver(t *testing.T) *services.KeyResolver {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// AES-256-GCM requires exactly 32 bytes.
	cryptoKey := make([]byte, 32)
	copy(cryptoKey, []byte("test-aes-key-for-unit-tests-xyz"))
	cr := services.NewCrypto(cryptoKey)

	return services.NewKeyResolver(
		cr,
		queries.NewAPIKeyQueries(db),
		queries.NewPlaceholderQueries(db),
		queries.NewGroupQueries(db),
		queries.NewApprovalQueries(db),
	)
}

// buildHandlerWithSecret writes the given secret to a temp dir and returns an
// InternalHandler loaded from that dir. This exercises the file-based secret
// path without touching the real DUCKWAY_INTERNAL_SECRET env var.
func buildHandlerWithSecret(t *testing.T, secret string, resolver *services.KeyResolver) *handlers.InternalHandler {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "internal-secret"), []byte(secret+"\n"), 0600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	return handlers.NewInternalHandler(resolver, dir)
}

// postResolve fires POST /internal/resolve and returns the recorded response.
func postResolve(t *testing.T, h *handlers.InternalHandler, secret string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/resolve", &buf)
	if secret != "" {
		req.Header.Set("X-Internal-Secret", secret)
	}
	w := httptest.NewRecorder()
	h.Resolve(w, req)
	return w
}

// ---- Resolve endpoint ----

func TestInternalHandler_Resolve_MissingSecret(t *testing.T) {
	h := buildHandlerWithSecret(t, "correct-secret", newTestResolver(t))

	w := postResolve(t, h, "" /* no header set */, map[string]string{"placeholder": "dk_test"})

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "secret") {
		t.Errorf("error message %q should mention 'secret'", resp["error"])
	}
}

func TestInternalHandler_Resolve_WrongSecret(t *testing.T) {
	h := buildHandlerWithSecret(t, "correct-secret", newTestResolver(t))

	w := postResolve(t, h, "definitely-wrong", map[string]string{"placeholder": "dk_test"})

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestInternalHandler_Resolve_InvalidJSONBody(t *testing.T) {
	const secret = "my-secret-abc"
	h := buildHandlerWithSecret(t, secret, newTestResolver(t))

	req := httptest.NewRequest(http.MethodPost, "/internal/resolve", strings.NewReader("{not valid json"))
	req.Header.Set("X-Internal-Secret", secret)
	w := httptest.NewRecorder()
	h.Resolve(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] == "" {
		t.Error("expected non-empty error field")
	}
}

func TestInternalHandler_Resolve_ValidRequestCallsResolver(t *testing.T) {
	const secret = "valid-secret-xyz"
	h := buildHandlerWithSecret(t, secret, newTestResolver(t))

	// The DB is empty, so the resolver returns "unknown placeholder" — not an
	// HTTP error. A valid request that reaches the resolver always returns 200.
	w := postResolve(t, h, secret, map[string]string{
		"placeholder": "dk_nonexistent",
		"client_id":   "client-1",
		"target_host": "api.example.com",
	})

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — resolver returned an error: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// permitted and need_approval must always be present in the response.
	if _, ok := resp["permitted"]; !ok {
		t.Error("response missing 'permitted' field")
	}
	if _, ok := resp["need_approval"]; !ok {
		t.Error("response missing 'need_approval' field")
	}
	// Unknown placeholder → not permitted.
	if permitted, _ := resp["permitted"].(bool); permitted {
		t.Error("expected permitted=false for unknown placeholder")
	}
	// real_key must NOT be present when not permitted.
	if _, ok := resp["real_key"]; ok {
		t.Error("real_key must not be present when permitted=false")
	}
}

// ---- loadOrCreateInternalSecret ----

func TestLoadOrCreateInternalSecret_CreatesFileOnFirstCall(t *testing.T) {
	dir := t.TempDir()
	// NewInternalHandler calls loadOrCreateInternalSecret when the env var is unset.
	_ = handlers.NewInternalHandler(newTestResolver(t), dir)

	secretPath := filepath.Join(dir, "internal-secret")
	data, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("secret file not created: %v", err)
	}
	secret := strings.TrimSpace(string(data))
	if len(secret) == 0 {
		t.Fatal("secret file is empty")
	}
	// 32 random bytes encoded as hex = 64 characters.
	if len(secret) != 64 {
		t.Errorf("secret length = %d, want 64 hex chars", len(secret))
	}
}

func TestLoadOrCreateInternalSecret_SecondCallReturnsSameSecret(t *testing.T) {
	dir := t.TempDir()

	// First construction — generates and persists the secret.
	h1 := handlers.NewInternalHandler(newTestResolver(t), dir)

	// Read what was persisted.
	data, err := os.ReadFile(filepath.Join(dir, "internal-secret"))
	if err != nil {
		t.Fatalf("read secret file: %v", err)
	}
	generatedSecret := strings.TrimSpace(string(data))

	// Second construction — must reuse the existing file.
	h2 := handlers.NewInternalHandler(newTestResolver(t), dir)

	// Both handlers must accept the same secret.
	for i, h := range []*handlers.InternalHandler{h1, h2} {
		w := postResolve(t, h, generatedSecret, map[string]string{"placeholder": "dk_x"})
		if w.Code == http.StatusUnauthorized {
			t.Errorf("handler %d rejected the persisted secret", i+1)
		}
	}
}

func TestLoadOrCreateInternalSecret_EnvVarTakesPriority(t *testing.T) {
	const envSecret = "env-override-secret-12345678"
	t.Setenv("DUCKWAY_INTERNAL_SECRET", envSecret)

	dir := t.TempDir()
	h := handlers.NewInternalHandler(newTestResolver(t), dir)

	// Handler must accept the env-var secret.
	w := postResolve(t, h, envSecret, map[string]string{"placeholder": "dk_x"})
	if w.Code == http.StatusUnauthorized {
		t.Error("handler rejected DUCKWAY_INTERNAL_SECRET value")
	}

	// Handler must reject any other secret, including one that may be in the file.
	w2 := postResolve(t, h, "some-other-secret", map[string]string{"placeholder": "dk_x"})
	if w2.Code != http.StatusUnauthorized {
		t.Error("handler accepted a wrong secret when DUCKWAY_INTERNAL_SECRET is set")
	}
}
