package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const sessionMaxAgeSecs = 86400 * 7 // 7 days, matches MaxAge below

type contextKey string

const AdminUserKey contextKey = "admin_user"

type AdminAuth struct {
	secret []byte
}

func NewAdminAuth(secret []byte) *AdminAuth {
	return &AdminAuth{secret: secret}
}

func (a *AdminAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("duckway_session")
		if err != nil {
			a.unauthorized(w, r)
			return
		}

		username, ok := a.validateSession(cookie.Value)
		if !ok {
			a.unauthorized(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), AdminUserKey, username)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *AdminAuth) unauthorized(w http.ResponseWriter, r *http.Request) {
	// HTML page requests → redirect to login
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	// API requests → JSON error
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":"authentication required"}`))
}

func (a *AdminAuth) CreateSession(username string, r *http.Request) *http.Cookie {
	ts := time.Now().Unix()
	data := fmt.Sprintf("%s|%d", username, ts)
	sig := a.sign(data)
	value := data + "|" + sig

	// Only set Secure on HTTPS connections — HTTP deployments (Tailscale,
	// localhost, reverse proxy without TLS) must still be able to send the
	// cookie or the user will be stuck in a redirect loop after login.
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"

	return &http.Cookie{
		Name:     "duckway_session",
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7, // 7 days
	}
}

func (a *AdminAuth) sign(data string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *AdminAuth) validateSession(value string) (string, bool) {
	parts := strings.SplitN(value, "|", 3)
	if len(parts) != 3 {
		return "", false
	}

	username := parts[0]
	data := parts[0] + "|" + parts[1]
	sig := parts[2]

	expected := a.sign(data)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}

	// Server-side expiry: reject tokens older than sessionMaxAgeSecs even if
	// the HMAC is valid (e.g. a stolen cookie that outlived the browser MaxAge).
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix()-ts > sessionMaxAgeSecs {
		return "", false
	}

	return username, true
}
