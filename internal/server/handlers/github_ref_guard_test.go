package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/server/services"
)

func guardPolicy() services.GitHubRepoBranchPolicy {
	return services.GitHubRepoBranchPolicy{BranchPolicy: services.GitHubBranchPolicy{
		Rules: []services.GitHubBranchRule{
			{Branches: "@default", Role: "protected"},
			{Branches: "agent/**", Role: "agent"},
			{Branches: "ci/**", Role: "agent", RunWorkflows: true},
			{Branches: "rel/**", Role: "protected", RunWorkflows: true},
			{Branches: "**", Role: "read"},
		},
		Tags: services.GitHubTagPolicy{Create: []string{"v*"}},
	}}
}

func defMain() (string, error) { return "main", nil }
func defFail() (string, error) { return "", errors.New("boom") }

func TestGuardRESTTable(t *testing.T) {
	merge := guardPolicy()
	merge.PullRequestsMerge, merge.PullRequestsApprove = true, true
	type tc struct {
		name   string
		pol    services.GitHubRepoBranchPolicy
		method string
		rest   string
		body   string
		allow  bool
	}
	g, m := guardPolicy(), merge
	cases := []tc{
		{"ref create agent branch", g, "POST", "git/refs", `{"ref":"refs/heads/agent/x","sha":"1"}`, true},
		{"ref create protected branch", g, "POST", "git/refs", `{"ref":"refs/heads/main","sha":"1"}`, false},
		{"ref create read branch", g, "POST", "git/refs", `{"ref":"refs/heads/other","sha":"1"}`, false},
		{"ref create tag ok", g, "POST", "git/refs", `{"ref":"refs/tags/v1","sha":"1"}`, true},
		{"ref create tag no match", g, "POST", "git/refs", `{"ref":"refs/tags/x1","sha":"1"}`, false},
		{"ref create other ns", g, "POST", "git/refs", `{"ref":"refs/notes/x","sha":"1"}`, false},
		{"ref create bare name", g, "POST", "git/refs", `{"ref":"agent/x"}`, false},
		{"ref create missing", g, "POST", "git/refs", `{}`, false},
		{"ref create malformed", g, "POST", "git/refs", `{`, false},
		{"ref create non-string", g, "POST", "git/refs", `{"ref":5}`, false},
		{"ref patch agent", g, "PATCH", "git/refs/heads/agent/x", `{"sha":"1","force":true}`, true},
		{"ref patch protected", g, "PATCH", "git/refs/heads/main", `{}`, false},
		{"ref delete agent", g, "DELETE", "git/refs/heads/agent/x", ``, true},
		{"ref delete default", g, "DELETE", "git/refs/heads/main", ``, false},
		{"ref delete tag", g, "DELETE", "git/refs/tags/v1", ``, false},
		{"ref patch tag", g, "PATCH", "git/refs/tags/v1", `{}`, false},
		{"ref delete weird ns", g, "DELETE", "git/refs/notes/x", ``, false},
		{"ref put refused", g, "PUT", "git/refs/heads/agent/x", ``, false},
		{"git blobs unchanged", g, "POST", "git/blobs", `{garbage`, true},
		{"git trees unchanged", g, "POST", "git/trees", ``, true},
		{"git commits unchanged", g, "POST", "git/commits", ``, true},
		{"git tags object unchanged", g, "POST", "git/tags", ``, true},
		{"contents agent", g, "PUT", "contents/a/b.txt", `{"branch":"agent/x","message":"m"}`, true},
		{"contents read", g, "PUT", "contents/a.txt", `{"branch":"zzz"}`, false},
		{"contents absent default protected", g, "PUT", "contents/a.txt", `{"message":"m"}`, false},
		{"contents null default", g, "PUT", "contents/a.txt", `{"branch":null}`, false},
		{"contents delete agent", g, "DELETE", "contents/a.txt", `{"branch":"ci/z"}`, true},
		{"contents refs prefix agent", g, "PUT", "contents/a.txt", `{"branch":"refs/heads/agent/x"}`, true},
		{"contents refs prefix protected", g, "PUT", "contents/a.txt", `{"branch":"refs/heads/main"}`, false},
		{"contents refs tags", g, "PUT", "contents/a.txt", `{"branch":"refs/tags/v1"}`, false},
		{"contents malformed", g, "PUT", "contents/a.txt", `nope`, false},
		{"merges agent", g, "POST", "merges", `{"base":"agent/x","head":"h"}`, true},
		{"merges protected", g, "POST", "merges", `{"base":"main","head":"h"}`, false},
		{"merges no base", g, "POST", "merges", `{"head":"h"}`, false},
		{"release create ok", g, "POST", "releases", `{"tag_name":"v1.0"}`, true},
		{"release create refused", g, "POST", "releases", `{"tag_name":"zz"}`, false},
		{"release create no tag", g, "POST", "releases", `{}`, false},
		{"release patch no tag", g, "PATCH", "releases/12", `{"body":"x"}`, true},
		{"release patch tag refused", g, "PATCH", "releases/12", `{"tag_name":"zz"}`, false},
		{"release patch tag ok", g, "PATCH", "releases/12", `{"tag_name":"v2"}`, true},
		{"release delete unchanged", g, "DELETE", "releases/12", ``, true},
		{"pull create protected base", g, "POST", "pulls", `{"base":"main","head":"agent/x"}`, true},
		{"pull create agent base", g, "POST", "pulls", `{"base":"agent/y","head":"agent/x"}`, true},
		{"pull create read base", g, "POST", "pulls", `{"base":"zzz","head":"agent/x"}`, false},
		{"pull create no base", g, "POST", "pulls", `{"head":"x"}`, false},
		{"pull create malformed", g, "POST", "pulls", `[]`, false},
		{"pull patch no base", g, "PATCH", "pulls/3", `{"title":"t"}`, true},
		{"pull patch read base", g, "PATCH", "pulls/3", `{"base":"zzz"}`, false},
		{"pull patch ok base", g, "PATCH", "pulls/3", `{"base":"main"}`, true},
		{"pull merge refused", g, "PUT", "pulls/3/merge", `{}`, false},
		{"pull update-branch refused", g, "PUT", "pulls/3/update-branch", ``, false},
		{"pull merge allowed", m, "PUT", "pulls/3/merge", `{}`, true},
		{"pull update-branch allowed", m, "PUT", "pulls/3/update-branch", ``, true},
		{"approve refused", g, "POST", "pulls/3/reviews", `{"event":"APPROVE"}`, false},
		{"approve lowercase refused", g, "POST", "pulls/3/reviews", `{"event":"approve"}`, false},
		{"approve allowed", m, "POST", "pulls/3/reviews", `{"event":"Approve"}`, true},
		{"comment review ok", g, "POST", "pulls/3/reviews", `{"event":"COMMENT"}`, true},
		{"request changes ok", g, "POST", "pulls/3/reviews", `{"event":"REQUEST_CHANGES"}`, true},
		{"pending review ok", g, "POST", "pulls/3/reviews", `{"body":"x"}`, true},
		{"empty review body ok", g, "POST", "pulls/3/reviews", ``, true},
		{"review malformed", g, "POST", "pulls/3/reviews", `{`, false},
		{"review event approve refused", g, "POST", "pulls/3/reviews/9/events", `{"event":"APPROVE"}`, false},
		{"review event approve allowed", m, "POST", "pulls/3/reviews/9/events", `{"event":"APPROVE"}`, true},
		{"review event comment ok", g, "POST", "pulls/3/reviews/9/events", `{"event":"COMMENT"}`, true},
		{"review event missing", g, "POST", "pulls/3/reviews/9/events", `{}`, false},
		{"dispatch run rule", g, "POST", "actions/workflows/ci.yml/dispatches", `{"ref":"ci/x"}`, true},
		{"dispatch refs/heads", g, "POST", "actions/workflows/ci.yml/dispatches", `{"ref":"refs/heads/rel/1"}`, true},
		{"dispatch agent without run", g, "POST", "actions/workflows/ci.yml/dispatches", `{"ref":"agent/x"}`, false},
		{"dispatch tag", g, "POST", "actions/workflows/ci.yml/dispatches", `{"ref":"refs/tags/v1"}`, false},
		{"dispatch no ref", g, "POST", "actions/workflows/ci.yml/dispatches", `{}`, false},
		{"dispatch malformed", g, "POST", "actions/workflows/ci.yml/dispatches", `x`, false},
		{"rerun unchanged", g, "POST", "actions/runs/5/rerun", ``, true},
		{"cancel unchanged", g, "POST", "actions/runs/5/cancel", ``, true},
		{"issues unchanged", g, "POST", "issues", `{`, true},
	}
	for _, c := range cases {
		msg := evaluateGitHubGuardREST(c.pol, defMain, c.method, strings.Split(c.rest, "/"), []byte(c.body))
		if (msg == "") != c.allow {
			t.Errorf("%s: allow=%v but refusal=%q", c.name, c.allow, msg)
		}
	}
}

