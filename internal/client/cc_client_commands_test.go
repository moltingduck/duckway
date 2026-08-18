package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a small recording HTTP server that returns canned
// responses for the create-channel and post-message endpoints used by
// the bind flow.
type fakeServer struct {
	mu              sync.Mutex
	creates         []map[string]string
	messages        []map[string]string
	reactions       []string
	reactionEntered chan string
	reactionRelease <-chan struct{}
	srv             *httptest.Server
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
		case r.Method == "GET" && r.URL.Path == "/client/cc/channels":
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"handle": "dwch_task", "name": "task", "kind": "task", "cwd": os.TempDir()},
				{"handle": "dwch_mgmt", "name": "control", "kind": "management", "cwd": os.TempDir()},
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
			if f.reactionEntered != nil {
				f.reactionEntered <- body["emoji"]
			}
			if f.reactionRelease != nil {
				<-f.reactionRelease
			}
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

func firstAttachmentPathFromPrompt(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		path := strings.TrimPrefix(line, "- ")
		if i := strings.Index(path, " ("); i >= 0 {
			path = path[:i]
		}
		return path
	}
	return ""
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

	w.cmdSessions(context.Background(), "dwch_mgmt", nil)

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

	w.cmdBind(context.Background(), "dwch_mgmt", []string{"33333333-3333-3333-3333-333333333333"})

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

	w.cmdBind(context.Background(), "dwch_mgmt", []string{"44444444-4444-4444-4444-444444444444"})

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

	w.cmdBind(context.Background(), "dwch_mgmt", []string{"55555555-5555-5555-5555-555555555555"})

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

	w.cmdNewProject(context.Background(), "dwch_mgmt", []string{"fix-login", "--cwd", cwd, "--topic", "bug"})

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

	w.cmdNewProject(context.Background(), "dwch_mgmt", []string{"new-app", "--cwd", cwd})
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

	w.cmdNewProjectConfirm(context.Background(), "dwch_mgmt", []string{token[1]})
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

func TestHandleClientCommand_DuckwayVersion(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)

	sendClientCommand(t, w, "dwch_mgmt", "!duckway-version", nil)

	msgs := fake.snapshotMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one reply, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0]["content"], "duckway ") {
		t.Fatalf("version reply = %q", msgs[0]["content"])
	}
}

func TestHandleClientCommand_DuckwayDoctor(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)

	sendClientCommand(t, w, "dwch_mgmt", "!duckway-doctor", nil)

	msgs := fake.snapshotMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one reply, got %d", len(msgs))
	}
	got := msgs[0]["content"]
	for _, want := range []string{"Duckway doctor", "Config:", "server auth", "ducklion"} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctor reply missing %q:\n%s", want, got)
		}
	}
}

func TestHandleClientCommand_DuckwayRestartStartsDetachedHelper(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	calls := captureDetachedDuckwayCommands(t)

	sendClientCommand(t, w, "dwch_mgmt", "!duckway-restart", nil)

	msgs := fake.snapshotMessages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"], "restart accepted") {
		t.Fatalf("restart ack = %+v", msgs)
	}
	if len(*calls) != 1 {
		t.Fatalf("detached calls = %+v", *calls)
	}
	if got := strings.Join((*calls)[0].args, " "); got != "restart" {
		t.Fatalf("detached args = %q", got)
	}
	if (*calls)[0].configDir != w.configDir || !strings.HasSuffix((*calls)[0].logPath, "cc-ops.log") {
		t.Fatalf("detached call = %+v", (*calls)[0])
	}
}

func TestHandleClientCommand_DuckwayUpdateStartsDetachedHelper(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	calls := captureDetachedDuckwayCommands(t)

	sendClientCommand(t, w, "dwch_mgmt", "!duckway-update", []string{"--restart"})

	msgs := fake.snapshotMessages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"], "update accepted") || !strings.Contains(msgs[0]["content"], "restart") {
		t.Fatalf("update ack = %+v", msgs)
	}
	if len(*calls) != 1 {
		t.Fatalf("detached calls = %+v", *calls)
	}
	want := []string{"update", "--server", fake.srv.URL, "--restart"}
	if strings.Join((*calls)[0].args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("detached args = %v, want %v", (*calls)[0].args, want)
	}
}

