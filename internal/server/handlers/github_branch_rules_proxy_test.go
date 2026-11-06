package handlers_test

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/services"
)

const brPlaceholder = "github_pat_dw_fake"

const v2BranchPolicyConfig = `{"version":"2","provider":"github","repositories":{"OWNER/REPO":{
 "capabilities":{"clone":true,"push":true,"pull_requests_read":true,"pull_requests_write":true,"actions_read":true,"workflow_dispatch":true},
 "workflow_allowlist":["ci.yml"],
 "branch_policy":{"rules":[
   {"branches":"@default","role":"protected"},
   {"branches":"agent/**","role":"agent"},
   {"branches":"ci/**","role":"agent","run_workflows":true},
   {"branches":"**","role":"read"}],
  "tags":{"create":["v*"]}}}}}`

const v2LegacyConfig = `{"version":"2","provider":"github","repositories":{"OWNER/REPO":{
 "capabilities":{"clone":true,"push":true,"pull_requests_read":true,"pull_requests_write":true}}}}`

// brEnv is a proxy handler fronting a fake GitHub that serves the REST API
// and git smart HTTP for a real bare repository.
type brEnv struct {
	t        *testing.T
	proxy    *httptest.Server
	h        *handlers.ProxyHandler
	f        *proxyFixture
	bare     string
	mu       sync.Mutex
	requests []string          // "METHOD path" seen by the fake upstream
	bodies   map[string][]byte // last body per "METHOD path"
	lookups  int32
	apiFail  atomic.Bool
}

func (e *brEnv) saw(prefix string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, r := range e.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func gitRun(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.x",
	}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func newBrEnv(t *testing.T, config string) *brEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	e := &brEnv{t: t, bodies: map[string][]byte{}}
	e.bare = filepath.Join(t.TempDir(), "upstream.git")
	if out, err := gitRun(t, filepath.Dir(e.bare), nil, "init", "--bare", "-q", e.bare); err != nil {
		t.Fatalf("init bare: %v %s", err, out)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		e.mu.Lock()
		e.requests = append(e.requests, key)
		e.bodies[key] = body
		e.mu.Unlock()
		switch {
		case r.Method == "GET" && strings.EqualFold(r.URL.Path, "/repos/owner/repo"):
			atomic.AddInt32(&e.lookups, 1)
			if e.apiFail.Load() {
				http.Error(w, "nope", 500)
				return
			}
			_, _ = io.WriteString(w, `{"default_branch":"main"}`)
		case strings.HasSuffix(r.URL.Path, ".git/info/refs"):
			svc := r.URL.Query().Get("service")
			cmd := exec.Command("git", strings.TrimPrefix(svc, "git-"), "--stateless-rpc", "--advertise-refs", e.bare)
			out, err := cmd.Output()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
			pl := "# service=" + svc + "\n"
			fmt.Fprintf(w, "%04x%s0000", len(pl)+4, pl)
			_, _ = w.Write(out)
		case strings.HasSuffix(r.URL.Path, ".git/git-receive-pack"):
			cmd := exec.Command("git", "receive-pack", "--stateless-rpc", e.bare)
			cmd.Stdin = bytes.NewReader(body)
			out, _ := cmd.Output()
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			_, _ = w.Write(out)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(upstream.Close)

	f := newProxyFixture(t, upstream.URL)
	seedGitHubService(t, f)
	realPAT := "github_pat_" + strings.Repeat("r", 82)
	enc, err := f.crypto.Encrypt(realPAT)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := f.apiKeyQ.GetByID(f.apiKeyID)
	key.KeyEncrypted = enc
	if err := f.apiKeyQ.Update(key); err != nil {
		t.Fatal(err)
	}
	if err := f.placeholderQ.UpdatePlaceholder(f.placeholderID, brPlaceholder); err != nil {
		t.Fatal(err)
	}
	e.f = f
	e.setConfig(config)
	e.h = newProxyHandler(f)
	e.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.h.Handle(w, withClient(r, f.client))
	}))
	t.Cleanup(e.proxy.Close)
	return e
}

func (e *brEnv) setConfig(config string) {
	ph, err := e.f.placeholderQ.GetByID(e.f.placeholderID)
	if err != nil {
		e.t.Fatal(err)
	}
	ph.PermissionConfig = &config
	if err := e.f.placeholderQ.Update(ph); err != nil {
		e.t.Fatal(err)
	}
}

