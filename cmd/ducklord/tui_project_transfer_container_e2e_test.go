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

// This drives the complete Project transfer route through a real Ducklord
// process in the Podman fixture. The file is deliberately kept outside the
// isolated HOME so the test also proves the path field is honored.
func TestDucklordProjectTransferContainerE2E(t *testing.T) {
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
	cliOwner := fmt.Sprintf("project-transfer-cli-%d", stamp)
	handle := fmt.Sprintf("project-transfer-%x", stamp&0xffffff)
	out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "start", "client-a", "--name", handle,
		"--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "bash").CombinedOutput()
	if err != nil {
		t.Fatalf("start transfer fixture shell: %v (output bytes=%d)", err, len(out))
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
	}, func() string { return "transfer fixture shell not listed" })
	identity, ok := ducklord.IdentityFromSession(remote)
	if !ok {
		t.Fatal("transfer fixture shell has no stable identity")
	}

	state := ducklord.NewActivityState()
	projectName := "Transfer Fixture"
	projectID, err := state.ProjectLayout.AddProject(projectName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	home := fmt.Sprintf("/tmp/ducklord-project-transfer-%d-%d", os.Getpid(), stamp)
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated transfer HOME: %v (output bytes=%d)", err, len(out))
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
		t.Fatalf("install transfer layout: %v (output bytes=%d)", err, len(out))
	}

	owner := fmt.Sprintf("project-transfer-tui-%d", stamp)
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		binary, "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 30, Cols: 140})
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
	capture := newSizedTUICapture(terminal, 30, 140)
	capture.waitCurrent(t, projectName, 20*time.Second)
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Project pane:", 10*time.Second)
	writePTY(t, terminal, "j")
	capture.waitCurrent(t, "› "+projectName, 10*time.Second)

	geometry := ducklord.CalculateWorkspaceGeometry(140, 30, 4)
	openProjectConfig := func() {
		t.Helper()
		// A click establishes the Project-pane context; Ctrl-B+c is the same
		// prefix route used by normal keyboard users.
		writePTY(t, terminal, string(workspaceMouse(0, geometry.Projects.X, geometry.Projects.Y, false))+string(workspaceMouse(0, geometry.Projects.X, geometry.Projects.Y, true)))
		writePTY(t, terminal, "\x02c")
		capture.waitCurrent(t, "Project pane config", 10*time.Second)
	}

	transferPath := home + "/project-transfer.json"
	openProjectConfig()
	writePTY(t, terminal, "jjj\r") // Export Project.
	capture.waitCurrent(t, "Export Project", 10*time.Second)
	if out, err := exec.Command(runtime, "exec", controller, "test", "!", "-e", transferPath).CombinedOutput(); err != nil {
		t.Fatalf("export was written before explicit confirmation: %s", out)
	}
	writePTY(t, terminal, transferPath+"\r")
	// The path is entered in the path step; the confirmation must follow it.
	capture.waitCurrent(t, "Confirm Project Export", 10*time.Second)
	writePTY(t, terminal, "\r")
	waitE2E(t, 15*time.Second, func() bool {
		_, err := exec.Command(runtime, "exec", controller, "test", "-s", transferPath).CombinedOutput()
		return err == nil
	}, func() string { return "confirmed export did not create the requested file" })
	if out, err := exec.Command(runtime, "exec", controller, "grep", "-F", handle, transferPath).CombinedOutput(); err == nil {
		t.Fatalf("export included live terminal output: %s", out)
	}
	capture.waitCurrent(t, projectName, 10*time.Second)

	openProjectConfig()
	writePTY(t, terminal, "jjjj\r") // Import Project.
	capture.waitCurrent(t, "Import Project", 10*time.Second)
	writePTY(t, terminal, transferPath+"\r")
	capture.waitCurrent(t, "Project Import Preview", 10*time.Second)
	preview := capture.currentText()
	if !strings.Contains(preview, "Name collision") || !strings.Contains(preview, "Import and persist") {
		t.Fatalf("import preview omitted collision or confirmation: %s", safeTerminalDiagnostic(preview))
	}
	before, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
	if err != nil || strings.Contains(string(before), projectName+" (imported)") {
		t.Fatal("import mutated state before explicit confirmation")
	}
	writePTY(t, terminal, "\r")
	waitE2E(t, 15*time.Second, func() bool {
		data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		return err == nil && strings.Contains(string(data), projectName+" (imported)")
	}, func() string {
		data, _ := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		return "confirmed import did not persist deterministic collision name: " + string(data) + "\nTUI: " + safeTerminalDiagnostic(capture.currentText())
	})
	// Closing the modal restores Project-pane ownership: j must move the
	// project selection rather than becoming terminal input.
	writePTY(t, terminal, "j")
	capture.waitCurrent(t, projectName+" (imported)", 10*time.Second)
	var persisted ducklord.ActivityState
	data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
	if err != nil || json.Unmarshal(data, &persisted) != nil || persisted.ProjectLayout.Project(projectID) == nil {
		t.Fatal("original Project disappeared during transfer")
	}
}
