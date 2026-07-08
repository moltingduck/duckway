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
	mu        sync.Mutex
	posts     []string
	replies   []string
	reactions []string
}

func (r *recordingPoster) post(ctx context.Context, handle, content string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, content)
	return nil
}

func (r *recordingPoster) postReply(ctx context.Context, handle, content, replyToMessageID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies = append(r.replies, replyToMessageID)
	r.posts = append(r.posts, content)
	return nil
}

func (r *recordingPoster) react(ctx context.Context, handle, messageID, emoji string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reactions = append(r.reactions, messageID+":"+emoji)
	return nil
}

func (r *recordingPoster) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.posts))
	copy(out, r.posts)
	return out
}

func (r *recordingPoster) allReactions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.reactions))
	copy(out, r.reactions)
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
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
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

func TestCCRunner_PostsProgressForLongRunningTask(t *testing.T) {
	oldFirst, oldInterval := ccLongRunFirstNotice, ccLongRunInterval
	ccLongRunFirstNotice = 20 * time.Millisecond
	ccLongRunInterval = 30 * time.Millisecond
	t.Cleanup(func() {
		ccLongRunFirstNotice = oldFirst
		ccLongRunInterval = oldInterval
	})

	fn := ccRunFn(func(_ context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		time.Sleep(90 * time.Millisecond)
		return "sess-abc", "done", false, nil
	})
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", RunFn: fn, UseTmux: false}
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	messageID := "1783330000000000001"
	if !r.Enqueue(ccTask{Content: "slow work", MessageID: messageID, ChannelKind: "task"}) {
		t.Fatal("Enqueue returned false")
	}
	waitForPosts(t, pp, 4)

	posts := strings.Join(pp.all(), "\n")
	if !strings.Contains(posts, "done") {
		t.Fatalf("missing final result:\n%s", posts)
	}
	if !strings.Contains(posts, "Still running") || !strings.Contains(posts, "Latest agent conversation") {
		t.Fatalf("missing long-run progress notice:\n%s", posts)
	}
	if got := strings.Count(posts, "Still running after"); got < 2 {
		t.Fatalf("long-running task should post repeated progress notices, got %d:\n%s", got, posts)
	}
	reactions := strings.Join(pp.allReactions(), "\n")
	if !strings.Contains(reactions, messageID+":⏳") {
		t.Fatalf("missing still-running reaction:\n%s", reactions)
	}
	if !strings.Contains(reactions, messageID+":✅") {
		t.Fatalf("missing completion reaction:\n%s", reactions)
	}
	if got := countStrings(pp.allReactions(), messageID+":⏳"); got != 1 {
		t.Fatalf("still-running reaction sent %d times, want 1:\n%s", got, reactions)
	}
	if got := countStrings(pp.allReactions(), messageID+":✅"); got != 1 {
		t.Fatalf("completion reaction sent %d times, want 1:\n%s", got, reactions)
	}
}

func TestCCRunner_DeduplicatesRepeatedTaskReactions(t *testing.T) {
	fn, _ := capturingRunFn("sess-abc", "ok")
	r, pp, _ := newTestRunner(t, fn)
	defer r.Stop()

	messageID := "1783330000000000002"
	task := ccTask{MessageID: messageID, ChannelKind: "task"}
	r.reactToTask(task, "⏳")
	r.reactToTask(task, "⏳")
	r.reactToTask(task, "✅")
	r.reactToTask(task, "✅")

	reactions := strings.Join(pp.allReactions(), "\n")
	if got := countStrings(pp.allReactions(), messageID+":⏳"); got != 1 {
		t.Fatalf("still-running reaction sent %d times, want 1:\n%s", got, reactions)
	}
	if got := countStrings(pp.allReactions(), messageID+":✅"); got != 1 {
		t.Fatalf("completion reaction sent %d times, want 1:\n%s", got, reactions)
	}
}

