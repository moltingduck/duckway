package handlers_test

import (
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type pathRig struct {
	f      *proxyFixture
	do     func(method, rawPath string) int
	doBody func(method, rawPath, body string) (int, map[string]string)
	seen   func() []string
}

func newPathRig(t *testing.T, acl string) *pathRig {
	t.Helper()
	var mu sync.Mutex
	var uris []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uris = append(uris, r.Method+" "+r.RequestURI)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	f := newProxyFixture(t, upstream.URL)
	if acl != "" {
		ph, err := f.placeholderQ.GetByID(f.placeholderID)
		if err != nil {
			t.Fatal(err)
		}
		ph.PermissionConfig = &acl
		if err := f.placeholderQ.Update(ph); err != nil {
			t.Fatal(err)
		}
	}
	h := newProxyHandler(f)
	return &pathRig{f: f,
		doBody: func(method, rawPath, body string) (int, map[string]string) {
			r := httptest.NewRequest(method, "/proxy/anthropic"+rawPath, strings.NewReader(body))
			r = withClient(r, f.client)
			code, b := doProxy(h, r)
			m := map[string]string{}
			_ = json.Unmarshal(b, &m)
			return code, m
		},
		do: func(method, rawPath string) int {
			r := httptest.NewRequest(method, "/proxy/anthropic"+rawPath, strings.NewReader(`{}`))
			r = withClient(r, f.client)
			code, _ := doProxy(h, r)
			return code
		},
		seen: func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), uris...) },
	}
}

// %3F / %23 decode to ? and # in r.URL.Path: the ACL would judge one path while
// upstream parsed a different path plus a query or fragment.
func TestProxyRejectsSmuggledPathCharacters(t *testing.T) {
	rig := newPathRig(t, "")
	for _, p := range []string{"/v1/a%3Fx=1", "/v1/a%23frag", "/v1/a%00b", "/v1/a%0Ab", "/v1/a%0db", "/v1/a%7F", "/v1/a%2Fb", "/v1/a%2fb", "/v1/a%5Cb"} {
		if code := rig.do("GET", p); code != http.StatusBadRequest {
			t.Errorf("%s => %d, want 400", p, code)
		}
	}
	if n := len(rig.seen()); n != 0 {
		t.Fatalf("rejected paths reached upstream: %v", rig.seen())
	}
}

// Legitimate encoded paths still work and upstream receives exactly the
// escaped form of the path the ACL evaluated.
func TestProxyForwardsLegitimateEncodedPathsExactly(t *testing.T) {
	rig := newPathRig(t, "")
	cases := []struct{ in, want string }{
		{"/v1/a%20b", "GET /v1/a%20b"},
		{"/v1/%E2%9C%93.txt", "GET /v1/%E2%9C%93.txt"},
		{"/v1/100%25", "GET /v1/100%25"},
		{"/v1/plain?x=1&y=%3F", "GET /v1/plain?x=1&y=%3F"},
		{"/v1/a%2Bb", "GET /v1/a+b"},
	}
	for _, c := range cases {
		before := len(rig.seen())
		if code := rig.do("GET", c.in); code != http.StatusOK {
			t.Fatalf("%s => %d", c.in, code)
		}
		got := rig.seen()
		if len(got) != before+1 || got[len(got)-1] != c.want {
			t.Errorf("%s: upstream saw %v, want %q", c.in, got[before:], c.want)
		}
	}
}

// Lower-case and exotic methods must not slip past method-based rules.
func TestProxyUppercasesMethodForACLAndUpstream(t *testing.T) {
	rig := newPathRig(t, approvePostsACL)
	if code := rig.do("post", "/v1/messages"); code != http.StatusForbidden {
		t.Fatalf("lower-case post must still be held for approval, got %d", code)
	}
	if n := len(rig.seen()); n != 0 {
		t.Fatalf("held request reached upstream: %v", rig.seen())
	}
	if code := rig.do("get", "/v1/models"); code != http.StatusOK {
		t.Fatalf("get => %d", code)
	}
	if got := rig.seen(); len(got) != 1 || !strings.HasPrefix(got[0], "GET ") {
		t.Fatalf("upstream must see an upper-case method: %v", got)
	}
}

// A scoped approval covers exactly one request: method, path, query and body.
func TestApprovalScopeCoversPathQueryAndBody(t *testing.T) {
	rig := newPathRig(t, approvePostsACL)
	_, a := rig.doBody("POST", "/v1/messages?x=1", `{"a":1}`)
	if a["approval_id"] == "" {
		t.Fatalf("expected a held request: %v", a)
	}
	_, same := rig.doBody("POST", "/v1/messages?x=1", `{"a":1}`)
	if same["approval_id"] != a["approval_id"] {
		t.Fatalf("identical retry must reuse the approval: %v vs %v", same, a)
	}
	for name, c := range map[string][2]string{"query": {"/v1/messages?x=2", `{"a":1}`}, "body": {"/v1/messages?x=1", `{"a":2}`}, "no query": {"/v1/messages", `{"a":1}`}} {
		_, other := rig.doBody("POST", c[0], c[1])
		if other["approval_id"] == "" || other["approval_id"] == a["approval_id"] {
			t.Errorf("%s change must need its own approval: %v", name, other)
		}
	}
	if err := rig.f.approvalQ.Approve(a["approval_id"], queries.ApprovalExpiry(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code, _ := rig.doBody("POST", "/v1/messages?x=1", `{"a":1}`); code != http.StatusOK {
		t.Fatalf("the approved exact request forwards, got %d", code)
	}
	if code, _ := rig.doBody("POST", "/v1/messages?x=1", `{"a":2}`); code == http.StatusOK {
		t.Fatal("approval must not unlock a different body")
	}
	if code, _ := rig.doBody("POST", "/v1/messages?x=9", `{"a":1}`); code == http.StatusOK {
		t.Fatal("approval must not unlock a different query")
	}
}

// Concurrent identical requests collapse onto one pending approval.
func TestConcurrentIdenticalApproveRequestsShareOnePending(t *testing.T) {
	rig := newPathRig(t, approvePostsACL)
	var wg sync.WaitGroup
	var bad int32
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, m := rig.doBody("POST", "/v1/messages?q=1", `{"z":true}`)
			if code != http.StatusForbidden || m["approval_id"] == "" {
				atomic.AddInt32(&bad, 1)
				return
			}
			ids <- m["approval_id"]
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("two approvals for one request: %s vs %s", first, id)
		}
	}
	pending, _ := rig.f.approvalQ.ListPending()
	if bad != 0 || len(pending) != 1 {
		t.Fatalf("bad=%d pending=%d, want 0 and exactly 1", bad, len(pending))
	}
}
