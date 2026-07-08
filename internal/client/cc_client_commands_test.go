package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a small recording HTTP server that returns canned
// responses for the create-channel and post-message endpoints used by
// the bind flow.
type fakeServer struct {
	mu        sync.Mutex
	creates   []map[string]string
	messages  []map[string]string
	reactions []string
	srv       *httptest.Server
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/client/cc/channels":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.creates = append(f.creates, body)
			n := len(f.creates)
			f.mu.Unlock()
			handle := "dwch_test" + string(rune('0'+n))
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"handle": handle, "name": body["name"], "topic": "", "cwd": body["cwd"], "kind": "task",
			})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["_path"] = r.URL.Path
			f.mu.Lock()
			f.messages = append(f.messages, body)
			f.mu.Unlock()
			w.WriteHeader(204)
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/reactions"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.reactions = append(f.reactions, r.URL.Path+":"+body["emoji"])
			f.mu.Unlock()
			w.WriteHeader(204)
		default:
			http.Error(w, "unhandled "+r.Method+" "+r.URL.Path, 404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) snapshotMessages() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]string, len(f.messages))
	copy(out, f.messages)
	return out
}

func (f *fakeServer) snapshotCreates() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]string, len(f.creates))
	copy(out, f.creates)
	return out
}

func (f *fakeServer) snapshotReactions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.reactions))
	copy(out, f.reactions)
	return out
}

// stubWatch wires a CCWatch with a fake server + temp config + temp
// claude-projects tree. Used to drive handleClientCommand end-to-end.
func stubWatch(t *testing.T, projectsRoot string, fake *fakeServer) *CCWatch {
	t.Helper()
	configDir := t.TempDir()
	return &CCWatch{
		cfg:         &Config{ServerURL: fake.srv.URL, Token: "tok"},
		configDir:   configDir,
		agentTypes:  map[string]string{},
		sessions:    NewCCSessionStore(configDir),
		processed:   NewCCProcessedStore(configDir),
		runners:     map[string]*ccRunner{},
		pendingNew:  map[string]pendingNewProject{},
		deleted:     map[string]struct{}{},
		recoverSeen: map[string]struct{}{},
		api:         NewAPIClient(fake.srv.URL, "tok"),
	}
}

func TestCmdSessions_PostsListingOfUnbound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := home + "/.claude/projects"
	writeFakeSession(t, projects, "-home-me-app", "11111111-1111-1111-1111-111111111111", []string{
		`{"type":"user","cwd":"/home/me/app","message":{"role":"user","content":"first prompt about app"}}`,
	})
	writeFakeSession(t, projects, "-home-me-lib", "22222222-2222-2222-2222-222222222222", []string{
		`{"type":"user","cwd":"/home/me/lib","message":{"role":"user","content":"lib question"}}`,
	})

	fake := newFakeServer(t)
	w := stubWatch(t, projects, fake)
	// Pretend `lib` is already bound — only `app` should show up.
	if err := w.sessions.Set("dwch_existing", "22222222-2222-2222-2222-222222222222"); err != nil {
		t.Fatal(err)
	}

	w.cmdSessions("dwch_mgmt", nil)

	msgs := fake.snapshotMessages()
	if len(msgs) != 1 {
		t.Fatalf("want 1 reply, got %d", len(msgs))
	}
	body := msgs[0]["content"]
	if !strings.Contains(body, "11111111") {
		t.Errorf("listing should show the unbound session, got: %s", body)
	}
	if strings.Contains(body, "22222222") {
		t.Errorf("listing should hide the bound session, got: %s", body)
	}
	if !strings.HasSuffix(msgs[0]["_path"], "/messages") {
		t.Errorf("reply went to wrong path: %s", msgs[0]["_path"])
	}
}

