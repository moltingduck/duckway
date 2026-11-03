package services

import (
	"encoding/json"
	"strings"
	"testing"
)

const approveWrites = `{"version":"1","provider":"x","rules":[{"name":"writes","endpoints":[
	{"method":"GET","path":"/v1/*","allow":true},
	{"method":"POST","path":"/v1/*","approve":true},
	{"method":"DELETE","path":"/v1/*","approve":true}],"deny_all_other":false}]}`

func TestApproveEffect(t *testing.T) {
	pc := NewPermissionChecker()
	cases := []struct {
		method, path           string
		allowed, needsApproval bool
	}{
		{"GET", "/v1/models", true, false},
		{"POST", "/v1/models", false, true},
		{"DELETE", "/v1/x", false, true},
		{"PUT", "/v1/x", true, false}, // unmatched: default allow, like any rule list
		{"POST", "/other", true, false},
	}
	for _, c := range cases {
		got := pc.Check(approveWrites, "ph", c.method, c.path, nil)
		if got.Allowed != c.allowed || got.NeedsApproval != c.needsApproval {
			t.Errorf("%s %s: got %+v", c.method, c.path, got)
		}
		if c.needsApproval && (got.RuleName != "writes" || !strings.Contains(got.Reason, "requires approval")) {
			t.Errorf("%s %s: approval reason %+v", c.method, c.path, got)
		}
		ex := pc.Explain(approveWrites, c.method, c.path, nil)
		if ex.Allowed != got.Allowed || ex.NeedsApproval != got.NeedsApproval || ex.Reason != got.Reason {
			t.Errorf("%s %s: Explain %+v != Check %+v", c.method, c.path, ex, got)
		}
		if c.needsApproval && (ex.RuleIndex != 0 || ex.RuleName != "writes") {
			t.Errorf("%s %s: explain rule %+v", c.method, c.path, ex)
		}
	}
}

func TestApproveFirstMatchOrderAndDenyWins(t *testing.T) {
	pc := NewPermissionChecker()
	// Deny in the same rule beats approve whatever the endpoint order.
	same := `{"version":"1","provider":"x","rules":[{"name":"r","endpoints":[{"method":"POST","path":"/v1/*","approve":true},{"method":"POST","path":"/v1/secret","allow":false}]}]}`
	if r := pc.Check(same, "p", "POST", "/v1/secret", nil); r.Allowed || r.NeedsApproval {
		t.Fatalf("deny must beat approve in one rule: %+v", r)
	}
	if r := pc.Check(same, "p", "POST", "/v1/other", nil); !r.NeedsApproval {
		t.Fatalf("approve expected: %+v", r)
	}
	// First matching rule decides: an earlier allow rule shadows a later approve.
	ordered := `{"version":"1","provider":"x","rules":[
		{"name":"first","endpoints":[{"method":"POST","path":"/v1/free","allow":true}]},
		{"name":"second","endpoints":[{"method":"POST","path":"/v1/*","approve":true}]}]}`
	if r := pc.Check(ordered, "p", "POST", "/v1/free", nil); !r.Allowed || r.NeedsApproval {
		t.Fatalf("earlier allow should win: %+v", r)
	}
	if r := pc.Check(ordered, "p", "POST", "/v1/else", nil); !r.NeedsApproval || r.RuleName != "second" {
		t.Fatalf("later approve expected: %+v", r)
	}
	// An earlier approve rule shadows a later deny rule (first match decides).
	rev := `{"version":"1","provider":"x","rules":[
		{"name":"a","endpoints":[{"method":"POST","path":"/v1/*","approve":true}]},
		{"name":"b","endpoints":[{"method":"POST","path":"/v1/*","allow":false}]}]}`
	if r := pc.Check(rev, "p", "POST", "/v1/x", nil); !r.NeedsApproval {
		t.Fatalf("first match should decide: %+v", r)
	}
}

func TestApproveBodyConstraintsStillApply(t *testing.T) {
	pc := NewPermissionChecker()
	cfg := `{"version":"1","provider":"x","rules":[{"name":"m","endpoints":[{"method":"POST","path":"/v1/chat","approve":true,"constraints":{"body":{"model":{"oneOf":["a"]}}}}]}]}`
	if !RequestBodyRequiredForPermission(cfg, "POST", "/v1/chat") {
		t.Fatal("approve endpoint with body constraints must require the body")
	}
	if r := pc.Check(cfg, "p", "POST", "/v1/chat", []byte(`{"model":"a"}`)); !r.NeedsApproval {
		t.Fatalf("matching body should need approval: %+v", r)
	}
	if r := pc.Check(cfg, "p", "POST", "/v1/chat", []byte(`{"model":"b"}`)); r.Allowed || r.NeedsApproval {
		t.Fatalf("constraint failure must deny, not approve: %+v", r)
	}
}

