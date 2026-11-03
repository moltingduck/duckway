package handlers_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"golang.org/x/crypto/bcrypt"
)

type flakyEpoch struct{ failGet, failSet bool }

func (f *flakyEpoch) GetE(string) (string, error) {
	if f.failGet {
		return "", errors.New("store down")
	}
	return "", nil
}
func (f *flakyEpoch) Set(string, string) error {
	if f.failSet {
		return errors.New("store down")
	}
	return nil
}

func envWithEpoch(t *testing.T, store *flakyEpoch) *authTestEnv {
	t.Helper()
	env := newAuthTestEnv(t)
	auth := middleware.NewAdminAuth([]byte("test-secret")).WithEpochStore(store)
	env.auth = auth
	env.handler = handlers.NewAuthHandler(env.users, auth)
	return env
}

// When the epoch cannot be read, login must fail with 500 and no cookie rather
// than report success with a cookie that can never validate.
func TestLoginFailsWith500WhenEpochUnreadable(t *testing.T) {
	store := &flakyEpoch{failGet: true}
	env := envWithEpoch(t, store)
	seedUser(t, env, "u1", "alice", "oldpassword")
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", jsonBody(t, map[string]string{"username": "alice", "password": "oldpassword"}))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.handler.Login(rr, req)
	if rr.Code != http.StatusInternalServerError || len(rr.Result().Cookies()) != 0 || strings.Contains(rr.Body.String(), `"status":"ok"`) {
		t.Fatalf("login with unreadable epoch: %d cookies=%v body=%s", rr.Code, rr.Result().Cookies(), rr.Body.String())
	}
	store.failGet = false
	rr = httptest.NewRecorder()
	env.handler.Login(rr, httptest.NewRequest(http.MethodPost, "/api/auth/login", jsonBody(t, map[string]string{"username": "alice", "password": "oldpassword"})))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthy store: %d", rr.Code)
	}
}

// Password first, then revoke: if revocation fails the new password is in
// force and the error says other sessions may still be active.
func TestChangePasswordRevocationFailureIsReportedHonestly(t *testing.T) {
	env := envWithEpoch(t, &flakyEpoch{failSet: true})
	seedUser(t, env, "u1", "alice", "oldpassword")
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", jsonBody(t, map[string]string{"current_password": "oldpassword", "new_password": "a-new-password-1"}))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.handler.ChangePassword(rr, injectUser(req, "alice"))
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "other sessions may still be active") {
		t.Fatalf("bump failure: %d %s", rr.Code, rr.Body.String())
	}
	u, err := env.users.GetByUsername("alice")
	if err != nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("a-new-password-1")) != nil {
		t.Fatalf("the password must already be changed: %v", err)
	}
}

func TestChangePasswordReissuesCookieAtNewEpoch(t *testing.T) {
	env := envWithEpoch(t, &flakyEpoch{})
	seedUser(t, env, "u1", "alice", "oldpassword")
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", jsonBody(t, map[string]string{"current_password": "oldpassword", "new_password": "a-new-password-1"}))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.handler.ChangePassword(rr, injectUser(req, "alice"))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var found bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == "duckway_session" && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("the current session cookie must be re-issued")
	}
}
