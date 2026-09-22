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
	startShell := func(handle string, logPath string) ducklord.RemoteSession {
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
		if logPath != "" {
			program := fmt.Sprintf("export DUCKWAY_NAV_LOG=%s; : > \"$DUCKWAY_NAV_LOG\"", logPath)
			if out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "send", "client-a", handle, program, "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
				t.Fatalf("initialize originating shell log: %v (output bytes=%d)", err, len(out))
			}
			waitE2E(t, 10*time.Second, func() bool {
				out, err := exec.Command(runtime, "exec", e2eContainerName("ducklion-client-a"), "test", "-f", logPath).CombinedOutput()
				return err == nil && len(out) == 0
			}, func() string { return "originating shell log was not initialized" })
		}
		return remote
	}
	handle := fmt.Sprintf("default-close-%x", stamp)
	originLog := "/tmp/" + handle + ".input"
	remote := startShell(handle, originLog)
	identity, ok := ducklord.IdentityFromSession(remote)
	if !ok {
		t.Fatal("fixture shell has no stable identity")
	}
	state := ducklord.NewActivityState()
	originPane, err := state.ProjectLayout.Place(ducklord.DefaultProjectID, identity, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	staleHandle := fmt.Sprintf("default-stale-%x", stamp)
	staleRemote := startShell(staleHandle, "")
	staleIdentity, ok := ducklord.IdentityFromSession(staleRemote)
	if !ok {
		t.Fatal("stale fixture shell has no stable identity")
	}
	if _, err := state.ProjectLayout.Place(ducklord.DefaultProjectID, staleIdentity, ducklord.PlaceNewTab, originPane); err != nil {
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
	// Keep the search query free of lowercase `o`: direct `o` is a valid
	// Session-list route, so typing a handle containing it would open Notes
	// before the search is accepted. The unique prefix still selects handle.
	writePTY(t, terminal, "/default-cl\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 20*time.Second)
	// A focused Default Session owns Ctrl-B d, but the destructive action is
	// still guarded by a modal. Verify that the modal owns arrow input while
	// the originating PTY remains focused: Up selects the destructive choice.
	modalStart := capture.position()
	writePTY(t, terminal, "\x02d")
	capture.waitAfter(t, modalStart, "Detach Session pane", 10*time.Second)
	selectionStart := capture.position()
	writePTY(t, terminal, "\x1b[A")
	waitE2E(t, 10*time.Second, func() bool {
		return strings.Contains(capture.since(selectionStart), "› Detach local pane")
	}, func() string {
		return "detach confirmation did not own the Up arrow: " + safeTerminalDiagnostic(capture.currentText())
	})
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "destroy", "client-a", staleHandle,
		"--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("remove stale session during detach: %v (output bytes=%d)", err, len(out))
	}
	waitE2E(t, 15*time.Second, func() bool {
		_, found := findContainerSession(t, runtime, controller, "client-a", staleRemote.SessionID)
		return !found
	}, func() string { return "stale session remained in remote inventory during detach" })
	restoreStart := capture.position()
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool {
		render := capture.since(restoreStart)
		return len(render) > 0 && !strings.Contains(capture.currentText(), "Detach Session pane")
	}, func() string {
		return "Esc did not produce a fresh render after closing the detach modal: " + safeTerminalDiagnostic(capture.currentText())
	})
	restoreMarker := fmt.Sprintf("DEFAULT_DETACH_RESTORE_%x", stamp)
	markerStart := capture.position()
	writePTY(t, terminal, fmt.Sprintf("printf '%s\\n' '%s' | tee -a \"$DUCKWAY_NAV_LOG\"\r", restoreMarker, restoreMarker))
	waitE2E(t, 10*time.Second, func() bool {
		return strings.Contains(capture.since(markerStart), restoreMarker)
	}, func() string {
		return "Esc did not restore the original focused PTY: " + safeTerminalDiagnostic(capture.currentText())
	})
	waitE2E(t, 10*time.Second, func() bool {
		out, err := exec.Command(runtime, "exec", e2eContainerName("ducklion-client-a"), "cat", originLog).Output()
		return err == nil && strings.Contains(string(out), restoreMarker)
	}, func() string { return "restore marker was not written by the originating shell" })

	// Reopen the same focused-terminal route and explicitly select Detach.
	confirmStart := capture.position()
	writePTY(t, terminal, "\x02d")
	capture.waitAfter(t, confirmStart, "Detach Session pane", 10*time.Second)
	writePTY(t, terminal, "\x1b[A\r")
	waitE2E(t, 10*time.Second, func() bool { return isClosed(loadState()) }, func() string {
		return "confirmed Default pane closure was not persisted"
	})

	// A newly discovered shell must be placed and saved, proving reconciliation
	// ran after closure instead of merely checking an unchanged state file.
	newRemote := startShell(fmt.Sprintf("default-discover-%x", stamp), "")
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
	writePTY(t, terminal, "/default-cl")
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "find › default-cl") && strings.Contains(screen, "client-a/"+handle)
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
