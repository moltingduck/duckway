package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"golang.org/x/crypto/bcrypt"
)

// authTestEnv holds all dependencies for auth handler tests.
type authTestEnv struct {
	handler *handlers.AuthHandler
	users   *queries.AdminUserQueries
	auth    *middleware.AdminAuth
}

// newAuthTestEnv opens an in-memory (temp-dir) SQLite DB, runs all migrations,
// and returns a fully-wired AuthHandler alongside the underlying queries object
// so individual tests can seed users.
func newAuthTestEnv(t *testing.T) *authTestEnv {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	userQ := queries.NewAdminUserQueries(db)
	auth := middleware.NewAdminAuth([]byte("test-secret"))
	h := handlers.NewAuthHandler(userQ, auth)

	return &authTestEnv{handler: h, users: userQ, auth: auth}
}

// seedUser creates an admin user with the given plaintext password and returns the bcrypt hash used.
func seedUser(t *testing.T, env *authTestEnv, id, username, plainPassword string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	if err := env.users.Create(id, username, string(hash)); err != nil {
		t.Fatalf("users.Create: %v", err)
	}
	return string(hash)
}

// jsonBody encodes v as JSON and returns a reader suitable for http.Request.Body.
func jsonBody(t *testing.T, v interface{}) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return bytes.NewReader(b)
}

// ---- Login ----

func TestLogin_CorrectCredentials_Returns200WithCookie(t *testing.T) {
	env := newAuthTestEnv(t)
	seedUser(t, env, "u1", "alice", "supersecret")

	body := jsonBody(t, map[string]string{"username": "alice", "password": "supersecret"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	env.handler.Login(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "duckway_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected Set-Cookie: duckway_session but none found")
		return
	}
	if sessionCookie.Value == "" {
		t.Error("duckway_session cookie value is empty")
	}
}

func TestLogin_WrongPassword_Returns401(t *testing.T) {
	env := newAuthTestEnv(t)
	seedUser(t, env, "u1", "alice", "supersecret")

	body := jsonBody(t, map[string]string{"username": "alice", "password": "wrongpassword"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	env.handler.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestLogin_UnknownUsername_Returns401(t *testing.T) {
	env := newAuthTestEnv(t)
	// No users seeded — "ghost" doesn't exist.

	body := jsonBody(t, map[string]string{"username": "ghost", "password": "anything"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	env.handler.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// ---- Logout ----

func TestLogout_ClearsCookie(t *testing.T) {
	env := newAuthTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	rr := httptest.NewRecorder()

	env.handler.Logout(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "duckway_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected Set-Cookie: duckway_session in Logout response but none found")
		return
	}
	if sessionCookie.MaxAge != -1 {
		t.Errorf("duckway_session MaxAge = %d, want -1 (delete cookie)", sessionCookie.MaxAge)
	}
}

// ---- ChangePassword ----

// injectUser returns a copy of req with the AdminUserKey injected into its context.
func injectUser(req *http.Request, username string) *http.Request {
	ctx := context.WithValue(req.Context(), middleware.AdminUserKey, username)
	return req.WithContext(ctx)
}

func TestChangePassword_CorrectCurrentPassword_Returns200(t *testing.T) {
	env := newAuthTestEnv(t)
	seedUser(t, env, "u1", "alice", "oldpassword")

	body := jsonBody(t, map[string]string{
		"current_password": "oldpassword",
		"new_password":     "newpassword123",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", body)
	req.Header.Set("Content-Type", "application/json")
	req = injectUser(req, "alice")
	rr := httptest.NewRecorder()

	env.handler.ChangePassword(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	// Verify the new password actually works via a subsequent bcrypt check.
	user, err := env.users.GetByUsername("alice")
	if err != nil {
		t.Fatalf("GetByUsername after ChangePassword: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte("newpassword123")); err != nil {
		t.Errorf("new password does not validate against stored hash: %v", err)
	}
}

func TestChangePassword_WrongCurrentPassword_Returns401(t *testing.T) {
	env := newAuthTestEnv(t)
	seedUser(t, env, "u1", "alice", "oldpassword")

	body := jsonBody(t, map[string]string{
		"current_password": "nottherightone",
		"new_password":     "newpassword123",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", body)
	req.Header.Set("Content-Type", "application/json")
	req = injectUser(req, "alice")
	rr := httptest.NewRecorder()

	env.handler.ChangePassword(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestChangePassword_NewPasswordTooShort_Returns400(t *testing.T) {
	env := newAuthTestEnv(t)
	seedUser(t, env, "u1", "alice", "oldpassword")

	body := jsonBody(t, map[string]string{
		"current_password": "oldpassword",
		"new_password":     "short",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", body)
	req.Header.Set("Content-Type", "application/json")
	req = injectUser(req, "alice")
	rr := httptest.NewRecorder()

	env.handler.ChangePassword(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestChangePassword_NotAuthenticated_Returns401(t *testing.T) {
	env := newAuthTestEnv(t)

	body := jsonBody(t, map[string]string{
		"current_password": "oldpassword",
		"new_password":     "newpassword123",
	})
	// No context user injected — simulates unauthenticated request.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	env.handler.ChangePassword(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}
