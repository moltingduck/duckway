package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures under testdata/gbr_ui are exactly what the admin UI serializer
// (web/static/github-branch-rules.js, phantom-common.js, phantom-acl.js)
// emits. Regenerate with web/testdata/gen_gbr_fixtures.js.

func readUIFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "gbr_ui", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func uiEndpointSet(t *testing.T, cfg string) map[string]bool {
	t.Helper()
	pc, err := ParsePermissionConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, r := range pc.Rules {
		for _, e := range r.Endpoints {
			set[e.Method+" "+e.Path] = true
		}
	}
	return set
}

func ruleSummary(p GitHubBranchPolicy) []string {
	var out []string
	for _, r := range p.Rules {
		s := r.Branches + ":" + r.Role
		if r.RunWorkflows {
			s += ":run"
		}
		out = append(out, s)
	}
	return out
}

func equalStrings(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

func TestUISerializedConfigsAreAccepted(t *testing.T) {
	t.Run("v2 agent template", func(t *testing.T) {
		cfg := readUIFixture(t, "v2_agent_template.json")
		if err := ValidatePermissionConfig(cfg); err != nil {
			t.Fatal(err)
		}
		pols, err := GitHubBranchPoliciesFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		p := pols["acme/api"]
		if got := ruleSummary(p.BranchPolicy); !equalStrings(got, []string{"@default:protected", "agent/**:agent", "**:read"}) {
			t.Fatalf("rules %v", got)
		}
		if p.PullRequestsMerge || p.PullRequestsApprove || len(p.BranchPolicy.Tags.Create) != 0 {
			t.Fatalf("flags %+v", p)
		}
		eps := uiEndpointSet(t, cfg)
		for _, want := range []string{"POST /acme/api.git/git-receive-pack", "POST /repos/acme/api/pulls"} {
			if !eps[want] {
				t.Errorf("missing endpoint %s", want)
			}
		}
		if eps["PUT /repos/acme/api/pulls/*/merge"] {
			t.Error("merge endpoint compiled although pull_requests_merge is false")
		}
	})
	t.Run("v2 cicd preset", func(t *testing.T) {
		cfg := readUIFixture(t, "v2_cicd_preset.json")
		if err := ValidatePermissionConfig(cfg); err != nil {
			t.Fatal(err)
		}
		pols, _ := GitHubBranchPoliciesFromConfig(cfg)
		bp := pols["acme/ci"].BranchPolicy
		if got := ruleSummary(bp); !equalStrings(got, []string{"@default:protected:run", "main:protected:run", "release/*:protected:run", "**:read"}) {
			t.Fatalf("rules %v", got)
		}
		eps := uiEndpointSet(t, cfg)
		if !eps["POST /repos/acme/ci/actions/workflows/ci.yml/dispatches"] {
			t.Error("dispatch endpoint missing")
		}
		pc, _ := ParsePermissionConfig(cfg)
		for _, r := range pc.Rules {
			for _, e := range r.Endpoints {
				if strings.HasSuffix(e.Path, "/dispatches") {
					if c := e.Constraints.Body["ref"]; !c.Required || len(c.Matches) != 0 {
						t.Errorf("dispatch ref constraint %+v: rules, not ref_allowlist, decide", c)
					}
				}
			}
		}
	})
	t.Run("builder github_branch_policies", func(t *testing.T) {
		cfg := readUIFixture(t, "builder_branch_policies.json")
		if err := ValidatePermissionConfig(cfg); err != nil {
			t.Fatal(err)
		}
		pols, err := GitHubBranchPoliciesFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		p, ok := pols["acme/api"]
		if !ok || !p.PullRequestsMerge || p.PullRequestsApprove {
			t.Fatalf("policy %+v ok=%v", p, ok)
		}
		if got := ruleSummary(p.BranchPolicy); !equalStrings(got, []string{"@default:protected", "agent/**:agent", "**:read"}) {
			t.Fatalf("rules %v", got)
		}
	})
	t.Run("converted legacy", func(t *testing.T) {
		cfg := readUIFixture(t, "v2_converted_legacy.json")
		if err := ValidatePermissionConfig(cfg); err != nil {
			t.Fatal(err)
		}
		pols, _ := GitHubBranchPoliciesFromConfig(cfg)
		if got := ruleSummary(pols["acme/api"].BranchPolicy); !equalStrings(got, []string{"@default:agent", "main:agent:run", "release/*:agent:run", "**:agent"}) {
			t.Fatalf("rules %v", got)
		}
		if strings.Contains(cfg, "ref_allowlist") {
			t.Error("converted grant still carries ref_allowlist")
		}
	})
}

// A builder entry never needs a run_workflows rule: Actions write also covers
// rerun and cancel, so the editor treats it as advisory there.
func TestBuilderDispatchWithoutRunRuleAccepted(t *testing.T) {
	cfg := readUIFixture(t, "builder_branch_policies.json")
	cfg = strings.Replace(cfg, `"pull_requests": "write"`, `"pull_requests": "write",
        "actions": "write"`, 1)
	if !strings.Contains(cfg, `"actions": "write"`) {
		t.Fatal("fixture edit failed")
	}
	if err := ValidatePermissionConfig(cfg); err != nil {
		t.Fatalf("builder entry without run_workflows rejected: %v", err)
	}
}

func TestUIInvalidStatesRejected(t *testing.T) {
	v2 := func(caps, extra, policy string) string {
		return `{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{` + caps + `}` + extra + `,"branch_policy":` + policy + `}}}`
	}
	rule := func(b, role string, run bool) string {
		r := `{"branches":"` + b + `","role":"` + role + `","run_workflows":false}`
		if run {
			r = strings.Replace(r, "false", "true", 1)
		}
		return `{"rules":[` + r + `],"tags":{"create":[]}}`
	}
	const base = `"clone":true,"push":true,"pull_requests_read":true,"pull_requests_write":true,"actions_read":true`
	cases := map[string]string{
		"dispatch without run rule":  v2(base+`,"workflow_dispatch":true`, `,"workflow_allowlist":["ci.yml"]`, rule("**", "read", false)),
		"dispatch without workflows": v2(base+`,"workflow_dispatch":true`, ``, rule("**", "read", true)),
		"run without dispatch":       v2(base, ``, rule("**", "agent", true)),
		"merge without pr write":     v2(`"clone":true,"pull_requests_read":true,"pull_requests_merge":true`, ``, rule("**", "read", false)),
		"ref_allowlist with policy":  v2(base, `,"ref_allowlist":["main"]`, rule("**", "read", false)),
		"question mark pattern":      v2(base, ``, rule("a?b", "agent", false)),
		"duplicate patterns":         v2(base, ``, `{"rules":[{"branches":"a","role":"read"},{"branches":"refs/heads/a","role":"agent"}]}`),
		"unknown role":               v2(base, ``, rule("a", "admin", false)),
		"tag pattern question mark":  v2(base, ``, `{"rules":[],"tags":{"create":["v?"]}}`),
	}
	for name, cfg := range cases {
		if err := ValidatePermissionConfig(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The UI and the server must reject exactly the same pattern characters.
func TestUIPatternValidationAgrees(t *testing.T) {
	var cases []struct {
		Pattern                  string  `json:"pattern"`
		BranchError              bool    `json:"branch_error"`
		BranchErrorDefaultAllows bool    `json:"branch_error_default_allowed"`
		TagError                 bool    `json:"tag_error"`
		Serialized               *string `json:"serialized"`
	}
	if err := json.Unmarshal([]byte(readUIFixture(t, "pattern_validation.json")), &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, c := range cases {
		if strings.TrimSpace(c.Pattern) != c.Pattern {
			// The editor trims before serializing, so the raw text never reaches
			// the server; the server itself stays strict about whitespace.
			if _, err := normalizeGitHubPattern(c.Pattern, "refs/heads/", false); err == nil {
				t.Errorf("server accepted untrimmed %q", c.Pattern)
			}
		} else {
			_, err := normalizeGitHubPattern(c.Pattern, "refs/heads/", false)
			if (err != nil) != c.BranchError {
				t.Errorf("branch %q: go err=%v, ui error=%v", c.Pattern, err, c.BranchError)
			}
			_, err = normalizeGitHubPattern(c.Pattern, "refs/heads/", true)
			if (err != nil) != c.BranchErrorDefaultAllows {
				t.Errorf("branch (default allowed) %q: go err=%v, ui error=%v", c.Pattern, err, c.BranchErrorDefaultAllows)
			}
			_, err = normalizeGitHubPattern(c.Pattern, "refs/tags/", false)
			if (err != nil) != c.TagError {
				t.Errorf("tag %q: go err=%v, ui error=%v", c.Pattern, err, c.TagError)
			}
		}
		// Whatever the UI accepts must serialize to a pattern the server accepts.
		if !c.BranchError {
			if c.Serialized == nil {
				t.Errorf("%q: accepted by the UI but not serialized", c.Pattern)
				continue
			}
			got, err := normalizeGitHubPattern(*c.Serialized, "refs/heads/", false)
			if err != nil || got != *c.Serialized || *c.Serialized != strings.TrimSpace(*c.Serialized) {
				t.Errorf("%q serialized as %q: server err=%v", c.Pattern, *c.Serialized, err)
			}
		}
	}
}