func TestHandleClientCommand_DuckwayOpsRejectBadArgs(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	calls := captureDetachedDuckwayCommands(t)

	sendClientCommand(t, w, "dwch_mgmt", "!duckway-version", []string{"extra"})
	sendClientCommand(t, w, "dwch_mgmt", "!duckway-restart", []string{"now"})
	sendClientCommand(t, w, "dwch_mgmt", "!duckway-update", []string{"--bad"})
	sendClientCommand(t, w, "dwch_mgmt", "!duckway-update", []string{"--restart", "junk"})
	sendClientCommand(t, w, "dwch_mgmt", "!duckway-doctor", []string{"--verbose"})

	if len(*calls) != 0 {
		t.Fatalf("bad args started detached helpers: %+v", *calls)
	}
	msgs := fake.snapshotMessages()
	if len(msgs) != 5 {
		t.Fatalf("expected five usage/error replies, got %d", len(msgs))
	}
	for _, msg := range msgs {
		if !strings.Contains(msg["content"], "usage:") && !strings.Contains(msg["content"], "Usage:") {
			t.Fatalf("expected usage/error reply, got %q", msg["content"])
		}
	}
}

func TestHandleClientCommand_RejectsInvalidArgumentsBeforeDispatch(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	calls := captureDetachedDuckwayCommands(t)

	tests := []struct {
		command string
		args    []string
	}{
		{"!sessions", []string{"--all"}},
		{"!bind", []string{"--all"}},
		{"!projects", []string{"--all"}},
		{"!new", []string{"task", "--bogus", "value"}},
		{"!new-confirm", []string{"--force"}},
		{"!log", []string{"--all"}},
		{"!duckway-version", []string{"--short"}},
		{"!duckway-doctor", []string{"--verbose"}},
		{"!duckway-restart", []string{"--force"}},
		{"!duckway-update", []string{"--force"}},
	}
	for _, tt := range tests {
		sendClientCommand(t, w, "dwch_mgmt", tt.command, tt.args)
	}

	if len(*calls) != 0 {
		t.Fatalf("invalid commands started detached helpers: %+v", *calls)
	}
	msgs := fake.snapshotMessages()
	if len(msgs) != len(tests) {
		t.Fatalf("validation replies = %d, want %d: %+v", len(msgs), len(tests), msgs)
	}
	for _, msg := range msgs {
		content := msg["content"]
		if !strings.Contains(content, "unsupported option") || !strings.Contains(content, "Usage:") {
			t.Fatalf("unexpected validation reply: %q", content)
		}
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

func sendClientCommand(t *testing.T, w *CCWatch, handle, command string, args []string) {
	t.Helper()
	payload, _ := json.Marshal(clientCommandPayload{Command: command, Args: args})
	env := sseEnvelope{
		Type:    "client_command",
		Handle:  handle,
		Payload: payload,
	}
	data, _ := json.Marshal(env)
	w.handleClientCommand(data)
}

type detachedDuckwayCall struct {
	configDir string
	logPath   string
	args      []string
}

func captureDetachedDuckwayCommands(t *testing.T) *[]detachedDuckwayCall {
	t.Helper()
	var calls []detachedDuckwayCall
	prev := startDetachedDuckwayCommand
	startDetachedDuckwayCommand = func(configDir, logPath string, args []string) error {
		copied := append([]string(nil), args...)
		calls = append(calls, detachedDuckwayCall{configDir: configDir, logPath: logPath, args: copied})
		return nil
	}
	t.Cleanup(func() { startDetachedDuckwayCommand = prev })
	return &calls
}

func TestParseLogCount(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    int
		wantErr bool
	}{
		{name: "default", args: nil, want: 3},
		{name: "number", args: []string{"10"}, want: 10},
		{name: "legacy last", args: []string{"last", "3"}, want: 3},
		{name: "zero", args: []string{"0"}, wantErr: true},
		{name: "too large", args: []string{"21"}, wantErr: true},
		{name: "junk", args: []string{"latest", "3"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLogCount(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseLogCount(%v) succeeded, want error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLogCount(%v): %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("parseLogCount(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestHandleClientCommand_LogAcceptsCount(t *testing.T) {
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
		Payload: json.RawMessage(`{"command":"!log","args":["4"]}`),
	}
	data, _ := json.Marshal(env)
	w.handleClientCommand(data)

	msgs := fake.snapshotMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one reply, got %d", len(msgs))
	}
	body := msgs[0]["content"]
	for _, want := range []string{"first", "second", "third", "fourth"} {
		if !strings.Contains(body, want) {
			t.Fatalf("!log 4 missing %q:\n%s", want, body)
		}
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

func TestHandleMessageCreateDownloadsAttachmentOnlyMessage(t *testing.T) {
	t.Setenv("DUCKWAY_CC_ALLOW_INSECURE_ATTACHMENT_URLS", "1")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/files/note.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("duckway attachment body"))
	}))
	defer cdn.Close()

	fake := newFakeServer(t)
	watch := stubWatch(t, t.TempDir(), fake)
	promptCh := make(chan string, 1)
	spec := ccAgentSpec{
		Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", UseTmux: false,
		RunFn: func(ctx context.Context, _, _, prompt, _ string, _ []string) (string, string, bool, error) {
			promptCh <- prompt
			return "sid", "ok", false, nil
		},
	}
	r, err := newCCRunner("dwch_task", watch.configDir, t.TempDir(), spec, watch.sessions, fakePostNoop, fakePostReplyNoop, fakeReactNoop, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	watch.runners["dwch_task"] = r

	payload := json.RawMessage(`{
		"id":"1783330000000002234",
		"content":"",
		"author":{"id":"U1","bot":false},
		"attachments":[{
			"id":"A1",
			"filename":"../note.txt",
			"content_type":"text/plain",
			"size":23,
			"url":` + strconv.Quote(cdn.URL+"/files/note.txt") + `
		}]
	}`)
	env := sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: payload}
	data, _ := json.Marshal(env)
	watch.handleMessageCreate(data)

	var prompt string
	select {
	case prompt = <-promptCh:
	case <-time.After(time.Second):
		t.Fatal("runner did not receive attachment prompt")
	}
	for _, want := range []string{"User uploaded file", "note.txt", "content_type: text/plain", "cc-attachments"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	path := firstAttachmentPathFromPrompt(prompt)
	if path == "" {
		t.Fatalf("could not find attachment path in prompt:\n%s", prompt)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read downloaded attachment: %v", err)
	}
	if string(body) != "duckway attachment body" {
		t.Fatalf("attachment body = %q", body)
	}
	if strings.Contains(path, "..") {
		t.Fatalf("unsafe attachment path: %s", path)
	}
}

func TestValidateDiscordAttachmentURLRejectsNonDiscordHosts(t *testing.T) {
	if err := validateDiscordAttachmentURL("https://cdn.discordapp.com/attachments/a/b/file.txt"); err != nil {
		t.Fatalf("discord cdn url rejected: %v", err)
	}
	if err := validateDiscordAttachmentURL("http://cdn.discordapp.com/attachments/a/b/file.txt"); err == nil {
		t.Fatal("http discord url accepted without insecure opt-in")
	}
	if err := validateDiscordAttachmentURL("https://127.0.0.1/secret"); err == nil {
		t.Fatal("non-discord attachment url accepted")
	}
}

func TestHandleMessageCreateWaitsForDuckBeforeStartingRunner(t *testing.T) {
	fake := newFakeServer(t)
	reactionEntered := make(chan string, 4)
	reactionRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseReaction := func() { releaseOnce.Do(func() { close(reactionRelease) }) }
	t.Cleanup(releaseReaction)
	fake.reactionEntered = reactionEntered
	fake.reactionRelease = reactionRelease
	w := stubWatch(t, t.TempDir(), fake)
	started := make(chan struct{})
	done := make(chan struct{})
	spec := ccAgentSpec{Type: "claude_code", DisplayName: "claude", Bin: "/fake/claude", RunFn: func(ctx context.Context, _, _, _, _ string, _ []string) (string, string, bool, error) {
		close(started)
		<-done
		return "sid", "ok", false, nil
	}, UseTmux: false}
	r, err := newCCRunner("dwch_task", w.configDir, t.TempDir(), spec, w.sessions, fakePostNoop, fakePostReplyNoop, w.api.ReactCC, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(done)
		r.Stop()
	}()
	w.runners["dwch_task"] = r

	payload := json.RawMessage(`{"id":"1783330000000001236","content":"work","author":{"id":"U1","bot":false}}`)
	env := sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: payload}
	data, _ := json.Marshal(env)
	handled := make(chan struct{})
	go func() {
		w.handleMessageCreate(data)
		close(handled)
	}()

	select {
	case emoji := <-reactionEntered:
		if emoji != "🦆" {
			t.Fatalf("first reaction = %q, want duck", emoji)
		}
	case <-time.After(time.Second):
		t.Fatal("duck reaction did not start")
	}
	select {
	case <-started:
		t.Fatal("runner started before duck reaction completed")
	case <-time.After(50 * time.Millisecond):
	}

	releaseReaction()
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("message handler did not finish after duck reaction")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start after duck reaction")
	}
	select {
	case emoji := <-reactionEntered:
		if emoji != "⏳" {
			t.Fatalf("second reaction = %q, want hourglass", emoji)
		}
	case <-time.After(time.Second):
		t.Fatal("hourglass reaction did not follow duck")
	}
}

func TestHandleMessageCreateDoubleBangDoesNotRequireAgentBinary(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	w.agentTypes["cc1"] = "definitely_missing_agent"

	payload := json.RawMessage(`{"id":"1783330000000001235","content":"!! printf direct-shell","author":{"id":"U1","bot":false}}`)
	env := sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: payload}
	data, _ := json.Marshal(env)
	w.handleMessageCreate(data)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msgs := fake.snapshotMessages()
		if len(msgs) > 0 {
			body := msgs[0]["content"]
			if !strings.Contains(body, "direct-shell") {
				t.Fatalf("shell reply missing output: %s", body)
			}
			reactions := strings.Join(fake.snapshotReactions(), "\n")
			if !strings.Contains(reactions, "1783330000000001235/reactions:🦆") ||
				!strings.Contains(reactions, "1783330000000001235/reactions:✅") {
				t.Fatalf("missing shell reactions:\n%s", reactions)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for direct shell reply")
}

func TestHandleMessageCreateDoubleBangDoesNotWaitForAgentPrompt(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	agentStarted := make(chan struct{})
	releaseAgent := make(chan struct{})
	spec := ccAgentSpec{
		Type: "codex", DisplayName: "codex", Bin: "/fake/codex", UseTmux: false,
		RunFn: func(context.Context, string, string, string, string, []string) (string, string, bool, error) {
			close(agentStarted)
			<-releaseAgent
			return "sid", "agent done", false, nil
		},
	}
	agentRunner, err := newCCRunner("dwch_task", w.configDir, t.TempDir(), spec, w.sessions, fakePostNoop, fakePostReplyNoop, fakeReactNoop, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	w.runners["dwch_task"] = agentRunner
	defer func() {
		close(releaseAgent)
		w.shutdown()
	}()

	if !agentRunner.Enqueue(ccTask{Content: "long agent prompt", ChannelKind: "task"}) {
		t.Fatal("agent enqueue failed")
	}
	select {
	case <-agentStarted:
	case <-time.After(time.Second):
		t.Fatal("agent prompt did not start")
	}

	payload := json.RawMessage(`{"id":"1783330000000001237","content":"!! printf shell-did-not-wait","author":{"id":"U1","bot":false}}`)
	env := sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: payload}
	data, _ := json.Marshal(env)
	w.handleMessageCreate(data)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range fake.snapshotMessages() {
			if strings.Contains(msg["content"], "shell-did-not-wait") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("direct shell command waited for the blocked agent prompt")
}

func TestClientCommandQueueDoesNotDelayAgentPrompt(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	commandStarted := make(chan struct{})
	releaseCommand := make(chan struct{})
	w.clientCommandHandler = func(context.Context, []byte) {
		close(commandStarted)
		<-releaseCommand
	}

	agentStarted := make(chan struct{})
	releaseAgent := make(chan struct{})
	spec := ccAgentSpec{
		Type: "codex", DisplayName: "codex", Bin: "/fake/codex", UseTmux: false,
		RunFn: func(context.Context, string, string, string, string, []string) (string, string, bool, error) {
			close(agentStarted)
			<-releaseAgent
			return "sid", "agent done", false, nil
		},
	}
	agentRunner, err := newCCRunner("dwch_task", w.configDir, t.TempDir(), spec, w.sessions, fakePostNoop, fakePostReplyNoop, fakeReactNoop, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	w.runners["dwch_task"] = agentRunner
	defer func() {
		close(releaseCommand)
		close(releaseAgent)
		w.shutdown()
	}()

	commandPayload, _ := json.Marshal(clientCommandPayload{Command: "!projects"})
	commandData, _ := json.Marshal(sseEnvelope{Type: "client_command", Handle: "dwch_task", Payload: commandPayload})
	w.handleEvent("client_command", commandData)
	select {
	case <-commandStarted:
	case <-time.After(time.Second):
		t.Fatal("client command did not start")
	}

	messagePayload := json.RawMessage(`{"id":"1783330000000001238","content":"agent prompt","author":{"id":"U1","bot":false}}`)
	messageData, _ := json.Marshal(sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: messagePayload})
	w.handleEvent("message_create", messageData)
	select {
	case <-agentStarted:
	case <-time.After(time.Second):
		t.Fatal("agent prompt waited for the blocked client command")
	}
}

func TestBlockedClientCommandDoesNotDelayDirectShell(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	commandStarted := make(chan struct{})
	releaseCommand := make(chan struct{})
	w.clientCommandHandler = func(context.Context, []byte) {
		close(commandStarted)
		<-releaseCommand
	}
	defer func() {
		close(releaseCommand)
		w.shutdown()
	}()

	commandPayload, _ := json.Marshal(clientCommandPayload{Command: "!projects"})
	commandData, _ := json.Marshal(sseEnvelope{Type: "client_command", Handle: "dwch_task", Payload: commandPayload})
	w.handleEvent("client_command", commandData)
	select {
	case <-commandStarted:
	case <-time.After(time.Second):
		t.Fatal("client command did not start")
	}

	shellPayload := json.RawMessage(`{"id":"1783330000000001239","content":"!! printf shell-independent","author":{"id":"U1","bot":false}}`)
	shellData, _ := json.Marshal(sseEnvelope{Type: "message_create", CCID: "cc1", Handle: "dwch_task", Kind: "task", Payload: shellPayload})
	w.handleEvent("message_create", shellData)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range fake.snapshotMessages() {
			if strings.Contains(msg["content"], "shell-independent") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("direct shell command waited for the blocked client command")
}

func TestCCCommandRunnerStopCancelsActiveAndDropsQueued(t *testing.T) {
	started := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	runner := newCCCommandRunner(func(ctx context.Context, _ []byte) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()
		if current == 1 {
			close(started)
			<-ctx.Done()
		}
	})
	if !runner.Enqueue([]byte("first")) {
		t.Fatal("first command enqueue failed")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first command did not start")
	}
	if !runner.Enqueue([]byte("second")) {
		t.Fatal("second command enqueue failed")
	}
	done := make(chan struct{})
	go func() {
		runner.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the active command")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("commands executed after Stop: calls=%d", calls)
	}
}

func TestDeletedChannelCannotRecreateShellOrCommandRunner(t *testing.T) {
	fake := newFakeServer(t)
	w := stubWatch(t, t.TempDir(), fake)
	w.deleted["dwch_deleted"] = struct{}{}
	called := make(chan struct{}, 1)
	w.clientCommandHandler = func(context.Context, []byte) { called <- struct{}{} }

	payload, _ := json.Marshal(clientCommandPayload{Command: "!projects"})
	data, _ := json.Marshal(sseEnvelope{Type: "client_command", Handle: "dwch_deleted", Payload: payload})
	w.enqueueClientCommand(data)
	if _, ok := w.commandRunners["dwch_deleted"]; ok {
		t.Fatal("deleted channel recreated a command runner")
	}
	select {
	case <-called:
		t.Fatal("deleted channel executed a command")
	default:
	}
	if _, err := w.runnerForDirectShell("dwch_deleted"); err == nil {
		t.Fatal("deleted channel recreated a shell runner")
	}
	if _, ok := w.shellRunners["dwch_deleted"]; ok {
		t.Fatal("deleted channel installed a shell runner")
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
