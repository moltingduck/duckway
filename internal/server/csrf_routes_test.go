package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func csrfReq(s *Server, method, path, body string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://console.example"+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestAdminMutationsRequireSameOrigin(t *testing.T) {
	s, shared := newClientDetailServer(t)
	cookie := shared.AdminAuth.CreateSession("duckway", httptest.NewRequest("GET", "/", nil))
	js := map[string]string{"Content-Type": "application/json"}
	with := func(k, v string) map[string]string { return map[string]string{"Content-Type": "application/json", k: v} }
	body := `{"name":"csrf-probe"}`

	if rec := csrfReq(s, "POST", "/api/clients", body, with("Origin", "http://evil.example"), cookie); rec.Code != 403 || !strings.Contains(rec.Body.String(), "cross-site request blocked") {
		t.Fatalf("cross-site origin: %d %s", rec.Code, rec.Body.String())
	}
	if rec := csrfReq(s, "POST", "/api/clients", body, with("Sec-Fetch-Site", "cross-site"), cookie); rec.Code != 403 {
		t.Fatalf("sec-fetch cross-site: %d", rec.Code)
	}
	if rec := csrfReq(s, "POST", "/api/clients", body, js, cookie); rec.Code != 403 {
		t.Fatalf("cookie without any origin headers: %d", rec.Code)
	}
	if rec := csrfReq(s, "POST", "/api/clients", body, with("Origin", "http://console.example"), cookie); rec.Code == 403 {
		t.Fatalf("same-origin blocked: %s", rec.Body.String())
	}
	for _, path := range []string{"/admin/approvals/x/approve"} {
		if rec := csrfReq(s, "POST", path, "", map[string]string{"Origin": "http://evil.example"}, cookie); rec.Code != 403 {
			t.Fatalf("%s cross-site: %d", path, rec.Code)
		}
	}
	// GET and the SSE stream are not subject to the check.
	if rec := csrfReq(s, "GET", "/api/clients", "", map[string]string{"Origin": "http://evil.example"}, cookie); rec.Code == 403 {
		t.Fatalf("GET blocked: %d", rec.Code)
	}
}

func TestLoginFormCSRF(t *testing.T) {
	s, shared := newClientDetailServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct-horse-battery"), bcrypt.MinCost)
	if _, err := s.db.Exec(`DELETE FROM admin_users`); err != nil {
		t.Fatal(err)
	}
	if err := shared.UserQ.Create("u1", "duckway", string(hash)); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"username": {"duckway"}, "password": {"correct-horse-battery"}}.Encode()
	fh := func(k, v string) map[string]string {
		return map[string]string{"Content-Type": "application/x-www-form-urlencoded", k: v}
	}
	rec := csrfReq(s, "POST", "/admin/login", form, fh("Origin", "http://evil.example"))
	if rec.Code != 403 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("cross-site login form: %d cookies=%v", rec.Code, rec.Result().Cookies())
	}
	if rec := csrfReq(s, "POST", "/admin/login", form, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); rec.Code != 403 {
		t.Fatalf("headerless login form: %d", rec.Code)
	}
	if rec := csrfReq(s, "POST", "/admin/login", form, fh("Origin", "http://console.example")); rec.Code != http.StatusSeeOther || len(rec.Result().Cookies()) == 0 {
		t.Fatalf("same-origin login form: %d", rec.Code)
	}
	api := `{"username":"duckway","password":"correct-horse-battery"}`
	if rec := csrfReq(s, "POST", "/api/auth/login", api, map[string]string{"Origin": "http://evil.example"}); rec.Code != 403 {
		t.Fatalf("cross-site api login: %d", rec.Code)
	}
	if rec := csrfReq(s, "POST", "/api/auth/login", api, map[string]string{"Origin": "http://console.example"}); rec.Code != 200 {
		t.Fatalf("same-origin api login: %d", rec.Code)
	}
	if rec := csrfReq(s, "POST", "/api/auth/logout", "", map[string]string{"Origin": "http://evil.example"}); rec.Code != 403 {
		t.Fatalf("cross-site logout: %d", rec.Code)
	}
}

func TestClientAPIUnaffectedBySameOrigin(t *testing.T) {
	s, _ := newClientDetailServer(t)
	// No Origin, bearer-style client auth: must reach the client auth layer
	// (401 for a bad token), never the CSRF check (403).
	rec := csrfReq(s, "POST", "/client/control/heartbeat", `{}`, map[string]string{"X-Duckway-Token": "bad", "Content-Type": "application/json"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("client API: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPasswordChangeRevokesOtherCookiesOverHTTP(t *testing.T) {
	s, shared := newClientDetailServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct-horse-battery"), bcrypt.MinCost)
	_, _ = s.db.Exec(`DELETE FROM admin_users`)
	if err := shared.UserQ.Create("u1", "duckway", string(hash)); err != nil {
		t.Fatal(err)
	}
	base := httptest.NewRequest("GET", "/", nil)
	other := shared.AdminAuth.CreateSession("duckway", base)
	current := shared.AdminAuth.CreateSession("duckway", base)
	h := map[string]string{"Origin": "http://console.example", "Content-Type": "application/json"}
	rec := csrfReq(s, "POST", "/api/auth/change-password", `{"current_password":"correct-horse-battery","new_password":"another-long-password"}`, h, current)
	if rec.Code != 200 {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := csrfReq(s, "GET", "/api/clients", "", nil, other); rec.Code != 401 {
		t.Fatalf("other session after change: %d", rec.Code)
	}
	var fresh *http.Cookie
	for _, c := range rec.Result().Cookies() {
		fresh = c
	}
	if fresh == nil {
		t.Fatal("no re-issued cookie")
	}
	if rec := csrfReq(s, "GET", "/api/clients", "", nil, fresh); rec.Code != 200 {
		t.Fatalf("current session after change: %d", rec.Code)
	}
	if rec := csrfReq(s, "GET", "/api/clients", "", nil, current); rec.Code != 401 {
		t.Fatalf("pre-change current cookie: %d", rec.Code)
	}
}