func TestGuardDefaultBranchLookupFailsClosed(t *testing.T) {
	p := guardPolicy()
	for name, c := range map[string]struct {
		method, rest, body string
	}{
		"contents default":  {"PUT", "contents/a", `{"message":"m"}`},
		"pull create":       {"POST", "pulls", `{"base":"agent/x"}`}, // policy has @default, so every selection needs it
		"ref create branch": {"POST", "git/refs", `{"ref":"refs/heads/agent/x"}`},
		"dispatch":          {"POST", "actions/workflows/ci.yml/dispatches", `{"ref":"ci/x"}`},
	} {
		msg := evaluateGitHubGuardREST(p, defFail, c.method, strings.Split(c.rest, "/"), []byte(c.body))
		if msg == "" || !strings.Contains(msg, "default branch") {
			t.Errorf("%s: want default-branch refusal, got %q", name, msg)
		}
	}
	// Without an @default rule only contents-without-branch needs the lookup.
	p.BranchPolicy.Rules = []services.GitHubBranchRule{{Branches: "agent/**", Role: "agent"}}
	if msg := evaluateGitHubGuardREST(p, defFail, "POST", []string{"git", "refs"}, []byte(`{"ref":"refs/heads/agent/x"}`)); msg != "" {
		t.Errorf("no @default rule: unexpected refusal %q", msg)
	}
	if msg := evaluateGitHubGuardREST(p, defFail, "PUT", []string{"contents", "a"}, []byte(`{}`)); msg == "" {
		t.Error("contents without branch must fail closed")
	}
	// Push commands too.
	e := &guardEval{policy: guardPolicy(), def: defFail}
	reasons := e.checkReceivePack([]receivePackCommand{{Old: strings.Repeat("0", 40), New: strings.Repeat("a", 40), Ref: "refs/heads/agent/x"}})
	if reasons[0] == "" {
		t.Error("push must fail closed when default branch is unknown")
	}
}

