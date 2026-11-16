package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
)

// simResp mirrors the simulate response contract.
type simResp struct {
	Verdict string `json:"verdict"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Layer   string `json:"layer"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason"`
	Steps   []struct {
		Status string `json:"status"`
		Label  string `json:"label"`
		Detail string `json:"detail"`
	} `json:"steps"`
	Refs []struct {
		Ref     string `json:"ref"`
		Verdict string `json:"verdict"`
		Pattern string `json:"pattern"`
		Role    string `json:"role"`
		Reason  string `json:"reason"`
	} `json:"refs"`
	Sees                   string `json:"sees"`
	DependsOnDefaultBranch bool   `json:"depends_on_default_branch"`
	Request                struct {
		Method   string `json:"method"`
		Path     string `json:"path"`
		Query    string `json:"query"`
		BodyText string `json:"body_text"`
		BodyHex  string `json:"body_hex"`
		BodySize int    `json:"body_size"`
	} `json:"request"`
	PerKey []struct {
		KeyID   string `json:"key_id"`
		KeyName string `json:"key_name"`
		Verdict string `json:"verdict"`
		Kind    string `json:"kind"`
	} `json:"per_key"`
	Error  string `json:"error"`
	Offset *int   `json:"offset"`
}

func simCall(t *testing.T, sim *handlers.AccessSimulator, phID string, req interface{}) (int, simResp) {
	t.Helper()
	b, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/api/phantom-tokens/"+phID+"/simulate", bytes.NewReader(b))
	r.SetPathValue("id", phID)
	w := httptest.NewRecorder()
	sim.Simulate(w, r)
	var out simResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("simulate response: %v %s", err, w.Body.String())
	}
	return w.Code, out
}

func newSimFor(f *proxyFixture) *handlers.AccessSimulator {
	return handlers.NewAccessSimulator(f.placeholderQ, f.svcQ, f.apiKeyQ, f.groupQ, f.approvalQ, f.crypto)
}