func TestRecoverPendingTurnsStillRunningMarksMessageProcessed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	handle := "dwch_still_running"
	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(chDir, "events"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeInFlight(filepath.Join(chDir, "in-flight.json"), handle, "msg-still", 100); err != nil {
		t.Fatal(err)
	}

	fake := newFakeServer(t)
	w := stubWatch(t, filepath.Join(home, ".claude", "projects"), fake)
	summary := &ccReconcileSummary{}
	w.recoverPendingTurns(context.Background(), summary, map[string]bool{handle: true})

	if summary.StillRunning != 1 {
		t.Fatalf("StillRunning = %d, want 1", summary.StillRunning)
	}
	if !w.processed.Seen("msg-still") {
		t.Fatal("still-running message id was not marked processed")
	}
	msgs := fake.snapshotMessages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"], "still appears to be running") {
		t.Fatalf("still-running notice not posted: %+v", msgs)
	}

	w.recoverPendingTurns(context.Background(), summary, map[string]bool{handle: true})
	if msgs := fake.snapshotMessages(); len(msgs) != 1 {
		t.Fatalf("still-running notice posted more than once: %+v", msgs)
	}
}

func TestRecoverPendingTurnsDiscardsInactiveChannel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	handle := "dwch_inactive"
	chDir, err := tmuxChannelDir(handle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(chDir, "events"), 0700); err != nil {
		t.Fatal(err)
	}
	inFlightPath := filepath.Join(chDir, "in-flight.json")
	if err := writeInFlight(inFlightPath, handle, "msg-inactive", 100); err != nil {
		t.Fatal(err)
	}

	fake := newFakeServer(t)
	w := stubWatch(t, filepath.Join(home, ".claude", "projects"), fake)
	w.recoverPendingTurns(context.Background(), &ccReconcileSummary{}, map[string]bool{})

	if msgs := fake.snapshotMessages(); len(msgs) != 0 {
		t.Fatalf("inactive channel should not receive recovery posts: %+v", msgs)
	}
	if _, err := os.Stat(inFlightPath); !os.IsNotExist(err) {
		t.Fatalf("inactive channel in-flight marker was not removed: %v", err)
	}
}

func TestAcquireCCWatchLockRejectsSecondInstance(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireCCWatchLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := acquireCCWatchLock(dir)
	if err == nil {
		releaseCCWatchLock(second)
		t.Fatal("second lock unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock error = %v", err)
	}
	releaseCCWatchLock(first)
	third, err := acquireCCWatchLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	releaseCCWatchLock(third)
}

func TestIsDiscordSnowflake(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"1783330000000000001", true},
		{"12345678901234567", true},
		{"12345678901234567890", true},
		{"duckway-admin-test-1783330000000000001", false},
		{"1234567890123456", false},
		{"123456789012345678901", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isDiscordSnowflake(tt.id); got != tt.want {
			t.Fatalf("isDiscordSnowflake(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestCmdBind_CreatesChannelAndWritesBinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := home + "/.claude/projects"
	writeFakeSession(t, projects, "-home-me-thing", "33333333-3333-3333-3333-333333333333", []string{
		`{"type":"user","cwd":"/home/me/thing","message":{"role":"user","content":"go!"}}`,
	})

	fake := newFakeServer(t)
	w := stubWatch(t, projects, fake)

	w.cmdBind("dwch_mgmt", []string{"33333333-3333-3333-3333-333333333333"})

	creates := fake.snapshotCreates()
	if len(creates) != 1 {
		t.Fatalf("want 1 channel created, got %d", len(creates))
	}
	if creates[0]["name"] != "thing" {
		t.Errorf("channel name = %q, want %q", creates[0]["name"], "thing")
	}
	if creates[0]["cwd"] != "/home/me/thing" {
		t.Errorf("channel cwd = %q", creates[0]["cwd"])
	}
	// Daemon should have written the binding to cc-sessions.json.
	if got := w.sessions.Get("dwch_test1"); got != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("binding not persisted: got %q", got)
	}
	// The reply mentions success.
	msgs := fake.snapshotMessages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"], "Bound") {
		t.Errorf("expected success reply, got %+v", msgs)
	}
}