func TestCCRunnerSkipsDiscordDeliveryForSyntheticAgentTestMessage(t *testing.T) {
	fn, _ := capturingRunFn("sess-abc", "ok")
	r, pp, _ := newTestRunner(t, fn)
	defer r.Stop()

	task := ccTask{MessageID: "duckway-admin-test-1783330000000000003", TestID: "cctest_abc", ChannelKind: "management"}
	if err := r.postTaskMessage(task, "synthetic test reply"); err != nil {
		t.Fatal(err)
	}
	r.reactToTask(task, "✅")

	if posts := pp.all(); len(posts) != 0 {
		t.Fatalf("synthetic test posted to Discord: %+v", posts)
	}
	if reactions := pp.allReactions(); len(reactions) != 0 {
		t.Fatalf("synthetic test reacted in Discord: %+v", reactions)
	}
}

func countStrings(items []string, want string) int {
	var count int
	for _, item := range items {
		if item == want {
			count++
		}
	}
	return count
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
	r, err := newCCRunner("dwch_t", configDir, t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
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

func TestCCRunner_KeepsOpenAIEnvForCodexWithLegacyOAuthMarker(t *testing.T) {
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
	r, err := newCCRunner("dwch_t", configDir, t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	r.Enqueue(ccTask{Content: "hello", ChannelKind: "task"})
	waitForPosts(t, pp, 1)

	envText := strings.Join(gotEnv, "\n")
	if !strings.Contains(envText, "OPENAI_API_KEY=sk-dw-placeholder") {
		t.Fatalf("OPENAI_API_KEY should remain for Codex phantom provider even with legacy OAuth marker:\n%s", envText)
	}
	if !strings.Contains(envText, "ANTHROPIC_API_KEY=sk-ant-placeholder") {
		t.Fatalf("non-OpenAI env should remain:\n%s", envText)
	}
}

func TestCCRunner_OmitsOpenAIEnvForCodexNativeOAuth(t *testing.T) {
	var gotEnv []string
	fn := func(_ context.Context, _, _, _, _ string, extraEnv []string) (string, string, bool, error) {
		gotEnv = append([]string(nil), extraEnv...)
		return "sess-abc", "ok", false, nil
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := t.TempDir()
	if err := os.WriteFile(KeysEnvPath(configDir), []byte(`
OPENAI_API_KEY=sk-dw-placeholder
ANTHROPIC_API_KEY=sk-ant-placeholder
`), 0600); err != nil {
		t.Fatal(err)
	}
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"id_token":"header.payload.sig"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(`model = "gpt-5"`), 0600); err != nil {
		t.Fatal(err)
	}

	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "codex", DisplayName: "codex", Bin: "/fake/codex", RunFn: fn, UseTmux: false}
	r, err := newCCRunner("dwch_t", configDir, t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	r.Enqueue(ccTask{Content: "hello", ChannelKind: "task"})
	waitForPosts(t, pp, 1)

	envText := strings.Join(gotEnv, "\n")
	if strings.Contains(envText, "OPENAI_API_KEY=sk-dw-placeholder") {
		t.Fatalf("OPENAI_API_KEY should not be passed to Codex native OAuth:\n%s", envText)
	}
	if !strings.Contains(envText, "ANTHROPIC_API_KEY=sk-ant-placeholder") {
		t.Fatalf("non-OpenAI env should remain:\n%s", envText)
	}
}

func TestCCRunner_AddsProxyEnvForAgents(t *testing.T) {
	var gotEnv []string
	fn := func(_ context.Context, _, _, _, _ string, extraEnv []string) (string, string, bool, error) {
		gotEnv = append([]string(nil), extraEnv...)
		return "sess-abc", "ok", false, nil
	}

	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(`server_url: http://duckway.local
token: tok
client_name: client
proxy_port: 19090
`), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "codex", DisplayName: "codex", Bin: "/fake/codex", RunFn: fn, UseTmux: false}
	r, err := newCCRunner("dwch_t", configDir, t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	r.Enqueue(ccTask{Content: "hello", ChannelKind: "task"})
	waitForPosts(t, pp, 1)

	envText := strings.Join(gotEnv, "\n")
	for _, want := range []string{
		"HTTP_PROXY=http://localhost:19090",
		"HTTPS_PROXY=http://localhost:19090",
		"http_proxy=http://localhost:19090",
		"https_proxy=http://localhost:19090",
		"NO_PROXY=localhost,127.0.0.1",
		"no_proxy=localhost,127.0.0.1",
	} {
		if !strings.Contains(envText, want) {
			t.Fatalf("extraEnv missing %q:\n%s", want, envText)
		}
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

func TestCCRunner_StaleCodexResumeDropsSessionAndRetriesFresh(t *testing.T) {
	store := NewCCSessionStore(t.TempDir())
	if err := store.Set("dwch_t", "sid-stale"); err != nil {
		t.Fatal(err)
	}
	pp := &recordingPoster{}
	var seenSIDs []string
	fn := func(_ context.Context, _, _, _, sid string, _ []string) (string, string, bool, error) {
		seenSIDs = append(seenSIDs, sid)
		if sid == "sid-stale" {
			return "", "", false, fmt.Errorf("codex exited with status 1: thread/resume failed: no rollout found for thread id sid-stale")
		}
		return "sid-new", "fresh reply", false, nil
	}
	spec := ccAgentSpec{Type: "codex", DisplayName: "codex", Bin: "/fake/codex", RunFn: fn, UseTmux: false}
	r, err := newCCRunner("dwch_t", t.TempDir(), t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	r.Enqueue(ccTask{Content: "continue", ChannelKind: "task"})
	waitForPosts(t, pp, 1)

	if strings.Join(seenSIDs, ",") != "sid-stale," {
		t.Fatalf("resume retry sids = %#v, want stale then empty", seenSIDs)
	}
	if got := store.Get("dwch_t"); got != "sid-new" {
		t.Fatalf("cached sid = %q, want sid-new", got)
	}
	if got := pp.all()[0]; got != "fresh reply" {
		t.Fatalf("posted %q, want fresh retry reply", got)
	}
}

func TestCCRunnerAdoptsPendingTurnBeforeQueuedMessage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	handle := "dwch_adopt_pending"
	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		t.Fatal(err)
	}
	eventsDir := filepath.Join(chDir, "events")
	if err := os.MkdirAll(eventsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeInFlight(filepath.Join(chDir, "in-flight.json"), handle, "msg-old", 100); err != nil {
		t.Fatal(err)
	}
	stop := `{"session_id":"sid-recovered","last_assistant_message":"old turn done"}`
	if err := os.WriteFile(filepath.Join(eventsDir, "200.stop.json"), []byte(stop), 0600); err != nil {
		t.Fatal(err)
	}

	store := NewCCSessionStore(t.TempDir())
	processed := NewCCProcessedStore(t.TempDir())
	pp := &recordingPoster{}
	var seenSID string
	fn := ccRunFn(func(_ context.Context, _, _, _, sid string, _ []string) (string, string, bool, error) {
		seenSID = sid
		return "sid-next", "next turn done", false, nil
	})
	yes := true
	tmuxAvailableMemo = &yes
	t.Cleanup(func() { tmuxAvailableMemo = nil })
	spec := ccAgentSpec{Type: "codex", DisplayName: "codex", Bin: "/fake/codex", TmuxRunFn: fn, UseTmux: true}
	r, err := newCCRunnerWithProcessed(handle, t.TempDir(), t.TempDir(), spec, store, processed, pp.post, pp.postReply, pp.react, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	waitForPosts(t, pp, 1)
	if got := pp.all()[0]; !strings.Contains(got, "old turn done") {
		t.Fatalf("first post = %q, want recovered old turn", got)
	}
	if got := store.Get(handle); got != "sid-recovered" {
		t.Fatalf("recovered sid = %q", got)
	}
	if !processed.Seen("msg-old") {
		t.Fatal("recovered message id was not marked processed")
	}

	r.Enqueue(ccTask{Content: "new message", MessageID: "msg-new", ChannelKind: "task"})
	waitForPosts(t, pp, 2)
	if seenSID != "sid-recovered" {
		t.Fatalf("queued turn sid = %q, want recovered sid", seenSID)
	}
	if got := pp.all()[1]; got != "next turn done" {
		t.Fatalf("second post = %q", got)
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

func TestCCRunnerStopCancelsInFlightTask(t *testing.T) {
	started := make(chan struct{})
	fn := ccRunFn(func(ctx context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		close(started)
		<-ctx.Done()
		return "", "", false, ctx.Err()
	})
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "codex", DisplayName: "codex", Bin: "/fake/codex", RunFn: fn, UseTmux: false}
	r, err := newCCRunner("dwch_cancel", t.TempDir(), t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Enqueue(ccTask{Content: "block", ChannelKind: "task"}) {
		t.Fatal("enqueue failed")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runFn did not start")
	}
	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel in-flight task")
	}
}

func TestCCRunner_ManagementPreamble_FirstTurnOnly(t *testing.T) {
	fn, captured := capturingRunFn("sess-mgmt-1", "response")
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", UseTmux: true}
	r, err := newCCRunner("dwch_mgmt", t.TempDir(), t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
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

	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", UseTmux: true}
	r, err := newCCRunner("dwch_x", t.TempDir(), "", spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
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

func TestCCRunnerLogsAgentSettingsAndDebugCLI(t *testing.T) {
	fn := ccRunFn(func(_ context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		return "sid-new", "ok", false, nil
	})
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{
		Type:        "codex",
		DisplayName: "codex",
		Bin:         "/fake/codex",
		RunFn:       fn,
		UseTmux:     false,
		ExtraEnv:    []string{"DUCKWAY_CC_CODEX_SANDBOX=danger-full-access"},
	}
	r, err := newCCRunner("dwch_log", t.TempDir(), t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	var logs []string
	var mu sync.Mutex
	r.logger = func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	prompt := "abcdefghijklmnopqrstuvwxyz"
	r.Enqueue(ccTask{Content: prompt, ChannelKind: "task"})
	waitForPosts(t, pp, 1)

	mu.Lock()
	logText := strings.Join(logs, "\n")
	mu.Unlock()
	for _, want := range []string{
		"agent_type=codex",
		"runner_mode=headless",
		"sandbox_mode=danger-full-access",
		"sandbox_arg_style=--sandbox",
		"debug cli='/fake/codex' 'exec'",
		"'abcde...vwxyz'",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log missing %q:\n%s", want, logText)
		}
	}
	if strings.Contains(logText, prompt) {
		t.Fatalf("debug log leaked full prompt:\n%s", logText)
	}
}

func TestCCRunnerLogsCodexResumeSandboxStyle(t *testing.T) {
	fn := ccRunFn(func(_ context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		return "sid-old", "ok", false, nil
	})
	store := NewCCSessionStore(t.TempDir())
	if err := store.Set("dwch_resume", "sid-old"); err != nil {
		t.Fatal(err)
	}
	pp := &recordingPoster{}
	spec := ccAgentSpec{
		Type:        "codex",
		DisplayName: "codex",
		Bin:         "/fake/codex",
		RunFn:       fn,
		UseTmux:     false,
		ExtraEnv:    []string{"DUCKWAY_CC_CODEX_SANDBOX=read-only"},
	}
	r, err := newCCRunner("dwch_resume", t.TempDir(), t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()

	var logs []string
	var mu sync.Mutex
	r.logger = func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	r.Enqueue(ccTask{Content: "hello resume", ChannelKind: "task"})
	waitForPosts(t, pp, 1)

	mu.Lock()
	logText := strings.Join(logs, "\n")
	mu.Unlock()
	for _, want := range []string{
		"sandbox_mode=read-only",
		"sandbox_arg_style=-c sandbox_mode",
		"'resume'",
		"'sandbox_mode=\"read-only\"'",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log missing %q:\n%s", want, logText)
		}
	}
	if strings.Contains(logText, "'--sandbox'") {
		t.Fatalf("resume debug log must not include --sandbox:\n%s", logText)
	}
}

func TestPromptLogSummary(t *testing.T) {
	if got := promptLogSummary("1234567890"); got != "1234567890" {
		t.Fatalf("short summary = %q", got)
	}
	if got := promptLogSummary("12345678901"); got != "12345...78901" {
		t.Fatalf("long summary = %q", got)
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
