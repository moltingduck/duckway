package handlers_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type denyRow struct {
	Decision    string
	Path        string
	RepeatCount int
	Detail      map[string]interface{}
}

func denyRows(t *testing.T, f *proxyFixture) []denyRow {
	t.Helper()
	rows, err := f.rawDB.Query(`SELECT decision, path, repeat_count, deny_detail FROM request_log WHERE decision = 'acl_denied' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []denyRow
	for rows.Next() {
		var r denyRow
		var d string
		if err := rows.Scan(&r.Decision, &r.Path, &r.RepeatCount, &d); err != nil {
			t.Fatal(err)
		}
		if d != "" {
			if err := json.Unmarshal([]byte(d), &r.Detail); err != nil {
				t.Fatalf("deny_detail is not JSON: %q", d)
			}
		}
		out = append(out, r)
	}
	return out
}

func wantDetail(t *testing.T, r denyRow, kind string) map[string]interface{} {
	t.Helper()
	if r.Detail == nil {
		t.Fatalf("%s: no deny_detail", r.Path)
	}
	if r.Detail["kind"] != kind {
		t.Fatalf("%s: kind %v want %s (%v)", r.Path, r.Detail["kind"], kind, r.Detail)
	}
	for _, k := range []string{"layer", "rule", "reason", "refs", "sees"} {
		if _, ok := r.Detail[k]; !ok {
			t.Fatalf("%s: detail lacks %q: %v", r.Path, k, r.Detail)
		}
	}
	if r.Detail["reason"] == "" || r.Detail["sees"] == "" {
		t.Fatalf("%s: empty reason/sees: %v", r.Path, r.Detail)
	}
	return r.Detail
}

// Every acl_denied path stores why: ACL layers, branch rules, the GitHub
// refusals decided before the rules (Content-Encoding, unparseable push,
// method override, unsupported write).
func TestProxyStoresDenyDetailOnEveryDeniedPath(t *testing.T) {
	e := newBrEnv(t, builderConfig(t, false, false))
	hit := func(method, path string, hdr map[string]string, body string) {
		t.Helper()
		req, _ := http.NewRequest(method, e.proxy.URL+"/proxy/github"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+brPlaceholder)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	hit("GET", "/repos/OWNER/REPO/issues", nil, "")                                                        // token_acl
	hit("POST", "/repos/OWNER/REPO/pulls", nil, `{"base":"feature/x"}`)                                    // branch_rules
	hit("PUT", "/repos/OWNER/REPO/pulls/1/merge", nil, `{}`)                                               // capability
	hit("DELETE", "/repos/OWNER/REPO/git/refs", nil, "")                                                   // uncheckable
	hit("POST", "/OWNER/REPO.git/git-receive-pack", map[string]string{"Content-Encoding": "gzip"}, "0000") // uncheckable
	hit("POST", "/OWNER/REPO.git/git-receive-pack", nil, "zzzz")                                           // uncheckable
	hit("POST", "/OWNER/REPO.git/git-receive-pack", nil,
		pushBody("report-status", [3]string{oidA, oidB, "refs/heads/agent/x"}, [3]string{oidA, oidB, "refs/heads/feature/x"})) // branch_rules + refs

	rows := denyRows(t, e.f)
	wantKinds := []string{"token_acl", "branch_rules", "capability", "uncheckable", "uncheckable", "uncheckable", "branch_rules"}
	if len(rows) != len(wantKinds) {
		t.Fatalf("rows: %d %+v", len(rows), rows)
	}
	for i, k := range wantKinds {
		wantDetail(t, rows[i], k)
	}
	if d := rows[0].Detail; d["layer"] != "placeholder" || d["rule"] != "GitHub repositories" {
		t.Errorf("acl detail: %v", d)
	}
	push := rows[6].Detail
	refs := push["refs"].([]interface{})
	if len(refs) != 2 {
		t.Fatalf("refs: %v", refs)
	}
	r0, r1 := refs[0].(map[string]interface{}), refs[1].(map[string]interface{})
	// Cold default-branch cache: the push refusal needs no lookup and the log
	// does not make one, so which rule governs the refs is left blank.
	if r0["verdict"] != "allowed" || r0["ref"] != "refs/heads/agent/x" ||
		r1["verdict"] != "refused" || r1["ref"] != "refs/heads/feature/x" || r1["reason"] == "" {
		t.Errorf("ref rows: %v", refs)
	}
	if !strings.Contains(push["sees"].(string), "ng refs/heads/feature/x duckway: ") {
		t.Errorf("sees: %v", push["sees"])
	}
}

// Identical denials in a minute are one row whose repeat_count counts them.
func TestProxyCoalescesDeniedRepeats(t *testing.T) {
	e := newBrEnv(t, builderConfig(t, false, false))
	for i := 0; i < 3; i++ {
		if code, _ := e.rest("PUT", "/repos/OWNER/REPO/pulls/1/merge", `{}`); code != 403 {
			t.Fatalf("status %d", code)
		}
	}
	rows := denyRows(t, e.f)
	if len(rows) != 1 || rows[0].RepeatCount != 3 {
		t.Fatalf("rows: %+v", rows)
	}
	wantDetail(t, rows[0], "capability")
}

func TestProxyDenyDetailACLLayers(t *testing.T) {
	rig := newApproveRig(t, simApproveACL)
	if code, _ := rig.do("DELETE", "/v1/models"); code != 403 {
		t.Fatalf("status %d", code)
	}
	rows := denyRows(t, rig.f)
	if len(rows) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	d := wantDetail(t, rows[0], "token_acl")
	if d["layer"] != "placeholder" || d["rule"] != "gated" {
		t.Errorf("detail: %v", d)
	}
	if !strings.HasPrefix(d["sees"].(string), `{"error":"permission denied (placeholder): `) {
		t.Errorf("sees: %v", d["sees"])
	}
}

// With the default branch already cached (by an earlier request) the log names
// the pattern and role behind each ref, still without a lookup of its own.
func TestProxyDenyDetailNamesRulesFromCachedDefault(t *testing.T) {
	e := newBrEnv(t, builderConfig(t, false, false))
	// A push to an agent branch needs the default branch to decide: it warms the cache.
	req := pushBody("report-status", [3]string{oidA, oidB, "refs/heads/agent/warm"})
	e.rest("POST", "/OWNER/REPO.git/git-receive-pack", req)
	warmed := e.lookups
	if warmed == 0 {
		t.Fatal("expected the allowed push to look up the default branch")
	}
	e.rest("POST", "/OWNER/REPO.git/git-receive-pack",
		pushBody("report-status", [3]string{oidA, oidB, "refs/heads/agent/x"}, [3]string{oidA, oidB, "refs/heads/main"}, [3]string{oidA, oidB, "refs/heads/feature/x"}))
	rows := denyRows(t, e.f)
	if len(rows) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	d := wantDetail(t, rows[0], "branch_rules")
	refs := d["refs"].([]interface{})
	want := [][3]string{{"allowed", "agent/**", "agent"}, {"refused", "@default", "protected"}, {"refused", "**", "read"}}
	for i, w := range want {
		r := refs[i].(map[string]interface{})
		if r["verdict"] != w[0] || r["pattern"] != w[1] || r["role"] != w[2] {
			t.Errorf("ref %d: %v want %v", i, r, w)
		}
	}
	if d["rule"] != "@default \u2192 protected" {
		t.Errorf("rule: %v", d["rule"])
	}
	if e.lookups != warmed {
		t.Errorf("the refusal looked the default branch up again: %d -> %d", warmed, e.lookups)
	}
}
