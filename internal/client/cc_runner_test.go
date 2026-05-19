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
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), "/fake/claude", store, pp.post, true)
	if err != nil {
		t.Fatal(err)
	}
	r.runFn = fn
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

func TestCCRunner_StripsClaudeSlashEscape(t *testing.T) {
	// Discord eats "/" prefix messages; users type "!/usage" instead.
	// The runner must strip the leading "!" before handing the prompt
	// to claude so claude sees its real "/usage" command.
	cases := []struct {
		in, want string
	}{
		{"!/usage", "/usage"},
		{"!/compact", "/compact"},
		{"  !/help foo bar  ", "/help foo bar"},
		// Not the escape — must pass through unchanged.
		{"hello world", "hello world"},
		{"!reset", "!reset"}, // server commands shouldn't reach here in prod but be defensive
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
	r, err := newCCRunner("dwch_mgmt", t.TempDir(), t.TempDir(), "/fake/claude", store, pp.post, true)
	if err != nil {
		t.Fatal(err)
	}
	r.runFn = fn
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

	r, err := newCCRunner("dwch_x", t.TempDir(), "", "/fake/claude", store, pp.post, true)
	if err != nil {
		t.Fatal(err)
	}
	r.runFn = fn
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
