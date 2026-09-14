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

// Disconnect is local to this Ducklord process. Ducklion and its shells keep
// running, while the detailed list must retain their known metadata and must
// never misrepresent the right pane as a live remote terminal.
func TestDucklordDetailedOfflineContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" || os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on a disposable Host")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	home := fmt.Sprintf("/tmp/ducklord-detail-offline-%d", time.Now().UnixNano())
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("link isolated SSH: %v: %s", err, out)
	}
	owner := fmt.Sprintf("detail-offline-%d", time.Now().UnixNano())
	config := "/root/.ducklord/config.yaml"
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		"ducklord", "tui", "--name", owner, "--config", config)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x1bq"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner, config)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	capture.waitCurrent(t, "alpha @client-a", 20*time.Second)
	// The Host connections form changes only this TUI's transport desire; it
	// must leave the known Session inventory available to detailed search.
	writePTY(t, terminal, "h\r")
	capture.waitCurrent(t, "Host connections", 10*time.Second)
	writePTY(t, terminal, " \r") // client-a is first in the disposable config.
	capture.waitCurrent(t, "client-a:DISCONNECTED", 10*time.Second)
	writePTY(t, terminal, "D")
	capture.waitCurrent(t, "Detailed Sessions:", 10*time.Second)
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "alpha @client-a") && strings.Contains(screen, "disconnected") && strings.Contains(screen, "[disconnected/stale]")
	}, func() string { return "known disconnected Session did not retain a stale detailed preview" })
	writePTY(t, terminal, "\r")
	time.Sleep(150 * time.Millisecond)
	if screen := capture.currentText(); strings.Contains(screen, "Session focus:") || !strings.Contains(screen, "Detailed Sessions:") {
		t.Fatal("offline detailed Enter appeared to acquire PTY control")
	}
	writePTY(t, terminal, "/alpha")
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "find › alpha") && strings.Contains(screen, "alpha @client-a") && strings.Contains(screen, "[disconnected/stale]")
	}, func() string { return "offline Session was not searchable or retained stale preview" })
	writePTY(t, terminal, "-no-such-session")
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "No matching Sessions") && !strings.Contains(screen, "alpha @client-a") && !strings.Contains(screen, "[disconnected/stale]")
	}, func() string { return "empty search retained an unrelated Session preview" })
	// Esc clears the query, then leaves search focus. It must recover the
	// disconnected preview without reconnecting or sending PTY input.
	writePTY(t, terminal, "\x1b")
	capture.waitCurrent(t, "alpha @client-a", 10*time.Second)
	if screen := capture.currentText(); !strings.Contains(screen, "[disconnected/stale]") {
		t.Fatal("clearing empty-result query did not recover offline preview")
	}
}
