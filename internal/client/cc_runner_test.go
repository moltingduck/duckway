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

	ok := r.Enqueue(ccTask{Content: "do a thing"})
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
