package handlers

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/server/services"
)

func TestBuildActionRequestFraming(t *testing.T) {
	sr, err := buildActionRequest(simulateRequest{Mode: "action", Repo: "o/r", Action: "push", Ref: "agent/x"})
	if err != nil {
		t.Fatal(err)
	}
	if sr.Method != http.MethodPost || sr.Path != "/o/r.git/git-receive-pack" {
		t.Fatalf("request: %s %s", sr.Method, sr.Path)
	}
	rp, perr := parseReceivePackCommands(bytes.NewReader(sr.Body), receivePackMaxCommandBytes, receivePackMaxCommands)
	if perr != nil || len(rp.Commands) != 1 {
		t.Fatalf("synthetic push does not parse like a real one: %v", perr)
	}
	c := rp.Commands[0]
	if c.Ref != "refs/heads/agent/x" || isZeroOID(c.Old) || isZeroOID(c.New) || !rp.Caps["report-status"] || !rp.Caps["side-band-64k"] {
		t.Fatalf("command: %+v caps %v", c, rp.Caps)
	}
	// The command section ends in a flush packet followed by pack bytes.
	if !bytes.Equal(sr.Body[len(rp.Prefix):len(rp.Prefix)+4], []byte("PACK")) {
		t.Fatalf("no pack after the flush: %q", sr.Body[len(rp.Prefix):])
	}
	del, _ := buildActionRequest(simulateRequest{Repo: "o/r", Action: "force_or_delete", Ref: "refs/heads/old"})
	rp, _ = parseReceivePackCommands(bytes.NewReader(del.Body), receivePackMaxCommandBytes, receivePackMaxCommands)
	if len(rp.Commands) != 1 || !isZeroOID(rp.Commands[0].New) || rp.Commands[0].Ref != "refs/heads/old" {
		t.Fatalf("delete command: %+v", rp.Commands)
	}
	wf, _ := buildActionRequest(simulateRequest{Repo: "o/r", Action: "dispatch", Ref: "main"})
	if wf.Method != "POST" || wf.Path != "/repos/o/r/actions/workflows/ci.yml/dispatches" || string(wf.Body) != `{"ref":"main"}` {
		t.Fatalf("dispatch: %s %s %s", wf.Method, wf.Path, wf.Body)
	}
	clone, _ := buildActionRequest(simulateRequest{Repo: "o/r", Action: "clone"})
	if clone.Method != "GET" || clone.Path != "/o/r.git/info/refs" || clone.RawQuery != "service=git-upload-pack" {
		t.Fatalf("clone: %+v", clone)
	}
	pr, _ := buildActionRequest(simulateRequest{Repo: "o/r", Action: "open_pr_into", Ref: "refs/heads/main"})
	if !strings.Contains(string(pr.Body), `"base":"main"`) || !strings.Contains(string(pr.Body), `"head":"agent/sim"`) {
		t.Fatalf("pr body: %s", pr.Body)
	}
	if _, err := buildActionRequest(simulateRequest{Repo: "o/r", Action: "dispatch", Ref: "x", Workflow: "a/b"}); err == nil {
		t.Fatal("workflow with a slash must be rejected")
	}
}

func TestParseHexBody(t *testing.T) {
	b, err := parseHexBody(" 00 FF\nab\tCd ")
	if err != nil || !bytes.Equal(b, []byte{0x00, 0xff, 0xab, 0xcd}) {
		t.Fatalf("%x %v", b, err)
	}
	if b, err = parseHexBody(""); err != nil || len(b) != 0 {
		t.Fatalf("empty: %v", err)
	}
	for in, off := range map[string]int{"0g": 1, "abc": 2, " x": 1, "a b c": 4} {
		if _, err := parseHexBody(in); err == nil || err.offset == nil || *err.offset != off {
			t.Errorf("%q: %+v", in, err)
		}
	}
	big := strings.Repeat("00", maxInspectableRequestBodyBytes+1)
	if _, err := parseHexBody(big); err == nil || err.status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: %+v", err)
	}
}

func TestGuardKindClassification(t *testing.T) {
	for msg, want := range map[string]string{
		msgMergeNotPermitted:   "capability",
		msgApproveNotPermitted: "capability",
		"the repository's default branch could not be determined, so the request was refused": "uncheckable",
		"endpoint not allowed under branch rules":                                             "uncheckable",
		"request body must be a JSON object":                                                  "uncheckable",
		"truncated git-receive-pack command section":                                          "uncheckable",
		"GraphQL writes cannot be checked against branch rules":                               "uncheckable",
		"method override parameters are not allowed under branch rules":                       "uncheckable",
		`branch "x" has role "read" under the repository's branch rules; ...`:                 "branch_rules",
		`tag "v" may not be created under the repository's branch rules`:                      "branch_rules",
	} {
		if got := guardKind(msg); got != want {
			t.Errorf("%q: %s want %s", msg, got, want)
		}
	}
}

