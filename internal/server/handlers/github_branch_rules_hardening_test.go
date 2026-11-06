package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/server/services"
)

func agentEverywherePolicy() services.GitHubRepoBranchPolicy {
	return services.GitHubRepoBranchPolicy{BranchPolicy: services.GitHubBranchPolicy{
		Rules: []services.GitHubBranchRule{
			{Branches: "@default", Role: "protected"},
			{Branches: "**", Role: "agent", RunWorkflows: true},
		},
		Tags: services.GitHubTagPolicy{Create: []string{"v*"}},
	}}
}

func TestGuardOversizeNamesRefusedQuickly(t *testing.T) {
	big := strings.Repeat("a", 1<<20)
	pol := agentEverywherePolicy()
	start := time.Now()
	cases := []struct {
		name, method, rest, body string
	}{
		{"contents branch", "PUT", "contents/x", `{"branch":"` + big + `"}`},
		{"contents refs/heads branch", "PUT", "contents/x", `{"branch":"refs/heads/` + big + `"}`},
		{"merges base", "POST", "merges", `{"base":"` + big + `"}`},
		{"pulls base", "POST", "pulls", `{"base":"` + big + `"}`},
		{"git refs ref", "POST", "git/refs", `{"ref":"refs/heads/` + big + `"}`},
		{"git refs tag", "POST", "git/refs", `{"ref":"refs/tags/v` + big + `"}`},
		{"release tag_name", "POST", "releases", `{"tag_name":"v` + big + `"}`},
		{"dispatch ref", "POST", "actions/workflows/ci.yml/dispatches", `{"ref":"` + big + `"}`},
		{"256 bytes", "PUT", "contents/x", `{"branch":"` + strings.Repeat("b", 256) + `"}`},
	}
	for _, c := range cases {
		if msg := evaluateGitHubGuardREST(pol, defMain, c.method, strings.Split(c.rest, "/"), []byte(c.body)); msg == "" {
			t.Errorf("%s: accepted", c.name)
		}
	}
	e := &guardEval{policy: pol, def: defMain}
	if r := e.checkPushCommand(receivePackCommand{Old: strings.Repeat("0", 40), New: strings.Repeat("1", 40), Ref: "refs/heads/" + strings.Repeat("a", 64<<10)}); r == "" || len(r) > 200 {
		t.Errorf("64 KB push ref: %q", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("oversize names took %v", d)
	}
	if msg := evaluateGitHubGuardREST(pol, defMain, "PUT", []string{"contents", "x"}, []byte(`{"branch":"`+strings.Repeat("b", 255)+`"}`)); msg != "" {
		t.Errorf("255-byte branch refused: %s", msg)
	}
}

func TestGuardRESTNameSpoofsRefused(t *testing.T) {
	pol := agentEverywherePolicy()
	for _, name := range []string{"heads/x", "tags/x", "remotes/x", "a..b", "a@{b", "a//b", "/a", "a/", "refs/heads/heads/x", "refs/heads//x"} {
		for _, c := range []struct{ method, rest, field string }{
			{"PUT", "contents/f", "branch"}, {"POST", "merges", "base"}, {"POST", "pulls", "base"},
			{"POST", "actions/workflows/ci.yml/dispatches", "ref"},
		} {
			body := fmt.Sprintf(`{%q:%q}`, c.field, name)
			if msg := evaluateGitHubGuardREST(pol, defMain, c.method, strings.Split(c.rest, "/"), []byte(body)); msg == "" {
				t.Errorf("%s %s %q accepted", c.method, c.rest, name)
			}
		}
	}
	// "MAIN" is treated as the default branch (protected), never as a free branch.
	for _, name := range []string{"MAIN", "Main", "refs/heads/MAIN"} {
		if msg := evaluateGitHubGuardREST(pol, defMain, "PUT", []string{"contents", "f"}, []byte(`{"branch":"`+name+`"}`)); msg == "" {
			t.Errorf("case variant %q of the default branch writable", name)
		}
	}
	if msg := evaluateGitHubGuardREST(pol, defMain, "PUT", []string{"contents", "f"}, []byte(`{"branch":"feature"}`)); msg != "" {
		t.Errorf("plain agent branch refused: %s", msg)
	}
}

func TestGuardDefaultDenyUnderPolicy(t *testing.T) {
	g := guardPolicy()
	cases := []struct {
		method, rest string
		allow        bool
	}{
		{"POST", "branches/main/rename", false},
		{"POST", "merge-upstream", false},
		{"POST", "actions/runners/registration-token", false},
		{"PUT", "collaborators/bob", false},
		{"POST", "forks", false},
		{"PATCH", "", false},
		{"DELETE", "branches/main/protection", false},
		{"POST", "pulls/1/merge", false},
		{"POST", "pulls/1/update-branch", false},
		{"POST", "releases/generate-notes", false},
		{"PATCH", "releases/assets/4", false},
		{"PUT", "git/refs", false},
		{"POST", "git/blobs/x", false},
		{"POST", "issues/1/comments", true},
		{"PATCH", "issues/1", true},
		{"POST", "pulls/1/comments", true},
		{"POST", "pulls/1/reviews", true},
		{"PUT", "pulls/1/requested_reviewers", true},
		{"POST", "actions/runs/9/rerun", true},
		{"POST", "actions/runs/9/cancel", true},
		{"POST", "git/blobs", true},
		{"POST", "git/trees", true},
		{"POST", "git/commits", true},
		{"POST", "git/tags", true},
		{"DELETE", "releases/5", true},
		{"POST", "releases/5/assets", true},
		{"DELETE", "releases/5/assets/6", true},
		{"GET", "branches/main/rename", true},
	}
	for _, c := range cases {
		var rest []string
		if c.rest != "" {
			rest = strings.Split(c.rest, "/")
		}
		msg := evaluateGitHubGuardREST(g, defMain, c.method, rest, []byte(`{}`))
		if (msg == "") != c.allow {
			t.Errorf("%s %s: allow=%v msg=%q", c.method, c.rest, c.allow, msg)
		}
		if !c.allow && !strings.Contains(msg, "endpoint not allowed under branch rules") && !strings.HasPrefix(c.rest, "git/") && !strings.HasPrefix(c.rest, "pulls/1/") {
			t.Errorf("%s %s: unexpected message %q", c.method, c.rest, msg)
		}
	}
}

func TestGuardSmartHTTPSplitBeforeReposPrefix(t *testing.T) {
	repo, _, rp, ok, _ := githubRepoAndRest("POST", "/repos/x.git/git-receive-pack")
	if repo != "repos/x" || !rp || !ok {
		t.Fatalf("repos/x receive-pack: %q rp=%v ok=%v", repo, rp, ok)
	}
	cfg := `{"version":"2","provider":"github","repositories":{"repos/x":{"capabilities":{"clone":true,"push":true},"branch_policy":{"rules":[{"branches":"agent/**","role":"agent"}]}}}}`
	g, err := newGitHubGuard(cfg, "POST", "/repos/x.git/git-receive-pack", "")
	if err != nil || g == nil || !g.receivePack || g.repo != "repos/x" {
		t.Fatalf("guard %+v err %v", g, err)
	}
	// Upload-pack and info/refs are reads and need no guard, other verbs are refused.
	for _, c := range []struct{ m, p string }{{"POST", "/repos/x.git/git-upload-pack"}, {"GET", "/repos/x.git/info/refs"}} {
		if g, err := newGitHubGuard(cfg, c.m, c.p, ""); g != nil || err != nil {
			t.Errorf("%s %s guarded: %+v %v", c.m, c.p, g, err)
		}
	}
	if g, _ := newGitHubGuard(cfg, "PUT", "/repos/x.git/git-upload-pack", ""); g == nil || g.deny == "" && g.restOK {
		t.Errorf("PUT upload-pack not refused: %+v", g)
	}
}

func TestGuardConfigWideDenies(t *testing.T) {
	cfg := `{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{"clone":true,"push":true},"branch_policy":{"rules":[{"branches":"agent/**","role":"agent"}]}}}}`
	for _, c := range []struct{ m, p, q string }{
		{"POST", "/graphql", ""}, {"POST", "/repositories/123/git/refs", ""}, {"GET", "/repositories/123", ""},
		{"PATCH", "/repositories/5/pulls/1", ""},
	} {
		g, err := newGitHubGuard(cfg, c.m, c.p, c.q)
		if err != nil || g == nil || g.deny == "" {
			t.Errorf("%s %s not denied: %+v %v", c.m, c.p, g, err)
		}
	}
	if g, _ := newGitHubGuard(cfg, "GET", "/graphql", ""); g != nil {
		t.Error("GET /graphql denied")
	}
	// Without any policy nothing changes.
	legacy := `{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{"clone":true}}}}`
	if g, _ := newGitHubGuard(legacy, "POST", "/graphql", ""); g != nil {
		t.Error("legacy config guarded")
	}
	// _method is refused for a repository with a policy, on any verb.
	for _, m := range []string{"POST", "GET"} {
		g, _ := newGitHubGuard(cfg, m, "/repos/acme/api/pulls/1/merge", "_method=PUT")
		if g == nil || g.deny == "" {
			t.Errorf("%s with _method not denied: %+v", m, g)
		}
	}
	if g, _ := newGitHubGuard(cfg, "POST", "/repos/other/repo/pulls/1/merge", "_method=PUT"); g != nil {
		t.Error("_method on an unrelated repository denied")
	}
}

func TestGuardPrecheckDefiniteVersusDeferred(t *testing.T) {
	pol := guardPolicy()
	mk := func(method, rest, body string) *githubGuard {
		return &githubGuard{policy: pol, method: method, rest: strings.Split(rest, "/"), restOK: true, body: []byte(body)}
	}
	if definite, msg, _ := mk("POST", "git/refs", `{"ref":"refs/heads/other"}`).precheck(); !definite || msg == "" {
		t.Fatalf("read branch must be a definite refusal: %v %q", definite, msg)
	}
	// Needing @default (writing to the default branch via contents without a
	// branch) is undecided until the lookup is possible.
	if definite, _, _ := mk("PUT", "contents/f", `{}`).precheck(); definite {
		t.Fatal("default-dependent decision reported as definite")
	}
	if definite, _, _ := mk("POST", "git/refs", `{"ref":"refs/heads/agent/x"}`).precheck(); definite {
		t.Fatal("allowed request reported as refused")
	}
	rp := &receivePackRequest{Commands: []receivePackCommand{
		{Old: strings.Repeat("0", 40), New: strings.Repeat("1", 40), Ref: "refs/heads/agent/x"},
		{Old: strings.Repeat("0", 40), New: strings.Repeat("1", 40), Ref: "refs/notes/x"},
	}}
	definite, _, reasons := (&githubGuard{policy: pol, receivePack: true, rp: rp}).precheck()
	if !definite || reasons[0] != "" || reasons[1] == "" {
		t.Fatalf("receive-pack precheck: %v %v", definite, reasons)
	}
}

func parsePktLines(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	for len(b) > 0 {
		if len(b) < 4 {
			t.Fatalf("short pkt header %q", b)
		}
		var n int
		if _, err := fmt.Sscanf(string(b[:4]), "%x", &n); err != nil {
			t.Fatalf("bad pkt length %q", b[:4])
		}
		if n == 0 {
			out = append(out, "0000")
			b = b[4:]
			continue
		}
		if n < 4 || n > 65520 || n > len(b) {
			t.Fatalf("pkt length %d invalid (have %d)", n, len(b))
		}
		out = append(out, string(b[4:n]))
		b = b[n:]
	}
	return out
}

func TestReceivePackNgLineAlwaysFitsOnePkt(t *testing.T) {
	req := &receivePackRequest{
		Caps: map[string]bool{"report-status": true},
		Commands: []receivePackCommand{
			{Ref: "refs/heads/" + strings.Repeat("a", 64<<10-100)},
			{Ref: "refs/heads/ok"},
		},
	}
	body := receivePackRejectionBody(req, []string{strings.Repeat("é", 70000), "x"})
	lines := parsePktLines(t, body)
	if len(lines) != 4 || lines[0] != "unpack ok\n" || lines[3] != "0000" {
		t.Fatalf("report shape: %d lines", len(lines))
	}
	if !strings.HasPrefix(lines[1], "ng refs/heads/aaaa") || !strings.HasSuffix(lines[1], "\n") || !strings.Contains(lines[1], " duckway: ") {
		t.Fatalf("ng line malformed: %.80q", lines[1])
	}
	if !strings.Contains(lines[1], "...") {
		t.Error("long ref not marked as truncated")
	}
	req.Caps["side-band-64k"] = true
	if pk := parsePktLines(t, receivePackRejectionBody(req, []string{strings.Repeat("z", 1<<20), ""})); len(pk) < 2 {
		t.Fatalf("side-band report malformed: %d", len(pk))
	}
	line := receivePackNgLine("refs/heads/x", strings.Repeat("é", 70000))
	if len(line) > receivePackMaxPktPayload || strings.ContainsRune(line, '�') {
		t.Errorf("reason not cut on a rune boundary within the limit: %d", len(line))
	}
}

type blockingBody struct{ release chan struct{} }

func (b *blockingBody) Read([]byte) (int, error) { <-b.release; return 0, io.EOF }

func TestReceivePackRefusalDrainIsBoundedByWallClock(t *testing.T) {
	old := receivePackDrainTimeout
	receivePackDrainTimeout = 150 * time.Millisecond
	t.Cleanup(func() { receivePackDrainTimeout = old })
	body := &blockingBody{release: make(chan struct{})}
	t.Cleanup(func() { close(body.release) })
	req := &receivePackRequest{Caps: map[string]bool{"report-status": true}, Commands: []receivePackCommand{{Ref: "refs/heads/main"}}}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		writeReceivePackRefusal(w, body, req, []string{"no"}, "no")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refusal blocked on a stalled body")
	}
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("ng refs/heads/main duckway: no")) {
		t.Fatalf("response %d %q", w.Code, w.Body.String())
	}
}

