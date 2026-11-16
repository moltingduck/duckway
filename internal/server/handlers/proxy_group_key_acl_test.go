package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/server/services"
)

const groupKeyDenyACL = `{"version":"1","provider":"anthropic","rules":[{"name":"key-deny","endpoints":[{"method":"GET","path":"/v1/models","allow":false}]}]}`

// moveToGroup repoints the fixture placeholder at a legacy key group holding
// only the fixture key, with aclJSON and an encrypted upstream proxy on it.
func moveToGroup(t *testing.T, f *proxyFixture, aclJSON, proxyURL string) {
	t.Helper()
	encProxy := ""
	if proxyURL != "" {
		var err error
		if encProxy, err = services.EncryptUpstreamProxyURL(f.crypto, proxyURL); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []struct {
		sql  string
		args []interface{}
	}{
		{`UPDATE api_keys SET acl = ?, upstream_proxy_url = ? WHERE id = ?`, []interface{}{aclJSON, encProxy, f.apiKeyID}},
		{`INSERT INTO api_key_groups (id, service_id, name, strategy) VALUES ('grp-acl', ?, 'pool', 'failover')`, []interface{}{f.serviceID}},
		{`INSERT INTO api_key_group_members (group_id, api_key_id, priority) VALUES ('grp-acl', ?, 1)`, []interface{}{f.apiKeyID}},
		{`UPDATE placeholder_keys SET api_key_id = NULL, group_id = 'grp-acl'`, nil},
	} {
		if _, err := f.rawDB.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

// A key reached through a legacy key group is subject to its own ACL, and the
// simulator reaches the same verdict.
func TestProxyGroupKeyACLIsEnforced(t *testing.T) {
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
	}))
	t.Cleanup(upstream.Close)
	f := newProxyFixture(t, upstream.URL)
	moveToGroup(t, f, groupKeyDenyACL, "")
	h := newProxyHandler(f)

	req := withClient(httptest.NewRequest("GET", "/proxy/anthropic/v1/models", nil), f.client)
	code, body := doProxy(h, req)
	if code != http.StatusForbidden {
		t.Fatalf("status %d body %s, want 403", code, body)
	}
	if n := atomic.LoadInt32(&upstreamCalls); n != 0 {
		t.Fatalf("denied request reached upstream (%d calls)", n)
	}
	rows := denyRows(t, f)
	if len(rows) != 1 {
		t.Fatalf("deny rows: %+v", rows)
	}
	if d := wantDetail(t, rows[0], "api_key_acl"); d["layer"] != "api_key" {
		t.Fatalf("deny detail: %v", d)
	}

	// An unaffected request passes through the same group.
	req = withClient(httptest.NewRequest("GET", "/proxy/anthropic/v1/other", nil), f.client)
	if code, body := doProxy(h, req); code != http.StatusOK {
		t.Fatalf("allowed request: %d %s", code, body)
	}

	sim := newSimFor(f)
	for path, want := range map[string]string{"/v1/models": "refused", "/v1/other": "allowed"} {
		c, s := simCall(t, sim, f.placeholderID, map[string]interface{}{"mode": "raw", "method": "GET", "path": path})
		if c != 200 || s.Verdict != want {
			t.Fatalf("simulator %s: %d %+v, want %s", path, c, s, want)
		}
		if want == "refused" && (s.Kind != "api_key_acl" || s.Layer != "api_key") {
			t.Fatalf("simulator kind/layer: %+v", s)
		}
	}
}

// The upstream proxy of a key reached through a key group is used.
func TestProxyGroupKeyUsesUpstreamProxy(t *testing.T) {
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		http.Error(w, "direct upstream should not be used", http.StatusTeapot)
	}))
	t.Cleanup(upstream.Close)
	var proxyCalls int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyCalls, 1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(proxySrv.Close)

	f := newProxyFixture(t, upstream.URL)
	moveToGroup(t, f, "", proxySrv.URL)
	h := newProxyHandler(f)
	req := withClient(httptest.NewRequest("POST", "/proxy/anthropic/v1/messages", strings.NewReader(`{}`)), f.client)
	if code, body := doProxy(h, req); code != http.StatusOK {
		t.Fatalf("status %d body %s", code, body)
	}
	if atomic.LoadInt32(&proxyCalls) != 1 || atomic.LoadInt32(&upstreamCalls) != 0 {
		t.Fatalf("proxy calls %d, direct upstream calls %d; want 1 and 0", proxyCalls, upstreamCalls)
	}
}
