package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingPoster collects calls to PostMessage so tests can assert
// what the runner posted to Discord.
type recordingPoster struct {
	mu    sync.Mutex
	posts []string
}

func (r *recordingPoster) post(ctx context.Context, handle, content string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, content)
	return nil
}

func (r *recordingPoster) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.posts))
	copy(out, r.posts)
	return out
}

// capturingRunFn returns a ccRunFn that records the prompt it receives and
// returns fixed sessionID/result values. Use capturedPrompt() to read the
// last prompt after waiting for a post.
func capturingRunFn(sessionID, result string) (fn ccRunFn, capturedPrompt func() string) {
	var mu sync.Mutex
	var last string
	fn = func(_ context.Context, _, _, prompt, _ string, _ []string) (string, string, bool, error) {
		mu.Lock()
		last = prompt
		mu.Unlock()
		return sessionID, result, false, nil
	}
	capturedPrompt = func() string {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	return
}

// newTestRunner creates a ccRunner with an injectable runFn and a recording poster.
func newTestRunner(t *testing.T, fn ccRunFn) (*ccRunner, *recordingPoster, *CCSessionStore) {
	t.Helper()
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", UseTmux: true}
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), spec, store, pp.post, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	r.runFn = fn
	r.statusPosts = false
	return r, pp, store
}

func waitForPosts(t *testing.T, pp *recordingPoster, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(pp.all()) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("only %d posts after 3s (wanted %d)", len(pp.all()), want)
}

func TestCCRunner_PostsResult(t *testing.T) {
	fn, _ := capturingRunFn("sess-abc", "hello from fake claude")
	r, pp, store := newTestRunner(t, fn)
	defer r.Stop()

	if !r.Enqueue(ccTask{Content: "do a thing", ChannelKind: "task"}) {
		t.Fatal("Enqueue returned false")
	}
	waitForPosts(t, pp, 1)

	if posts := pp.all(); !strings.Contains(posts[0], "hello from fake claude") {
		t.Errorf("post content = %q", posts[0])
	}
	if got := store.Get("dwch_t"); got != "sess-abc" {
		t.Errorf("session_id not persisted: got %q", got)
	}
}

func TestCCRunner_PostsProgressForLongRunningTask(t *testing.T) {
	oldFirst, oldInterval := ccLongRunFirstNotice, ccLongRunInterval
	ccLongRunFirstNotice = 20 * time.Millisecond
	ccLongRunInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		ccLongRunFirstNotice = oldFirst
		ccLongRunInterval = oldInterval
	})

	fn := ccRunFn(func(_ context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		time.Sleep(80 * time.Millisecond)
		return "sess-abc", "done", false, nil
	})
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", RunFn: fn, UseTmux: false}
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), spec, store, pp.post, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	if !r.Enqueue(ccTask{Content: "slow work", ChannelKind: "task"}) {
		t.Fatal("Enqueue returned false")
	}
	waitForPosts(t, pp, 3)

	posts := strings.Join(pp.all(), "\n")
	if !strings.Contains(posts, "started") {
		t.Fatalf("missing started status:\n%s", posts)
	}
	if !strings.Contains(posts, "still running") {
		t.Fatalf("missing still-running status:\n%s", posts)
	}
	if !strings.Contains(posts, "done") {
		t.Fatalf("missing final result:\n%s", posts)
	}
}

