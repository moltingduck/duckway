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

// Each shell inherits a different log path, so the native files prove which
// shell executed input after a prefix handoff, including accidental broadcasts.
func TestDucklordPrefixNavigationContainerE2E(t *testing.T) {
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
	owner := fmt.Sprintf("prefix-nav-%d", stamp)
	state := ducklord.NewActivityState()
	projectID, err := state.ProjectLayout.AddProject("Prefix navigation")
	if err != nil {
		t.Fatal(err)
	}
	handles, logs := make([]string, 3), make([]string, 3)
	firstPane := ""
	for i := range handles {
		handles[i] = fmt.Sprintf("nav%d-%x", i, stamp)
		logs[i] = "/tmp/" + handles[i] + ".input"
		handle, log := handles[i], logs[i]
		program := "export DUCKWAY_NAV_LOG=" + log + "; : > \"$DUCKWAY_NAV_LOG\""
		if out, err := exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "start", "client-a", "--name", handle,
			"--kind", "shell", "--cwd", "/home/duck", "--config", config, "--", "bash").CombinedOutput(); err != nil {
			t.Fatalf("start navigation shell: %v: %s", err, out)
		}
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "destroy", "client-a", handle, "--config", config).CombinedOutput()
			_, _ = exec.Command(runtime, "exec", "ducklion-client-a", "rm", "-f", log).CombinedOutput()
		})
		if out, err := exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "send", "client-a", handle, program, "--config", config).CombinedOutput(); err != nil {
			t.Fatalf("initialize navigation shell: %v: %s", err, out)
		}
		var identity ducklord.SessionIdentity
		waitE2E(t, 10*time.Second, func() bool {
			for _, summary := range listContainerSessions(t, runtime, controller, "client-a") {
				if summary.Handle == handle {
					remote, found := findContainerSession(t, runtime, controller, "client-a", summary.SessionID)
					if found {
						var valid bool
						identity, valid = ducklord.IdentityFromSession(remote)
						return valid
					}
				}
			}
			return false
		}, func() string { return "navigation shell not listed: " + handle })
		placement := ducklord.PlaceNewTab
		if i == 1 {
			placement = ducklord.PlaceVertical
		}
		pane, err := state.ProjectLayout.Place(projectID, identity, placement, firstPane)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstPane = pane
		}
	}
	home := "/tmp/ducklord-" + owner
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("link SSH configuration: %v: %s", err, out)
	}
	localState := filepath.Join(t.TempDir(), "state.json")
	if err := (ducklord.ActivityStateStore{Path: localState}).Save(state); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(runtime, "cp", localState, controller+":"+home+"/.ducklord/state.json").CombinedOutput(); err != nil {
		t.Fatalf("install navigation layout: %v: %s", err, out)
	}
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		binary, "tui", "--name", owner, "--config", config)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 30, Cols: 160})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x1bq"))
		killNamedContainerTUI(runtime, controller, owner, config)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 30, 160)
	capture.waitCurrent(t, handles[0], 20*time.Second)
	writePTY(t, terminal, "/"+handles[0]+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	want := make([]string, len(handles))
	readLog := func(i int) string {
		t.Helper()
		out, err := exec.Command(runtime, "exec", "ducklion-client-a", "cat", logs[i]).Output()
		if err != nil {
			t.Fatalf("read native input log %d: %v", i, err)
		}
		return string(out)
	}
	for phase, step := range []struct {
		key    string
		target int
	}{
		{"", 0}, {"\x1b[C", 1}, {"\x1b[D", 0},
		{"\x1b[6~", 2}, {"\x1b[5~", 0},
	} {
		if step.key != "" {
			// Exercise separately delivered prefix and terminal escape sequence.
			writePTY(t, terminal, "\x02")
			writePTY(t, terminal, step.key)
		}
		activeTitle := "▣ client-a/" + handles[step.target]
		waitE2E(t, 20*time.Second, func() bool { return strings.Contains(capture.currentText(), activeTitle) },
			func() string {
				return fmt.Sprintf("phase %d: expected %q; screen=%q", phase, activeTitle, safeTerminalDiagnostic(capture.currentText()))
			})
		marker := fmt.Sprintf("NAV_%d_%x", phase, stamp)
		writePTY(t, terminal, fmt.Sprintf("printf '%%s%%s\\n' 'NAV_' '%d_%x' | tee -a \"$DUCKWAY_NAV_LOG\"\r", phase, stamp))
		want[step.target] += marker + "\n"
		waitE2E(t, 15*time.Second, func() bool { return readLog(step.target) == want[step.target] },
			func() string {
				return fmt.Sprintf("phase %d: input did not execute once on target %d", phase, step.target)
			})
		capture.waitCurrent(t, marker, 15*time.Second)
		// Check all native logs past the execution barrier to catch delayed
		// duplicate writes or delivery to the old pane during control handoff.
		until := time.Now().Add(500 * time.Millisecond)
		for {
			for i := range handles {
				if got := readLog(i); got != want[i] {
					t.Fatalf("phase %d: shell %d input = %q, want %q", phase, i, got, want[i])
				}
			}
			if time.Now().After(until) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
