package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCSPNonceUniqueAndHeader(t *testing.T) {
	var seen []string
	h := CSP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, CSPNonce(r.Context()))
	}))
	var headers []string
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))
		headers = append(headers, rec.Header().Get("Content-Security-Policy"))
		if rec.Header().Get("Content-Security-Policy-Report-Only") != "" {
			t.Fatal("report-only header set while SetEnforceCSP(true)")
		}
	}
	if seen[0] == "" || seen[0] == seen[1] {
		t.Fatalf("nonces not fresh: %q", seen)
	}
	if len(seen[0]) != 22 { // 16 bytes unpadded URL-safe base64
		t.Fatalf("nonce %q is not 128 bits base64", seen[0])
	}
	if !strings.Contains(headers[0], "'nonce-"+seen[0]+"'") || !strings.Contains(headers[0], "frame-ancestors 'none'") {
		t.Fatalf("unexpected header %q", headers[0])
	}
}

func TestCSPDefaultsToEnforcing(t *testing.T) {
	if !EnforceCSPEnabled() {
		t.Fatal("enforcement must default to true")
	}
}

func TestCSPReportOnlyToggle(t *testing.T) {
	SetEnforceCSP(false)
	t.Cleanup(func() { SetEnforceCSP(true) })
	rec := httptest.NewRecorder()
	CSP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Content-Security-Policy-Report-Only") == "" || rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatalf("headers = %v", rec.Header())
	}
}
