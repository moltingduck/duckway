package handlers_test

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Wherever the deciding rule is known, "Decided by" is filled: in the stored
// log detail of a real refused request and in the simulator.
func TestDecidingRuleIsFilledWhereKnown(t *testing.T) {
	e := newBrEnv(t, builderConfig(t, false, false))
	sim := newSimFor(e.f)
	act := func(action, ref string) map[string]interface{} {
		return map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": action, "ref": ref}
	}
	cases := []struct {
		name    string
		req     map[string]interface{}
		kind    string
		prefix  string // rule must start with this
		refRule bool   // refs[] carry pattern and role
	}{
		{"push read branch", act("push", "feature/x"), "branch_rules", "** → read", true},
		{"push agent-less nested", act("push", "release/1"), "branch_rules", "** → read", true},
		{"delete read branch", act("force_or_delete", "feature/y"), "branch_rules", "** → read", true},
		{"push moves existing tag", act("push", "refs/tags/v1"), "branch_rules", "existing tags never move", false},
		{"push invalid ref name", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/OWNER/REPO.git/git-receive-pack",
			"body_hex": hex.EncodeToString([]byte(pushBody("report-status", [3]string{oidA, oidB, "refs/heads/a..b"})))}, "branch_rules", "invalid ref name", false},
		{"push outside refs", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/OWNER/REPO.git/git-receive-pack",
			"body_hex": hex.EncodeToString([]byte(pushBody("report-status", [3]string{oidA, oidB, "refs/notes/x"})))}, "branch_rules", "refs outside branches and tags", false},
		{"rest invalid branch name", act("create_branch", "a..b"), "branch_rules", "invalid ref name", false},
		{"rest ref of the wrong type", act("create_branch", "refs/remotes/x"), "branch_rules", "ref type not allowed", false},
		{"rest create read branch", act("create_branch", "feature/z"), "branch_rules", "** → read", false},
		{"rest pr into read branch", act("open_pr_into", "feature/x"), "branch_rules", "** → read", false},
		{"rest tag not allowed", act("create_tag", "nightly"), "branch_rules", "tag create patterns: v*", false},
		{"rest release not allowed", act("release", "x2"), "branch_rules", "tag create patterns: v*", false},
		{"dispatch on agent branch", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "dispatch", "ref": "agent/x", "workflow": "ci.yml"}, "branch_rules", "agent/** → agent", false},
		{"merge toggle", act("merge_pr", ""), "capability", "pull_requests_merge", false},
		{"approve toggle", act("approve_pr", ""), "capability", "pull_requests_approve", false},
		{"token acl", map[string]interface{}{"mode": "raw", "method": "GET", "path": "/repos/OWNER/REPO/issues"}, "token_acl", "GitHub repositories", false},
		{"mixed push", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/OWNER/REPO.git/git-receive-pack",
			"body_hex": hex.EncodeToString([]byte(pushBody("report-status", [3]string{oidA, oidB, "refs/heads/agent/x"}, [3]string{oidA, oidB, "refs/heads/feature/x"})))}, "branch_rules", "** → read", true},
		{"create tag by push", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/OWNER/REPO.git/git-receive-pack",
			"body_hex": hex.EncodeToString([]byte(pushBody("report-status", [3]string{oid0, oidB, "refs/tags/nightly"})))}, "branch_rules", "tag create patterns: v*", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, s := simCall(t, sim, e.f.placeholderID, tc.req)
			if code != 200 || s.Verdict != "refused" || s.Kind != tc.kind {
				t.Fatalf("simulate: %d %+v", code, s)
			}
			if !strings.HasPrefix(s.Rule, tc.prefix) {
				t.Errorf("simulator rule %q, want prefix %q", s.Rule, tc.prefix)
			}
			// The same request through the proxy logs the same deciding rule.
			before := len(denyRows(t, e.f))
			e.replay(t, s, nil)
			rows := denyRows(t, e.f)
			if len(rows) != before+1 {
				t.Fatalf("expected one new refusal row, got %d -> %d", before, len(rows))
			}
			d := rows[len(rows)-1].Detail
			rule, _ := d["rule"].(string)
			if !strings.HasPrefix(rule, tc.prefix) {
				t.Errorf("logged rule %q, want prefix %q (%v)", rule, tc.prefix, d)
			}
			if causeOnly[tc.prefix] { // refused for a cause no rule decided
				for _, r := range s.Refs {
					if r.Verdict == "refused" && (r.Pattern != "" || r.Role != "") {
						t.Errorf("simulated ref names a rule that did not decide: %+v", r)
					}
				}
				for _, r := range logRefs(d["refs"]) {
					if r["verdict"] == "refused" && (r["pattern"] != "" || r["role"] != "") {
						t.Errorf("logged ref names a rule that did not decide: %v", r)
					}
				}
			}
			if tc.refRule {
				for _, list := range [][]map[string]interface{}{logRefs(d["refs"])} {
					refused := 0
					for _, r := range list {
						if r["verdict"] == "refused" {
							refused++
							if r["pattern"] == "" || r["role"] == "" {
								t.Errorf("logged refused ref lacks pattern/role: %v", r)
							}
						}
					}
					if refused == 0 {
						t.Errorf("no refused ref in log: %v", d["refs"])
					}
				}
				for _, r := range s.Refs {
					if r.Verdict == "refused" && (r.Pattern == "" || r.Role == "") {
						t.Errorf("simulated refused ref lacks pattern/role: %+v", r)
					}
				}
			}
		})
	}
}

// causeOnly are the deciding "rules" that name a cause, not a rule: refs
// refused for them must not name a branch or tag-create rule.
var causeOnly = map[string]bool{
	"existing tags never move": true, "invalid ref name": true,
	"refs outside branches and tags": true, "ref type not allowed": true,
}

func logRefs(v interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	list, _ := v.([]interface{})
	for _, x := range list {
		if m, ok := x.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}
