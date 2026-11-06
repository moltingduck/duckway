package services

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type branchVectorFile struct {
	Glob []struct {
		Pattern string `json:"pattern"`
		Branch  string `json:"branch"`
		Match   bool   `json:"match"`
	} `json:"glob"`
	Select []struct {
		Name          string             `json:"name"`
		DefaultBranch string             `json:"default_branch"`
		Rules         []GitHubBranchRule `json:"rules"`
		Cases         []struct {
			Branch       string  `json:"branch"`
			Rule         *string `json:"rule"`
			Role         string  `json:"role"`
			RunWorkflows *bool   `json:"run_workflows"`
		} `json:"cases"`
	} `json:"select"`
	Tags []struct {
		Create  []string `json:"create"`
		Tag     string   `json:"tag"`
		Allowed bool     `json:"allowed"`
	} `json:"tags"`
}

func loadBranchVectors(t *testing.T) branchVectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/github_branch_match_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v branchVectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestGitHubBranchVectors(t *testing.T) {
	v := loadBranchVectors(t)
	for _, c := range v.Glob {
		if got := GitHubGlobMatch(c.Pattern, c.Branch); got != c.Match {
			t.Errorf("glob %q vs %q = %v, want %v", c.Pattern, c.Branch, got, c.Match)
		}
	}
	for _, s := range v.Select {
		p := GitHubBranchPolicy{Rules: s.Rules}
		for _, c := range s.Cases {
			rule, matched := p.SelectRule(c.Branch, s.DefaultBranch)
			role := rule.Role
			if !matched {
				role = GitHubRoleRead
			}
			if c.Rule == nil {
				if matched {
					t.Errorf("%s/%s: matched %q, want none", s.Name, c.Branch, rule.Branches)
				}
			} else if !matched || rule.Branches != *c.Rule {
				t.Errorf("%s/%s: rule %q (matched=%v), want %q", s.Name, c.Branch, rule.Branches, matched, *c.Rule)
			}
			if role != c.Role {
				t.Errorf("%s/%s: role %q, want %q", s.Name, c.Branch, role, c.Role)
			}
			if matched && c.RunWorkflows != nil && rule.RunWorkflows != *c.RunWorkflows {
				t.Errorf("%s/%s: run_workflows %v, want %v", s.Name, c.Branch, rule.RunWorkflows, *c.RunWorkflows)
			}
		}
	}
	for _, c := range v.Tags {
		p := GitHubBranchPolicy{Tags: GitHubTagPolicy{Create: c.Create}}
		if got := p.TagCreateAllowed(c.Tag); got != c.Allowed {
			t.Errorf("tag %q with %v = %v, want %v", c.Tag, c.Create, got, c.Allowed)
		}
	}
}

func TestSelectRuleUnknownDefaultSkipsDefaultRule(t *testing.T) {
	p := GitHubBranchPolicy{Rules: []GitHubBranchRule{
		{Branches: "@default", Role: "agent"}, {Branches: "**", Role: "read"},
	}}
	if r, _ := p.SelectRule("main", ""); r.Role != "read" {
		t.Fatalf("role = %s", r.Role)
	}
}

func v2cfg(repo string) string {
	return `{"version":"2","provider":"github","repositories":{"acme/api":` + repo + `}}`
}

