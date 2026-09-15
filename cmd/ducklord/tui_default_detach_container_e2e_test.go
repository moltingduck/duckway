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

// Exercise local Default closure through the actual TUI, including fresh
// inventory reconciliation, process restart, read-only preview, and explicit jump.
func TestDucklordDefaultDetachContainerE2E(t *testing.T) {
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
	cliOwner := fmt.Sprintf("default-detach-cli-%d", stamp)
	startShell := func(handle string) ducklord.RemoteSession {
		t.Helper()
		out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "start", "client-a", "--name", handle,
			"--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "bash").CombinedOutput()
		if err != nil {
			t.Fatalf("start fixture shell: %v (output bytes=%d)", err, len(out))
		}
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "destroy", "client-a", handle,
				"--config", "/root/.ducklord/config.yaml").CombinedOutput()
		})
		var remote ducklord.RemoteSession
		waitE2E(t, 15*time.Second, func() bool {
			for _, session := range listContainerRemoteSessions(t, runtime, controller, "client-a") {
				if session.Name == handle && session.RuntimeGeneration > 0 {
					remote = session
					return true
				}
			}
			return false
		}, func() string { return "fixture shell not discovered" })
		return remote
	}
	handle := fmt.Sprintf("default-close-%x", stamp)
	remote := startShell(handle)
	identity, ok := ducklord.IdentityFromSession(remote)
	if !ok {
		t.Fatal("fixture shell has no stable identity")
	}
	state := ducklord.NewActivityState()
	if _, err := state.ProjectLayout.Place(ducklord.DefaultProjectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	home := fmt.Sprintf("/tmp/ducklord-default-detach-%d-%d", os.Getpid(), stamp)
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
	loadState := func() *ducklord.ActivityState {
		t.Helper()
		data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		if err != nil {
			t.Fatal("read persisted layout: ", err)
		}
		var persisted ducklord.ActivityState
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Fatal("decode persisted layout: ", err)
		}
		return &persisted
	}
	isClosed := func(persisted *ducklord.ActivityState) bool {
		// Default membership survives local closure; only the visible pane is
		// removed. Check the persisted tree as well as suppression so a stale
		// pane cannot pass merely because a suppression entry was written.
		projects := persisted.ProjectLayout.ProjectsFor(identity)
		if len(projects) != 1 || projects[0] != ducklord.DefaultProjectID {
			return false
		}
		for _, project := range persisted.ProjectLayout.Projects {
			for _, tab := range project.Tabs {
				for _, paneIdentity := range persisted.ProjectLayout.TabSessions(project.ID, tab.ID) {
					if paneIdentity == identity {
						return false
					}
				}
			}
		}
		for _, suppressed := range persisted.ProjectLayout.SuppressedDefaultSessions {
			if suppressed == identity {
				return true
			}
		}
		return false
	}
	launch := func(suffix string) (*os.File, *tuiCapture, func()) {
		t.Helper()
		owner := fmt.Sprintf("default-detach-tui-%d-%s", stamp, suffix)
		command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
			binary, "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
		terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
		if err != nil {
			t.Fatal(err)
		}
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			_, _ = terminal.Write([]byte("\x1dq"))
			time.Sleep(200 * time.Millisecond)
			killNamedContainerTUI(runtime, controller, owner, "/root/.ducklord/config.yaml")
			_ = terminal.Close()
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		t.Cleanup(stop)
		capture := newSizedTUICapture(terminal, 28, 130)
		waitLiveAgentScreen(t, capture, "Session list pane:", 20*time.Second)
		return terminal, capture, stop
	}
	terminal, capture, stop := launch("first")
	// Startup follows the quick-list selection, which may be a demo Session
	// even though the fixture was seeded as Default's first tab.
	writePTY(t, terminal, "/"+handle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 20*time.Second)
	writePTY(t, terminal, "\x1d")
	capture.waitCurrent(t, "Session list pane:", 10*time.Second)
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Project pane:", 10*time.Second)
	writePTY(t, terminal, "x")
	capture.waitCurrent(t, "Detach Session pane", 10*time.Second)
	writePTY(t, terminal, "\x1b[A\r")
	waitE2E(t, 10*time.Second, func() bool { return isClosed(loadState()) }, func() string {
		return "confirmed Default pane closure was not persisted"
	})

	// A newly discovered shell must be placed and saved, proving reconciliation
	// ran after closure instead of merely checking an unchanged state file.
	newRemote := startShell(fmt.Sprintf("default-discover-%x", stamp))
	newIdentity, ok := ducklord.IdentityFromSession(newRemote)
	if !ok {
		t.Fatal("rediscovery fixture has no stable identity")
	}
	writePTY(t, terminal, "r")
	waitE2E(t, 20*time.Second, func() bool {
		persisted := loadState()
		projects := persisted.ProjectLayout.ProjectsFor(newIdentity)
		return isClosed(persisted) && len(projects) == 1 && projects[0] == ducklord.DefaultProjectID
	}, func() string { return "rediscovery did not preserve the closed Default pane" })
	stop()
	terminal, capture, _ = launch("reload")
	writePTY(t, terminal, "l")
	capture.waitCurrent(t, "Detailed Sessions:", 10*time.Second)
	writePTY(t, terminal, "/"+handle)
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "find › "+handle) && strings.Contains(screen, "client-a/"+handle)
	}, func() string { return "reloaded TUI did not rediscover the closed Session" })
	writePTY(t, terminal, "\r") // Search acceptance remains a read-only preview.
	if !isClosed(loadState()) {
		t.Fatal("restart or detailed preview reopened the closed Default pane")
	}
	latest, found := findContainerSession(t, runtime, controller, "client-a", remote.SessionID)
	latestIdentity, valid := ducklord.IdentityFromSession(latest)
	if !found || !valid || latestIdentity != identity || latest.RuntimeGeneration != remote.RuntimeGeneration {
		t.Fatal("local closure or reload changed remote Session identity/runtime")
	}
	assertContainerShellPWD(t, runtime, controller, binary, cliOwner, "client-a", handle, "/home/duck")
	writePTY(t, terminal, "g")
	waitE2E(t, 20*time.Second, func() bool {
		persisted := loadState()
		projects := persisted.ProjectLayout.ProjectsFor(identity)
		for _, suppressed := range persisted.ProjectLayout.SuppressedDefaultSessions {
			if suppressed == identity {
				return false
			}
		}
		return len(projects) == 1 && projects[0] == ducklord.DefaultProjectID && strings.Contains(capture.currentText(), "Session focus:")
	}, func() string { return "explicit detailed jump did not reopen and persist the Default pane" })
	latest, found = findContainerSession(t, runtime, controller, "client-a", remote.SessionID)
	latestIdentity, valid = ducklord.IdentityFromSession(latest)
	if !found || !valid || latestIdentity != identity || latest.RuntimeGeneration != remote.RuntimeGeneration {
		t.Fatal("reopening changed remote Session identity/runtime")
	}
}