// replay sends the exact request a simulation evaluated through the real proxy.
func (e *brEnv) replay(t *testing.T, s simResp, headers map[string]string) (int, []byte) {
	t.Helper()
	body, err := hex.DecodeString(s.Request.BodyHex)
	if err != nil {
		t.Fatal(err)
	}
	u := e.proxy.URL + "/proxy/github" + s.Request.Path
	if s.Request.Query != "" {
		u += "?" + s.Request.Query
	}
	req, _ := http.NewRequest(s.Request.Method, u, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+brPlaceholder)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func proxyRefused(code int, body []byte) bool {
	return bytes.Contains(body, []byte("duckway: ")) || bytes.Contains(body, []byte("permission denied")) || code == http.StatusForbidden
}

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// simPolicyConfig is v2BranchPolicyConfig with every capability the actions need.
const simPolicyConfig = `{"version":"2","provider":"github","repositories":{"OWNER/REPO":{
 "capabilities":{"clone":true,"push":true,"issues_read":true,"issues_write":true,"pull_requests_read":true,"pull_requests_write":true,
  "releases_read":true,"releases_write":true,"actions_read":true,"workflow_dispatch":true},
 "workflow_allowlist":["ci.yml"],
 "branch_policy":{"rules":[
   {"branches":"@default","role":"protected"},
   {"branches":"agent/**","role":"agent"},
   {"branches":"ci/**","role":"agent","run_workflows":true},
   {"branches":"**","role":"read"}],
  "tags":{"create":["v*"]}}}}}`

const (
	oidA = "0123456789abcdef0123456789abcdef01234567"
	oidB = "89abcdef0123456789abcdef0123456789abcdef"
	oid0 = "0000000000000000000000000000000000000000"
)

// pushBody frames receive-pack commands like git does (caps after NUL on the first).
func pushBody(caps string, cmds ...[3]string) string {
	var b strings.Builder
	for i, c := range cmds {
		line := c[0] + " " + c[1] + " " + c[2]
		if i == 0 {
			line += "\x00" + caps
		}
		b.WriteString(pkt(line))
	}
	b.WriteString("0000PACK\x00\x00\x00\x02\x00\x00\x00\x00")
	return b.String()
}

// TestSimulatorMatchesProxy replays every simulated request through the real
// proxy: the verdict must agree and, for refusals, what the agent sees must be
// exactly what the simulator says (bytes for JSON refusals, the ng lines for
// report-status rejections).
func TestSimulatorMatchesProxy(t *testing.T) {
	e := newBrEnv(t, builderConfig(t, false, false))
	sim := newSimFor(e.f)

	cases := []struct {
		name    string
		req     map[string]interface{}
		verdict string
		kind    string
	}{
		{"clone", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "clone"}, "allowed", ""},
		{"push own branch", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "push", "ref": "agent/x"}, "allowed", ""},
		{"push default", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "push", "ref": "main"}, "refused", "branch_rules"},
		{"push other", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "push", "ref": "feature/x"}, "refused", "branch_rules"},
		{"delete own", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "force_or_delete", "ref": "agent/x"}, "allowed", ""},
		{"delete default", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "force_or_delete", "ref": "main"}, "refused", "branch_rules"},
		{"create branch ok", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "create_branch", "ref": "agent/new"}, "allowed", ""},
		{"create branch bad", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "create_branch", "ref": "main2"}, "refused", "branch_rules"},
		{"pr into default", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "open_pr_into", "ref": "main"}, "allowed", ""},
		{"pr into read", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "open_pr_into", "ref": "feature/x"}, "refused", "branch_rules"},
		{"merge toggle off", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "merge_pr"}, "refused", "capability"},
		{"approve toggle off", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "approve_pr"}, "refused", "capability"},
		{"tag ok", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "create_tag", "ref": "v1.0"}, "allowed", ""},
		{"tag bad", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "create_tag", "ref": "nightly"}, "refused", "branch_rules"},
		{"dispatch ok", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "dispatch", "ref": "ci/x", "workflow": "ci.yml"}, "allowed", ""},
		{"dispatch bad", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "dispatch", "ref": "agent/x", "workflow": "ci.yml"}, "refused", "branch_rules"},
		{"release ok", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "release", "ref": "v2"}, "allowed", ""},
		{"release bad", map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "release", "ref": "x2"}, "refused", "branch_rules"},
		{"raw unclassified write", map[string]interface{}{"mode": "raw", "method": "DELETE", "path": "/repos/OWNER/REPO/branches/main/protection"}, "refused", "token_acl"},
		{"raw unsupported refs write", map[string]interface{}{"mode": "raw", "method": "DELETE", "path": "/repos/OWNER/REPO/git/refs"}, "refused", "uncheckable"},
		{"raw method override", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/repos/OWNER/REPO/pulls", "query": "_method=DELETE", "body_text": "{}"}, "refused", "uncheckable"},
		{"raw json force move main", map[string]interface{}{"mode": "raw", "method": "PATCH", "path": "/repos/OWNER/REPO/git/refs/heads/main", "body_text": `{"sha":"` + oidA + `","force":true}`}, "refused", "branch_rules"},
		{"raw graphql write", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/graphql", "body_text": `{"query":"mutation{}"}`}, "refused", "token_acl"},
		{"raw mixed push", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/OWNER/REPO.git/git-receive-pack",
			"body_hex": hex.EncodeToString([]byte(pushBody("report-status side-band-64k", [3]string{oidA, oidB, "refs/heads/agent/x"}, [3]string{oidA, oidB, "refs/heads/main"})))}, "refused", "branch_rules"},
		{"raw push no report-status", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/OWNER/REPO.git/git-receive-pack",
			"body_hex": hex.EncodeToString([]byte(pushBody("", [3]string{oidA, oidB, "refs/heads/main"})))}, "refused", "branch_rules"},
		{"raw push unparseable", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/OWNER/REPO.git/git-receive-pack", "body_text": "zzzz"}, "refused", "uncheckable"},
		{"raw push gzip", map[string]interface{}{"mode": "raw", "method": "POST", "path": "/OWNER/REPO.git/git-receive-pack", "headers": map[string]string{"Content-Encoding": "gzip"},
			"body_hex": hex.EncodeToString([]byte(pushBody("report-status", [3]string{oidA, oidB, "refs/heads/agent/x"})))}, "refused", "uncheckable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, s := simCall(t, sim, e.f.placeholderID, tc.req)
			if code != 200 {
				t.Fatalf("simulate: %d %s", code, s.Error)
			}
			if s.Verdict != tc.verdict || s.Kind != tc.kind {
				t.Fatalf("verdict %q kind %q (reason %q), want %q %q", s.Verdict, s.Kind, s.Reason, tc.verdict, tc.kind)
			}
			var hdrs map[string]string
			if h, ok := tc.req["headers"].(map[string]string); ok {
				hdrs = h
			}
			pcode, pbody := e.replay(t, s, hdrs)
			if got := proxyRefused(pcode, pbody); got != (s.Verdict == "refused") {
				t.Fatalf("proxy refused=%v (status %d body %q), simulator says %q", got, pcode, pbody, s.Verdict)
			}
			if s.Verdict != "refused" {
				return
			}
			if strings.HasPrefix(s.Sees, "unpack ok\n") {
				for _, line := range strings.Split(strings.TrimSpace(s.Sees), "\n") {
					if line == "unpack ok" {
						continue
					}
					if !bytes.Contains(pbody, []byte(line)) {
						t.Errorf("proxy body %q lacks simulated line %q", pbody, line)
					}
				}
				return
			}
			if s.Sees != string(pbody) {
				t.Errorf("sees %q != proxy body %q", s.Sees, pbody)
			}
		})
	}
}