func TestBranchPolicyValidation(t *testing.T) {
	caps := `"capabilities":{"clone":true,"push":true,"pull_requests_read":true,"pull_requests_write":true,"actions_read":true,"workflow_dispatch":true,"pull_requests_merge":true}`
	rules := `"branch_policy":{"rules":[{"branches":"agent/**","role":"agent","run_workflows":true}],"tags":{"create":["v*"]}}`
	ok := v2cfg(`{` + caps + `,"workflow_allowlist":["ci.yml"],` + rules + `}`)
	cfg, err := ParsePermissionConfig(ok)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	var sawDispatch, sawMerge bool
	for _, ep := range cfg.Rules[0].Endpoints {
		if strings.HasSuffix(ep.Path, "/dispatches") {
			sawDispatch = true
			if ep.Constraints.Body["ref"].Matches != nil || !ep.Constraints.Body["ref"].Required {
				t.Fatalf("dispatch ref constraint = %+v", ep.Constraints.Body["ref"])
			}
		}
		if ep.Method == "PUT" && strings.HasSuffix(ep.Path, "/merge") {
			sawMerge = true
		}
	}
	if !sawDispatch || !sawMerge {
		t.Fatalf("dispatch=%v merge=%v", sawDispatch, sawMerge)
	}

	many := make([]string, 0, 51)
	for i := 0; i < 51; i++ {
		many = append(many, `{"branches":"b`+strings.Repeat("x", i)+`","role":"read"}`)
	}
	tags := make([]string, 21)
	for i := range tags {
		tags[i] = `"t` + strings.Repeat("x", i) + `"`
	}
	base := `"capabilities":{"clone":true,"push":true,"pull_requests_read":true,"pull_requests_write":true}`
	bad := map[string]string{
		"too many rules":       `{` + base + `,"branch_policy":{"rules":[` + strings.Join(many, ",") + `]}}`,
		"too many tags":        `{` + base + `,"branch_policy":{"rules":[],"tags":{"create":[` + strings.Join(tags, ",") + `]}}}`,
		"long pattern":         `{` + base + `,"branch_policy":{"rules":[{"branches":"` + strings.Repeat("a", 201) + `","role":"read"}]}}`,
		"empty pattern":        `{` + base + `,"branch_policy":{"rules":[{"branches":"","role":"read"}]}}`,
		"dotdot":               `{` + base + `,"branch_policy":{"rules":[{"branches":"a/../b","role":"read"}]}}`,
		"leading slash":        `{` + base + `,"branch_policy":{"rules":[{"branches":"/a","role":"read"}]}}`,
		"bad char":             `{` + base + `,"branch_policy":{"rules":[{"branches":"a b","role":"read"}]}}`,
		"bad role":             `{` + base + `,"branch_policy":{"rules":[{"branches":"a","role":"admin"}]}}`,
		"duplicate":            `{` + base + `,"branch_policy":{"rules":[{"branches":"a","role":"read"},{"branches":"refs/heads/a","role":"agent"}]}}`,
		"run without disp":     `{` + base + `,"branch_policy":{"rules":[{"branches":"a","role":"agent","run_workflows":true}]}}`,
		"allowlist conflict":   `{` + base + `,"ref_allowlist":["main"],"branch_policy":{"rules":[]}}`,
		"default in tags":      `{` + base + `,"branch_policy":{"rules":[],"tags":{"create":["@default"]}}}`,
		"unknown field":        `{` + base + `,"branch_policy":{"rules":[],"extra":1}}`,
		"merge without prw":    `{"capabilities":{"clone":true,"pull_requests_read":true,"pull_requests_merge":true}}`,
		"approve without prw":  `{"capabilities":{"clone":true,"pull_requests_read":true,"pull_requests_approve":true}}`,
		"dispatch no run rule": `{"capabilities":{"clone":true,"actions_read":true,"workflow_dispatch":true},"workflow_allowlist":["ci.yml"],"branch_policy":{"rules":[{"branches":"a","role":"agent"}]}}`,
	}
	for name, repo := range bad {
		if _, err := ParsePermissionConfig(v2cfg(repo)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Legacy still works.
	legacy := v2cfg(`{"capabilities":{"clone":true,"actions_read":true,"workflow_dispatch":true},"workflow_allowlist":["ci.yml"],"ref_allowlist":["main"]}`)
	if _, err := ParsePermissionConfig(legacy); err != nil {
		t.Fatalf("legacy: %v", err)
	}
}

func TestBranchPolicyNormalizesPrefix(t *testing.T) {
	cfg := v2cfg(`{"capabilities":{"clone":true,"push":true},"branch_policy":{"rules":[{"branches":"refs/heads/agent/**","role":"agent"}],"tags":{"create":["refs/tags/v*"]}}}`)
	pol, err := GitHubBranchPoliciesFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := pol["acme/api"]
	if p.BranchPolicy.Rules[0].Branches != "agent/**" || p.BranchPolicy.Tags.Create[0] != "v*" {
		t.Fatalf("%+v", p)
	}
}

func TestBuilderBranchPolicies(t *testing.T) {
	mk := func(rows, pol string) string {
		return `{"version":"1","provider":"github","rules":[{"name":"GitHub repositories","deny_all_other":true,
		"endpoints":[{"method":"GET","path":"/repos/acme/api","allow":true}],
		"github_capabilities":` + rows + `,"github_branch_policies":` + pol + `}]}`
	}
	rowsW := `{"acme/api":{"contents":"write","pull_requests":"write","actions":"write"}}`
	rowsR := `{"acme/api":{"contents":"write","pull_requests":"read","actions":"read"}}`
	okPol := `{"acme/api":{"branch_policy":{"rules":[{"branches":"ci/**","role":"agent","run_workflows":true}],"tags":{"create":[]}},"pull_requests_merge":true}}`
	if err := ValidatePermissionConfig(mk(rowsW, okPol)); err != nil {
		t.Fatalf("valid builder rejected: %v", err)
	}
	pols, err := GitHubBranchPoliciesFromConfig(mk(rowsW, `{"Acme/Api":`+strings.TrimPrefix(okPol, `{"acme/api":`)))
	if err != nil || !pols["acme/api"].PullRequestsMerge {
		t.Fatalf("policies %+v err %v", pols, err)
	}
	for name, cfg := range map[string]string{
		"run needs actions write": mk(rowsR, `{"acme/api":{"branch_policy":{"rules":[{"branches":"ci/**","role":"agent","run_workflows":true}]}}}`),
		"merge needs pr write":    mk(rowsR, `{"acme/api":{"branch_policy":{"rules":[]},"pull_requests_merge":true}}`),
		"bad repo":                mk(rowsW, `{"nope":{"branch_policy":{"rules":[]}}}`),
		"bad glob":                mk(rowsW, `{"acme/api":{"branch_policy":{"rules":[{"branches":"a..b","role":"read"}]}}}`),
	} {
		if err := ValidatePermissionConfig(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A config without the field behaves as legacy.
	legacy := `{"version":"1","provider":"github","rules":[{"name":"x","deny_all_other":true,"endpoints":[{"method":"GET","path":"/repos/acme/api","allow":true}]}]}`
	if pols, err := GitHubBranchPoliciesFromConfig(legacy); err != nil || len(pols) != 0 {
		t.Fatalf("legacy %v %v", pols, err)
	}
}

func TestBranchPolicyRejectsInvalidRefCharacters(t *testing.T) {
	for _, pat := range []string{"a?b", "a[b", `a\b`, "a b", "a\tb", "a\x7fb", "a~b", "a^b", "a:b", "a//b", "a@{b"} {
		bp := GitHubBranchPolicy{Rules: []GitHubBranchRule{{Branches: pat, Role: GitHubRoleRead}}}
		if err := ValidateGitHubBranchPolicy(&bp, false); err == nil {
			t.Errorf("branch pattern %q accepted", pat)
		}
		tp := GitHubBranchPolicy{Tags: GitHubTagPolicy{Create: []string{pat}}}
		if err := ValidateGitHubBranchPolicy(&tp, false); err == nil {
			t.Errorf("tag pattern %q accepted", pat)
		}
	}
	// The matcher still treats ? as a literal.
	if !GitHubGlobMatch("a?b", "a?b") || GitHubGlobMatch("a?b", "axb") {
		t.Error("matcher must treat ? literally")
	}
}

func TestGlobMatchBoundsName(t *testing.T) {
	big := strings.Repeat("a", 1<<20)
	start := time.Now()
	if GitHubGlobMatch("**", big) || GitHubGlobMatch("a*a*a*b", big) {
		t.Fatal("oversize name matched")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	if !GitHubGlobMatch("**", strings.Repeat("a", MaxGitHubMatchNameBytes)) {
		t.Fatal("name at the limit refused")
	}
}

func TestSelectRuleDefaultBranchCaseInsensitive(t *testing.T) {
	p := GitHubBranchPolicy{Rules: []GitHubBranchRule{{Branches: "@default", Role: GitHubRoleProtected}, {Branches: "**", Role: GitHubRoleAgent}}}
	for _, b := range []string{"main", "MAIN", "Main", "refs/heads/MAIN"} {
		if r, ok := p.SelectRule(b, "main"); !ok || r.Role != GitHubRoleProtected {
			t.Errorf("%q selected %+v", b, r)
		}
	}
}

func TestBranchPolicyMessagesNameThePattern(t *testing.T) {
	bp := GitHubBranchPolicy{Rules: []GitHubBranchRule{{Branches: "ok", Role: "read"}, {Branches: "agent/**", Role: "agent", RunWorkflows: true}}}
	err := ValidateGitHubBranchPolicy(&bp, false)
	if err == nil || !strings.Contains(err.Error(), `branch rule "agent/**"`) || strings.Contains(err.Error(), "rule 1") {
		t.Fatalf("message %v", err)
	}
	bad := GitHubBranchPolicy{Rules: []GitHubBranchRule{{Branches: "a?b", Role: "read"}}}
	if err := ValidateGitHubBranchPolicy(&bad, false); err == nil || !strings.Contains(err.Error(), `branch rule "a?b"`) {
		t.Fatalf("message %v", err)
	}
	// Builder wording names Actions write, not the v2 capability.
	cfg := `{"version":"1","provider":"github","rules":[{"name":"x","deny_all_other":true,"endpoints":[{"method":"GET","path":"/repos/acme/api","allow":true}],
	 "github_capabilities":{"acme/api":{"contents":"write","actions":"read"}},
	 "github_branch_policies":{"acme/api":{"branch_policy":{"rules":[{"branches":"ci/**","role":"agent","run_workflows":true}]}}}}]}`
	if err := ValidatePermissionConfig(cfg); err == nil || !strings.Contains(err.Error(), "Actions write") || strings.Contains(err.Error(), "workflow_dispatch") {
		t.Fatalf("builder message %v", err)
	}
	v2 := `{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{"clone":true,"push":true},"branch_policy":{"rules":[{"branches":"x","role":"agent","run_workflows":true}]}}}}`
	if err := ValidatePermissionConfig(v2); err == nil || !strings.Contains(err.Error(), "workflow_dispatch") {
		t.Fatalf("v2 message %v", err)
	}
}

func TestTruncateRunesKeepsUTF8(t *testing.T) {
	s := strings.Repeat("é", 300)
	if got := truncateRunes(s, 20); got != strings.Repeat("é", 20) {
		t.Fatalf("got %q", got)
	}
	if got := truncateRunes("abc", 20); got != "abc" {
		t.Fatalf("got %q", got)
	}
	bp := GitHubBranchPolicy{Rules: []GitHubBranchRule{{Branches: strings.Repeat("é", 150), Role: "read"}}}
	err := ValidateGitHubBranchPolicy(&bp, false)
	if err == nil || !utf8.ValidString(err.Error()) {
		t.Fatalf("error not valid UTF-8: %v", err)
	}
}

func TestCloneAndValidateDoesNotMutateInput(t *testing.T) {
	in := GitHubBranchPolicy{Rules: []GitHubBranchRule{{Branches: "refs/heads/a", Role: "read"}}, Tags: GitHubTagPolicy{Create: []string{"refs/tags/v*"}}}
	out, err := in.cloneAndValidate(false, "x")
	if err != nil || out.Rules[0].Branches != "a" || out.Tags.Create[0] != "v*" {
		t.Fatalf("%+v %v", out, err)
	}
	if in.Rules[0].Branches != "refs/heads/a" || in.Tags.Create[0] != "refs/tags/v*" {
		t.Fatalf("input rewritten: %+v", in)
	}
}

func TestNormalizePermissionConfigPolicies(t *testing.T) {
	v2 := `{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{"clone":true,"push":true},"branch_policy":{"rules":[{"branches":"refs/heads/agent/**","role":"agent","run_workflows":false}],"tags":{"create":["refs/tags/v*","rel-*"]}}}}}`
	got, err := NormalizePermissionConfigPolicies(v2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "refs/") || !strings.Contains(got, `"agent/**"`) || !strings.Contains(got, `"v*"`) || !strings.Contains(got, `"rel-*"`) {
		t.Fatalf("v2 not normalised: %s", got)
	}
	if err := ValidatePermissionConfig(got); err != nil {
		t.Fatal(err)
	}
	builder := `{"version":"1","provider":"github","rules":[{"name":"x","deny_all_other":true,"endpoints":[{"method":"GET","path":"/repos/acme/api","allow":true}],
	 "github_capabilities":{"acme/api":{"contents":"write"}},
	 "github_branch_policies":{"acme/api":{"branch_policy":{"rules":[{"branches":"refs/heads/x","role":"read"}],"tags":{"create":["refs/tags/t"]}}}}}]}`
	got, err = NormalizePermissionConfigPolicies(builder)
	if err != nil || strings.Contains(got, "refs/heads/") || strings.Contains(got, "refs/tags/") || !strings.Contains(got, `"endpoints"`) {
		t.Fatalf("builder: %v %s", err, got)
	}
	// Already-normal and legacy configs come back byte for byte.
	clean := strings.Replace(strings.Replace(v2, "refs/heads/", "", 1), "refs/tags/", "", 1)
	for _, c := range []string{clean, `{"version":"1","provider":"x","rules":[]}`, ""} {
		if out, err := NormalizePermissionConfigPolicies(c); err != nil || out != c {
			t.Errorf("config rewritten: %v %q", err, out)
		}
	}
}
