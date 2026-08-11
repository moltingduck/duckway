//go:build smoke

package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSmokeSessionManagerTmuxLifecycle(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}
	tmuxAvailableMemo = nil
	configDir := t.TempDir()
	workDir := t.TempDir()
	outPath := filepath.Join(workDir, "input.txt")
	name := "smoke" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	defer exec.Command("tmux", "kill-session", "-t", terminalTmuxSessionName(name)).Run()

	m := NewTmuxSessionManager(configDir, nil)
	rec, err := m.Start(SessionStartOptions{
		Name:      name,
		Kind:      "terminal",
		AgentType: "shell",
		Cwd:       workDir,
		Backend:   SessionBackendTmux,
		Command:   []string{"sh", "-c", "while IFS= read -r line; do printf '%s\\n' \"$line\" >> input.txt; done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.TmuxSession != terminalTmuxSessionName(name) {
		t.Fatalf("tmux session = %q", rec.TmuxSession)
	}
	if err := m.Send(name, "hello smoke"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(outPath)
		if strings.Contains(string(data), "hello smoke") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello smoke") {
		t.Fatalf("input file = %q", string(data))
	}
	if _, err := m.Read(name, 20); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(name); err != nil {
		t.Fatal(err)
	}
	if tmuxHasSession(rec.TmuxSession) {
		t.Fatalf("tmux session %s still alive after stop", rec.TmuxSession)
	}
}