func (e *brEnv) rest(method, path, body string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.proxy.URL+"/proxy/github"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+brPlaceholder)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *brEnv) work() string {
	e.t.Helper()
	dir := e.t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := gitRun(e.t, dir, nil, args...); err != nil {
			e.t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func (e *brEnv) push(dir string, args ...string) (string, error) {
	auth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+brPlaceholder))
	all := append([]string{"-c", "http.extraHeader=" + auth, "push", e.proxy.URL + "/proxy/github/OWNER/REPO.git"}, args...)
	return gitRun(e.t, dir, nil, all...)
}

func (e *brEnv) remoteRefs() string {
	out, _ := gitRun(e.t, e.bare, nil, "for-each-ref", "--format=%(refname)")
	return out
}

func TestBranchRulesRealGitPush(t *testing.T) {
	e := newBrEnv(t, v2BranchPolicyConfig)
	dir := e.work()

	// Refused: main is the default branch (protected). Nothing reaches upstream.
	out, err := e.push(dir, "main:main")
	if err == nil {
		t.Fatalf("push to protected default branch succeeded:\n%s", out)
	}
	if !strings.Contains(out, "! [remote rejected] main -> main (duckway:") {
		t.Fatalf("missing remote-rejected text:\n%s", out)
	}
	if n := e.saw("POST /OWNER/REPO.git/git-receive-pack"); n != 0 {
		t.Fatalf("refused push reached upstream %d times", n)
	}
	if e.remoteRefs() != "" {
		t.Fatalf("refs created upstream: %s", e.remoteRefs())
	}

	// Atomic refusal: an allowed ref in the same push is not forwarded either.
	out, err = e.push(dir, "main:agent/ok", "main:main")
	if err == nil || !strings.Contains(out, "another ref in this push was refused") || !strings.Contains(out, "(duckway:") {
		t.Fatalf("mixed push: err=%v\n%s", err, out)
	}
	if e.remoteRefs() != "" || e.saw("POST /OWNER/REPO.git/git-receive-pack") != 0 {
		t.Fatal("mixed push was partially forwarded")
	}

	// Allowed: agent branch streams the pack and succeeds, with real objects.
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), bytes.Repeat([]byte("duckway-pack-"), 200000), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "big.bin"}, {"commit", "-q", "-m", "payload"}} {
		if o, err := gitRun(t, dir, nil, args...); err != nil {
			t.Fatalf("%v %s", err, o)
		}
	}
	out, err = e.push(dir, "main:refs/heads/agent/work")
	if err != nil {
		t.Fatalf("allowed push failed: %v\n%s", err, out)
	}
	if !strings.Contains(e.remoteRefs(), "refs/heads/agent/work") {
		t.Fatalf("branch missing upstream: %q", e.remoteRefs())
	}
	if o, err := gitRun(t, e.bare, nil, "fsck", "--strict"); err != nil {
		t.Fatalf("upstream repository corrupt (pack not streamed intact): %v\n%s", err, o)
	}
	if o, _ := gitRun(t, e.bare, nil, "rev-parse", "refs/heads/agent/work"); strings.TrimSpace(o) == "" {
		t.Fatal("no commit")
	}

	// Force update and delete of an agent branch are allowed.
	if out, err = e.push(dir, "--force", "main~1:refs/heads/agent/work"); err != nil {
		t.Fatalf("force push: %v\n%s", err, out)
	}
	if out, err = e.push(dir, ":refs/heads/agent/work"); err != nil {
		t.Fatalf("delete: %v\n%s", err, out)
	}

	// Tags: create matching tags.create ok; other namespaces refused.
	if o, err := gitRun(t, dir, nil, "tag", "v1"); err != nil {
		t.Fatalf("%v %s", err, o)
	}
	if out, err = e.push(dir, "v1"); err != nil {
		t.Fatalf("tag push: %v\n%s", err, out)
	}
	_, _ = gitRun(t, dir, nil, "tag", "nope")
	if out, err = e.push(dir, "nope"); err == nil || !strings.Contains(out, "remote rejected") {
		t.Fatalf("unmatched tag push: %v\n%s", err, out)
	}
	if out, err = e.push(dir, "main:refs/notes/x"); err == nil || !strings.Contains(out, "remote rejected") {
		t.Fatalf("notes push: %v\n%s", err, out)
	}
	_, _ = gitRun(t, dir, nil, "commit", "-q", "--allow-empty", "-m", "again")
	_, _ = gitRun(t, dir, nil, "tag", "-f", "v1")
	if out, err = e.push(dir, "--force", "v1"); err == nil || !strings.Contains(out, "remote rejected") {
		t.Fatalf("tag move: %v\n%s", err, out)
	}

	// Refusals are recorded like ACL denials.
	var n int
	if err := e.f.rawDB.QueryRow(`SELECT COUNT(*) FROM request_log WHERE decision = ? AND method = 'POST' AND path LIKE '%git-receive-pack'`, queries.DecisionACLDenied).Scan(&n); err != nil || n == 0 {
		t.Fatalf("no acl_denied log row for refused pushes: n=%d err=%v", n, err)
	}
	if got := e.saw("GET /repos/owner/repo"); got < 1 || got > 2 {
		t.Fatalf("default branch lookups = %d (want cached)", got)
	}
}