func TestCCRunner_LoadsKeysEnvForAgent(t *testing.T) {
	var gotEnv []string
	fn := func(_ context.Context, _, _, _, _ string, extraEnv []string) (string, string, bool, error) {
		gotEnv = append([]string(nil), extraEnv...)
		return "sess-abc", "ok", false, nil
	}

	configDir := t.TempDir()
	if err := os.WriteFile(KeysEnvPath(configDir), []byte(`
# generated
OPENAI_API_KEY=sk-dw-placeholder
ANTHROPIC_API_KEY=sk-ant-placeholder
bad-name=ignored
`), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "codex", DisplayName: "codex", Bin: "/fake/codex", RunFn: fn, UseTmux: false}
	r, err := newCCRunner("dwch_t", configDir, t.TempDir(), spec, store, pp.post, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	r.statusPosts = false
	defer r.Stop()

	r.Enqueue(ccTask{Content: "hello", ChannelKind: "task"})
	waitForPosts(t, pp, 1)

	envText := strings.Join(gotEnv, "\n")
	for _, want := range []string{
		"DUCKWAY_CC_CHANNEL_HANDLE=dwch_t",
		"OPENAI_API_KEY=sk-dw-placeholder",
		"ANTHROPIC_API_KEY=sk-ant-placeholder",
	} {
		if !strings.Contains(envText, want) {
			t.Fatalf("extraEnv missing %q:\n%s", want, envText)
		}
	}
	if strings.Contains(envText, "bad-name=ignored") {
		t.Fatalf("invalid env name leaked into extraEnv:\n%s", envText)
	}
}

func TestCCRunner_SuppressesOpenAIEnvForCodexOAuth(t *testing.T) {
	var gotEnv []string
	fn := func(_ context.Context, _, _, _, _ string, extraEnv []string) (string, string, bool, error) {
		gotEnv = append([]string(nil), extraEnv...)
		return "sess-abc", "ok", false, nil
	}

	configDir := t.TempDir()
	if err := os.WriteFile(KeysEnvPath(configDir), []byte(`
OPENAI_API_KEY=sk-dw-placeholder
ANTHROPIC_API_KEY=sk-ant-placeholder
`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CodexOAuthModePath(configDir), []byte("oauth\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "codex", DisplayName: "codex", Bin: "/fake/codex", RunFn: fn, UseTmux: false}
	r, err := newCCRunner("dwch_t", configDir, t.TempDir(), spec, store, pp.post, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	r.statusPosts = false
	defer r.Stop()

	r.Enqueue(ccTask{Content: "hello", ChannelKind: "task"})
	waitForPosts(t, pp, 1)

	envText := strings.Join(gotEnv, "\n")
	if strings.Contains(envText, "OPENAI_API_KEY=") {
		t.Fatalf("OPENAI_API_KEY leaked into Codex OAuth env:\n%s", envText)
	}
	if !strings.Contains(envText, "ANTHROPIC_API_KEY=sk-ant-placeholder") {
		t.Fatalf("non-OpenAI env should remain:\n%s", envText)
	}
}

func TestCCRunner_ClearDropsCachedSessionID(t *testing.T) {
	// First turn: a normal message returns a session_id which gets
	// cached in CCSessionStore.
	fn, _ := capturingRunFn("sid-old", "hi")
	r, pp, store := newTestRunner(t, fn)
	defer r.Stop()
	r.Enqueue(ccTask{Content: "hello", ChannelKind: "task"})
	waitForPosts(t, pp, 1)
	if got := store.Get("dwch_t"); got != "sid-old" {
		t.Fatalf("setup: cached sid = %q, want sid-old", got)
	}

	// Now send /clear via the !/ escape. The slash flow returns no
	// session_id, but the runner must still drop the cached mapping
	// so a daemon restart doesn't `--resume` into the old state.
	r.runFn = func(_ context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		return "", "cleared", false, nil
	}
	r.Enqueue(ccTask{Content: "!/clear", ChannelKind: "task"})
	waitForPosts(t, pp, 2)
	if got := store.Get("dwch_t"); got != "" {
		t.Errorf("after /clear: cached sid = %q, want empty", got)
	}
}

func TestCCRunner_StripsClaudeEscapes(t *testing.T) {
	// Users escape claude trigger chars with a leading "!" because
	// Discord eats "/" prefixes and the daemon eats "!" prefixes.
	// "!/X" → "/X", "!!X" → "!X". The runner strips ONE leading "!"
	// before handing the prompt to claude.
	cases := []struct {
		in, want string
	}{
		// Slash-command escape
		{"!/usage", "/usage"},
		{"!/compact", "/compact"},
		{"  !/help foo bar  ", "/help foo bar"},
		// Bash-shell escape
		{"!! ls", "! ls"},
		{"!!cargo test", "!cargo test"},
		{"  !! cat README  ", "! cat README"},
		// Not an escape — must pass through unchanged.
		{"hello world", "hello world"},
		{"!reset", "!reset"}, // server cmd shouldn't reach here, defensive
	}
	for _, tt := range cases {
		fn, captured := capturingRunFn("sid", "ok")
		r, pp, _ := newTestRunner(t, fn)
		r.Enqueue(ccTask{Content: tt.in, ChannelKind: "task"})
		waitForPosts(t, pp, 1)
		if got := captured(); got != tt.want {
			t.Errorf("Content=%q → prompt to claude = %q, want %q", tt.in, got, tt.want)
		}
		r.Stop()
	}
}

func TestCCRunner_QueueOverflow(t *testing.T) {
	fn := ccRunFn(func(_ context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		time.Sleep(200 * time.Millisecond)
		return "s", "ok", false, nil
	})
	r, _, _ := newTestRunner(t, fn)
	defer r.Stop()

	// First task is picked up immediately (off the queue); buffer holds ccQueueDepth more.
	accepted := 0
	for i := 0; i < 50; i++ {
		if r.Enqueue(ccTask{Content: "x"}) {
			accepted++
		}
	}
	if accepted < ccQueueDepth || accepted > ccQueueDepth+1 {
		t.Errorf("expected ~%d accepted, got %d", ccQueueDepth, accepted)
	}
}

func TestCCRunner_ManagementPreamble_FirstTurnOnly(t *testing.T) {
	fn, captured := capturingRunFn("sess-mgmt-1", "response")
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", UseTmux: true}
	r, err := newCCRunner("dwch_mgmt", t.TempDir(), t.TempDir(), spec, store, pp.post, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	r.runFn = fn
	r.statusPosts = false
	defer r.Stop()

	// First management message: preamble should be injected into the prompt.
	r.Enqueue(ccTask{Content: "do something", ChannelKind: "management"})
	waitForPosts(t, pp, 1)
	first := captured()
	if !strings.Contains(first, "Duckway Control Channel") {
		t.Errorf("first management turn missing preamble in prompt: %q", first)
	}
	if !strings.Contains(first, "discord_create_task_channel") {
		t.Errorf("first management turn missing task-channel nudge: %q", first)
	}

	// Second message in same session: no preamble (claude keeps context via --resume).
	r.Enqueue(ccTask{Content: "another thing", ChannelKind: "management"})
	waitForPosts(t, pp, 2)
	second := captured()
	if strings.Contains(second, "Duckway Control Channel — system note") {
		t.Errorf("preamble re-injected on follow-up turn: %q", second)
	}
}

func TestCCRunner_NoPreambleForTaskChannel(t *testing.T) {
	fn, captured := capturingRunFn("sess-task-1", "response")
	r, pp, _ := newTestRunner(t, fn)
	defer r.Stop()

	r.Enqueue(ccTask{Content: "task work", ChannelKind: "task"})
	waitForPosts(t, pp, 1)
	if strings.Contains(captured(), "Duckway Control Channel — system note") {
		t.Errorf("preamble should not appear in task channel prompt")
	}
}

func TestCCRunner_DefaultsCwd(t *testing.T) {
	fn, _ := capturingRunFn("s", "ok")
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", UseTmux: true}
	r, err := newCCRunner("dwch_x", t.TempDir(), "", spec, store, pp.post, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	r.runFn = fn
	r.statusPosts = false
	defer r.Stop()

	want := filepath.Join(tmpHome, ".duckway", "cc-workspace", "dwch_x")
	if r.cwd != want {
		t.Errorf("cwd = %q, want %q", r.cwd, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected cwd dir created: %v", err)
	}
}

func TestCCRunner_EmptyResultFallback(t *testing.T) {
	fn := ccRunFn(func(_ context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		return "s", "", false, nil // empty result
	})
	r, pp, _ := newTestRunner(t, fn)
	defer r.Stop()

	r.Enqueue(ccTask{Content: "x"})
	waitForPosts(t, pp, 1)
	if !strings.Contains(pp.all()[0], "no response") {
		t.Errorf("expected no-response fallback, got %q", pp.all()[0])
	}
}

func TestCCRunner_RunFnError(t *testing.T) {
	fn := ccRunFn(func(_ context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		return "", "", false, fmt.Errorf("PTY failed: boom")
	})
	r, pp, _ := newTestRunner(t, fn)
	defer r.Stop()

	r.Enqueue(ccTask{Content: "x"})
	waitForPosts(t, pp, 1)
	if !strings.Contains(pp.all()[0], "boom") {
		t.Errorf("expected error message, got %q", pp.all()[0])
	}
}

func TestCCRunner_IsErrorFlagged(t *testing.T) {
	fn := ccRunFn(func(_ context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		return "s", "something went wrong internally", true, nil
	})
	r, pp, _ := newTestRunner(t, fn)
	defer r.Stop()

	r.Enqueue(ccTask{Content: "x"})
	waitForPosts(t, pp, 1)
	if !strings.Contains(pp.all()[0], "⚠️") {
		t.Errorf("expected error prefix, got %q", pp.all()[0])
	}
}
