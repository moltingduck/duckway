package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func changePw(t *testing.T, env *authTestEnv, newPw string) *httptest.ResponseRecorder {
	t.Helper()
	body := jsonBody(t, map[string]string{"current_password": "oldpassword", "new_password": newPw})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", body)
	req.Header.Set("Content-Type", "application/json")
	req = injectUser(req, "alice")
	rr := httptest.NewRecorder()
	env.handler.ChangePassword(rr, req)
	return rr
}

// Length is counted in characters (runes), not bytes: 11 is rejected, 12 accepted.
func TestChangePasswordLengthBoundaryCountsRunes(t *testing.T) {
	env := newAuthTestEnv(t)
	seedUser(t, env, "u1", "alice", "oldpassword")
	if rr := changePw(t, env, strings.Repeat("é", 11)); rr.Code != http.StatusBadRequest {
		t.Fatalf("11 runes (22 bytes) => %d, want 400", rr.Code)
	}
	if rr := changePw(t, env, strings.Repeat("a", 11)); rr.Code != http.StatusBadRequest {
		t.Fatalf("11 chars => %d, want 400", rr.Code)
	}
	rr := changePw(t, env, strings.Repeat("é", 12))
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("12 runes => %d cache=%q", rr.Code, rr.Header().Get("Cache-Control"))
	}
}
