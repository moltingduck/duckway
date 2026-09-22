package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestDucklordHostResourcesContainerE2E drives Host -> Resources through a
// real PTY and verifies refresh preserves the resource route and parent focus.
func TestDucklordHostResourcesContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	home := fmt.Sprintf("/tmp/ducklord-host-resources-e2e-%d", time.Now().UnixNano())
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("prepare SSH config: %v: %s", err, out)
	}
	owner := fmt.Sprintf("host-resources-e2e-%d", time.Now().UnixNano())
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", "ducklord", "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x03"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	capture.waitCurrent(t, "PROJECTS", 20*time.Second)
	writePTY(t, terminal, "h")
	capture.waitCurrent(t, "Choose a host to view its settings", 10*time.Second)
	capture.waitCurrent(t, "› client-a · connected", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Host actions · client-a", 10*time.Second)
	for range 8 {
		writePTY(t, terminal, "j")
	}
	waitE2E(t, 3*time.Second, func() bool { return strings.Contains(capture.currentText(), "› Resources") }, func() string { return safeTerminalDiagnostic(capture.currentText()) })
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Host resources · client-a", 15*time.Second)
	capture.waitCurrent(t, "OS / arch:", 15*time.Second)
	writePTY(t, terminal, "r")
	capture.waitCurrent(t, "Host resources · client-a", 15*time.Second)
	capture.waitCurrent(t, "OS / arch:", 15*time.Second)
	writePTY(t, terminal, "\x1b")
	capture.waitCurrent(t, "Host actions · client-a", 10*time.Second)
	if !strings.Contains(capture.currentText(), "› Resources") {
		t.Fatalf("Esc did not restore Resources focus after refresh: %s", safeTerminalDiagnostic(capture.currentText()))
	}

	// Return to the host selector and open the same route for client-b. This
	// proves the route follows the selected Host instead of retaining A's view.
	writePTY(t, terminal, "\x1b")
	capture.waitCurrent(t, "Choose a host to view its settings", 10*time.Second)
	capture.waitCurrent(t, "› client-a · connected", 10*time.Second)
	writePTY(t, terminal, "\x1b[B")
	capture.waitCurrent(t, "› client-b · connected", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Host actions · client-b", 10*time.Second)
	for range 8 {
		writePTY(t, terminal, "j")
	}
	waitE2E(t, 3*time.Second, func() bool { return strings.Contains(capture.currentText(), "› Resources") }, func() string { return safeTerminalDiagnostic(capture.currentText()) })
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Host resources · client-b", 15*time.Second)
	capture.waitCurrent(t, "OS / arch:", 15*time.Second)
	writePTY(t, terminal, "\x1b")
	capture.waitCurrent(t, "Host actions · client-b", 10*time.Second)
	if !strings.Contains(capture.currentText(), "› Resources") {
		t.Fatalf("Esc did not restore Resources focus for client-b: %s", safeTerminalDiagnostic(capture.currentText()))
	}
}