func TestSimulatorSteps(t *testing.T) {
	e := newBrEnv(t, simPolicyConfig)
	sim := newSimFor(e.f)
	_, ok := simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "push", "ref": "agent/x"})
	_, no := simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "push", "ref": "main"})
	if ok.Title != "Allowed" || no.Title != "Refused by branch rules" {
		t.Fatalf("titles: %q %q", ok.Title, no.Title)
	}
	if !ok.DependsOnDefaultBranch || !no.DependsOnDefaultBranch {
		t.Fatalf("a policy with an @default rule depends on the default branch")
	}
	var stop string
	for _, s := range no.Steps {
		if s.Status == "stop" {
			stop = s.Label
		}
	}
	if stop != "Branch rules" {
		t.Fatalf("stop step %q in %+v", stop, no.Steps)
	}
	if len(no.Refs) != 1 || no.Refs[0].Verdict != "refused" || no.Refs[0].Pattern != "@default" || no.Refs[0].Role != "protected" {
		t.Fatalf("refs: %+v", no.Refs)
	}
	if len(ok.Refs) != 1 || ok.Refs[0].Pattern != "agent/**" || ok.Refs[0].Role != "agent" || ok.Refs[0].Verdict != "allowed" {
		t.Fatalf("allowed refs: %+v", ok.Refs)
	}
	if no.Rule != "@default \u2192 protected" {
		t.Fatalf("rule %q", no.Rule)
	}
}

func TestSimulatorMixedPushRefsAndCollateral(t *testing.T) {
	e := newBrEnv(t, simPolicyConfig)
	sim := newSimFor(e.f)
	body := pushBody("report-status", [3]string{oidA, oidB, "refs/heads/agent/x"}, [3]string{oidA, oidB, "refs/heads/main"})
	_, s := simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "raw", "method": "POST", "path": "/OWNER/REPO.git/git-receive-pack", "body_hex": hex.EncodeToString([]byte(body))})
	if len(s.Refs) != 2 || s.Refs[0].Verdict != "allowed" || s.Refs[1].Verdict != "refused" {
		t.Fatalf("refs: %+v", s.Refs)
	}
	if !strings.Contains(s.Sees, "ng refs/heads/agent/x duckway: another ref in this push was refused") ||
		!strings.Contains(s.Sees, "ng refs/heads/main duckway: ") {
		t.Fatalf("sees: %q", s.Sees)
	}
}