func TestApproveRateLimitAndValidation(t *testing.T) {
	pc := NewPermissionChecker()
	cfg := `{"version":"1","provider":"x","rules":[{"name":"rl","rate_limit":{"requests_per_minute":1},"endpoints":[{"method":"POST","path":"/v1/*","approve":true}]}]}`
	if r := pc.Check(cfg, "p", "POST", "/v1/a", nil); !r.NeedsApproval {
		t.Fatalf("first: %+v", r)
	}
	if r := pc.Check(cfg, "p", "POST", "/v1/a", nil); r.Allowed || r.NeedsApproval || !strings.Contains(r.Reason, "rate limit") {
		t.Fatalf("second should be rate limited: %+v", r)
	}
	if ex := pc.Explain(cfg, "POST", "/v1/a", nil); !ex.NeedsApproval || !ex.RateLimited {
		t.Fatalf("explain should flag approval + rate limit without counting: %+v", ex)
	}
	both := `{"version":"1","provider":"x","rules":[{"endpoints":[{"method":"GET","path":"/a","allow":true,"approve":true}]}]}`
	if err := ValidatePermissionConfig(both); err == nil {
		t.Fatal("allow+approve must be rejected")
	}
}

// Golden: configs written before the approve effect existed must decide
// exactly as before and never report NeedsApproval.
func TestLegacyConfigsGolden(t *testing.T) {
	pc := NewPermissionChecker()
	legacy := map[string]string{
		"denyDelete":   `{"version":"1","provider":"x","rules":[{"name":"d","endpoints":[{"method":"DELETE","path":"/v1/*","allow":false}],"deny_all_other":false}]}`,
		"allowOnly":    `{"version":"1","provider":"x","rules":[{"name":"r","endpoints":[{"method":"GET","path":"/v1/*","allow":true}],"deny_all_other":true}]}`,
		"missingAllow": `{"version":"1","provider":"x","rules":[{"name":"m","endpoints":[{"method":"GET","path":"/v1/*"}]}]}`,
		"empty":        ``,
	}
	type want struct{ allowed bool }
	golden := map[string][]want{
		// order: GET /v1/a, DELETE /v1/a, POST /other
		"denyDelete":   {{true}, {false}, {true}},
		"allowOnly":    {{true}, {false}, {false}},
		"missingAllow": {{false}, {true}, {true}},
		"empty":        {{true}, {true}, {true}},
	}
	reqs := [][2]string{{"GET", "/v1/a"}, {"DELETE", "/v1/a"}, {"POST", "/other"}}
	for name, cfg := range legacy {
		for i, rq := range reqs {
			r := pc.Check(cfg, "p", rq[0], rq[1], nil)
			if r.NeedsApproval || r.RuleName != "" {
				t.Errorf("%s %v: legacy config reported approval: %+v", name, rq, r)
			}
			if r.Allowed != golden[name][i].allowed {
				t.Errorf("%s %v: allowed=%v want %v (%s)", name, rq, r.Allowed, golden[name][i].allowed, r.Reason)
			}
		}
	}
}

func TestExplainParityWithApprove(t *testing.T) {
	pc := NewPermissionChecker()
	for _, c := range []string{approveWrites, ``, `{"version":"1","provider":"x","rules":[{"name":"d","endpoints":[{"method":"POST","path":"/v1/*","approve":true}],"deny_all_other":true}]}`} {
		for _, rq := range [][2]string{{"GET", "/v1/a"}, {"POST", "/v1/a"}, {"DELETE", "/zzz"}} {
			got, ex := pc.Check(c, "p", rq[0], rq[1], nil), pc.Explain(c, rq[0], rq[1], nil)
			if got.Allowed != ex.Allowed || got.Reason != ex.Reason || got.NeedsApproval != ex.NeedsApproval {
				t.Errorf("%q %v: Check=%+v Explain=%+v", c, rq, got, ex)
			}
		}
	}
}

