package handlers_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/hackerduck/duckway/internal/server/services"
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
	notif  *countNotifier
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
	notif := &countNotifier{}
	h := newProxyHandler(f).WithApprovalNotifier(notif)
	return &pathRig{f: f, notif: notif,
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

func (rig *pathRig) enableEncodedSlash(t *testing.T) {
	t.Helper()
	svc, err := rig.f.svcQ.GetByID(rig.f.serviceID)
	if err != nil {
		t.Fatal(err)
	}
	svc.AllowEncodedSlash = true
	if err := rig.f.svcQ.Update(svc); err != nil {
		t.Fatal(err)
	}
}

// With the per-service flag on, %2F stays inside its segment for the ACL and
// upstream receives the same escaped path; %5C, ? and # are still refused.
func TestEncodedSlashAllowedPerServiceKeepsSegment(t *testing.T) {
	const acl = `{"version":"1","provider":"anthropic","rules":[{"name":"deny exact a/b","endpoints":[
		{"method":"GET","path":"/v1/projects/a/b","allow":false}]}]}`
	rig := newPathRig(t, acl)
	// Flag off (default): 400, nothing forwarded.
	if code := rig.do("GET", "/v1/projects/a%2Fb"); code != http.StatusBadRequest {
		t.Fatalf("flag off: %d, want 400", code)
	}
	if len(rig.seen()) != 0 {
		t.Fatalf("flag off reached upstream: %v", rig.seen())
	}
	rig.enableEncodedSlash(t)
	// The rule for /projects/a/b must NOT match /projects/a%2Fb (one segment).
	if code := rig.do("GET", "/v1/projects/a%2Fb"); code != http.StatusOK {
		t.Fatalf("a%%2Fb must not match the a/b deny rule: %d", code)
	}
	if got := rig.seen(); len(got) != 1 || got[0] != "GET /v1/projects/a%2Fb" {
		t.Fatalf("upstream must receive the escaped path: %v", got)
	}
	// The real two-segment path is still denied.
	if code := rig.do("GET", "/v1/projects/a/b"); code != http.StatusForbidden {
		t.Fatalf("a/b must still be denied: %d", code)
	}
	// Still always refused with the flag on.
	for _, p := range []string{"/v1/p%5Cq", "/v1/p%3Fq", "/v1/p%23q", "/v1/p%00q", "/v1/p%2Fq%0Ar"} {
		if code := rig.do("GET", p); code != http.StatusBadRequest {
			t.Errorf("%s with flag on: %d, want 400", p, code)
		}
	}
	// A literal %2F text (from %252F) next to an encoded slash is ambiguous.
	if code := rig.do("GET", "/v1/p%2Fq%252F"); code != http.StatusBadRequest {
		t.Errorf("ambiguous segment: %d, want 400", code)
	}
}

func TestEncodedSlashMatchesSingleSegmentWildcard(t *testing.T) {
	const acl = `{"version":"1","provider":"anthropic","rules":[{"name":"only projects","endpoints":[
		{"method":"GET","path":"/v1/projects/*","allow":true}],"deny_all_other":true}]}`
	rig := newPathRig(t, acl)
	rig.enableEncodedSlash(t)
	if code := rig.do("GET", "/v1/projects/a%2Fb"); code != http.StatusOK {
		t.Fatalf("/projects/* must match /projects/a%%2Fb: %d", code)
	}
	if code := rig.do("GET", "/v1/other/a%2Fb"); code != http.StatusForbidden {
		t.Fatalf("outside the allowed prefix: %d", code)
	}
	if got := rig.seen(); len(got) != 1 || got[0] != "GET /v1/projects/a%2Fb" {
		t.Fatalf("upstream saw %v", got)
	}
}

type countNotifier struct {
	mu    sync.Mutex
	calls []services.ApprovalNotification
}

func (c *countNotifier) NotifyApprovalNeeded(n services.ApprovalNotification) {
	c.mu.Lock()
	c.calls = append(c.calls, n)
	c.mu.Unlock()
}

func (c *countNotifier) all() []services.ApprovalNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]services.ApprovalNotification(nil), c.calls...)
}

// Concurrent identical requests produce exactly one notification.
func TestConcurrentIdenticalApproveRequestsNotifyOnce(t *testing.T) {
	rig := newPathRig(t, approvePostsACL)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); rig.doBody("POST", "/v1/messages?q=1", `{"z":true}`) }()
	}
	wg.Wait()
	if n := len(rig.notif.all()); n != 1 {
		t.Fatalf("notifications=%d, want exactly 1", n)
	}
}

// What the admin sees: the query string and a short body hash, in the stored
// request_info and in the notification.
func TestApprovalShowsQueryAndShortBodyHash(t *testing.T) {
	rig := newPathRig(t, approvePostsACL)
	_, m := rig.doBody("POST", "/v1/messages?mode=live&x=1", `{"amount":100}`)
	a, err := rig.f.approvalQ.GetByID(m["approval_id"])
	if err != nil || a.RequestInfo == nil {
		t.Fatalf("approval: %v", err)
	}
	var info map[string]string
	_ = json.Unmarshal([]byte(*a.RequestInfo), &info)
	if info["query"] != "mode=live&x=1" || len(info["body_hash"]) != 12 || info["path"] != "/v1/messages" {
		t.Fatalf("request_info=%v", info)
	}
	sum := sha256.Sum256([]byte(`{"amount":100}`))
	if info["body_hash"] != hex.EncodeToString(sum[:])[:12] {
		t.Fatalf("body hash is not the SHA-256 prefix: %s", info["body_hash"])
	}
	if strings.Contains(*a.RequestInfo, "amount") {
		t.Fatal("the body itself must never be stored")
	}
	n := rig.notif.all()
	if len(n) != 1 || n[0].Query != "mode=live&x=1" || n[0].BodyHash != info["body_hash"] {
		t.Fatalf("notification=%+v", n)
	}
	// A different body changes the hash the admin sees.
	_, m2 := rig.doBody("POST", "/v1/messages?mode=live&x=1", `{"amount":999}`)
	a2, _ := rig.f.approvalQ.GetByID(m2["approval_id"])
	var info2 map[string]string
	_ = json.Unmarshal([]byte(*a2.RequestInfo), &info2)
	if info2["body_hash"] == info["body_hash"] {
		t.Fatal("different bodies must show different hashes")
	}
}