func TestSimulatorDefaultLookupFailureFailsClosed(t *testing.T) {
	e := newBrEnv(t, simPolicyConfig)
	sim := newSimFor(e.f)
	e.apiFail.Store(true)
	_, s := simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "push", "ref": "agent/x"})
	if s.Verdict != "refused" || s.Kind != "uncheckable" || !strings.Contains(s.Reason, "default branch could not be determined") {
		t.Fatalf("lookup failure must fail closed: %+v", s)
	}
}

func TestSimulatorHexErrors(t *testing.T) {
	e := newBrEnv(t, simPolicyConfig)
	sim := newSimFor(e.f)
	for _, tc := range []struct {
		hex    string
		offset int
		msg    string
	}{
		{"abc", 2, "odd number"},
		{"ab cx", 4, "invalid hex character"},
		{"ab\n\tg1", 4, "invalid hex character"},
		{"0", 0, "odd number"},
	} {
		code, s := simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "raw", "method": "POST", "path": "/repos/OWNER/REPO/issues", "body_hex": tc.hex})
		if code != 400 || s.Offset == nil || *s.Offset != tc.offset || !strings.Contains(s.Error, tc.msg) || !strings.Contains(s.Error, fmt.Sprintf("offset %d", tc.offset)) {
			t.Errorf("%q: %d %+v", tc.hex, code, s)
		}
	}
	code, s := simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "raw", "method": "POST", "path": "/x", "body_hex": "00", "body_text": "x"})
	if code != 400 {
		t.Errorf("both bodies: %d %s", code, s.Error)
	}
	// whitespace between digits is fine and decodes the same bytes
	_, a := simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "raw", "method": "POST", "path": "/repos/OWNER/REPO/issues", "body_hex": "7b 7d\n"})
	if a.Request.BodyText != "{}" || a.Request.BodyHex != "7b7d" {
		t.Errorf("hex round trip: %+v", a.Request)
	}
}

