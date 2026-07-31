package ducklion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("DUCKLION_SUPERVISE") == "1" && len(os.Args) > 1 && os.Args[1] == "__supervise" {
		opts, err := ParseSupervisorArgs(os.Args[2:])
		if err != nil {
			panic(err)
		}
		if err := RunSupervisor(opts); err != nil {
			panic(err)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestValidateNameRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"", "../x", "a.b", "a b", "a:b"} {
		if _, err := ValidateName(name); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
}

func TestValidateAgentTypeRejectsUnsafeValues(t *testing.T) {
	for _, agentType := range []string{"codex", "shell", "claude_code", "openclaw-1"} {
		if _, err := ValidateAgentType(agentType); err != nil {
			t.Fatalf("agent type %q rejected: %v", agentType, err)
		}
	}
	for _, agentType := range []string{"bad value", "bad/type", "\x1b[2J"} {
		if _, err := ValidateAgentType(agentType); err == nil {
			t.Fatalf("agent type %q accepted", agentType)
		}
	}
}

func TestScrubSessionEnvDropsSSHAuthSock(t *testing.T) {
	got := scrubSessionEnv([]string{"PATH=/bin", "SSH_AUTH_SOCK=/tmp/agent.sock", "HOME=/home/duck"})
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "SSH_AUTH_SOCK=") {
		t.Fatalf("SSH_AUTH_SOCK survived: %#v", got)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/home/duck") {
		t.Fatalf("env was over-scrubbed: %#v", got)
	}
}

func TestManagerStartReadSendStopWithPTY(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, "")
	rec, err := m.Start(StartOptions{
		Name:      "alpha",
		AgentType: "shell",
		Cwd:       root,
		Command:   []string{"sh", "-lc", "printf ready; while IFS= read -r line; do echo got:$line; done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Socket == "" || rec.Log == "" || rec.PID == 0 {
		t.Fatalf("record = %+v", rec)
	}
	waitForText(t, m, "alpha", "ready")
	if err := m.Send("alpha", "hello"); err != nil {
		t.Fatal(err)
	}
	waitForText(t, m, "alpha", "got:hello")
	if err := m.Stop("alpha"); err != nil {
		t.Fatal(err)
	}
	records, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != StatusStopped {
		t.Fatalf("records = %+v", records)
	}
}

func TestManagerRejectsDuplicateLiveSession(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, "")
	_, err := m.Start(StartOptions{Name: "alpha", Cwd: root, Command: []string{"sh", "-lc", "sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop("alpha") })
	if _, err := m.Start(StartOptions{Name: "alpha", Cwd: root, Command: []string{"sh", "-lc", "sleep 30"}}); err == nil {
		t.Fatal("duplicate session accepted")
	}
}

func TestParseSupervisorArgs(t *testing.T) {
	opts, err := ParseSupervisorArgs([]string{"--name", "alpha", "--agent", "shell", "--cwd", "/tmp", "--socket", "/tmp/a.sock", "--log", "/tmp/a.log", "--", "sh", "-lc", "echo ok"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Name != "alpha" || strings.Join(opts.Command, " ") != "sh -lc echo ok" {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestReadMissingLogReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, "")
	state := &State{Version: stateVersion, Sessions: []Record{{
		Name: "alpha", Status: StatusRunning, PID: os.Getpid(), Log: filepath.Join(root, "missing.log"), Socket: filepath.Join(root, "missing.sock"),
	}}}
	unlock, err := m.lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.saveLocked(state); err != nil {
		t.Fatal(err)
	}
	unlock()
	text, err := m.Read("alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Fatalf("text = %q", text)
	}
}

func waitForText(t *testing.T, m *Manager, name, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		text, err := m.Read(name, 20)
		if err == nil && strings.Contains(text, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	text, _ := m.Read(name, 20)
	t.Fatalf("timed out waiting for %q in %q", want, text)
}