func decideWith(t *testing.T, cfg, method, path string, body gatewayBody) (*gatewayState, *GatewayDecision) {
	t.Helper()
	pc := services.NewPermissionChecker()
	st := &gatewayState{in: gatewayInput{
		Service: "github", Method: method, Path: path,
		Layers: []proxyACLLayer{{"placeholder", ""}}, PermissionConfig: cfg, // ACL layers are covered elsewhere: reach the guard
		Check: dryACLCheck(pc), Body: body,
	}}
	return st, st.decide()
}

const internalPolicyConfig = `{"version":"2","provider":"github","repositories":{"o/r":{
 "capabilities":{"clone":true,"push":true,"pull_requests_read":true,"pull_requests_write":true},
 "branch_policy":{"rules":[{"branches":"@default","role":"protected"},{"branches":"agent/**","role":"agent"},{"branches":"**","role":"read"}]}}}}`

// Paths that need a failing reader or an unusual policy, which the HTTP-level
// tests cannot reach: unreadable JSON body, uncheckable path, invalid policy.
func TestDecideUncheckableAndInvalidPaths(t *testing.T) {
	_, d := decideWith(t, internalPolicyConfig, "POST", "/repos/o/r/pulls", gatewayBody{
		Stream:  strings.NewReader("{}"),
		ReadAll: func(io.Reader) ([]byte, error) { return nil, errors.New("too large") },
	})
	if d == nil || d.Kind != "uncheckable" || d.status != http.StatusRequestEntityTooLarge || !d.logDenied || !strings.Contains(d.Sees, "too large or unreadable") {
		t.Fatalf("unreadable body: %+v", d)
	}
	// A write under /repos/ that does not name owner/repo cannot be attributed.
	_, d = decideWith(t, internalPolicyConfig, "POST", "/repos/justone", gatewayBody{InHand: true})
	if d == nil || d.Kind != "uncheckable" || d.jsonMsg != "duckway: request path cannot be checked against branch rules" {
		t.Fatalf("unresolved path: %+v", d)
	}
	// A path with a dot segment under a policy repo: restOK is false.
	_, d = decideWith(t, internalPolicyConfig, "POST", "/repos/o/r/pulls/./1", gatewayBody{InHand: true, Bytes: []byte("{}")})
	if d == nil || d.Kind != "uncheckable" || d.status != http.StatusForbidden {
		t.Fatalf("dot segment: %+v", d)
	}
	// An unusable policy fails closed with its own kind and an internal error to log.
	bad := `{"version":"2","provider":"github","repositories":{"o/r":{"capabilities":{"clone":true,"push":true},
 "branch_policy":{"rules":[{"branches":"x","role":"bogus"}]}}}}`
	_, d = decideWith(t, bad, "POST", "/repos/o/r/pulls", gatewayBody{InHand: true})
	if d == nil || d.Kind != "invalid_policy" || d.internal == nil || d.jsonMsg != "duckway: the branch policy of this grant is invalid" {
		t.Fatalf("invalid policy: %+v", d)
	}
}

// decide never reads past the receive-pack command section and reports what it
// consumed for replay, exactly as the proxy streams the pack afterwards.
func TestDecideReceivePackStreamsRemainder(t *testing.T) {
	sr, _ := buildActionRequest(simulateRequest{Repo: "o/r", Action: "push", Ref: "agent/x"})
	src := bytes.NewReader(sr.Body)
	st, d := decideWith(t, internalPolicyConfig, "POST", "/o/r.git/git-receive-pack", gatewayBody{Stream: src})
	if d != nil {
		t.Fatalf("precheck must not refuse an agent branch: %+v", d)
	}
	rest, _ := io.ReadAll(src)
	if !bytes.HasPrefix(rest, []byte("PACK")) || !bytes.Equal(append(append([]byte(nil), st.streamPrefix...), rest...), sr.Body) {
		t.Fatalf("prefix+remainder must rebuild the body (prefix %d bytes, rest %d)", len(st.streamPrefix), len(rest))
	}
	fin := st.finish(func() (string, error) { return "main", nil })
	if fin.Verdict != GatewayAllowed || len(fin.Refs) != 1 || fin.Refs[0].Pattern != "agent/**" {
		t.Fatalf("finish: %+v", fin)
	}
	// A refused streamed push asks the proxy to drain the unread body.
	sr, _ = buildActionRequest(simulateRequest{Repo: "o/r", Action: "push", Ref: "main"})
	st, d = decideWith(t, internalPolicyConfig, "POST", "/o/r.git/git-receive-pack", gatewayBody{Stream: bytes.NewReader(sr.Body)})
	if d == nil || !d.receive || !d.drain || d.Verdict != GatewayRefused {
		t.Fatalf("refused push: %+v", d)
	}
	_ = st
}