func TestCmdBind_AlreadyBoundSkipsCreate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := home + "/.claude/projects"
	writeFakeSession(t, projects, "-home-me-dup", "44444444-4444-4444-4444-444444444444", []string{
		`{"type":"user","cwd":"/home/me/dup","message":{"role":"user","content":"x"}}`,
	})

	fake := newFakeServer(t)
	w := stubWatch(t, projects, fake)
	if err := w.sessions.Set("dwch_already", "44444444-4444-4444-4444-444444444444"); err != nil {
		t.Fatal(err)
	}

	w.cmdBind("dwch_mgmt", []string{"44444444-4444-4444-4444-444444444444"})

	if len(fake.snapshotCreates()) != 0 {
		t.Errorf("should not have created a channel for already-bound session: %+v", fake.snapshotCreates())
	}
	msgs := fake.snapshotMessages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"], "Already bound") {
		t.Errorf("expected already-bound notice, got %+v", msgs)
	}
}

func TestCmdBind_UnknownSessionReportsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// no sessions on disk
	fake := newFakeServer(t)
	w := stubWatch(t, home, fake)

	w.cmdBind("dwch_mgmt", []string{"55555555-5555-5555-5555-555555555555"})

	msgs := fake.snapshotMessages()
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0]["content"], "not found") {
		t.Errorf("expected not-found error, got %q", msgs[0]["content"])
	}
}

func TestCmdNewWithExistingCwdCreatesChannel(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "app")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	fake := newFakeServer(t)
	w := stubWatch(t, filepath.Join(root, ".claude", "projects"), fake)

	w.cmdNewProject("dwch_mgmt", []string{"fix-login", "--cwd", cwd, "--topic", "bug"})

	creates := fake.snapshotCreates()
	if len(creates) != 1 {
		t.Fatalf("creates len = %d, want 1", len(creates))
	}
	if creates[0]["name"] != "fix-login" || creates[0]["cwd"] != cwd || creates[0]["topic"] != "bug" {
		t.Fatalf("unexpected create payload: %+v", creates[0])
	}
	msgs := fake.snapshotMessages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"], "Created") {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
}

func TestCmdNewWithMissingCwdRequiresConfirmationThenAddsProject(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "new-app")
	fake := newFakeServer(t)
	w := stubWatch(t, filepath.Join(root, ".claude", "projects"), fake)

	w.cmdNewProject("dwch_mgmt", []string{"new-app", "--cwd", cwd})
	if len(fake.snapshotCreates()) != 0 {
		t.Fatalf("should not create channel before confirmation: %+v", fake.snapshotCreates())
	}
	msgs := fake.snapshotMessages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"], "Project folder does not exist") {
		t.Fatalf("unexpected confirmation prompt: %+v", msgs)
	}
	token := regexp.MustCompile("!new-confirm ([a-f0-9]+)").FindStringSubmatch(msgs[0]["content"])
	if len(token) != 2 {
		t.Fatalf("confirmation token not found in %q", msgs[0]["content"])
	}

	w.cmdNewProjectConfirm("dwch_mgmt", []string{token[1]})
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		t.Fatalf("cwd not created: stat=%v err=%v", st, err)
	}
	projects, err := NewCCProjectStore(w.configDir).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Path != cwd {
		t.Fatalf("projects = %+v, want cwd %s", projects, cwd)
	}
	creates := fake.snapshotCreates()
	if len(creates) != 1 || creates[0]["cwd"] != cwd {
		t.Fatalf("creates = %+v, want cwd %s", creates, cwd)
	}
}

