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

// Deleting a Project is local layout work: cancellation must be inert, and
// confirmation must rehome its last pane without touching the remote shell.
func TestDucklordProjectDeleteContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	binary := os.Getenv("DUCKLORD_E2E_BINARY")
	if binary == "" {
		binary = "ducklord"
	}
	stamp := time.Now().UnixNano()
	handle := fmt.Sprintf("project-delete-%x", stamp&0xffffff)
	cliOwner := fmt.Sprintf("project-delete-cli-%d", stamp)
	out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "start", "client-a", "--name", handle,
		"--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "bash").CombinedOutput()
	if err != nil {
		t.Fatalf("start remote shell: %v (output bytes=%d)", err, len(out))
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "destroy", "client-a", handle,
			"--config", "/root/.ducklord/config.yaml").CombinedOutput()
	})
	var remote ducklord.RemoteSession
	waitE2E(t, 15*time.Second, func() bool {
		for _, item := range listContainerSessions(t, runtime, controller, "client-a") {
			if item.Handle == handle {
				var ok bool
				remote, ok = findContainerSession(t, runtime, controller, "client-a", item.SessionID)
				return ok && remote.RuntimeGeneration > 0
			}
		}
		return false
	}, func() string { return "project delete fixture shell not listed" })
	identity, ok := ducklord.IdentityFromSession(remote)
	if !ok {
		t.Fatal("fixture shell has no stable identity")
	}
	state := ducklord.NewActivityState()
	projectName := "Delete Fixture"
	projectID, err := state.ProjectLayout.AddProject(projectName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	home := fmt.Sprintf("/tmp/ducklord-project-delete-%d-%d", os.Getpid(), stamp)
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
		t.Fatalf("install isolated Project layout: %v (output bytes=%d)", err, len(out))
	}
	owner := fmt.Sprintf("project-delete-tui-%d", stamp)
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		binary, "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x03"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner, "/root/.ducklord/config.yaml")
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 24, 120)
	capture.waitCurrent(t, projectName, 20*time.Second)
	writePTY(t, terminal, "bj")
	capture.waitCurrent(t, "› "+projectName, 10*time.Second)
	writePTY(t, terminal, "Z")
	capture.waitCurrent(t, "Delete Project", 10*time.Second)
	writePTY(t, terminal, "\r") // Cancel is the safe default.
	if data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output(); err != nil {
		t.Fatal("read canceled Project state: ", err)
	} else {
		var persisted ducklord.ActivityState
		if json.Unmarshal(data, &persisted) != nil || persisted.ProjectLayout.Project(projectID) == nil {
			t.Fatal("Cancel deleted the Project")
		}
	}
	writePTY(t, terminal, "i")
	capture.waitCurrent(t, "Focus: "+projectName, 10*time.Second)
	writePTY(t, terminal, "Z")
	capture.waitCurrent(t, "Delete Project", 10*time.Second)
	writePTY(t, terminal, "\x1b[A\r") // Explicitly confirm local-only deletion.
	defaultTabCount := 0
	waitE2E(t, 15*time.Second, func() bool {
		data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		if err != nil {
			return false
		}
		var persisted ducklord.ActivityState
		if json.Unmarshal(data, &persisted) != nil {
			return false
		}
		projects := persisted.ProjectLayout.ProjectsFor(identity)
		defaultProject := persisted.ProjectLayout.Project(ducklord.DefaultProjectID)
		if defaultProject == nil {
			return false
		}
		defaultTabCount = len(defaultProject.Tabs)
		return persisted.ProjectLayout.Project(projectID) == nil && len(projects) == 1 && projects[0] == ducklord.DefaultProjectID
	}, func() string { return "confirmed Project deletion did not rehome its pane to Default" })
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "› Default Project") && !strings.Contains(screen, "Focus: "+projectName)
	}, func() string { return "deleted Project remained selected or notification focus was not cleared" })
	if latest, found := findContainerSession(t, runtime, controller, "client-a", remote.SessionID); !found || latest.RuntimeGeneration != remote.RuntimeGeneration {
		t.Fatal("local Project deletion changed the remote shell identity")
	}
	// Default also contains the demo's other Sessions. Navigate its tabs to
	// the rehomed fixture before checking the live framebuffer.
	for tab := 0; tab < defaultTabCount && !strings.Contains(capture.currentText(), "client-a/"+handle); tab++ {
		writePTY(t, terminal, "\x1b[6~")
		time.Sleep(150 * time.Millisecond)
	}
	capture.waitCurrent(t, "client-a/"+handle, 10*time.Second)
	marker := fmt.Sprintf("PROJECTDELETEALIVE-%d", stamp)
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "send", "client-a", handle,
		"printf '"+marker+"\\n'", "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("send to shell after local deletion: %v (output bytes=%d)", err, len(out))
	}
	capture.waitCurrent(t, marker, 15*time.Second)
}
