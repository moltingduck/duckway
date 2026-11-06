package handlers_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

const policyAllowAllConfig = `{"version":"1","provider":"github","rules":[{"name":"all","endpoints":[{"method":"*","path":"*","allow":true}],
 "github_branch_policies":{"owner/repo":{"branch_policy":{"rules":[{"branches":"agent/**","role":"agent"},{"branches":"**","role":"read"}],"tags":{"create":[]}}}}}]}`

func (e *brEnv) restWith(method, path, body string, hdr map[string]string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.proxy.URL+"/proxy/github"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+brPlaceholder)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestBranchRulesDefaultDenyUnderPolicyThroughProxy(t *testing.T) {
	e := newBrEnv(t, policyAllowAllConfig)
	r := "/repos/OWNER/REPO"
	for _, c := range []struct{ m, p string }{
		{"POST", r + "/branches/main/rename"},
		{"POST", r + "/merge-upstream"},
		{"POST", r + "/actions/runners/registration-token"},
		{"POST", "/graphql"},
		{"DELETE", "/graphql"},
		{"POST", "/repositories/1/git/refs"},
		{"GET", "/repositories/1"},
	} {
		if code, body := e.restWith(c.m, c.p, `{"query":"mutation{x}"}`, nil); code != 403 {
			t.Errorf("%s %s: %d %s", c.m, c.p, code, body)
		}
	}
	if n := e.saw("POST /repos/owner/repo/branches") + e.saw("POST /repos/owner/repo/merge-upstream") + e.saw("POST /graphql") + e.saw("POST /repositories"); n != 0 {
		t.Fatalf("%d refused requests reached the upstream", n)
	}
	for _, c := range []struct{ m, p, b string }{
		{"POST", r + "/pulls/1/comments", `{"body":"x"}`},
		{"POST", r + "/pulls/1/comments", `{"body":"x"}`},
		{"POST", r + "/pulls/1/reviews", `{"event":"COMMENT","body":"x"}`},
		{"POST", r + "/actions/runs/7/rerun", ``},
		{"GET", "/graphql", ``},
	} {
		if code, body := e.restWith(c.m, c.p, c.b, nil); code != 200 {
			t.Errorf("%s %s should stay allowed: %d %s", c.m, c.p, code, body)
		}
	}
}

func TestBranchRulesMethodOverrideNeutralised(t *testing.T) {
	e := newBrEnv(t, v2BranchPolicyConfig)
	// A POST that the table refuses stays refused whatever the override says.
	for _, h := range []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"} {
		if code, _ := e.restWith("POST", "/repos/OWNER/REPO/pulls/1/merge", `{}`, map[string]string{h: "PUT"}); code != 403 {
			t.Errorf("POST merge with %s: %d", h, code)
		}
	}
	if e.saw("POST /repos/owner/repo/pulls/1/merge") != 0 && e.saw("POST /repos/OWNER/REPO/pulls/1/merge") != 0 {
		t.Fatal("merge reached upstream")
	}
	// The query form is refused outright for a repository with a policy.
	if code, body := e.restWith("POST", "/repos/OWNER/REPO/pulls/1/merge?_method=PUT", `{}`, nil); code != 403 || !strings.Contains(body, "method override") {
		t.Errorf("_method: %d %s", code, body)
	}
	if code, _ := e.restWith("POST", "/repos/OWNER/REPO/pulls/1/comments?_METHOD=DELETE", `{"body":"x"}`, nil); code != 403 {
		t.Errorf("upper-case _METHOD: %d", code)
	}
	// An allowed request reaches the upstream without any override header.
	hdr := map[string]string{"X-HTTP-Method-Override": "DELETE", "X-HTTP-Method": "DELETE", "X-Method-Override": "DELETE", "X-Keep": "yes"}
	if code, body := e.restWith("POST", "/repos/OWNER/REPO/pulls/1/comments", `{"body":"x"}`, hdr); code != 200 {
		t.Fatalf("issue comment: %d %s", code, body)
	}
	e.mu.Lock()
	got := e.headers["POST /repos/OWNER/REPO/pulls/1/comments"]
	if got == nil {
		got = e.headers["POST /repos/owner/repo/pulls/1/comments"]
	}
	e.mu.Unlock()
	if got == nil {
		t.Fatal("upstream saw no request")
	}
	for _, h := range []string{"X-Http-Method-Override", "X-Http-Method", "X-Method-Override"} {
		if v := got.Get(h); v != "" {
			t.Errorf("%s forwarded: %q", h, v)
		}
	}
	if got.Get("X-Keep") != "yes" {
		t.Error("unrelated header dropped")
	}
}