func TestDiscordChannelNameFromCwd(t *testing.T) {
	cases := map[string]string{
		"/home/me/MyApp":            "myapp",
		"/Users/alice/Code/foo bar": "foo-bar",
		"/var/lib/whatever":         "whatever",
		"":                          "session",
		"/":                         "session",
		"/home/me/x@y!":             "x-y",
	}
	for in, want := range cases {
		if got := discordChannelNameFromCwd(in); got != want {
			t.Errorf("discordChannelNameFromCwd(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleClientCommand_DispatchesByName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake := newFakeServer(t)
	w := stubWatch(t, home+"/.claude/projects", fake)

	env := sseEnvelope{
		Type:    "client_command",
		Handle:  "dwch_mgmt",
		Payload: json.RawMessage(`{"command":"!sessions","args":[]}`),
	}
	data, _ := json.Marshal(env)
	w.handleClientCommand(data)

	msgs := fake.snapshotMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one reply, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0]["content"], "no unbound local claude sessions") {
		t.Errorf("unexpected reply: %q", msgs[0]["content"])
	}
}

func TestHandleClientCommand_LogReturnsRunnerHistory(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	store := NewCCSessionStore(t.TempDir())
	pp := &recordingPoster{}
	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", RunFn: func(context.Context, string, string, string, string, []string) (string, string, bool, error) {
		return "sid", "ok", false, nil
	}, UseTmux: false}
	r, err := newCCRunner("dwch_task", w.configDir, t.TempDir(), spec, store, pp.post, pp.postReply, pp.react, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	r.appendHistory("user", "first")
	r.appendHistory("assistant", "second")
	r.appendHistory("user", "third")
	r.appendHistory("assistant", "fourth")
	w.runners["dwch_task"] = r

	env := sseEnvelope{
		Type:    "client_command",
		Handle:  "dwch_task",
		Payload: json.RawMessage(`{"command":"!log","args":[]}`),
	}
	data, _ := json.Marshal(env)
	w.handleClientCommand(data)

	msgs := fake.snapshotMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one reply, got %d", len(msgs))
	}
	body := msgs[0]["content"]
	if strings.Contains(body, "first") || !strings.Contains(body, "second") || !strings.Contains(body, "third") || !strings.Contains(body, "fourth") {
		t.Fatalf("unexpected !log body:\n%s", body)
	}
}

func TestHandleMessageCreateReactsDuckAfterEnqueue(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	started := make(chan struct{})
	done := make(chan struct{})
	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", RunFn: func(ctx context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		close(started)
		<-done
		return "sid", "ok", false, nil
	}, UseTmux: false}
	r, err := newCCRunner("dwch_task", w.configDir, t.TempDir(), spec, w.sessions, fakePostNoop, fakePostReplyNoop, fakeReactNoop, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(done)
		r.Stop()
	}()
	w.runners["dwch_task"] = r

	payload := json.RawMessage(`{"id":"1783330000000001234","content":"work","author":{"id":"U1","bot":false}}`)
	env := sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: payload}
	data, _ := json.Marshal(env)
	w.handleMessageCreate(data)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	reactions := strings.Join(fake.snapshotReactions(), "\n")
	if !strings.Contains(reactions, "1783330000000001234/reactions:🦆") {
		t.Fatalf("missing queued duck reaction:\n%s", reactions)
	}
}

func fakePostNoop(context.Context, string, string) error { return nil }

func fakePostReplyNoop(context.Context, string, string, string) error { return nil }

func fakeReactNoop(context.Context, string, string, string) error { return nil }

func TestBindLocalSessions_FullCLIEntry(t *testing.T) {
	// Same as cmdBind but exercises the standalone func the CLI uses.
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := home + "/.claude/projects"
	writeFakeSession(t, projects, "-tmp-x", "66666666-6666-6666-6666-666666666666", []string{
		`{"type":"user","cwd":"/tmp/x","message":{"role":"user","content":"cli"}}`,
	})
	fake := newFakeServer(t)
	configDir := t.TempDir()
	store := NewCCSessionStore(configDir)
	api := NewAPIClient(fake.srv.URL, "tok")

	results := BindLocalSessions(context.Background(), api, store, []string{"66666666-6666-6666-6666-666666666666"})

	if len(results) != 1 || results[0].Error != "" || results[0].Channel == "" {
		t.Fatalf("unexpected results: %+v", results)
	}
	if store.Get(results[0].Channel) != "66666666-6666-6666-6666-666666666666" {
		t.Errorf("store not updated: %v", store.Snapshot())
	}
}