func TestGitHubMinterAcceptsCapabilityRuleAndRejectsWider(t *testing.T) {
	rule, err := CompileGitHubCapabilityRule(GitHubCapabilityRows{
		"Acme/App": {"contents": "write", "pull_requests": "read", "issues": "none", "actions": "write"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := PermissionConfig{Version: "1", Provider: "github", Rules: []PermissionRule{rule}}
	raw := mustJSON(t, cfg)
	if err := ValidateGitHubRepoScopePermissionConfig(raw); err != nil {
		t.Fatalf("builder output rejected for minter: %v", err)
	}
	if err := ValidatePermissionConfig(raw); err != nil {
		t.Fatal(err)
	}
	pc := NewPermissionChecker()
	allow := [][2]string{
		{"GET", "/Acme/App.git/info/refs"}, {"POST", "/acme/app.git/git-upload-pack"}, {"POST", "/Acme/App.git/git-receive-pack"},
		{"GET", "/repos/Acme/App"}, {"PUT", "/repos/Acme/App/contents/a/b.txt"}, {"GET", "/repos/Acme/App/pulls/3"},
		{"POST", "/repos/Acme/App/actions/workflows/ci.yml/dispatches"},
	}
	deny := [][2]string{
		{"POST", "/repos/Acme/App/pulls"}, {"GET", "/repos/Acme/App/issues"}, {"GET", "/repos/Other/App"},
		{"POST", "/Other/App.git/git-receive-pack"}, {"DELETE", "/repos/Acme/App/pulls/1"}, {"GET", "/user"},
	}
	for _, r := range allow {
		if got := pc.Check(raw, "p", r[0], r[1], nil); !got.Allowed {
			t.Errorf("%v should be allowed: %+v", r, got)
		}
	}
	for _, r := range deny {
		if got := pc.Check(raw, "p", r[0], r[1], nil); got.Allowed {
			t.Errorf("%v should be denied", r)
		}
	}
	// Hand-written rules may precede the builder rule; deny_all_other must be last.
	hand := PermissionRule{Name: "ops", Endpoints: []EndpointRule{{Method: "DELETE", Path: "/repos/Acme/App/contents/*", Approve: true}}}
	if err := ValidateGitHubRepoScopePermissionConfig(mustJSON(t, PermissionConfig{Version: "1", Provider: "github", Rules: []PermissionRule{hand, rule}})); err != nil {
		t.Fatalf("hand-written approve rule before builder rule: %v", err)
	}
	if err := ValidateGitHubRepoScopePermissionConfig(mustJSON(t, PermissionConfig{Version: "1", Provider: "github", Rules: []PermissionRule{rule, hand}})); err == nil {
		t.Fatal("builder rule followed by a rule without deny_all_other must be rejected")
	}
	if err := ValidateGitHubRepoScopePermissionConfig(`{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"GET","path":"/repos/Acme/*/issues","allow":true}],"deny_all_other":true}]}`); err == nil {
		t.Fatal("wildcard repository must be rejected")
	}
	if err := ValidateGitHubRepoScopePermissionConfig(`{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"GET","path":"/user","allow":true}],"deny_all_other":true}]}`); err == nil {
		t.Fatal("non-repository path must be rejected")
	}
}

func TestGitHubCapabilityRecognizeRoundTrip(t *testing.T) {
	rows := GitHubCapabilityRows{"o/r": {"contents": "read", "issues": "write"}, "o/none": {"contents": "none"}}
	rule, err := CompileGitHubCapabilityRule(rows)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := RecognizeGitHubCapabilityRule(rule)
	if !ok || got["o/r"]["issues"] != "write" || len(got) != 2 {
		t.Fatalf("recognize: %v %v", got, ok)
	}
	// Tampered endpoints are not builder output any more.
	rule.Endpoints = append(rule.Endpoints, EndpointRule{Method: "POST", Path: "/repos/o/r/git/*", Allow: true})
	if _, ok := RecognizeGitHubCapabilityRule(rule); ok {
		t.Fatal("hand-edited rule must not be recognised")
	}
	if _, err := CompileGitHubCapabilityRule(GitHubCapabilityRows{"bad repo": {"contents": "read"}}); err == nil {
		t.Fatal("invalid repo accepted")
	}
	if _, err := CompileGitHubCapabilityRule(GitHubCapabilityRows{"o/r": {"admin": "write"}}); err == nil {
		t.Fatal("unknown capability accepted")
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
