package services

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
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
