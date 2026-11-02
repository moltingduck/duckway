package middleware

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
)

// EnforceCSP selects the CSP header name used by the CSP middleware.
//
// When false it sends Content-Security-Policy-Report-Only so
// templates that still carry inline scripts or handlers keep working while
// they are converted. It defaults to true (enforcing Content-Security-Policy). Set it before
// the server starts serving; it is read on every request without locking.
var EnforceCSP = true

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
		if EnforceCSP {
			name = "Content-Security-Policy"
		}
		w.Header().Set(name, CSPHeaderValue(nonce))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), cspNonceKey{}, nonce)))
	})
}
