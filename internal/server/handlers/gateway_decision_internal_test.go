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
	if d == nil || d.Refs[0].Pattern != "" {
		t.Fatalf("without the default the rule is unknown: %+v", d)
	}
	st.enrich(d, func() (string, error) { return "", errors.New("down") })
	if d.Refs[0].Pattern != "" || d.Rule != "" {
		t.Fatalf("a failed lookup must leave it unnamed: %+v", d)
	}
	st.enrich(d, func() (string, error) { return "main", nil })
	if d.Refs[0].Pattern != "@default" || d.Refs[0].Role != "protected" || d.Rule != "@default (protected)" {
		t.Fatalf("enriched: %+v", d)
	}
	if !st.DependsOnDefault() {
		t.Fatal("a policy with an @default rule depends on the default branch")
	}
}
