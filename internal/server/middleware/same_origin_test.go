package middleware

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func sameOriginStatus(method string, mutate func(*http.Request)) int {
	h := SameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(method, "http://console.example/api/x", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestSameOriginDecisions(t *testing.T) {
	set := func(k, v string) func(*http.Request) { return func(r *http.Request) { r.Header.Set(k, v) } }
	cases := []struct {
		name   string
		method string
		mut    func(*http.Request)
		want   int
	}{
		{"cross-site origin", "POST", set("Origin", "http://evil.example"), 403},
		{"same origin", "POST", set("Origin", "http://console.example"), 200},
		{"same origin put/patch/delete", "DELETE", set("Origin", "http://console.example"), 200},
		{"null origin", "POST", set("Origin", "null"), 403},
		{"origin wins over sec-fetch same-origin", "POST", func(r *http.Request) {
			r.Header.Set("Origin", "http://evil.example")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}, 403},
		{"sec-fetch cross-site", "POST", set("Sec-Fetch-Site", "cross-site"), 403},
		{"sec-fetch same-site is not same-origin", "POST", set("Sec-Fetch-Site", "same-site"), 403},
		{"sec-fetch same-origin", "POST", set("Sec-Fetch-Site", "same-origin"), 200},
		{"sec-fetch none", "POST", set("Sec-Fetch-Site", "none"), 200},
		{"referer same host", "POST", set("Referer", "http://console.example/admin/settings"), 200},
		{"referer other host", "POST", set("Referer", "http://evil.example/"), 403},
		{"cookie only, no headers", "POST", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "duckway_session", Value: "x"}) }, 403},
		{"no headers", "PATCH", nil, 403},
		{"GET never checked", "GET", set("Origin", "http://evil.example"), 200},
		{"HEAD never checked", "HEAD", nil, 200},
		{"scheme mismatch behind proxy", "POST", func(r *http.Request) {
			r.Header.Set("Origin", "http://console.example")
			r.Header.Set("X-Forwarded-Proto", "https")
		}, 403},
		{"scheme match behind proxy", "POST", func(r *http.Request) {
			r.Header.Set("Origin", "https://console.example")
			r.Header.Set("X-Forwarded-Proto", "https")
		}, 200},
		{"tls scheme mismatch", "POST", func(r *http.Request) {
			r.TLS = &tls.ConnectionState{}
			r.Header.Set("Origin", "http://console.example")
		}, 403},
		{"forwarded host", "POST", func(r *http.Request) {
			r.Host = "internal:8080"
			r.Header.Set("X-Forwarded-Host", "console.example")
			r.Header.Set("Origin", "http://console.example")
		}, 200},
	}
	for _, tc := range cases {
		if got := sameOriginStatus(tc.method, tc.mut); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestSameOriginRejectionBody(t *testing.T) {
	h := SameOrigin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler reached") }))
	req := httptest.NewRequest("POST", "/api/x", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 || rec.Body.String() != `{"error":"cross-site request blocked"}` {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}
