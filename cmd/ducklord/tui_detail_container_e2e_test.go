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

// TestDucklordDetailedListContainerE2E exercises the production (ungated)
// two-column view against a real Ducklion shell and owner-gated PTY input.
func TestDucklordDetailedListContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	var alphaID string
	for _, session := range listContainerSessions(t, runtime, controller, "client-a") {
		if session.Handle == "alpha" {
			alphaID = session.SessionID
		}
	}
	if alphaID == "" {
		t.Fatal("demo alpha shell is missing")
	}
	home := fmt.Sprintf("/tmp/ducklord-detail-e2e-%d", time.Now().UnixNano())
	if err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").Run(); err != nil {
		t.Fatal(err)
	}
	owner := fmt.Sprintf("detail-e2e-%d", time.Now().UnixNano())
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		"ducklord", "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1dq"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	waitLiveAgentScreen(t, capture, "Session list pane:", 20*time.Second)
	writePTY(t, terminal, "t")
	waitLiveAgentScreen(t, capture, "sort:event_importance", 10*time.Second)
	writePTY(t, terminal, "t")
	waitLiveAgentScreen(t, capture, "sort:host", 10*time.Second)
	writePTY(t, terminal, "t")
	waitLiveAgentScreen(t, capture, "sort:type", 10*time.Second)
	writePTY(t, terminal, "t")
	waitLiveAgentScreen(t, capture, "sort:event_time", 10*time.Second)
	writePTY(t, terminal, "\x14")
	waitLiveAgentScreen(t, capture, "sort:event_time (oldest)", 10*time.Second)
	writePTY(t, terminal, "b")
	waitLiveAgentScreen(t, capture, "Project pane:", 10*time.Second)
	workspaceHeading := func(screen string) string {
		for _, line := range strings.Split(screen, "\n") {
			if strings.Contains(line, "PROJECTS") {
				return line // Includes the current Project and selected Terminal tab.
			}
		}
		return ""
	}
	var beforeDetail string
	waitE2E(t, 10*time.Second, func() bool {
		beforeDetail = workspaceHeading(capture.currentText())
		return beforeDetail != ""
	}, func() string { return "normal workspace heading was missing before detailed mode" })
	for _, exitKey := range []string{"l", "\x1b"} {
		writePTY(t, terminal, "l")
		waitLiveAgentScreen(t, capture, "Detailed Sessions:", 10*time.Second)
		writePTY(t, terminal, exitKey)
		waitE2E(t, 10*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "Project pane:") && !strings.Contains(screen, "Detailed Sessions:") && workspaceHeading(screen) == beforeDetail
		}, func() string {
			return "leaving detailed mode did not restore Project keyboard focus and the original Project/tab"
		})
	}
	writePTY(t, terminal, "i")
	waitLiveAgentScreen(t, capture, "Focus:", 10*time.Second)
	writePTY(t, terminal, "i")
	writePTY(t, terminal, "b")
	waitLiveAgentScreen(t, capture, "Session list pane:", 10*time.Second)
	writePTY(t, terminal, "l")
	waitLiveAgentScreen(t, capture, "Detailed Sessions:", 10*time.Second)
	writePTY(t, terminal, "/alpha")
	waitE2E(t, 10*time.Second, func() bool {
		text := capture.currentText()
		return strings.Contains(text, "find › alpha") && strings.Contains(text, "client-a/alpha")
	}, func() string { return "detailed fuzzy search did not preview alpha" })
	writePTY(t, terminal, "\r") // leave search field; preview is still read-only
	writePTY(t, terminal, "\r") // focus selected shell through Ducklion
	waitE2E(t, 20*time.Second, func() bool { return strings.Contains(capture.currentText(), "Session focus:") }, func() string {
		screen := capture.currentText()
		return fmt.Sprintf("detailed focus did not open (detail=%t project=%t pending=%t selected-unavailable=%t pane-changed=%t disconnected=%t readonly=%t focus-label=%t); PTY output suppressed",
			strings.Contains(screen, "Detailed Sessions:"), strings.Contains(screen, "Project pane:"), strings.Contains(screen, "opening PTY"),
			strings.Contains(screen, "selected detailed Session"), strings.Contains(screen, "Session pane or writer changed"),
			strings.Contains(screen, "control disconnected"), strings.Contains(screen, "read-only"), strings.Contains(screen, "Session focus"))
	})
	marker := fmt.Sprintf("DETAIL_E2E_%d", time.Now().UnixNano())
	writePTY(t, terminal, "printf '"+marker+"\\n'\r")
	waitLiveAgentScreen(t, capture, marker, 10*time.Second)
	output, err := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", alphaID,
		"--lines", "80", "--config", "/tmp/e2e-inspector.yaml").Output()
	if err != nil || !strings.Contains(string(output), marker) {
		t.Fatal("detailed pane input did not reach the original Ducklion session; remote output suppressed")
	}
	writePTY(t, terminal, "\x1d")
	waitLiveAgentScreen(t, capture, "Detailed Sessions:", 10*time.Second)
	writePTY(t, terminal, "g")
	waitE2E(t, 20*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "Session focus:") && strings.Contains(screen, "PROJECTS") && !strings.Contains(screen, "Detailed Sessions:")
	}, func() string { return "g did not jump to the normal Project Terminal area and focus its Session pane" })
}