const approveRefusedConfig = `{"version":"1","provider":"github","rules":[{"name":"gate content writes","endpoints":[
 {"method":"PUT","path":"/repos/owner/repo/contents/*","approve":true},
 {"method":"*","path":"*","allow":true}],
 "github_branch_policies":{"owner/repo":{"branch_policy":{"rules":[{"branches":"agent/**","role":"agent"},{"branches":"**","role":"read"}],"tags":{"create":[]}}}}}]}`

func TestBranchRulesDefiniteRefusalSkipsApprovalHold(t *testing.T) {
	e := newBrEnv(t, approveRefusedConfig)
	count := func() int {
		var n int
		if err := e.f.rawDB.QueryRow(`SELECT COUNT(*) FROM approvals`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	code, body := e.restWith("PUT", "/repos/OWNER/REPO/contents/f.txt", `{"message":"m","content":"eA==","branch":"somewhere"}`, nil)
	if code != 403 || strings.Contains(body, "approval") || !strings.Contains(body, "duckway:") {
		t.Fatalf("refused ref must be a plain 403: %d %s", code, body)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d approval rows for a definitely refused request", n)
	}
	// An allowed branch still goes through the approval flow.
	code, body = e.restWith("PUT", "/repos/OWNER/REPO/contents/f.txt", `{"message":"m","content":"eA==","branch":"agent/x"}`, nil)
	var resp map[string]string
	_ = json.Unmarshal([]byte(body), &resp)
	if code != 403 || resp["error"] != "duckway_approval_pending" || count() != 1 {
		t.Fatalf("allowed branch must be held: %d %s approvals=%d", code, body, count())
	}
	if e.saw("PUT ") != 0 {
		t.Fatal("held request forwarded")
	}
}

func TestBranchRulesOversizedGuardBodyIsLogged(t *testing.T) {
	e := newBrEnv(t, v2BranchPolicyConfig)
	big := strings.Repeat("x", 17<<20)
	code, _ := e.restWith("POST", "/repos/OWNER/REPO/pulls", `{"base":"main","title":"`+big+`"}`, nil)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", code)
	}
	var n int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := e.f.rawDB.QueryRow(`SELECT COUNT(*) FROM request_log WHERE decision = ? AND method = 'POST' AND path LIKE '%/pulls'`, queries.DecisionACLDenied).Scan(&n); err == nil && n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("413 refusal not recorded in the request log")
}

func TestBranchRulesOversizeNamesThroughProxy(t *testing.T) {
	e := newBrEnv(t, v2BranchPolicyConfig)
	start := time.Now()
	big := strings.Repeat("a", 1<<20)
	for _, c := range []struct{ m, p, b string }{
		{"PUT", "/repos/OWNER/REPO/contents/f", `{"branch":"agent/` + big + `","message":"m","content":"eA=="}`},
		{"POST", "/repos/OWNER/REPO/actions/workflows/ci.yml/dispatches", `{"ref":"ci/` + big + `"}`},
		{"POST", "/repos/OWNER/REPO/releases", `{"tag_name":"v` + big + `"}`},
	} {
		if code, _ := e.restWith(c.m, c.p, c.b, nil); code != 403 {
			t.Errorf("%s %s: %d", c.m, c.p, code)
		}
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
}