func TestEnrichNamesRuleFromDefault(t *testing.T) {
	sr, _ := buildActionRequest(simulateRequest{Repo: "o/r", Action: "push", Ref: "main"})
	st, d := decideWith(t, internalPolicyConfig, "POST", "/o/r.git/git-receive-pack", gatewayBody{InHand: true, Bytes: sr.Body})
	// Refused whether or not main is the default: the non-@default rule decided it.
	if d == nil || d.Refs[0].Pattern != "**" || d.Refs[0].Role != "read" || d.Rule != "** \u2192 read" {
		t.Fatalf("without the default the deciding rule is the non-@default match: %+v", d)
	}
	st.enrich(d, func() (string, error) { return "", errors.New("down") })
	if d.Refs[0].Pattern != "**" || d.Rule != "** \u2192 read" {
		t.Fatalf("a failed lookup must keep the fallback: %+v", d)
	}
	st.enrich(d, func() (string, error) { return "main", nil })
	if d.Refs[0].Pattern != "@default" || d.Refs[0].Role != "protected" || d.Rule != "@default \u2192 protected" {
		t.Fatalf("enriched: %+v", d)
	}
	if !st.DependsOnDefault() {
		t.Fatal("a policy with an @default rule depends on the default branch")
	}
}

func TestParseHexBodyReportsTheCharacterNotAByte(t *testing.T) {
	_, err := parseHexBody("ab€c")
	if err == nil || err.offset == nil || *err.offset != 2 || !strings.Contains(err.msg, "'€'") || !strings.Contains(err.msg, "offset 2") {
		t.Fatalf("multi-byte character: %+v", err)
	}
	// The offset counts characters, not bytes: two 2-byte letters precede the problem.
	_, err = parseHexBody("ééz")
	if err == nil || *err.offset != 0 {
		t.Fatalf("first character is already invalid: %+v", err)
	}
	_, err = parseHexBody("0 1\n2")
	if err == nil || *err.offset != 4 || !strings.Contains(err.msg, "odd number") {
		t.Fatalf("odd digits: %+v", err)
	}
}

func TestBuildRawRequestPathChecks(t *testing.T) {
	for path, want := range map[string]string{
		"/repos/o/r#frag":         "only be sent in the query string",
		"/repos/o/r?x=1#frag":     "fragment",
		"/repos/o/../x":           "redirect this path to /repos/x",
		"/repos//o/r":             "redirect this path to /repos/o/r",
		"/proxy/github/a/./b":     "redirect this path to /a/b",
		"/repos/o/r/../../..":     "redirect",
		"/repos/o/r/contents/%23": "invalid character",
	} {
		_, err := buildRawRequest(simulateRequest{Mode: "raw", Method: "GET", Path: path}, "github")
		if err == nil || err.status != http.StatusBadRequest || !strings.Contains(err.msg, want) {
			t.Errorf("%s: %+v (want %q)", path, err, want)
		}
	}
	if sr, err := buildRawRequest(simulateRequest{Mode: "raw", Method: "GET", Path: "/repos/o/r/contents/dir/"}, "github"); err != nil || sr.Path != "/repos/o/r/contents/dir/" {
		t.Errorf("a trailing slash needs no cleaning: %v %+v", err, sr)
	}
}

