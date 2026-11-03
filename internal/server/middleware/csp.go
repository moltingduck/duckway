package middleware

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"sync/atomic"
)

var enforceCSP atomic.Bool

func init() { enforceCSP.Store(true) }

// SetEnforceCSP selects the CSP header name used by the CSP middleware: true
// (the default) sends the enforcing Content-Security-Policy, false sends
// Content-Security-Policy-Report-Only. It is safe to call concurrently with
// request handling.
func SetEnforceCSP(enforce bool) { enforceCSP.Store(enforce) }

// EnforceCSPEnabled reports whether the enforcing header is in use.
func EnforceCSPEnabled() bool { return enforceCSP.Load() }

type cspNonceKey struct{}

// CSPNonce returns the per-request CSP nonce stored by CSP, or "".
func CSPNonce(ctx context.Context) string {
	n, _ := ctx.Value(cspNonceKey{}).(string)
	return n
}

// CSPHeaderValue builds the policy string for a nonce.
func CSPHeaderValue(nonce string) string {
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'nonce-" + nonce + "' 'strict-dynamic'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"form-action 'self'",
		// Trusted Types: DOM XSS sinks only accept values from the single `dw`
		// policy created in dw.js (no default policy, no other policy names).
		"require-trusted-types-for 'script'",
		"trusted-types dw",
	}, "; ")
}

func newNonce() (string, error) {
	var b [16]byte // 128 bits; URL-safe base64 so html/template never entity-escapes it
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// CSP sets a Content-Security-Policy with a fresh nonce on every response and
// stores the nonce in the request context.
func CSP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, err := newNonce()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		name := "Content-Security-Policy-Report-Only"
		if enforceCSP.Load() {
			name = "Content-Security-Policy"
		}
		w.Header().Set(name, CSPHeaderValue(nonce))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), cspNonceKey{}, nonce)))
	})
}
