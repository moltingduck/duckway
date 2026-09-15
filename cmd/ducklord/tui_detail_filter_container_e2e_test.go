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

// Clearing unread must not invalidate the detailed pane receiving keyboard
// input, including when an inventory update arrives before Ctrl-] returns.
func TestDucklordDetailedUnreadFilterContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" || os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on a disposable Host")
	}
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprintf("unread_%d", count), func(t *testing.T) {
			testDucklordDetailedUnreadFilterContainerE2E(t, count)
		})
	}
}

func testDucklordDetailedUnreadFilterContainerE2E(t *testing.T, count int) {
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	stamp := time.Now().UnixNano()
	owner := fmt.Sprintf("detail-filter-%d", stamp)
	prefix := fmt.Sprintf("df-%d", stamp)
	config := "/root/.ducklord/config.yaml"
	run := func(args ...string) error {
		return exec.Command(runtime, append([]string{"exec", controller}, args...)...).Run()
	}
	sessions := make([]ducklord.RemoteSession, count)
	for i := range sessions {
		handle := fmt.Sprintf("%s-%d", prefix, i)
		if err := run("ducklord", "--name", owner, "start", "client-a", "--name", handle,
			"--kind", "shell", "--cwd", "/home/duck", "--config", config, "--", "sh"); err != nil {
			t.Fatalf("start isolated detail fixture: %v; output suppressed", err)
		}
		t.Cleanup(func() {
			_ = run("ducklord", "--name", owner, "destroy", "client-a", handle, "--config", config)
		})
		waitE2E(t, 15*time.Second, func() bool {
			for _, item := range listContainerSessions(t, runtime, controller, "client-a") {
				if item.Handle == handle {
					var ok bool
					sessions[i], ok = findContainerSession(t, runtime, controller, "client-a", item.SessionID)
					return ok && sessions[i].RuntimeGeneration > 0
				}
			}
			return false
		}, func() string { return "isolated detail fixture did not enter inventory" })
	}
	home := fmt.Sprintf("/tmp/ducklord-detail-filter-%d", stamp)
	if err := run("mkdir", "-p", home+"/.ducklord"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run("rm", "-r", home) })
	if err := run("ln", "-s", "/root/.ssh", home+"/.ssh"); err != nil {
		t.Fatal(err)
	}
	// Badge-only attention needs no credentials, agent, desktop, or sound service.
	localConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(localConfig, []byte("name: detail-filter\nnotification_levels:\n  terminal_attention: indicator\nhosts:\n  - name: client-a\n    host: client-a\n    user: duck\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tuiConfig := home + "/.ducklord/config.yaml"
	if err := exec.Command(runtime, "cp", localConfig, controller+":"+tuiConfig).Run(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		"ducklord", "tui", "--name", owner+"-tui", "--config", tuiConfig)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 140})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x1bq"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner+"-tui", tuiConfig)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 140)
	waitLiveAgentScreen(t, capture, "Session list pane:", 20*time.Second)
	writePTY(t, terminal, "l/"+sessions[0].Name+"\r")
	waitLiveAgentScreen(t, capture, "client-a/"+sessions[0].Name, 10*time.Second)
	readState := func() ducklord.ActivityState {
		data, _ := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		var state ducklord.ActivityState
		_ = json.Unmarshal(data, &state)
		return state
	}
	category := model.NotificationTerminalAttention
	for _, session := range sessions {
		identity, ok := ducklord.IdentityFromSession(session)
		if !ok {
			t.Fatal("fixture has no stable identity")
		}
		if err := run("ducklord", "--name", owner, "send", "client-a", session.SessionID, "printf '\\a'", "--config", config); err != nil {
			t.Fatalf("emit fixture attention: %v; output suppressed", err)
		}
		waitE2E(t, 15*time.Second, func() bool {
			return readState().Sessions[identity.Key()].Unread[category]
		}, func() string { return "fixture attention did not become unread" })
	}
	writePTY(t, terminal, "/\x7f\x7f\rf") // Remove the -0 suffix; retain its preview selection.
	waitLiveAgentScreen(t, capture, "SESSIONS · unread", 10*time.Second)
	// The first preview remains selected as the unread filter is applied.
	target := sessions[0]
	identity, _ := ducklord.IdentityFromSession(target)
	waitLiveAgentScreen(t, capture, "client-a/"+target.Name, 10*time.Second)
	writePTY(t, terminal, "\r")
	waitLiveAgentScreen(t, capture, "Session focus: keys go to PTY", 20*time.Second)
	waitE2E(t, 15*time.Second, func() bool {
		entry := readState().Sessions[identity.Key()]
		return entry.Seen[category] > target.ActivitySequences[category] && !entry.Unread[category]
	}, func() string { return "focusing unread detail did not persist seen state" })
	assertFocused := func() {
		t.Helper()
		screen := capture.currentText()
		if !strings.Contains(screen, "Session focus: keys go to PTY") || !strings.Contains(screen, "client-a/"+target.Name) || strings.Contains(screen, "No matching Sessions") {
			t.Fatal("clearing unread changed or emptied the focused detail pane; PTY output suppressed")
		}
	}
	assertFocused()
	// A new bell from the focused shell forces a later inventory/activity
	// reconciliation. Its sequence must be observed and seen while still focused.
	before := readState().Sessions[identity.Key()].Seen[category]
	time.Sleep(1100 * time.Millisecond) // Ducklion coalesces attention within one second.
	writePTY(t, terminal, "printf '\\a'\r")
	waitE2E(t, 15*time.Second, func() bool {
		entry := readState().Sessions[identity.Key()]
		return entry.Observed[category] > before && entry.Seen[category] >= entry.Observed[category] && !entry.Unread[category]
	}, func() string { return "focused detail did not survive the next inventory attention update" })
	assertFocused()
	marker := fmt.Sprintf("DETAIL_FILTER_INPUT_%d", stamp)
	writePTY(t, terminal, "printf '"+marker+"\\n'\r")
	waitLiveAgentScreen(t, capture, marker, 10*time.Second)
	output, err := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", target.SessionID,
		"--lines", "40", "--config", "/tmp/e2e-inspector.yaml").Output()
	if err != nil || !strings.Contains(string(output), marker) {
		t.Fatal("input after inventory refresh did not reach the original shell; remote output suppressed")
	}
	writePTY(t, terminal, "\x1d")
	waitE2E(t, 15*time.Second, func() bool {
		screen := capture.currentText()
		if !strings.Contains(screen, "Detailed Sessions:") || !strings.Contains(screen, "SESSIONS · unread") || strings.Contains(screen, "client-a/"+target.Name) || strings.Contains(screen, target.Name+" @") {
			return false
		}
		if count == 1 {
			return strings.Contains(screen, "No matching Sessions")
		}
		return strings.Contains(screen, "client-a/"+sessions[1].Name) && !strings.Contains(screen, "No matching Sessions")
	}, func() string { return "Ctrl-] did not reapply unread filtering and restore the correct preview" })
}
