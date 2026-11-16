package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestDucklordApprovalsPTYE2E drives the real ducklord binary through a PTY
// against an httptest approver server: login CLI, header pending count, panel
// open with I, live SSE update, reject, approve for a chosen duration, the
// 80-column layout and the NO_COLOR rendering. It needs no container, so it runs
// beside the container E2Es from scripts/ducklord-tui-e2e.sh.
func TestDucklordApprovalsPTYE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "ducklord")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build ducklord: %v: %s", err, output)
	}
	f := newApproverFixture(t)
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, ".ducklord"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, ".ducklord", "config.yaml")
	env := append(os.Environ(), "HOME="+home, "DUCKLORD_CONFIG="+cfg, "TERM=xterm-256color", "NO_COLOR=")

	login := exec.Command(binary, "approver", "login", "--server", f.srv.URL)
	login.Env = env
	login.Stdin = strings.NewReader("dwa_fixture\n")
	if output, err := login.CombinedOutput(); err != nil || !strings.Contains(string(output), "2 pending") || strings.Contains(string(output), "dwa_fixture") {
		t.Fatalf("login: %v: %s", err, output)
	}
	if info, err := os.Stat(filepath.Join(home, ".ducklord", "approver.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", info, err)
	}

	start := func(extraEnv ...string) (*os.File, *tuiCapture, *exec.Cmd) {
		command := exec.Command(binary, "tui", "--name", "approvals-e2e")
		command.Env = append(append([]string{}, env...), extraEnv...)
		terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 30, Cols: 130})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = terminal.Close()
			_ = command.Process.Kill()
			_ = command.Wait()
		})
		return terminal, newSizedTUICapture(terminal, 30, 130), command
	}

	terminal, capture, command := start()
	waitE2E(t, 20*time.Second, func() bool { return strings.Contains(capture.currentText(), "approvals: 2 pending") },
		func() string { return "header: " + strings.SplitN(capture.currentText(), "\n", 3)[0] })
	writePTY(t, terminal, "I")
	capture.waitCurrent(t, "Approvals · pending", 10*time.Second)
	capture.waitCurrent(t, "› pending", 10*time.Second)
	capture.waitCurrent(t, "POST /repos/x/y/issues", 10*time.Second)

	// A live update from /approver/events adds a third approval.
	f.mu.Lock()
	f.items = append(f.items, sampleApprovals()[0])
	f.items[2].ID = "approval-cccc"
	f.mu.Unlock()
	f.events <- "event: approvals\ndata: {\"pending\":3,\"total\":3}\n\n"
	capture.waitCurrent(t, "3 pending", 10*time.Second)

	writePTY(t, terminal, "n")
	capture.waitCurrent(t, "rejected approval", 10*time.Second)
	capture.waitCurrent(t, "2 pending", 10*time.Second)

	writePTY(t, terminal, "Y")
	capture.waitCurrent(t, "Approve for how long?", 10*time.Second)
	writePTY(t, terminal, "3")
	capture.waitCurrent(t, "approved approval", 10*time.Second)
	f.mu.Lock()
	calls := strings.Join(f.calls, "|")
	f.mu.Unlock()
	if !strings.Contains(calls, "/reject") || !strings.Contains(calls, "/approve map[duration_minutes:240]") {
		t.Fatalf("server calls = %s", calls)
	}

	// 80 columns: detail goes below the list.
	resizeTUICapture(t, terminal, capture, 30, 80)
	capture.waitCurrent(t, "Request:", 10*time.Second)
	if strings.Contains(capture.currentText(), "pending") && strings.Contains(capture.currentText(), " │ Status:") {
		t.Fatalf("80-column panel still uses columns: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool { return !strings.Contains(capture.currentText(), "Approvals · pending") },
		func() string { return "panel did not close on esc" })
	writePTY(t, terminal, "\x03")
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ducklord did not exit on Ctrl-C")
	}

	// NO_COLOR: the panel is readable and carries no colour escapes.
	terminal2, capture2, _ := start("NO_COLOR=1")
	capture2.waitCurrent(t, "approvals:", 20*time.Second)
	writePTY(t, terminal2, "I")
	capture2.waitCurrent(t, "› pending", 10*time.Second)
	capture2.mu.Lock()
	raw := append([]byte(nil), capture2.data...)
	capture2.mu.Unlock()
	if bytes.Contains(raw, []byte("\x1b[38;2")) || bytes.Contains(raw, []byte("\x1b[48;2")) {
		t.Fatal("NO_COLOR output still contains colour escapes")
	}
}
