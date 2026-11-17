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
// address within approverFailWindow, further invalid requests from it get 429
// until the window passes. Requests with a valid token are never throttled.
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
	if len(a.fails) >= approverMaxTracked && a.fails[host] == nil {
		// Make room: drop expired windows first, then the oldest one. Resetting the
		// whole table would let an attacker flush every other address's counter.
		for k, w := range a.fails {
			if now.Sub(w.start) >= approverFailWindow {
				delete(a.fails, k)
			}
		}
		for len(a.fails) >= approverMaxTracked {
			oldest, first := "", true
			var ot time.Time
			for k, w := range a.fails {
				if first || w.start.Before(ot) {
					oldest, ot, first = k, w.start, false
				}
			}
			delete(a.fails, oldest)
		}
	}
	w := a.fails[host]
	if w == nil || now.Sub(w.start) >= approverFailWindow {
		w = &failWindow{start: now}
		a.fails[host] = w
	}
	w.n++
}

// Middleware authenticates the request. The token is looked up first: a request
// presenting a valid token is never throttled, so an attacker spraying bad
// tokens from a shared address (a NAT, or a reverse proxy that hides client
// addresses) cannot lock real approvers out. Invalid attempts are counted, and
// once a host exceeds the limit its invalid requests get 429 instead of 401.
//
// The throttle keys on the connection's remote address. Behind a reverse proxy
// that is the proxy's address, so all clients share one counter; the valid-token
// exemption keeps that harmless, and Duckway does not trust X-Forwarded-For here.
func (a *ApproverAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := remoteHost(r)
		w.Header().Set("Cache-Control", "no-store")
		tok, ok := a.Lookup(r)
		if !ok {
			blocked := a.blocked(host)
			if r.Header.Get("Authorization") != "" {
				a.recordFailure(host)
			}
			if blocked {
				w.Header().Set("Retry-After", "60")
				writeAuthError(w, http.StatusTooManyRequests, "too many failed authentication attempts")
				return
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
