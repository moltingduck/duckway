package main

import (
	"encoding/json"
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

// Shell End must make its distinct effect clear before confirmation. Cancel
// leaves the process alone; confirmation removes the live pane but preserves
// separately retrievable diagnostic PTY output.
func TestDucklordShellEndModalContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" || os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on a disposable Host")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	stamp := time.Now().UnixNano()
	handle := fmt.Sprintf("end-modal-%x", stamp&0xffffff)
	marker := "RETAINED_END_MODAL_" + handle
	cliOwner := fmt.Sprintf("end-modal-cli-%d", stamp)
	config := "/root/.ducklord/config.yaml"
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", cliOwner, "start", "client-a", "--name", handle,
		"--kind", "shell", "--cwd", "/home/duck", "--config", config, "--", "sh").CombinedOutput(); err != nil {
		t.Fatalf("start shell: %v (output bytes=%d)", err, len(out))
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", cliOwner, "destroy", "client-a", handle, "--config", config).CombinedOutput()
	})
	var session ducklord.RemoteSession
	waitE2E(t, 15*time.Second, func() bool {
		for _, item := range listContainerSessions(t, runtime, controller, "client-a") {
			if item.Handle == handle {
				var ok bool
				session, ok = findContainerSession(t, runtime, controller, "client-a", item.SessionID)
				return ok && session.RuntimeGeneration > 0
			}
		}
		return false
	}, func() string { return "Shell End fixture did not enter inventory" })
	identity, ok := ducklord.IdentityFromSession(session)
	if !ok {
		t.Fatal("fixture has no stable Session identity")
	}
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", cliOwner, "send", "client-a", handle,
		"printf '"+marker+"\\n'", "--config", config).CombinedOutput(); err != nil {
		t.Fatalf("seed retained log marker: %v (output bytes=%d)", err, len(out))
	}
	waitE2E(t, 10*time.Second, func() bool {
		out, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", cliOwner, "read", "client-a", handle,
			"--lines", "30", "--config", config).Output()
		return err == nil && strings.Contains(string(out), marker)
	}, func() string { return "retained log marker did not reach Shell PTY" })
	state := ducklord.NewActivityState()
	projectID, err := state.ProjectLayout.AddProject("End Fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	home := fmt.Sprintf("/tmp/ducklord-end-modal-%d", stamp)
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("link SSH: %v: %s", err, out)
	}
	localState := filepath.Join(t.TempDir(), "state.json")
	if err := (ducklord.ActivityStateStore{Path: localState}).Save(state); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(runtime, "cp", localState, controller+":"+home+"/.ducklord/state.json").CombinedOutput(); err != nil {
		t.Fatalf("copy Project state: %v: %s", err, out)
	}
	tuiOwner := fmt.Sprintf("end-modal-tui-%d", stamp)
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		"ducklord", "tui", "--name", tuiOwner, "--config", config)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x1bq"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, tuiOwner, config)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	capture.waitCurrent(t, "End Fixture", 20*time.Second)
	writePTY(t, terminal, "/"+handle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\x1b") // close search without focusing the PTY
	waitE2E(t, 10*time.Second, func() bool { return !strings.Contains(capture.currentText(), "search ›") },
		func() string { return "search modal did not close before Shell End" })
	writePTY(t, terminal, "E")
	capture.waitCurrent(t, "END SESSION", 10*time.Second)
	capture.waitCurrent(t, "Shell and Session pane disappear; retained PTY logs remain.", 10*time.Second)
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return !strings.Contains(screen, "END SESSION") && strings.Contains(screen, handle)
	}, func() string { return "cancel did not close End confirmation" })
	if current, ok := findContainerSession(t, runtime, controller, "client-a", session.SessionID); !ok || current.Status != "running" {
		t.Fatalf("Cancel affected remote Shell: found=%t status=%s", ok, current.Status)
	}
	writePTY(t, terminal, "E")
	capture.waitCurrent(t, "END SESSION", 10*time.Second)
	writePTY(t, terminal, "\r")
	waitE2E(t, 20*time.Second, func() bool {
		for _, item := range listContainerSessions(t, runtime, controller, "client-a") {
			if item.SessionID == session.SessionID {
				return false
			}
		}
		return true
	}, func() string { return "confirmed Shell End left Session selectable" })
	waitE2E(t, 15*time.Second, func() bool {
		data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		if err != nil {
			return false
		}
		var current ducklord.ActivityState
		return json.Unmarshal(data, &current) == nil && len(current.ProjectLayout.ProjectsFor(identity)) == 0
	}, func() string { return "confirmed End left a ghost Session pane in Project layout" })
	read, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", cliOwner, "read-retained", "client-a", session.SessionID,
		fmt.Sprint(session.RuntimeGeneration), "--lines", "30", "--config", config).CombinedOutput()
	if err != nil || !strings.Contains(string(read), marker) {
		t.Fatalf("confirmed End lost retained PTY output: err=%v bytes=%d", err, len(read))
	}
}
