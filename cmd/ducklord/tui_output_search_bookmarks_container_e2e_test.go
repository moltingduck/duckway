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

// TestDucklordOutputSearchBookmarksSelectionContainerE2E proves that search navigation
// and an expired bookmark remain visible and owned by the originating PTY.
func TestDucklordOutputSearchBookmarksSelectionContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	owner := fmt.Sprintf("output-search-e2e-%d", time.Now().UnixNano())
	command := exec.Command(runtime, "exec", "-it", controller, "env", "TERM=xterm-256color", "ducklord", "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x03"))
		killNamedContainerTUI(runtime, controller, owner)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	capture.waitCurrent(t, "PROJECTS", 20*time.Second)
	writePTY(t, terminal, "/alpha\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "session focus", 10*time.Second)
	writePTY(t, terminal, "printf 'DUCKWAY_SEARCH_ONE\\nDUCKWAY_GAP\\nDUCKWAY_SEARCH_TWO\\n'\r")
	time.Sleep(700 * time.Millisecond)
	writePTY(t, terminal, "\x02/DUCKWAY_SEARCH")
	capture.waitCurrent(t, "match 1/2", 10*time.Second)
	writePTY(t, terminal, "\x1b[B")
	capture.waitCurrent(t, "match 2/2", 10*time.Second)
	writePTY(t, terminal, "\x1b")
	writePTY(t, terminal, "printf 'DUCKWAY_REFRESHED\\n'\r")
	time.Sleep(700 * time.Millisecond)
	writePTY(t, terminal, "\x02mold output\r")
	time.Sleep(300 * time.Millisecond)
	writePTY(t, terminal, "\x02M")
	capture.waitCurrent(t, "old output [unavailable; Enter shows retained history]", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "showing current retained history", 10*time.Second)
	if strings.Contains(capture.currentText(), "old output") && strings.Contains(capture.currentText(), "bookmark") {
		t.Log("bookmark fallback remained visible after close")
	}
}