// Every message the guard can produce for a request classifies as the kind and
// cause the decision reports; driving the real evaluator keeps guardKind and
// causeRule honest if a message changes.
func TestGuardKindsAndCausesForRealRefusals(t *testing.T) {
	pols, err := services.ParseGitHubBranchPolicies(internalPolicyConfig)
	if err != nil {
		t.Fatal(err)
	}
	pol := pols["o/r"]
	known := func() (string, error) { return "main", nil }
	cases := []struct {
		name, method string
		rest         []string
		body         string
		def          defaultBranchFn
		kind, cause  string
	}{
		{"merge off", "PUT", []string{"pulls", "1", "merge"}, "", known, "capability", ""},
		{"approve off", "POST", []string{"pulls", "1", "reviews"}, `{"event":"APPROVE"}`, known, "capability", ""},
		{"unclassified write", "DELETE", []string{"branches", "main", "protection"}, "", known, "uncheckable", ""},
		{"unsupported refs write", "DELETE", []string{"git", "refs"}, "", known, "uncheckable", ""},
		{"body not an object", "POST", []string{"git", "refs"}, "[]", known, "uncheckable", ""},
		{"missing field", "POST", []string{"git", "refs"}, `{}`, known, "uncheckable", ""},
		{"wrong field type", "POST", []string{"git", "refs"}, `{"ref":1}`, known, "uncheckable", ""},
		{"default unknown", "POST", []string{"pulls"}, `{"base":"x"}`, func() (string, error) { return "", errors.New("down") }, "uncheckable", ""},
		{"branch role", "POST", []string{"git", "refs"}, `{"ref":"refs/heads/feature"}`, known, "branch_rules", ""},
		{"tag not allowed", "POST", []string{"git", "refs"}, `{"ref":"refs/tags/x"}`, known, "branch_rules", ""},
		{"neither branch nor tag", "POST", []string{"git", "refs"}, `{"ref":"refs/notes/x"}`, known, "branch_rules", "ref type not allowed"},
		{"invalid branch name", "POST", []string{"git", "refs"}, `{"ref":"refs/heads/a..b"}`, known, "branch_rules", "invalid ref name"},
		{"existing tag", "PATCH", []string{"git", "refs", "tags", "v1"}, `{}`, known, "branch_rules", "existing tags never move"},
		{"other ref namespace", "PATCH", []string{"git", "refs", "notes", "x"}, `{}`, known, "branch_rules", "ref type not allowed"},
		{"dispatch on a tag", "POST", []string{"actions", "workflows", "ci.yml", "dispatches"}, `{"ref":"refs/tags/v1"}`, known, "branch_rules", "ref type not allowed"},
	}
	for _, tc := range cases {
		msg := evaluateGitHubGuardREST(pol, tc.def, tc.method, tc.rest, []byte(tc.body))
		if msg == "" {
			t.Errorf("%s: expected a refusal", tc.name)
			continue
		}
		if got := guardKind(msg); got != tc.kind {
			t.Errorf("%s: kind %s want %s (%q)", tc.name, got, tc.kind, msg)
		}
		st := &gatewayState{guard: &githubGuard{policy: pol}}
		rule := st.ruleBehind(guardKind(msg), msg)
		switch {
		case tc.kind == "capability":
			if rule != "pull_requests_merge" && rule != "pull_requests_approve" {
				t.Errorf("%s: capability rule %q", tc.name, rule)
			}
		case tc.cause != "":
			if rule != tc.cause {
				t.Errorf("%s: rule %q want cause %q (%q)", tc.name, rule, tc.cause, msg)
			}
		case tc.kind == "uncheckable":
			if rule != "" {
				t.Errorf("%s: an uncheckable request has no deciding rule, got %q", tc.name, rule)
			}
		default:
			if rule == "" {
				t.Errorf("%s: rule-bearing refusal has no rule (%q)", tc.name, msg)
			}
		}
	}
	// Push commands: invalid, outside and tag refusals are cause-only; rule
	// refusals name a rule; a ref decided by no rule never names one.
	e := &guardEval{policy: pol, def: known}
	st := &gatewayState{guard: &githubGuard{policy: pol}}
	for ref, wantRule := range map[string]bool{
		"refs/heads/feature/x": true, "refs/heads/main": true, "refs/tags/new": true,
		"refs/heads/a..b": false, "refs/notes/x": false, strings.Repeat("r", 300): false,
	} {
		cmd := receivePackCommand{Old: oidZero, New: strings.Repeat("a", 40), Ref: ref}
		reason := e.checkPushCommand(cmd)
		if reason == "" {
			t.Errorf("%s: expected refusal", ref)
			continue
		}
		p, r := st.refRule(e, DecisionRef{Ref: ref, Verdict: "refused", Reason: reason})
		if (p != "" || r != "") != wantRule {
			t.Errorf("%s: pattern %q role %q for %q", ref, p, r, reason)
		}
	}
	// An existing tag moved is refused for a cause, never by a rule.
	reason := e.checkPushCommand(receivePackCommand{Old: strings.Repeat("b", 40), New: strings.Repeat("a", 40), Ref: "refs/tags/v1"})
	if p, r := st.refRule(e, DecisionRef{Ref: "refs/tags/v1", Verdict: "refused", Reason: reason}); p != "" || r != "" || causeRule(reason) != "existing tags never move" {
		t.Errorf("tag move: %q %q %q", p, r, reason)
	}
}

const oidZero = "0000000000000000000000000000000000000000"

func TestRefuseGuardComparesMethodCaseInsensitively(t *testing.T) {
	st := &gatewayState{in: gatewayInput{Method: "post", Path: "/o/r.git/git-receive-pack"}}
	d := st.refuseGuard(nil, nil, "pushes are refused", false, nil)
	if !d.receive || d.Sees != "duckway: pushes are refused\n" {
		t.Fatalf("%+v", d)
	}
}
