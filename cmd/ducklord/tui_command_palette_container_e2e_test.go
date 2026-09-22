package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// Verify that the command palette owns both navigation and query input, then
// restores the exact focused PTY when it closes.
func TestDucklordCommandPaletteContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" || os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on a disposable Host")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	binary := os.Getenv("DUCKLORD_E2E_BINARY")
	if binary == "" {
		binary = "ducklord"
	}
	const config = "/root/.ducklord/config.yaml"
	stamp := time.Now().UnixNano()
	owner := fmt.Sprintf("command-palette-%d", stamp)
	handle := fmt.Sprintf("palette-%x", stamp)
	logPath := "/tmp/" + handle + ".input"
	queryMarker := fmt.Sprintf("PALETTE_QUERY_%x", stamp)
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "start", "client-a", "--name", handle, "--kind", "shell", "--cwd", "/home/duck", "--config", config, "--", "bash").CombinedOutput(); err != nil {
		t.Fatalf("start fixture shell: %v (output bytes=%d)", err, len(out))
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "destroy", "client-a", handle, "--config", config).CombinedOutput()
		_, _ = exec.Command(runtime, "exec", e2eContainerName("ducklion-client-a"), "rm", "-f", logPath).CombinedOutput()
	})
	waitE2E(t, 15*time.Second, func() bool {
		for _, session := range listContainerRemoteSessions(t, runtime, controller, "client-a") {
			if session.Name == handle && session.RuntimeGeneration > 0 {
				return true
			}
		}
		return false
	}, func() string { return "fixture shell not discovered" })
	program := fmt.Sprintf("export DUCKWAY_NAV_LOG=%s; : > \"$DUCKWAY_NAV_LOG\"", logPath)
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "send", "client-a", handle, program, "--config", config).CombinedOutput(); err != nil {
		t.Fatalf("initialize shell log: %v (output bytes=%d)", err, len(out))
	}
	waitE2E(t, 10*time.Second, func() bool {
		out, err := exec.Command(runtime, "exec", e2eContainerName("ducklion-client-a"), "test", "-f", logPath).CombinedOutput()
		return err == nil && len(out) == 0
	}, func() string { return "shell log was not initialized" })
	var remote ducklord.RemoteSession
	for _, session := range listContainerRemoteSessions(t, runtime, controller, "client-a") {
		if session.Name == handle {
			remote = session
			break
		}
	}
	identity, ok := ducklord.IdentityFromSession(remote)
	if !ok {
		t.Fatal("fixture shell has no stable identity")
	}
	state := ducklord.NewActivityState()
	if _, err := state.ProjectLayout.Place(ducklord.DefaultProjectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	home := fmt.Sprintf("/tmp/ducklord-%s", owner)
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v (output bytes=%d)", err, len(out))
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("link isolated SSH: %v (output bytes=%d)", err, len(out))
	}
	localState := filepath.Join(t.TempDir(), "state.json")
	if err := (ducklord.ActivityStateStore{Path: localState}).Save(state); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(runtime, "cp", localState, controller+":"+home+"/.ducklord/state.json").CombinedOutput(); err != nil {
		t.Fatalf("install isolated layout: %v (output bytes=%d)", err, len(out))
	}
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", binary, "tui", "--name", owner, "--config", config)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1dq"))
		killNamedContainerTUI(runtime, controller, owner, config)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	capture.waitCurrent(t, "Session list pane:", 20*time.Second)
	writePTY(t, terminal, "/"+handle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus: keys go to PTY", 20*time.Second)
	paletteStart := capture.position()
	writePTY(t, terminal, "\x02 ")
	capture.waitAfter(t, paletteStart, "Command palette", 10*time.Second)
	writePTY(t, terminal, "\x1b[B")
	waitE2E(t, 10*time.Second, func() bool { return strings.Contains(capture.currentText(), "Command palette") }, func() string { return "command palette did not retain arrow-key ownership" })
	query := fmt.Sprintf("printf '%s\\n' >> \"$DUCKWAY_NAV_LOG\"", queryMarker)
	writePTY(t, terminal, query)
	capture.waitCurrent(t, queryMarker, 10*time.Second)
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool { return !strings.Contains(capture.currentText(), "Command palette") }, func() string {
		return "Esc did not close command palette: " + safeTerminalDiagnostic(capture.currentText())
	})
	// Flush a possible leaked command line. If the modal owned the query, this is an empty shell line.
	writePTY(t, terminal, "\r")
	readLog := func() string {
		out, err := exec.Command(runtime, "exec", e2eContainerName("ducklion-client-a"), "cat", logPath).Output()
		if err != nil {
			t.Fatalf("read native shell log: %v", err)
		}
		return string(out)
	}
	waitE2E(t, 10*time.Second, func() bool { return readLog() == "" }, func() string { return "command palette query reached the shell: " + readLog() })
	restoreMarker := fmt.Sprintf("PALETTE_RESTORE_%x", stamp)
	markerStart := capture.position()
	writePTY(t, terminal, fmt.Sprintf("printf '%s\\n' '%s' | tee -a \"$DUCKWAY_NAV_LOG\"\r", restoreMarker, restoreMarker))
	waitE2E(t, 10*time.Second, func() bool { return strings.Contains(capture.since(markerStart), restoreMarker) }, func() string {
		return "Esc did not restore the focused PTY: " + safeTerminalDiagnostic(capture.currentText())
	})
	waitE2E(t, 10*time.Second, func() bool { return readLog() == restoreMarker+"\n" }, func() string { return "restore marker was not written by the focused shell: " + readLog() })
}