func TestBranchRulesPushDefaultLookupFailureFailsClosed(t *testing.T) {
	e := newBrEnv(t, v2BranchPolicyConfig)
	e.apiFail.Store(true)
	dir := e.work()
	out, err := e.push(dir, "main:refs/heads/agent/x")
	if err == nil || !strings.Contains(out, "remote rejected") || !strings.Contains(out, "default branch") {
		t.Fatalf("want fail-closed rejection, got err=%v\n%s", err, out)
	}
	if e.saw("POST /OWNER/REPO.git/git-receive-pack") != 0 {
		t.Fatal("forwarded despite failed lookup")
	}
}

func TestBranchRulesLegacyPushUnchanged(t *testing.T) {
	e := newBrEnv(t, v2LegacyConfig)
	dir := e.work()
	if out, err := e.push(dir, "main:main"); err != nil {
		t.Fatalf("legacy push to main must work: %v\n%s", err, out)
	}
	if e.saw("GET /repos/owner/repo") != 0 {
		t.Fatal("legacy config triggered a default-branch lookup")
	}
}

func TestBranchRulesReceivePackContentEncodingRefused(t *testing.T) {
	e := newBrEnv(t, v2BranchPolicyConfig)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("0000"))
	_ = zw.Close()
	req, _ := http.NewRequest("POST", e.proxy.URL+"/proxy/github/OWNER/REPO.git/git-receive-pack", &gz)
	req.Header.Set("Authorization", "Bearer "+brPlaceholder)
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if e.saw("POST ") != 0 {
		t.Fatal("forwarded")
	}
}

func TestBranchRulesReceivePackMalformedCommandsRefused(t *testing.T) {
	e := newBrEnv(t, v2BranchPolicyConfig)
	for name, body := range map[string]string{"garbage": "zzzzzzzz", "truncated": "0032abc", "empty": ""} {
		code, _ := e.rest("POST", "/OWNER/REPO.git/git-receive-pack", body)
		if code != http.StatusForbidden {
			t.Errorf("%s: status %d", name, code)
		}
	}
	if e.saw("POST ") != 0 {
		t.Fatal("malformed push forwarded")
	}
}