func TestGuardPushCommands(t *testing.T) {
	zero, a := strings.Repeat("0", 40), strings.Repeat("a", 40)
	e := &guardEval{policy: guardPolicy(), def: defMain}
	for _, c := range []struct {
		cmd   receivePackCommand
		allow bool
	}{
		{receivePackCommand{zero, a, "refs/heads/agent/new"}, true},
		{receivePackCommand{a, a, "refs/heads/agent/upd"}, true},
		{receivePackCommand{a, zero, "refs/heads/agent/del"}, true},
		{receivePackCommand{a, a, "refs/heads/main"}, false},
		{receivePackCommand{zero, a, "refs/heads/other"}, false},
		{receivePackCommand{zero, a, "refs/tags/v1"}, true},
		{receivePackCommand{a, a, "refs/tags/v1"}, false},
		{receivePackCommand{a, zero, "refs/tags/v1"}, false},
		{receivePackCommand{zero, a, "refs/tags/nope"}, false},
		{receivePackCommand{zero, a, "refs/notes/x"}, false},
		{receivePackCommand{zero, a, "refs/pull/1/head"}, false},
		{receivePackCommand{zero, a, "HEAD"}, false},
		{receivePackCommand{zero, a, "refs/heads/agent/../main"}, false},
		{receivePackCommand{zero, a, "refs/heads/"}, false},
	} {
		got := e.checkPushCommand(c.cmd)
		if (got == "") != c.allow {
			t.Errorf("%s %s->%s: allow=%v refusal=%q", c.cmd.Ref, c.cmd.Old[:3], c.cmd.New[:3], c.allow, got)
		}
	}
}

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

