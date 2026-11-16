package middleware

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/services"
)

// ApproverTokenPrefix starts every approver token.
const ApproverTokenPrefix = "dwa_"

const ApproverKey contextKey = "approver"

// Failed-authentication throttle: after approverMaxFailures bad tokens from one
// address within approverFailWindow, every request from it gets 429 until the
// window passes.
const (
	approverMaxFailures = 10
	approverFailWindow  = time.Minute
	approverMaxTracked  = 4096
)

// ApproverAuth authenticates /approver/ requests with "Authorization: Bearer
// dwa_...". It accepts nothing else: client tokens, X-Duckway-Token and admin
// cookies never authenticate here, and an approver token authenticates nowhere
// else because no other middleware looks it up.
type ApproverAuth struct {
	tokens *queries.ApproverTokenQueries
	now    func() time.Time

	mu    sync.Mutex
	fails map[string]*failWindow
}

type failWindow struct {
	start time.Time
	n     int
}

func NewApproverAuth(tokens *queries.ApproverTokenQueries) *ApproverAuth {
	return &ApproverAuth{tokens: tokens, now: time.Now, fails: map[string]*failWindow{}}
}

// Lookup resolves the bearer token on r to an active approver token, comparing
// the stored SHA-256 in constant time. It has no side effects.
func (a *ApproverAuth) Lookup(r *http.Request) (*queries.ApproverToken, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return nil, false
	}
	raw := strings.TrimSpace(h[7:])
	if !strings.HasPrefix(raw, ApproverTokenPrefix) {
		return nil, false
	}
	hash := services.HashToken(raw)
	tok, stored, err := a.tokens.GetActiveByHash(hash)
	if err != nil || subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) != 1 {
		return nil, false
	}
	return tok, true
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *ApproverAuth) blocked(host string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.fails[host]
	if w == nil {
		return false
	}
	if a.now().Sub(w.start) >= approverFailWindow {
		delete(a.fails, host)
		return false
	}
	return w.n >= approverMaxFailures
}

func (a *ApproverAuth) recordFailure(host string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if len(a.fails) >= approverMaxTracked {
		for k, w := range a.fails {
			if now.Sub(w.start) >= approverFailWindow {
				delete(a.fails, k)
			}
		}
		if len(a.fails) >= approverMaxTracked { // all fresh: drop the table rather than grow
			a.fails = map[string]*failWindow{}
		}
	}
	w := a.fails[host]
	if w == nil || now.Sub(w.start) >= approverFailWindow {
		w = &failWindow{start: now}
		a.fails[host] = w
	}
	w.n++
}

func (a *ApproverAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := remoteHost(r)
		w.Header().Set("Cache-Control", "no-store")
		if a.blocked(host) {
			w.Header().Set("Retry-After", "60")
			writeAuthError(w, http.StatusTooManyRequests, "too many failed authentication attempts")
			return
		}
		tok, ok := a.Lookup(r)
		if !ok {
			if r.Header.Get("Authorization") != "" {
				a.recordFailure(host)
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="duckway-approver"`)
			writeAuthError(w, http.StatusUnauthorized, "approver token required")
			return
		}
		a.tokens.TouchLastUsed(tok.ID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ApproverKey, tok)))
	})
}

func writeAuthError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}

// GetApprover returns the authenticated approver token of an /approver/ request.
func GetApprover(r *http.Request) *queries.ApproverToken {
	t, _ := r.Context().Value(ApproverKey).(*queries.ApproverToken)
	return t
}
