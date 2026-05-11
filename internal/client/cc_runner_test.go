package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClaude writes a small shell script to tmp that mimics
// `claude -p --output-format json` — prints a deterministic JSON line
// containing session_id + result. The test can override the result text
// or session id via env vars.
func fakeClaude(t *testing.T, sessionID, result string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := `#!/bin/sh
SID="` + sessionID + `"
RESULT='` + result + `'
# Capture last positional (the prompt) so tests can verify it
PROMPT="$@"
printf '{"type":"result","subtype":"success","session_id":"%s","result":"%s","is_error":false}\n' "$SID" "$RESULT"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// recordingPoster collects calls to PostMessage so the test can assert
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

func TestCCRunner_PostsResult(t *testing.T) {
	bin := fakeClaude(t, "sess-abc", "hello from fake claude")
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	cwd := t.TempDir()
	r, err := newCCRunner("dwch_t", t.TempDir(), cwd, bin, store, pp.post)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	ok := r.Enqueue(ccTask{Content: "do a thing", ChannelKind: "task"})
	if !ok {
		t.Fatal("Enqueue returned false")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if posts := pp.all(); len(posts) > 0 {
			if !strings.Contains(posts[0], "hello from fake claude") {
				t.Errorf("post content = %q", posts[0])
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(pp.all()) == 0 {
		t.Fatal("runner never posted result")
	}
	if got := store.Get("dwch_t"); got != "sess-abc" {
		t.Errorf("session_id not persisted: %q", got)
	}
}

func TestCCRunner_QueueOverflow(t *testing.T) {
	// Use a fake claude that sleeps so the queue stays full.
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(`#!/bin/sh
sleep 1
printf '{"type":"result","subtype":"success","session_id":"s","result":"ok","is_error":false}\n'
`), 0700); err != nil {
		t.Fatal(err)
	}
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), bin, store, pp.post)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	// Buffer = ccQueueDepth (10). First task starts running immediately
	// (consumed off the queue). So we can enqueue another 10 before it
	// rejects — buffer + the in-flight slot.
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

// fakeClaudeEchoArgs writes a shell script that just echoes its full
// argv as the result, so tests can assert exactly what we passed.
func fakeClaudeEchoArgs(t *testing.T, sessionID string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := `#!/bin/sh
SID="` + sessionID + `"
# JSON-escape everything we got — naive but enough for ASCII-clean tests.
PAYLOAD=$(printf "%s" "$*" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr "\n" " ")
printf '{"type":"result","subtype":"success","session_id":"%s","result":"argv=%s","is_error":false}\n' "$SID" "$PAYLOAD"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestCCRunner_ManagementPreamble_FirstTurnOnly(t *testing.T) {
	bin := fakeClaudeEchoArgs(t, "sess-mgmt-1")
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	r, err := newCCRunner("dwch_mgmt", t.TempDir(), t.TempDir(), bin, store, pp.post)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	// First management-channel message: preamble should be injected.
	r.Enqueue(ccTask{Content: "do something", ChannelKind: "management"})
	waitForPosts(t, pp, 1)

	first := pp.all()[0]
	if !strings.Contains(first, "Duckway Control Channel") {
		t.Errorf("first management turn missing preamble: %q", first)
	}
	if !strings.Contains(first, "discord_create_task_channel") {
		t.Errorf("first management turn missing the create_task_channel nudge: %q", first)
	}

	// Second message in same session: preamble should NOT be re-injected
	// (claude remembers via --resume).
	r.Enqueue(ccTask{Content: "another thing", ChannelKind: "management"})
	waitForPosts(t, pp, 2)
	second := pp.all()[1]
	if strings.Contains(second, "Duckway Control Channel — system note") {
		t.Errorf("preamble re-injected on follow-up turn: %q", second)
	}
}

func TestCCRunner_NoPreambleForTaskChannel(t *testing.T) {
	bin := fakeClaudeEchoArgs(t, "sess-task-1")
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), bin, store, pp.post)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	r.Enqueue(ccTask{Content: "task work", ChannelKind: "task"})
	waitForPosts(t, pp, 1)
	if strings.Contains(pp.all()[0], "Duckway Control Channel — system note") {
		t.Errorf("preamble should not appear in task channel: %q", pp.all()[0])
	}
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
	t.Fatalf("only %d posts (wanted %d)", len(pp.all()), want)
}

func TestCCRunner_DefaultsCwd(t *testing.T) {
	bin := fakeClaude(t, "s", "ok")
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	r, err := newCCRunner("dwch_x", t.TempDir(), "", bin, store, pp.post)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	// Default cwd should be ~/.duckway/cc-workspace/<handle>
	want := filepath.Join(tmpHome, ".duckway", "cc-workspace", "dwch_x")
	if r.cwd != want {
		t.Errorf("cwd = %q, want %q", r.cwd, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected cwd dir created: %v", err)
	}
}

func TestCCRunner_NonJSONOutputFallback(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(`#!/bin/sh
echo "not json output"
`), 0700); err != nil {
		t.Fatal(err)
	}
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), bin, store, pp.post)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	r.Enqueue(ccTask{Content: "x"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(pp.all()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	posts := pp.all()
	if len(posts) == 0 {
		t.Fatal("no fallback post made")
	}
	if !strings.Contains(posts[0], "non-JSON") {
		t.Errorf("expected fallback message, got %q", posts[0])
	}
}

func TestCCRunner_ErrorExit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(`#!/bin/sh
echo "boom" >&2
exit 5
`), 0700); err != nil {
		t.Fatal(err)
	}
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), bin, store, pp.post)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	r.Enqueue(ccTask{Content: "x"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(pp.all()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	posts := pp.all()
	if len(posts) == 0 {
		t.Fatal("no error post made")
	}
	if !strings.Contains(posts[0], "exited with error") || !strings.Contains(posts[0], "boom") {
		t.Errorf("expected error report, got %q", posts[0])
	}
}
