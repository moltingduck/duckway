package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestDucklordHostRetentionContainerE2E drives the real TUI, SSH bridge, and
// Ducklion host configuration. The fixture contains no live agent credentials.
func TestDucklordHostRetentionContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	home := fmt.Sprintf("/tmp/ducklord-retention-e2e-%d", time.Now().UnixNano())
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("prepare SSH config: %v: %s", err, out)
	}
	owner := fmt.Sprintf("retention-e2e-%d", time.Now().UnixNano())
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		"ducklord", "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
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
	writePTY(t, terminal, "\r") // select client-a from the Host list.
	capture.waitCurrent(t, "Host actions · client-a", 10*time.Second)
	writePTY(t, terminal, "jj\r") // Host PTY log retention is action index 2.
	capture.waitCurrent(t, "Current: 7 days", 20*time.Second)
	writePTY(t, terminal, "8\r")
	capture.waitCurrent(t, "Change 7 → 8 days?", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Saved: 8 days", 20*time.Second)
	readDays := func() (int, bool) {
		output, readErr := exec.Command(runtime, "exec", "-u", "duck", e2eContainerName("ducklion-client-a"), "sh", "-lc",
			"cat \"$HOME/.duckway/ducklion/host-settings.json\"").Output()
		if readErr != nil {
			return 0, false
		}
		var setting struct {
			Days int `json:"pty_log_retention_days"`
		}
		if json.Unmarshal(output, &setting) != nil {
			return 0, false
		}
		return setting.Days, true
	}
	waitDays := func(want int) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if days, ok := readDays(); ok && days == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		days, ok := readDays()
		t.Fatalf("Ducklion did not persist %d retention days: days=%d readable=%v", want, days, ok)
	}
	waitDays(8)
	writePTY(t, terminal, "\r") // close the persistent success confirmation
	// Restore the fixture through the same TUI path so the shared host starts
	// subsequent cases with its seven-day default.
	writePTY(t, terminal, "h")
	capture.waitCurrent(t, "Choose a host to view its settings", 10*time.Second)
	capture.waitCurrent(t, "› client-a · connected", 10*time.Second)
	writePTY(t, terminal, "\r") // select client-a from the Host list.
	capture.waitCurrent(t, "Host actions · client-a", 10*time.Second)
	writePTY(t, terminal, "jj\r") // Host PTY log retention is action index 2.
	capture.waitCurrent(t, "Current: 8 days", 20*time.Second)
	writePTY(t, terminal, "7\r")
	capture.waitCurrent(t, "Change 8 → 7 days?", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Saved: 7 days", 20*time.Second)
	waitDays(7)
}
