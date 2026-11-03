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
	"sync"
	"time"
)

// EpochStore persists the admin session epoch (settings.session_epoch).
type EpochStore interface {
	Get(key string) string
	Set(key, value string) error
}

const epochSettingKey = "session_epoch"

const sessionMaxAgeSecs = 86400 * 7 // 7 days, matches MaxAge below

type contextKey string

const AdminUserKey contextKey = "admin_user"

type AdminAuth struct {
	secret  []byte
	epochMu sync.Mutex
	epochs  EpochStore
}

// WithEpochStore enables session revocation: cookies carry the epoch current
// at issue time and are rejected once it changes. Without a store the epoch is
// always 0.
func (a *AdminAuth) WithEpochStore(s EpochStore) *AdminAuth {
	a.epochs = s
	return a
}

// Epoch returns the current session epoch (0 when unset or unparsable). It is
// read from the store on every call so a bump takes effect immediately.
func (a *AdminAuth) Epoch() int64 {
	if a.epochs == nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(a.epochs.Get(epochSettingKey)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// BumpEpoch invalidates every previously issued session cookie.
func (a *AdminAuth) BumpEpoch() (int64, error) {
	if a.epochs == nil {
		return 0, nil
	}
	a.epochMu.Lock()
	defer a.epochMu.Unlock()
	n := a.Epoch() + 1
	if err := a.epochs.Set(epochSettingKey, strconv.FormatInt(n, 10)); err != nil {
		return 0, err
	}
	return n, nil
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

// SessionUser reports the username of a valid session cookie on r.
func (a *AdminAuth) SessionUser(r *http.Request) (string, bool) {
	c, err := r.Cookie("duckway_session")
	if err != nil {
		return "", false
	}
	return a.validateSession(c.Value)
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
	data := fmt.Sprintf("%s|%d|%d", username, ts, a.Epoch())
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
	// Current format: user|ts|epoch|sig. Legacy cookies (user|ts|sig) predate
	// revocation and count as epoch 0.
	parts := strings.Split(value, "|")
	var epoch int64
	var data, sig string
	switch len(parts) {
	case 3:
		data, sig = parts[0]+"|"+parts[1], parts[2]
	case 4:
		n, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || n < 0 {
			return "", false
		}
		epoch = n
		data, sig = parts[0]+"|"+parts[1]+"|"+parts[2], parts[3]
	default:
		return "", false
	}
	username := parts[0]

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

	if epoch != a.Epoch() {
		return "", false
	}
	return username, true
}