func TestDefaultBranchCacheDeletesExpiredEntries(t *testing.T) {
	c := newGitHubDefaultBranchCache()
	now := time.Unix(5000, 0)
	var mu sync.Mutex
	c.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	ok := func(b string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return b, nil }
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, err := c.get(context.Background(), k, ok(k)); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.entries) != 3 {
		t.Fatalf("entries %d", len(c.entries))
	}
	mu.Lock()
	now = now.Add(githubDefaultBranchTTL + time.Second)
	mu.Unlock()
	// Access to an expired key drops it before refetching.
	if _, err := c.get(context.Background(), "a", ok("a2")); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("expired entries kept: %d", n)
	}
	mu.Lock()
	now = now.Add(githubDefaultBranchTTL + time.Second)
	mu.Unlock()
	if _, err := c.get(context.Background(), "z", ok("z")); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	n = len(c.entries)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("sweep on insertion left %d entries", n)
	}
}

func TestDefaultBranchCacheWaiterCancelledWhileLeaderInFlight(t *testing.T) {
	c := newGitHubDefaultBranchCache()
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	var calls int32
	fetch := func(context.Context) (string, error) {
		atomic.AddInt32(&calls, 1)
		once.Do(func() { close(started) })
		<-release
		return "trunk", nil
	}
	leaderDone := make(chan string, 1)
	go func() {
		b, _ := c.get(context.Background(), "k", fetch)
		leaderDone <- b
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, "k", fetch)
		waiterErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-waiterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return on cancel")
	}
	close(release)
	if b := <-leaderDone; b != "trunk" {
		t.Fatalf("leader %q", b)
	}
	if b, err := c.get(context.Background(), "k", fetch); err != nil || b != "trunk" || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("cache not populated after waiter cancelled: %q %v calls=%d", b, err, calls)
	}
}