func TestParseReceivePackCommands(t *testing.T) {
	zero, a, b := strings.Repeat("0", 40), strings.Repeat("a", 40), strings.Repeat("b", 40)
	first := pkt(zero + " " + a + " refs/heads/x\x00report-status side-band-64k agent=git/2\n")
	second := pkt(a + " " + b + " refs/tags/t\n")
	pack := "PACK\x00\x00binary-tail"

	t.Run("caps after NUL and prefix is exact", func(t *testing.T) {
		in := first + second + "0000" + pack
		r := strings.NewReader(in)
		req, err := parseReceivePackCommands(r, 1<<20, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Commands) != 2 || req.Commands[0].Ref != "refs/heads/x" || req.Commands[1].Ref != "refs/tags/t" {
			t.Fatalf("%+v", req.Commands)
		}
		if !req.Caps["report-status"] || !req.Caps["side-band-64k"] {
			t.Fatalf("caps %v", req.Caps)
		}
		rest, _ := io.ReadAll(r)
		if string(rest) != pack {
			t.Fatalf("parser over-read; rest=%q", rest)
		}
		if string(req.Prefix) != first+second+"0000" {
			t.Fatalf("prefix %q", req.Prefix)
		}
	})
	t.Run("shallow lines skipped", func(t *testing.T) {
		in := pkt("shallow "+a+"\n") + pkt("shallow "+b+"\n") + first + "0000"
		req, err := parseReceivePackCommands(strings.NewReader(in), 1<<20, 1000)
		if err != nil || len(req.Commands) != 1 || !req.Caps["report-status"] {
			t.Fatalf("%+v %v", req, err)
		}
	})
	t.Run("zero commands allowed", func(t *testing.T) {
		req, err := parseReceivePackCommands(strings.NewReader("0000"), 1<<20, 1000)
		if err != nil || len(req.Commands) != 0 {
			t.Fatalf("%+v %v", req, err)
		}
	})
	t.Run("sha256 object ids", func(t *testing.T) {
		z, x := strings.Repeat("0", 64), strings.Repeat("c", 64)
		req, err := parseReceivePackCommands(strings.NewReader(pkt(z+" "+x+" refs/heads/y\n")+"0000"), 1<<20, 1000)
		if err != nil || !req.Commands[0].isCreate() {
			t.Fatalf("%+v %v", req, err)
		}
	})
	t.Run("many commands within limit", func(t *testing.T) {
		var sb strings.Builder
		for i := 0; i < 1000; i++ {
			sb.WriteString(pkt(fmt.Sprintf("%s %s refs/heads/agent/b%d\n", zero, a, i)))
		}
		sb.WriteString("0000")
		req, err := parseReceivePackCommands(strings.NewReader(sb.String()), 1<<20, 1000)
		if err != nil || len(req.Commands) != 1000 {
			t.Fatalf("%v %d", err, len(req.Commands))
		}
		sb.Reset()
		for i := 0; i < 1001; i++ {
			sb.WriteString(pkt(fmt.Sprintf("%s %s refs/heads/agent/b%d\n", zero, a, i)))
		}
		sb.WriteString("0000")
		if _, err := parseReceivePackCommands(strings.NewReader(sb.String()), 1<<20, 1000); err == nil {
			t.Fatal("1001 commands accepted")
		}
	})
	for name, in := range map[string]string{
		"empty":            "",
		"truncated header": "00",
		"truncated body":   "0032" + zero,
		"no flush":         first,
		"bad hex length":   "zzzz",
		"delim packet":     "0001",
		"short length":     "0003",
		"bad command":      pkt("hello world\n") + "0000",
		"bad oid":          pkt("xx "+a+" refs/heads/x\n") + "0000",
		"mixed oid len":    pkt(zero+" "+strings.Repeat("a", 64)+" refs/heads/x\n") + "0000",
		"empty ref":        pkt(zero+" "+a+" \n") + "0000",
		"push cert":        pkt("push-cert\x00report-status\n") + "0000",
		"oversize pkt":     "fff1" + strings.Repeat("a", 70000),
	} {
		if _, err := parseReceivePackCommands(strings.NewReader(in), 1<<20, 1000); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	t.Run("byte bound", func(t *testing.T) {
		in := first + second + "0000"
		if _, err := parseReceivePackCommands(strings.NewReader(in), 40, 1000); err == nil {
			t.Fatal("oversize section accepted")
		}
	})
}

func TestReceivePackRejectionBody(t *testing.T) {
	req := &receivePackRequest{
		Caps:     map[string]bool{"report-status": true, "side-band-64k": true},
		Commands: []receivePackCommand{{Ref: "refs/heads/main"}, {Ref: "refs/heads/agent/x"}},
	}
	body := string(receivePackRejectionBody(req, []string{"branch main is protected", ""}))
	if !strings.HasSuffix(body, "0000") || body[4] != 1 {
		t.Fatalf("not side-band wrapped: %q", body)
	}
	for _, want := range []string{"unpack ok", "ng refs/heads/main duckway: branch main is protected", "ng refs/heads/agent/x duckway: another ref in this push was refused"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %q", want, body)
		}
	}
	req.Caps = map[string]bool{"report-status": true}
	plain := string(receivePackRejectionBody(req, []string{"x", "y"}))
	if !strings.HasPrefix(plain, "000eunpack ok\n") || !strings.HasSuffix(plain, "0000") {
		t.Fatalf("plain body %q", plain)
	}
}

