package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionManagerMissingStateLoadsEmptyVersionOne(t *testing.T) {
	m := NewSessionManager(t.TempDir(), &fakeSessionTmux{})
	state, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 {
		t.Fatalf("version = %d, want 1", state.Version)
	}
	if len(state.Sessions) != 0 {
		t.Fatalf("sessions = %+v, want empty", state.Sessions)
	}
}

func TestSessionManagerRejectsFutureStateVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent-sessions.json"), []byte(`{"version":99,"sessions":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewSessionManager(dir, &fakeSessionTmux{})
	if _, err := m.Load(); err == nil || !strings.Contains(err.Error(), "unsupported session state version") {
		t.Fatalf("Load error = %v, want unsupported version", err)
	}
}

func TestSessionManagerStartWritesTerminalRecord(t *testing.T) {
	tmux := &fakeSessionTmux{}
	m := NewSessionManager(t.TempDir(), tmux)
	rec, err := m.Start(SessionStartOptions{Name: "review", Kind: "terminal", AgentType: "codex", Cwd: t.TempDir(), Command: []string{"codex", "exec"}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.TmuxSession != "duckway-term-review" {
		t.Fatalf("tmux session = %q", rec.TmuxSession)
	}
	if tmux.startedSession != rec.TmuxSession {
		t.Fatalf("tmux started %q, want %q", tmux.startedSession, rec.TmuxSession)
	}
	state, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].Name != "review" {
		t.Fatalf("state sessions = %+v", state.Sessions)
	}
	if tmuxSessionName("review") == rec.TmuxSession {
		t.Fatalf("terminal session name collides with CC tmux naming: %q", rec.TmuxSession)
	}
}

func TestSessionManagerRejectsDuplicateLiveStart(t *testing.T) {
	dir := t.TempDir()
	tmux := &fakeSessionTmux{}
	m := NewSessionManager(dir, tmux)
	opts := SessionStartOptions{Name: "review", Kind: "terminal", AgentType: "codex", Cwd: t.TempDir(), Command: []string{"codex"}}
	if _, err := m.Start(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(opts); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("duplicate Start error = %v, want already running", err)
	}
}

func TestSessionManagerReplacesStaleRecord(t *testing.T) {
	dir := t.TempDir()
	tmux := &fakeSessionTmux{}
	m := NewSessionManager(dir, tmux)
	opts := SessionStartOptions{Name: "review", Kind: "terminal", AgentType: "codex", Cwd: t.TempDir(), Command: []string{"codex"}}
	if _, err := m.Start(opts); err != nil {
		t.Fatal(err)
	}
	tmux.alive = map[string]bool{}
	rec, err := m.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].ID != rec.ID {
		t.Fatalf("stale record was not replaced: %+v new=%+v", state.Sessions, rec)
	}
}

func TestSessionManagerSendReadStopTargetStateTmuxSession(t *testing.T) {
	tmux := &fakeSessionTmux{capture: "hello\nworld\n"}
	m := NewSessionManager(t.TempDir(), tmux)
	if _, err := m.Start(SessionStartOptions{Name: "review", Kind: "terminal", AgentType: "shell", Cwd: t.TempDir(), Command: []string{"sh"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("review", "git status"); err != nil {
		t.Fatal(err)
	}
	if tmux.sentSession != "duckway-term-review" || tmux.sentText != "git status" {
		t.Fatalf("send target=(%q,%q)", tmux.sentSession, tmux.sentText)
	}
	out, err := m.Read("review", 40)
	if err != nil {
		t.Fatal(err)
	}
	if out != tmux.capture || tmux.capturedSession != "duckway-term-review" {
		t.Fatalf("read output=%q target=%q", out, tmux.capturedSession)
	}
	if err := m.Stop("review"); err != nil {
		t.Fatal(err)
	}
	if tmux.killedSession != "duckway-term-review" {
		t.Fatalf("killed %q", tmux.killedSession)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != SessionStatusStopped {
		t.Fatalf("list after stop = %+v", list)
	}
}

func TestSessionManagerRejectsInvalidNameAndCwd(t *testing.T) {
	m := NewSessionManager(t.TempDir(), &fakeSessionTmux{})
	for _, name := range []string{"", "bad/name", "bad.name", "bad:name", "bad name", "bad\\name", "bad\nname", strings.Repeat("a", 65)} {
		if _, err := m.Start(SessionStartOptions{Name: name, Kind: "terminal", AgentType: "codex", Cwd: t.TempDir(), Command: []string{"codex"}}); err == nil {
			t.Fatalf("invalid name %q accepted", name)
		}
	}
	if _, err := m.Start(SessionStartOptions{Name: "ok", Kind: "terminal", AgentType: "codex", Cwd: filepath.Join(t.TempDir(), "missing"), Command: []string{"codex"}}); err == nil {
		t.Fatal("missing cwd accepted")
	}
}

func TestSessionManagerStoresEvaluatedAbsoluteCwd(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	m := NewSessionManager(t.TempDir(), &fakeSessionTmux{})
	rec, err := m.Start(SessionStartOptions{Name: "cwdtest", Kind: "terminal", AgentType: "shell", Cwd: linkDir, Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Cwd != realDir {
		t.Fatalf("cwd = %q, want evaluated %q", rec.Cwd, realDir)
	}
}

type fakeSessionTmux struct {
	alive           map[string]bool
	startedSession  string
	sentSession     string
	sentText        string
	capturedSession string
	killedSession   string
	capture         string
}

func (f *fakeSessionTmux) HasSession(name string) bool {
	if f.alive == nil {
		f.alive = map[string]bool{}
	}
	return f.alive[name]
}

func (f *fakeSessionTmux) NewSession(name, cwd string, command []string) error {
	if f.alive == nil {
		f.alive = map[string]bool{}
	}
	f.startedSession = name
	f.alive[name] = true
	return nil
}

func (f *fakeSessionTmux) Send(name, text string) error {
	f.sentSession = name
	f.sentText = text
	return nil
}

func (f *fakeSessionTmux) Capture(name string, lines int) (string, error) {
	f.capturedSession = name
	return f.capture, nil
}

func (f *fakeSessionTmux) Kill(name string) error {
	f.killedSession = name
	if f.alive != nil {
		delete(f.alive, name)
	}
	return nil
}
