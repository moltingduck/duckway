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
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// One remote Session can be displayed in two Projects, but its notification
// receipt and seen state must remain shared rather than copied per pane.
func TestDucklordSharedProjectNotificationContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" || os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on a disposable Host")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	stamp := time.Now().UnixNano()
	handle := fmt.Sprintf("shared-project-%d", stamp)
	owner := fmt.Sprintf("shared-project-owner-%d", stamp)
	config := "/root/.ducklord/config.yaml"
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "start", "client-a", "--name", handle,
		"--kind", "shell", "--cwd", "/home/duck", "--config", config, "--", "sh").CombinedOutput(); err != nil {
		t.Fatalf("start shared shell: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "destroy", "client-a", handle, "--config", config).CombinedOutput()
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
	}, func() string { return "shared Session did not appear in Ducklion inventory" })
	identity, ok := ducklord.IdentityFromSession(session)
	if !ok {
		t.Fatal("shared Session has no stable identity")
	}
	state := ducklord.NewActivityState()
	first, err := state.ProjectLayout.AddProject("Shared A")
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.ProjectLayout.AddProject("Shared B")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first, second} {
		if _, err := state.ProjectLayout.Place(id, identity, ducklord.PlaceNewTab, ""); err != nil {
			t.Fatal(err)
		}
	}
	home := fmt.Sprintf("/tmp/ducklord-shared-project-%d", stamp)
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
		t.Fatalf("copy state: %v: %s", err, out)
	}
	tuiOwner := fmt.Sprintf("shared-project-tui-%d", stamp)
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", "ducklord", "tui", "--name", tuiOwner, "--config", config)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 26, Cols: 130})
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
	capture := newSizedTUICapture(terminal, 26, 130)
	capture.waitCurrent(t, "Shared A", 20*time.Second)
	capture.waitCurrent(t, "Shared B", 20*time.Second)
	baseline, ok := findContainerSession(t, runtime, controller, "client-a", session.SessionID)
	if !ok {
		t.Fatal("shared Session disappeared before event")
	}
	baselineSeq := baseline.ActivitySequences[model.NotificationTerminalAttention]
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "send", "client-a", handle,
		"printf '\\a'", "--config", config).CombinedOutput(); err != nil {
		t.Fatalf("emit attention: %v: %s", err, out)
	}
	waitE2E(t, 10*time.Second, func() bool {
		got, ok := findContainerSession(t, runtime, controller, "client-a", session.SessionID)
		return ok && got.ActivitySequences[model.NotificationTerminalAttention] > baselineSeq
	}, func() string { return "Ducklion did not record shared Session attention" })
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "Shared A •") && strings.Contains(screen, "Shared B •")
	}, func() string { return "both Project badges did not reflect the shared unread event" })
	// Selecting the second Project must not clear unread; only its pane focus does.
	writePTY(t, terminal, "Pjj")
	capture.waitCurrent(t, "› Shared B", 10*time.Second)
	if screen := capture.currentText(); !strings.Contains(screen, "Shared A •") || !strings.Contains(screen, "Shared B •") {
		t.Fatal("Project navigation marked a shared notification seen")
	}
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 20*time.Second)
	waitE2E(t, 10*time.Second, func() bool {
		data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		if err != nil {
			return false
		}
		var saved ducklord.ActivityState
		if json.Unmarshal(data, &saved) != nil {
			return false
		}
		return !saved.Sessions[identity.Key()].Unread[model.NotificationTerminalAttention]
	}, func() string { return "focusing one Project did not clear the shared notification" })
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return !strings.Contains(screen, "Shared A •") && !strings.Contains(screen, "Shared B •")
	}, func() string { return "Project badges diverged after one shared Session focus" })
}