func TestSimulatorValidation(t *testing.T) {
	e := newBrEnv(t, simPolicyConfig)
	sim := newSimFor(e.f)
	for name, req := range map[string]map[string]interface{}{
		"bad mode":      {"mode": "x"},
		"bad action":    {"mode": "action", "repo": "OWNER/REPO", "action": "nuke", "ref": "x"},
		"bad repo":      {"mode": "action", "repo": "no-slash", "action": "push", "ref": "x"},
		"missing ref":   {"mode": "action", "repo": "OWNER/REPO", "action": "push"},
		"bad method":    {"mode": "raw", "method": "ge t", "path": "/x"},
		"bad path":      {"mode": "raw", "method": "GET", "path": "nope"},
		"enc slash":     {"mode": "raw", "method": "GET", "path": "/repos/a%2fb"},
		"other service": {"mode": "raw", "method": "GET", "path": "/proxy/openai/v1"},
		"query twice":   {"mode": "raw", "method": "GET", "path": "/x?a=1", "query": "b=2"},
		"bad header":    {"mode": "raw", "method": "GET", "path": "/x", "headers": map[string]string{"X": "a\r\nb"}},
	} {
		if code, s := simCall(t, sim, e.f.placeholderID, req); code != 400 {
			t.Errorf("%s: %d %+v", name, code, s)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`))
	r.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	sim.Simulate(w, r)
	if w.Code != 404 {
		t.Errorf("unknown token: %d", w.Code)
	}
	// A "proxy/github" prefix is accepted and stripped.
	code, s := simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "raw", "method": "get", "path": "/proxy/github/repos/OWNER/REPO/issues?state=open"})
	if code != 200 || s.Request.Path != "/repos/OWNER/REPO/issues" || s.Request.Query != "state=open" || s.Request.Method != "GET" || s.Verdict != "allowed" {
		t.Errorf("prefix: %d %+v", code, s)
	}
}

// TestSimulatorNoSideEffects: simulating never writes a log row, creates an
// approval, or touches usage counters / the key pool's round-robin index.
func TestSimulatorNoSideEffects(t *testing.T) {
	e := newBrEnv(t, simPolicyConfig)
	sim := newSimFor(e.f)
	db := e.f.rawDB
	count := func(q string) int {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	snap := func() [5]int {
		return [5]int{
			count(`SELECT COUNT(*) FROM request_log`), count(`SELECT COUNT(*) FROM approvals`),
			count(`SELECT COALESCE(SUM(usage_count),0) FROM placeholder_keys`), count(`SELECT COALESCE(SUM(usage_count),0) FROM api_keys`),
			count(`SELECT COALESCE(SUM(last_index),0) FROM api_key_groups`),
		}
	}
	before := snap()
	for _, ref := range []string{"main", "agent/x", "feature/x"} {
		simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "push", "ref": ref})
	}
	simCall(t, sim, e.f.placeholderID, map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "merge_pr"})
	if after := snap(); after != before {
		t.Fatalf("simulation had side effects: %v -> %v", before, after)
	}
	// And the real proxy does count (the check above is meaningful).
	e.rest("GET", "/repos/OWNER/REPO/issues", "")
	if after := snap(); after == before {
		t.Fatalf("proxy request should change the counters")
	}
}

const simApproveACL = `{"version":"1","provider":"anthropic","rules":[{"name":"gated","deny_all_other":true,"endpoints":[
	{"method":"GET","path":"/v1/*","allow":true},
	{"method":"POST","path":"/v1/messages","approve":true}]}]}`

// TestSimulatorNonGitHubRaw covers a non-GitHub token: deny, allow, approve
// hold, a service-level default ACL, and token-wide approval.
func TestSimulatorNonGitHubRaw(t *testing.T) {
	rig := newApproveRig(t, simApproveACL)
	f := rig.f
	sim := newSimFor(f)
	raw := func(method, path string) simResp {
		code, s := simCall(t, sim, f.placeholderID, map[string]interface{}{"mode": "raw", "method": method, "path": path, "body_text": "{}"})
		if code != 200 {
			t.Fatalf("simulate: %d %s", code, s.Error)
		}
		return s
	}
	if s := raw("GET", "/v1/models"); s.Verdict != "allowed" || s.Kind != "" || s.Title != "Allowed" {
		t.Fatalf("allow: %+v", s)
	}
	s := raw("DELETE", "/v1/models")
	if s.Verdict != "refused" || s.Kind != "token_acl" || s.Layer != "placeholder" || s.Rule != "gated" {
		t.Fatalf("deny: %+v", s)
	}
	if code, body := rig.do("DELETE", "/v1/models"); code != 403 || body["error"] == "" || s.Sees != jsonLine(body["error"]) {
		t.Fatalf("proxy parity: %d %v vs %q", code, body, s.Sees)
	}
	h := raw("POST", "/v1/messages")
	if h.Verdict != "held" || h.Kind != "approval" || h.Layer != "placeholder" || h.Rule != "gated" {
		t.Fatalf("held: %+v", h)
	}
	var rows int
	_ = f.rawDB.QueryRow(`SELECT COUNT(*) FROM approvals`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("a simulation must not create an approval, found %d", rows)
	}

	// Service default ACL denies first.
	svc, _ := f.svcQ.GetByID(f.serviceID)
	svc.DefaultACL = `{"version":"1","provider":"anthropic","rules":[{"name":"svc-deny","endpoints":[{"method":"GET","path":"/v1/secret","allow":false}]}]}`
	if err := f.svcQ.Update(svc); err != nil {
		t.Fatal(err)
	}
	if s := raw("GET", "/v1/secret"); s.Verdict != "refused" || s.Kind != "service_acl" || s.Layer != "service" {
		t.Fatalf("service acl: %+v", s)
	}
	// Credential ACL.
	key, _ := f.apiKeyQ.GetByID(f.apiKeyID)
	key.ACL = `{"version":"1","provider":"anthropic","rules":[{"name":"key-deny","endpoints":[{"method":"GET","path":"/v1/models","allow":false}]}]}`
	if err := f.apiKeyQ.Update(key); err != nil {
		t.Fatal(err)
	}
	if s := raw("GET", "/v1/models"); s.Verdict != "refused" || s.Kind != "api_key_acl" || s.Layer != "api_key" {
		t.Fatalf("api key acl: %+v", s)
	}

	// Token-wide approval: held until a valid approval exists.
	ph, _ := f.placeholderQ.GetByID(f.placeholderID)
	ph.RequiresApproval = true
	if err := f.placeholderQ.Update(ph); err != nil {
		t.Fatal(err)
	}
	s = raw("GET", "/v1/ok")
	if s.Verdict != "held" || s.Kind != "approval" {
		t.Fatalf("token-wide hold: %+v", s)
	}

	// Inactive token / credential.
	_, _ = f.rawDB.Exec(`UPDATE placeholder_keys SET requires_approval = 0, is_active = 0`)
	if s := raw("GET", "/v1/ok"); s.Verdict != "refused" || s.Kind != "credential" {
		t.Fatalf("inactive token: %+v", s)
	}
	_, _ = f.rawDB.Exec(`UPDATE placeholder_keys SET is_active = 1`)
	_, _ = f.rawDB.Exec(`UPDATE api_keys SET is_active = 0`)
	if s := raw("GET", "/v1/ok"); s.Verdict != "refused" || s.Kind != "credential" {
		t.Fatalf("inactive key: %+v", s)
	}
	// Action mode is GitHub only.
	if code, _ := simCall(t, sim, f.placeholderID, map[string]interface{}{"mode": "action", "repo": "o/r", "action": "clone"}); code != 400 {
		t.Fatalf("action mode on a non-github token: %d", code)
	}
}

func jsonLine(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b) + "\n"
}

// TestSimulatorPerKey: a key group whose members resolve @default differently
// is reported per key, without advancing the round-robin index.
func TestSimulatorPerKey(t *testing.T) {
	e := newBrEnv(t, `{"version":"2","provider":"github","repositories":{"OWNER/REPO":{
 "capabilities":{"clone":true,"push":true,"pull_requests_read":true,"pull_requests_write":true},
 "branch_policy":{"rules":[{"branches":"@default","role":"protected"},{"branches":"**","role":"agent"}]}}}}`)
	f := e.f
	k1, k2 := "github_pat_"+strings.Repeat("a", 82), "github_pat_"+strings.Repeat("b", 82)
	for i, k := range []string{k1, k2} {
		enc, _ := f.crypto.Encrypt(k)
		if _, err := f.rawDB.Exec(`INSERT INTO api_keys (id, service_id, name, key_encrypted) VALUES (?, ?, ?, ?)`, fmt.Sprintf("gk-%d", i), f.serviceID, fmt.Sprintf("pool-%d", i), enc); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO api_key_groups (id, service_id, name, strategy) VALUES ('grp-1', '` + f.serviceID + `', 'pool', 'round-robin')`,
		`INSERT INTO api_key_group_members (group_id, api_key_id, priority) VALUES ('grp-1', 'gk-0', 1)`,
		`INSERT INTO api_key_group_members (group_id, api_key_id, priority) VALUES ('grp-1', 'gk-1', 2)`,
		`UPDATE placeholder_keys SET api_key_id = NULL, group_id = 'grp-1'`,
	} {
		if _, err := f.rawDB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	sim := newSimFor(f).WithGitHubDefaultBranchFetcher(func(_ context.Context, req handlers.GitHubDefaultBranchRequest) (string, error) {
		if req.Key == k1 {
			return "main", nil
		}
		return "trunk", nil
	})
	_, s := simCall(t, sim, f.placeholderID, map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "push", "ref": "main"})
	if len(s.PerKey) != 2 || s.Verdict != "refused" {
		t.Fatalf("expected a per-key split, refused on top: %+v", s)
	}
	got := map[string]string{}
	for _, k := range s.PerKey {
		got[k.KeyName] = k.Verdict
	}
	if got["pool-0"] != "refused" || got["pool-1"] != "allowed" {
		t.Fatalf("per key: %v", got)
	}
	var idx int
	_ = f.rawDB.QueryRow(`SELECT last_index FROM api_key_groups WHERE id = 'grp-1'`).Scan(&idx)
	var uses int
	_ = f.rawDB.QueryRow(`SELECT COALESCE(SUM(usage_count),0) FROM api_keys`).Scan(&uses)
	if idx != 0 || uses != 0 {
		t.Fatalf("key pool state changed: last_index=%d usage=%d", idx, uses)
	}
	// Same outcome on every key: no per_key.
	_, s = simCall(t, sim, f.placeholderID, map[string]interface{}{"mode": "action", "repo": "OWNER/REPO", "action": "clone"})
	if len(s.PerKey) != 0 || s.Verdict != "allowed" {
		t.Fatalf("identical outcomes must not be split: %+v", s)
	}
}

func TestAccessSummary(t *testing.T) {
	e := newBrEnv(t, simPolicyConfig)
	sim := newSimFor(e.f)
	get := func() (int, map[string]interface{}) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.SetPathValue("id", e.f.placeholderID)
		w := httptest.NewRecorder()
		sim.AccessSummary(w, r)
		var out map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, out := get()
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	repos := out["repos"].([]interface{})
	if len(repos) != 1 {
		t.Fatalf("repos: %v", repos)
	}
	r := repos[0].(map[string]interface{})
	if r["repo"] != "owner/repo" || r["branch_rules"] != true || r["clone"] != true || r["merge"] != false || r["approve"] != false || r["tags"] != true {
		t.Fatalf("repo flags: %v", r)
	}
	if r["default_branch"] != "main" {
		t.Fatalf("default_branch: %v", r["default_branch"])
	}
	rules := r["rules"].([]interface{})
	if len(rules) != 4 {
		t.Fatalf("rules: %v", rules)
	}
	def := rules[0].(map[string]interface{})
	agent := rules[1].(map[string]interface{})
	ci := rules[2].(map[string]interface{})
	if def["pattern"] != "@default" || def["push"] != false || def["pr_into"] != true ||
		agent["push"] != true || agent["create_force_delete"] != true || agent["run_workflows"] != false || ci["run_workflows"] != true {
		t.Fatalf("rule rows: %v", rules)
	}
	// Legacy grant: no rule rows, flagged legacy.
	e.setConfig(v2LegacyConfig)
	_, out = get()
	lr := out["repos"].([]interface{})[0].(map[string]interface{})
	if lr["default_branch"] != nil {
		t.Fatalf("legacy grants need no default branch: %v", lr["default_branch"])
	}
	if lr["legacy"] != true || lr["branch_rules"] != false || len(lr["rules"].([]interface{})) != 0 {
		t.Fatalf("legacy: %v", lr)
	}
	// Builder (v1) config with rows and a branch policy.
	e.setConfig(builderConfig(t, true, false))
	_, out = get()
	br := out["repos"].([]interface{})[0].(map[string]interface{})
	if br["merge"] != true || br["approve"] != false || br["branch_rules"] != true {
		t.Fatalf("builder: %v", br)
	}
}

func TestAccessSummaryNonGitHub(t *testing.T) {
	rig := newApproveRig(t, simApproveACL)
	sim := newSimFor(rig.f)
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.SetPathValue("id", rig.f.placeholderID)
	w := httptest.NewRecorder()
	sim.AccessSummary(w, r)
	if w.Code != 400 {
		t.Fatalf("non-github summary: %d", w.Code)
	}
}

var _ = sql.ErrNoRows
var _ = queries.DecisionACLDenied
