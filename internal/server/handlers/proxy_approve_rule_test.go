package handlers_test

import (
	"time"

	"encoding/json"
	"github.com/hackerduck/duckway/internal/database/queries"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const approvePostsACL = `{"version":"1","provider":"anthropic","rules":[{"name":"gate writes","endpoints":[
	{"method":"GET","path":"/v1/*","allow":true},
	{"method":"POST","path":"/v1/messages","approve":true}]}]}`

type approveRig struct {
	f     *proxyFixture
	calls *int32
	do    func(method, path string) (int, map[string]string)
}

func newApproveRig(t *testing.T, placeholderACL string) *approveRig {
	t.Helper()
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	f := newProxyFixture(t, upstream.URL)
	if placeholderACL != "" {
		ph, err := f.placeholderQ.GetByID(f.placeholderID)
		if err != nil {
			t.Fatal(err)
		}
		ph.PermissionConfig = &placeholderACL
		if err := f.placeholderQ.Update(ph); err != nil {
			t.Fatal(err)
		}
	}
	h := newProxyHandler(f)
	rig := &approveRig{f: f, calls: &calls}
	rig.do = func(method, path string) (int, map[string]string) {
		r := httptest.NewRequest(method, "/proxy/anthropic"+path, strings.NewReader(`{}`))
		r = withClient(r, f.client)
		code, body := doProxy(h, r)
		resp := map[string]string{}
		_ = json.Unmarshal(body, &resp)
		return code, resp
	}
	return rig
}

func TestProxyApproveRuleHoldsUntilApproved(t *testing.T) {
	rig := newApproveRig(t, approvePostsACL)

	code, resp := rig.do("POST", "/v1/messages")
	if code != http.StatusForbidden || resp["error"] != "duckway_approval_pending" || resp["approval_id"] == "" {
		t.Fatalf("approve-matched request must be held: %d %v", code, resp)
	}
	if n := atomic.LoadInt32(rig.calls); n != 0 {
		t.Fatalf("request forwarded before approval (%d upstream calls)", n)
	}
	id := resp["approval_id"]
	a, err := rig.f.approvalQ.GetByID(id)
	if err != nil || a.Status != "pending" || a.RequestInfo == nil {
		t.Fatalf("pending approval: %+v %v", a, err)
	}
	var info map[string]string
	_ = json.Unmarshal([]byte(*a.RequestInfo), &info)
	if info["method"] != "POST" || info["path"] != "/v1/messages" || info["rule"] != "gate writes (placeholder)" {
		t.Fatalf("approval must say which rule triggered it: %v", info)
	}

	// A retry while pending reuses the same approval instead of queueing another.
	if code, again := rig.do("POST", "/v1/messages"); code != http.StatusForbidden || again["approval_id"] != id {
		t.Fatalf("retry should reuse approval %s: %d %v", id, code, again)
	}
	pending, _ := rig.f.approvalQ.ListPending()
	if len(pending) != 1 {
		t.Fatalf("want 1 pending approval, got %d", len(pending))
	}

	// Approval (with a relative TTL) releases exactly that request.
	if err := rig.f.approvalQ.Approve(id, queries.ApprovalExpiry(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code, _ := rig.do("POST", "/v1/messages"); code != http.StatusOK {
		t.Fatalf("approved request should forward, got %d", code)
	}
	if n := atomic.LoadInt32(rig.calls); n != 1 {
		t.Fatalf("want 1 upstream call after approval, got %d", n)
	}
	// Allowed requests never needed approval.
	if code, _ := rig.do("GET", "/v1/models"); code != http.StatusOK {
		t.Fatalf("allow rule: %d", code)
	}
}

func TestProxyApproveRuleApprovalIsScopedToRequest(t *testing.T) {
	acl := `{"version":"1","provider":"anthropic","rules":[{"name":"w","endpoints":[
		{"method":"POST","path":"/v1/a","approve":true},{"method":"DELETE","path":"/v1/a","approve":true},{"method":"POST","path":"/v1/b","approve":true}]}]}`
	rig := newApproveRig(t, acl)
	_, resp := rig.do("POST", "/v1/a")
	if err := rig.f.approvalQ.Approve(resp["approval_id"], queries.ApprovalExpiry(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code, _ := rig.do("POST", "/v1/a"); code != http.StatusOK {
		t.Fatalf("approved signature: %d", code)
	}
	for _, rq := range [][2]string{{"DELETE", "/v1/a"}, {"POST", "/v1/b"}} {
		if code, r := rig.do(rq[0], rq[1]); code != http.StatusForbidden || r["error"] != "duckway_approval_pending" {
			t.Fatalf("%v must not be unlocked by another request's approval: %d %v", rq, code, r)
		}
	}
	// A rule approval does not satisfy token-wide approval either.
	if _, err := rig.f.approvalQ.GetValidApproval(rig.f.placeholderID); err == nil {
		t.Fatal("scoped approval must not count as token-wide approval")
	}
}

func TestProxyApproveRuleRejectedAndExpiredAreNotForwarded(t *testing.T) {
	rig := newApproveRig(t, approvePostsACL)
	_, resp := rig.do("POST", "/v1/messages")
	if err := rig.f.approvalQ.Reject(resp["approval_id"]); err != nil {
		t.Fatal(err)
	}
	code, again := rig.do("POST", "/v1/messages")
	if code != http.StatusForbidden || again["error"] != "duckway_approval_pending" || again["approval_id"] == resp["approval_id"] {
		t.Fatalf("rejected request must be held again under a new approval: %d %v", code, again)
	}
	// An approval whose TTL has run out no longer releases the request.
	if err := rig.f.approvalQ.Approve(again["approval_id"], "2000-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	if code, r := rig.do("POST", "/v1/messages"); code != http.StatusForbidden || r["error"] != "duckway_approval_pending" {
		t.Fatalf("expired approval must not forward: %d %v", code, r)
	}
	if n := atomic.LoadInt32(rig.calls); n != 0 {
		t.Fatalf("nothing may reach upstream, got %d calls", n)
	}
}

func TestProxyApproveRuleDenyBeatsApproveAcrossLayers(t *testing.T) {
	denyPost := `{"version":"1","provider":"anthropic","rules":[{"name":"no posts","endpoints":[{"method":"POST","path":"/v1/messages","allow":false}]}]}`
	// approve on the service layer, deny on the token layer (and the reverse order of evaluation).
	for name, layers := range map[string][2]string{"service approve, token deny": {approvePostsACL, denyPost}, "service deny, token approve": {denyPost, approvePostsACL}} {
		t.Run(name, func(t *testing.T) {
			rig := newApproveRig(t, layers[1])
			svc, err := rig.f.svcQ.GetByID(rig.f.serviceID)
			if err != nil {
				t.Fatal(err)
			}
			svc.DefaultACL = layers[0]
			if err := rig.f.svcQ.Update(svc); err != nil {
				t.Fatal(err)
			}
			code, resp := rig.do("POST", "/v1/messages")
			if code != http.StatusForbidden || !strings.Contains(resp["error"], "permission denied") {
				t.Fatalf("deny must beat approve: %d %v", code, resp)
			}
			if pending, _ := rig.f.approvalQ.ListPending(); len(pending) != 0 {
				t.Fatalf("a denied request must not create an approval, got %d", len(pending))
			}
			if n := atomic.LoadInt32(rig.calls); n != 0 {
				t.Fatalf("forwarded %d times", n)
			}
		})
	}
}

func TestProxyApproveRuleOnAPIKeyAndServiceLayers(t *testing.T) {
	rig := newApproveRig(t, "")
	if err := rig.f.apiKeyQ.UpdateACL(rig.f.apiKeyID, approvePostsACL); err != nil {
		t.Fatal(err)
	}
	code, resp := rig.do("POST", "/v1/messages")
	if code != http.StatusForbidden || resp["error"] != "duckway_approval_pending" {
		t.Fatalf("api_key layer approve rule: %d %v", code, resp)
	}
	a, _ := rig.f.approvalQ.GetByID(resp["approval_id"])
	if a == nil || a.RequestInfo == nil || !strings.Contains(*a.RequestInfo, "api_key") {
		t.Fatalf("approval should name the api_key layer: %+v", a)
	}
}

func TestProxyAllowOnlyConfigUnchanged(t *testing.T) {
	rig := newApproveRig(t, `{"version":"1","provider":"anthropic","rules":[{"name":"r","endpoints":[{"method":"POST","path":"/v1/messages","allow":true}],"deny_all_other":true}]}`)
	if code, _ := rig.do("POST", "/v1/messages"); code != http.StatusOK {
		t.Fatalf("allow rule must forward directly, got %d", code)
	}
	if code, resp := rig.do("DELETE", "/v1/x"); code != http.StatusForbidden || !strings.Contains(resp["error"], "permission denied") {
		t.Fatalf("deny_all_other must still deny: %d %v", code, resp)
	}
	if pending, _ := rig.f.approvalQ.ListPending(); len(pending) != 0 {
		t.Fatalf("no approval expected, got %d", len(pending))
	}
	if n := atomic.LoadInt32(rig.calls); n != 1 {
		t.Fatalf("want exactly the allowed request upstream, got %d", n)
	}
}
