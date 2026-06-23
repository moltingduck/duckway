package middleware

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestAuth returns an AdminAuth with a fixed secret for testing.
func newTestAuth() *AdminAuth {
	return NewAdminAuth([]byte("test-secret-key"))
}

// buildCookieValue manually crafts a session cookie value with the given
// username and timestamp so tests can inject stale or tampered values.
func buildCookieValue(a *AdminAuth, username string, ts int64) string {
	data := fmt.Sprintf("%s|%d", username, ts)
	sig := a.sign(data)
	return data + "|" + sig
}

// ---- validateSession / CreateSession round-trip ----

func TestCreateAndValidateSession_RoundTrip(t *testing.T) {
	a := newTestAuth()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	cookie := a.CreateSession("alice", req)
	if cookie == nil {
		t.Fatal("CreateSession returned nil")
	}

	username, ok := a.validateSession(cookie.Value)
	if !ok {
		t.Fatal("validateSession returned false for a freshly-created session")
	}
	if username != "alice" {
		t.Errorf("username = %q, want %q", username, "alice")
	}
}

func TestValidateSession_ExpiredTimestamp(t *testing.T) {
	a := newTestAuth()
	// Build a value with a timestamp older than sessionMaxAgeSecs.
	oldTS := time.Now().Unix() - sessionMaxAgeSecs - 1
	value := buildCookieValue(a, "alice", oldTS)

	_, ok := a.validateSession(value)
	if ok {
		t.Error("expected validateSession to return false for expired session, got true")
	}
}

func TestValidateSession_TamperedSignature(t *testing.T) {
	a := newTestAuth()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	cookie := a.CreateSession("alice", req)

	// Flip the last character of the signature.
	v := cookie.Value
	lastIdx := len(v) - 1
	var tampered string
	if v[lastIdx] == 'a' {
		tampered = v[:lastIdx] + "b"
	} else {
		tampered = v[:lastIdx] + "a"
	}

	_, ok := a.validateSession(tampered)
	if ok {
		t.Error("expected validateSession to return false for tampered signature, got true")
	}
}

func TestValidateSession_TamperedUsername(t *testing.T) {
	a := newTestAuth()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	cookie := a.CreateSession("alice", req)

	// Replace the username part while keeping the original signature — the
	// HMAC covers "alice|<ts>", not "bob|<ts>", so it must fail.
	parts := strings.SplitN(cookie.Value, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected cookie format: %q", cookie.Value)
	}
	tampered := "bob|" + parts[1] + "|" + parts[2]

	_, ok := a.validateSession(tampered)
	if ok {
		t.Error("expected validateSession to return false for tampered username, got true")
	}
}

func TestValidateSession_MalformedValue(t *testing.T) {
	a := newTestAuth()
	for _, bad := range []string{"", "onlyone", "two|parts"} {
		_, ok := a.validateSession(bad)
		if ok {
			t.Errorf("expected false for malformed value %q, got true", bad)
		}
	}
}

// ---- CreateSession Secure flag ----

func TestCreateSession_SecureFalse_PlainHTTP(t *testing.T) {
	a := newTestAuth()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	// r.TLS is nil by default for httptest.NewRequest and no X-Forwarded-Proto.
	cookie := a.CreateSession("alice", req)
	if cookie.Secure {
		t.Error("Secure should be false for plain HTTP request")
	}
}

func TestCreateSession_SecureTrue_TLS(t *testing.T) {
	a := newTestAuth()
	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	// Simulate TLS by setting req.TLS to a non-nil value.
	req.TLS = &tls.ConnectionState{}
	cookie := a.CreateSession("alice", req)
	if !cookie.Secure {
		t.Error("Secure should be true when r.TLS != nil")
	}
}

func TestCreateSession_SecureTrue_XForwardedProto(t *testing.T) {
	a := newTestAuth()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	cookie := a.CreateSession("alice", req)
	if !cookie.Secure {
		t.Error("Secure should be true when X-Forwarded-Proto=https")
	}
}

func TestCreateSession_SecureFalse_XForwardedProtoHTTP(t *testing.T) {
	a := newTestAuth()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("X-Forwarded-Proto", "http")
	cookie := a.CreateSession("alice", req)
	if cookie.Secure {
		t.Error("Secure should be false when X-Forwarded-Proto=http")
	}
}

// ---- Middleware ----

func validSessionCookie(t *testing.T, a *AdminAuth) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	return a.CreateSession("alice", req)
}

func TestMiddleware_CallsNext_WhenSessionValid(t *testing.T) {
	a := newTestAuth()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		// Context should carry the username.
		user, _ := r.Context().Value(AdminUserKey).(string)
		if user != "alice" {
			t.Errorf("context AdminUserKey = %q, want %q", user, "alice")
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := a.Middleware(next)

	req := httptest.NewRequest(http.MethodGet, "/some/path", nil)
	req.AddCookie(validSessionCookie(t, a))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("next handler was not called")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestMiddleware_RedirectsAdminPath_WhenInvalid(t *testing.T) {
	a := newTestAuth()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next should not be called for unauthenticated /admin/ request")
	})

	handler := a.Middleware(next)

	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
	// No cookie — unauthenticated.
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 SeeOther", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if loc != "/admin/login" {
		t.Errorf("Location = %q, want %q", loc, "/admin/login")
	}
}

func TestMiddleware_Returns401JSON_ForNonAdminPath_WhenInvalid(t *testing.T) {
	a := newTestAuth()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next should not be called for unauthenticated API request")
	})

	handler := a.Middleware(next)

	req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	// No cookie — unauthenticated.
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "authentication required") {
		t.Errorf("body %q does not contain 'authentication required'", body)
	}
}

func TestMiddleware_Returns401JSON_ExpiredCookie(t *testing.T) {
	a := newTestAuth()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next should not be called for expired session")
	})

	handler := a.Middleware(next)

	oldTS := time.Now().Unix() - sessionMaxAgeSecs - 1
	expiredValue := buildCookieValue(a, "alice", oldTS)

	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.AddCookie(&http.Cookie{Name: "duckway_session", Value: expiredValue})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestMiddleware_CookieValue_ContainsTimestamp(t *testing.T) {
	a := newTestAuth()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	before := time.Now().Unix()
	cookie := a.CreateSession("bob", req)
	after := time.Now().Unix()

	parts := strings.SplitN(cookie.Value, "|", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected cookie value format: %q", cookie.Value)
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("timestamp parse error: %v", err)
	}
	if ts < before || ts > after {
		t.Errorf("timestamp %d outside expected range [%d, %d]", ts, before, after)
	}
}
