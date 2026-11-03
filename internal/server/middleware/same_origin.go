package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// SameOrigin blocks cross-site state-changing requests to the admin console.
//
// Safe methods (GET/HEAD/OPTIONS) always pass, so SSE streams and page loads
// are unaffected. For POST/PUT/PATCH/DELETE the request must prove it came
// from the console's own origin:
//
//  1. Origin header present: its scheme and host must match this request's.
//  2. Origin absent: Sec-Fetch-Site must be "same-origin" or "none".
//  3. Neither: the Referer host must match.
//  4. Nothing at all: rejected. Browsers always send Origin on these methods;
//     non-browser admin tooling must send "Origin: <server base URL>".
//
// The server derives its own origin the way it does elsewhere (Host header,
// X-Forwarded-Proto/Host from a fronting proxy). The scheme is compared only
// when the connection is TLS or a proxy declared one; otherwise the host alone
// is compared, so plain-HTTP deployments behind scheme-agnostic proxies work.
//
// Wrap only admin routes; the gateway client API and proxy traffic are
// authenticated by bearer/placeholder tokens and must not be wrapped.
func SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !sameOriginRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"cross-site request blocked"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOriginRequest(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		return originMatches(r, origin, true)
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "same-origin", "none":
		return true
	case "":
	default:
		return false
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return originMatches(r, ref, false)
	}
	return false
}

func originMatches(r *http.Request, raw string, checkScheme bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := r.Host
	if fh := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); fh != "" {
		if strings.EqualFold(fh, u.Host) {
			host = fh
		}
	}
	if !strings.EqualFold(u.Host, host) {
		return false
	}
	if !checkScheme {
		return true
	}
	scheme := ""
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])); fwd != "" {
		scheme = fwd
	}
	return scheme == "" || scheme == u.Scheme
}