// builderConfig is what the phantom token page stores: capability-table
// endpoints plus github_branch_policies.
func builderConfig(t *testing.T, merge, approve bool) string {
	t.Helper()
	rule, err := services.CompileGitHubCapabilityRule(services.GitHubCapabilityRows{
		"owner/repo": {"contents": "write", "pull_requests": "write", "actions": "write"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rule.GitHubBranchPolicies = map[string]services.GitHubRepoBranchPolicy{"owner/repo": {
		BranchPolicy: services.GitHubBranchPolicy{
			Rules: []services.GitHubBranchRule{
				{Branches: "@default", Role: "protected"},
				{Branches: "agent/**", Role: "agent"},
				{Branches: "ci/**", Role: "agent", RunWorkflows: true},
				{Branches: "**", Role: "read"},
			},
			Tags: services.GitHubTagPolicy{Create: []string{"v*"}},
		},
		PullRequestsMerge: merge, PullRequestsApprove: approve,
	}}
	cfg, _ := json.Marshal(services.PermissionConfig{Version: "1", Provider: "github", Rules: []services.PermissionRule{rule}})
	if err := services.ValidatePermissionConfig(string(cfg)); err != nil {
		t.Fatal(err)
	}
	return string(cfg)
}

func TestBranchRulesBuilderRESTEnforcement(t *testing.T) {
	e := newBrEnv(t, builderConfig(t, false, false))
	type rc struct {
		method, path, body string
		want               int
	}
	const r = "/repos/OWNER/REPO"
	for _, c := range []rc{
		{"PUT", r + "/contents/a.txt", `{"branch":"agent/x","message":"m","content":"eA=="}`, 200},
		{"PUT", r + "/contents/a.txt", `{"branch":"main","message":"m","content":"eA=="}`, 403},
		{"PUT", r + "/contents/a.txt", `{"message":"m","content":"eA=="}`, 403}, // default branch is protected
		{"PUT", r + "/contents/a.txt", `{not json`, 403},
		{"POST", r + "/git/refs", `{"ref":"refs/heads/agent/y","sha":"1"}`, 200},
		{"POST", r + "/git/refs", `{"ref":"refs/heads/main","sha":"1"}`, 403},
		{"POST", r + "/git/refs", `{"ref":"refs/tags/v2","sha":"1"}`, 200},
		{"PATCH", r + "/git/refs/heads/main", `{"sha":"1"}`, 403},
		{"PATCH", r + "/git/refs/heads/agent/y", `{"sha":"1","force":true}`, 200},
		{"DELETE", r + "/git/refs/tags/v2", ``, 403},
		{"POST", r + "/git/blobs", `{"content":"x"}`, 200},
		{"POST", r + "/merges", `{"base":"agent/y","head":"agent/x"}`, 200},
		{"POST", r + "/merges", `{"base":"main","head":"agent/x"}`, 403},
		{"POST", r + "/releases", `{"tag_name":"v3"}`, 200},
		{"POST", r + "/releases", `{"tag_name":"zzz"}`, 403},
		{"POST", r + "/pulls", `{"base":"main","head":"agent/x","title":"t"}`, 200},
		{"POST", r + "/pulls", `{"base":"other","head":"agent/x","title":"t"}`, 403},
		{"PATCH", r + "/pulls/1", `{"title":"t"}`, 200},
		{"PATCH", r + "/pulls/1", `{"base":"other"}`, 403},
		{"PUT", r + "/pulls/1/merge", `{}`, 403},
		{"PUT", r + "/pulls/1/update-branch", ``, 403},
		{"POST", r + "/pulls/1/reviews", `{"event":"APPROVE"}`, 403},
		{"POST", r + "/pulls/1/reviews", `{"event":"COMMENT","body":"x"}`, 200},
		{"POST", r + "/pulls/1/reviews/4/events", `{"event":"APPROVE"}`, 403},
		{"POST", r + "/actions/workflows/ci.yml/dispatches", `{"ref":"ci/z"}`, 200},
		{"POST", r + "/actions/workflows/ci.yml/dispatches", `{"ref":"agent/z"}`, 403},
		{"POST", r + "/actions/runs/9/rerun", ``, 200},
	} {
		code, body := e.rest(c.method, c.path, c.body)
		if code != c.want {
			t.Errorf("%s %s %s: status %d, want %d (%s)", c.method, c.path, c.body, code, c.want, body)
		}
		if c.want == 403 {
			var m map[string]string
			if json.Unmarshal([]byte(body), &m) != nil || !strings.HasPrefix(m["error"], "duckway: ") {
				t.Errorf("%s %s: refusal body %q is not {error: duckway: ...}", c.method, c.path, body)
			}
		}
	}
	// Refused requests were never forwarded; allowed ones carry their body intact.
	e.mu.Lock()
	if _, ok := e.bodies["PUT /repos/OWNER/REPO/pulls/1/merge"]; ok {
		t.Error("refused merge was forwarded")
	}
	if got := string(e.bodies["PUT /repos/OWNER/REPO/contents/a.txt"]); !strings.Contains(got, `"agent/x"`) && !strings.Contains(got, `"content"`) {
		t.Errorf("allowed body not forwarded intact: %q", got)
	}
	e.mu.Unlock()
	var n int
	if err := e.f.rawDB.QueryRow(`SELECT COUNT(*) FROM request_log WHERE decision = ?`, queries.DecisionACLDenied).Scan(&n); err != nil || n == 0 {
		t.Fatalf("refusals not logged: n=%d err=%v", n, err)
	}

	// Flags on: merge and approve pass, ref rules still apply.
	e2 := newBrEnv(t, builderConfig(t, true, true))
	for _, c := range []rc{
		{"PUT", r + "/pulls/1/merge", `{}`, 200},
		{"PUT", r + "/pulls/1/update-branch", ``, 200},
		{"POST", r + "/pulls/1/reviews", `{"event":"approve"}`, 200},
		{"POST", r + "/pulls/1/reviews/4/events", `{"event":"APPROVE"}`, 200},
		{"POST", r + "/git/refs", `{"ref":"refs/heads/main","sha":"1"}`, 403},
	} {
		if code, body := e2.rest(c.method, c.path, c.body); code != c.want {
			t.Errorf("flags on: %s %s: %d want %d (%s)", c.method, c.path, code, c.want, body)
		}
	}
}

func TestBranchRulesBuilderDefaultLookupFailure(t *testing.T) {
	e := newBrEnv(t, builderConfig(t, false, false))
	e.apiFail.Store(true)
	code, body := e.rest("PUT", "/repos/OWNER/REPO/contents/a.txt", `{"message":"m"}`)
	if code != 403 || !strings.Contains(body, "default branch") {
		t.Fatalf("want fail-closed 403, got %d %s", code, body)
	}
	// A request that does not depend on @default still works with the lookup down... except
	// that this policy has an @default rule, so even agent branches fail closed.
	code, _ = e.rest("PUT", "/repos/OWNER/REPO/contents/a.txt", `{"branch":"agent/x","message":"m"}`)
	if code != 403 {
		t.Fatalf("agent branch with lookup down: %d", code)
	}
	if e.saw("PUT ") != 0 {
		t.Fatal("forwarded")
	}
}

func TestBranchRulesV2RESTAndLegacyIssues(t *testing.T) {
	e := newBrEnv(t, v2BranchPolicyConfig)
	// v2 opens PR create (POST /pulls) and dispatch; guard narrows them.
	if code, _ := e.rest("POST", "/repos/OWNER/REPO/pulls", `{"base":"main","head":"agent/x"}`); code != 200 {
		t.Fatalf("pr create: %d", code)
	}
	if code, _ := e.rest("POST", "/repos/OWNER/REPO/pulls", `{"base":"other","head":"agent/x"}`); code != 403 {
		t.Fatalf("pr create to read branch: %d", code)
	}
	// Merge endpoint is not opened at all without pull_requests_merge.
	if code, body := e.rest("PUT", "/repos/OWNER/REPO/pulls/1/merge", `{}`); code != 403 || !strings.Contains(body, "permission denied") {
		t.Fatalf("merge: %d %s", code, body)
	}
	if code, _ := e.rest("POST", "/repos/OWNER/REPO/actions/workflows/ci.yml/dispatches", `{"ref":"ci/x"}`); code != 200 {
		t.Fatalf("dispatch on run_workflows branch: %d", code)
	}
	if code, _ := e.rest("POST", "/repos/OWNER/REPO/actions/workflows/ci.yml/dispatches", `{"ref":"main"}`); code != 403 {
		t.Fatalf("dispatch on default: %d", code)
	}
	if code, _ := e.rest("POST", "/repos/OWNER/REPO/actions/workflows/ci.yml/dispatches", `{"ref":"agent/x"}`); code != 403 {
		t.Fatalf("dispatch on agent branch without run: %d", code)
	}

	// Legacy v2: nothing changes, no lookups.
	legacy := newBrEnv(t, v2LegacyConfig)
	if code, _ := legacy.rest("POST", "/repos/OWNER/REPO/pulls", `{"base":"anything"}`); code != 200 {
		t.Fatalf("legacy pr create: %d", code)
	}
	if legacy.saw("GET /repos/owner/repo") != 0 {
		t.Fatal("legacy lookup")
	}
}

func TestBranchRulesV2MergeOpenedByFlag(t *testing.T) {
	cfg := strings.Replace(v2BranchPolicyConfig, `"pull_requests_write":true,`, `"pull_requests_write":true,"pull_requests_merge":true,`, 1)
	e := newBrEnv(t, cfg)
	if code, body := e.rest("PUT", "/repos/OWNER/REPO/pulls/1/merge", `{}`); code != 200 {
		t.Fatalf("merge: %d %s", code, body)
	}
	if code, _ := e.rest("PUT", "/repos/OWNER/REPO/pulls/1/update-branch", ``); code != 200 {
		t.Fatalf("update-branch: %d", code)
	}
	// Approve is still refused (flag off).
	if code, _ := e.rest("POST", "/repos/OWNER/REPO/pulls/1/reviews", `{"event":"APPROVE"}`); code != 403 {
		t.Fatalf("approve: %d", code)
	}
}