func TestGuardRESTPathClassification(t *testing.T) {
	repo, rest, rp, ok := githubRepoAndRest("PUT", "/repos/Acme/Api/contents/x/y")
	if repo != "acme/api" || rp || !ok || len(rest) != 3 {
		t.Fatalf("%q %v %v %v", repo, rest, rp, ok)
	}
	repo, _, rp, _ = githubRepoAndRest("POST", "/Acme/Api.git/git-receive-pack")
	if repo != "acme/api" || !rp {
		t.Fatalf("receive-pack %q %v", repo, rp)
	}
	if _, _, _, ok := githubRepoAndRest("PUT", "/repos/a/b/contents//x"); ok {
		t.Fatal("empty segment accepted")
	}
	if _, _, _, ok := githubRepoAndRest("PUT", "/repos/a/b/contents/../x"); ok {
		t.Fatal("dot-dot accepted")
	}
	if g, err := newGitHubGuard(`{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{"clone":true}}}}`, "POST", "/repos/acme/api/git/refs"); g != nil || err != nil {
		t.Fatalf("legacy repo must have no guard: %v %v", g, err)
	}
	if g, _ := newGitHubGuard(`{"version":"2","provider":"github","repositories":{"acme/api":{"capabilities":{"clone":true},"branch_policy":{"rules":[]}}}}`, "GET", "/repos/acme/api/git/refs"); g != nil {
		t.Fatal("GET must not be guarded")
	}
}

func TestDefaultBranchCache(t *testing.T) {
	c := newGitHubDefaultBranchCache()
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	c.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	var calls int32
	release := make(chan struct{})
	fetch := func(context.Context) (string, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return "trunk", nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := c.get(context.Background(), "k", fetch)
			if err != nil || b != "trunk" {
				t.Errorf("%q %v", b, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("concurrent callers made %d fetches", n)
	}
	if _, err := c.get(context.Background(), "k", fetch); err != nil || atomic.LoadInt32(&calls) != 1 {
		t.Fatal("cache miss within TTL")
	}
	mu.Lock()
	now = now.Add(githubDefaultBranchTTL + time.Second)
	mu.Unlock()
	if _, err := c.get(context.Background(), "k", fetch); err != nil || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expired entry not refetched, calls=%d", calls)
	}
	// Failures are not cached.
	var fails int32
	bad := func(context.Context) (string, error) { atomic.AddInt32(&fails, 1); return "", errors.New("x") }
	for i := 0; i < 2; i++ {
		if _, err := c.get(context.Background(), "bad", bad); err == nil {
			t.Fatal("error swallowed")
		}
	}
	if fails != 2 {
		t.Fatalf("failure cached, fetches=%d", fails)
	}
	// Different credential or repo -> separate entries.
	if b, _ := c.get(context.Background(), "other", func(context.Context) (string, error) { return "dev", nil }); b != "dev" {
		t.Fatal("keys collide")
	}
}