func TestDefaultBranchCacheSharedFailureThenRefetch(t *testing.T) {
	c := newGitHubDefaultBranchCache()
	release := make(chan struct{})
	var calls int32
	var fail atomic.Bool
	fail.Store(true)
	fetch := func(context.Context) (string, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		if fail.Load() {
			return "", errors.New("upstream 500")
		}
		return "main", nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.get(context.Background(), "k", fetch)
			errs <- err
		}()
	}
	time.Sleep(80 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("a caller saw success from a failed shared lookup")
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("shared failing lookup fetched %d times", n)
	}
	fail.Store(false)
	if b, err := c.get(context.Background(), "k", fetch); err != nil || b != "main" || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("refetch after failure: %q %v calls=%d", b, err, calls)
	}
}

func TestGitHubMintPermissionsForMerge(t *testing.T) {
	with := func(merge bool) string {
		return fmt.Sprintf(`{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{"clone":true,"pull_requests_read":true,"pull_requests_write":true,"pull_requests_merge":%v},"branch_policy":{"rules":[{"branches":"**","role":"read"}]}}}}`, merge)
	}
	legacy := `{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{"clone":true,"pull_requests_read":true,"pull_requests_write":true}}}}`
	want := func(got map[string]string, kv ...string) {
		t.Helper()
		if len(got) != len(kv)/2 {
			t.Fatalf("permissions %v, want %v", got, kv)
		}
		for i := 0; i < len(kv); i += 2 {
			if got[kv[i]] != kv[i+1] {
				t.Fatalf("permissions %v, want %v", got, kv)
			}
		}
	}
	for _, p := range []string{"/repos/acme/api/pulls/3/merge", "/repos/Acme/Api/pulls/3/update-branch"} {
		want(githubMintPermissions("PUT", p, "", with(true)), "pull_requests", "write", "contents", "write")
		want(githubMintPermissions("PUT", p, "", with(false)), "pull_requests", "write")
		want(githubMintPermissions("PUT", p, "", legacy), "pull_requests", "write")
	}
	// Nothing else widens.
	want(githubMintPermissions("POST", "/repos/acme/api/pulls/3/reviews", "", with(true)), "pull_requests", "write")
	want(githubMintPermissions("GET", "/repos/acme/api/pulls/3", "", with(true)), "pull_requests", "read")
	want(githubMintPermissions("PUT", "/repos/other/repo/pulls/3/merge", "", with(true)), "pull_requests", "write")
}
