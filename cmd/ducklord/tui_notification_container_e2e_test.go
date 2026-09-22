package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestDucklordLocalNotificationContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" || os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on a disposable Host")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	// Keep the fixture handle free of lowercase `o`: while the Session list
	// search is active, direct `o` is now the Notes route. This test searches
	// the handle before entering its notification settings.
	home, handle, owner := "/tmp/ducklord-notify-e2e-"+stamp, "nfy-e2e-"+stamp, "notify-owner-"+stamp
	inspectorOwner := "notify-inspector-" + stamp
	start := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "start", "client-a", "--name", handle, "--kind", "shell",
		"--cwd", "/home/duck/projects/alpha", "--config", "/root/.ducklord/config.yaml", "--", "sh")
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start notification shell: %v: %s", err, output)
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "destroy", "client-a", handle,
			"--config", "/root/.ducklord/config.yaml").CombinedOutput()
	})
	for _, args := range [][]string{
		{"exec", controller, "mkdir", "-p", home + "/.ducklord", home + "/bin"},
		{"exec", controller, "ln", "-s", "/root/.ssh", home + "/.ssh"},
	} {
		if output, err := exec.Command(runtime, args...).CombinedOutput(); err != nil {
			t.Fatalf("prepare controller: %v: %s", err, output)
		}
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	// All files are confined to this disposable controller. The fake desktop
	// backend records argv and never contacts a real notification service.
	config := "name: notify-e2e\nnotification_levels:\n  task_completed: system\nhosts:\n  - name: client-a\n    host: client-a\n    user: duck\n"
	program := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DUCKLORD_NOTIFY_E2E_LOG\"\n"
	fixtureDir := t.TempDir()
	for path, data := range map[string]string{home + "/.ducklord/config.yaml": config, home + "/bin/notify-send": program} {
		local := fixtureDir + "/" + strings.ReplaceAll(strings.TrimPrefix(path, home+"/"), "/", "-")
		if err := os.WriteFile(local, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command(runtime, "cp", local, controller+":"+path).CombinedOutput(); err != nil {
			t.Fatalf("copy fixture: %v: %s", err, output)
		}
	}
	if output, err := exec.Command(runtime, "exec", controller, "chmod", "700", home+"/bin/notify-send").CombinedOutput(); err != nil {
		t.Fatalf("make fake notifier executable: %v: %s", err, output)
	}
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home,
		"PATH="+home+"/bin:/usr/local/bin:/usr/bin:/bin", "DUCKLORD_NOTIFY_E2E_LOG="+home+"/notifications.log",
		"TERM=xterm-256color", "ducklord", "tui", "--name", owner, "--config", home+"/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x03"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner, home+"/.ducklord/config.yaml")
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	waitSessionList := func(timeout time.Duration) {
		waitE2E(t, timeout, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "SESSIONS") && strings.Contains(screen, handle)
		}, func() string {
			return "workspace did not return to the selected Session list entry: " + safeTerminalDiagnostic(capture.currentText())
		})
	}
	waitSessionListAfter := func(position int, timeout time.Duration) {
		waitE2E(t, timeout, func() bool {
			if capture.position() <= position {
				return false
			}
			screen := capture.currentText()
			return strings.Contains(screen, "SESSIONS") && strings.Contains(screen, handle)
		}, func() string {
			return "workspace did not return to the selected Session list entry: " + safeTerminalDiagnostic(capture.currentText())
		})
	}
	waitSessionList(20 * time.Second)
	capture.waitCurrent(t, handle, 20*time.Second)
	ctrlStartGlobal := capture.position()
	writePTY(t, terminal, "\x1d") // Leave any focused PTY before opening a global modal.
	waitSessionListAfter(ctrlStartGlobal, 10*time.Second)
	writePTY(t, terminal, "\x0f") // Global notification settings.
	capture.waitCurrent(t, "Global notification settings", 10*time.Second)
	writePTY(t, terminal, "s")
	capture.waitCurrent(t, "Restart Ducklord TUI to load settings?", 10*time.Second)
	writePTY(t, terminal, "n") // Explicitly defer restart without Esc-chord ambiguity.
	waitSessionList(10 * time.Second)
	writePTY(t, terminal, "h")
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "Choose a host") && strings.Contains(screen, "› client-a")
	}, func() string {
		return "host selector did not select client-a: " + safeTerminalDiagnostic(capture.currentText())
	})
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Host actions · client-a", 10*time.Second)
	// Skills is now a Host action before Notification defaults. Deliver the
	// selection one step at a time so the modal cannot retain the Skills route
	// when the PTY/event loop is still processing the menu movement.
	for i := 0; i < 5; i++ {
		start := capture.position()
		writePTY(t, terminal, "j")
		waitE2E(t, 10*time.Second, func() bool {
			return capture.position() > start && strings.Contains(capture.currentText(), "Host actions")
		}, func() string { return "Host action selection did not repaint" })
	}
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Host notification defaults", 10*time.Second)
	writePTY(t, terminal, "j")
	capture.waitCurrent(t, "task failed", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "› inherit global", 10*time.Second)
	writePTY(t, terminal, "j")
	capture.waitCurrent(t, "› off", 10*time.Second)
	writePTY(t, terminal, "j")
	capture.waitCurrent(t, "› indicator", 10*time.Second)
	writePTY(t, terminal, "\r")
	writePTY(t, terminal, "s")
	capture.waitCurrent(t, "Restart Ducklord TUI to load settings?", 10*time.Second)
	writePTY(t, terminal, "n")
	configAfter, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/config.yaml").Output()
	if err != nil || !strings.Contains(string(configAfter), "task_failed: indicator") {
		t.Fatalf("Host notification override was not persisted: %v: %s", err, configAfter)
	}
	var target protocol.SessionSummary
	for _, candidate := range listContainerSessionsAs(t, runtime, controller, "client-a", inspectorOwner) {
		if candidate.Handle == handle {
			target = candidate
		}
	}
	if target.SessionID == "" {
		t.Fatal("notification fixture Session missing before edit")
	}
	writePTY(t, terminal, "/"+handle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus", 10*time.Second)
	ctrlStartNotifications := capture.position()
	writePTY(t, terminal, "\x1d")
	waitSessionListAfter(ctrlStartNotifications, 10*time.Second)
	writePTY(t, terminal, "n")
	capture.waitCurrent(t, "Notifications ·", 10*time.Second)
	capture.waitCurrent(t, "ID "+target.SessionID, 10*time.Second)
	for range len(model.NotificationCategories()) {
		writePTY(t, terminal, "j")
	}
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Notification delivery", 10*time.Second)
	writePTY(t, terminal, "j\rs") // inherit Host → off, then save the Session state.
	waitSessionList(10 * time.Second)
	targetRemote, ok := findContainerSessionAs(t, runtime, controller, "client-a", target.SessionID, inspectorOwner)
	if !ok {
		t.Fatal("fixture Session identity missing after notification edit")
	}
	baselineSequence := targetRemote.ActivitySequences[model.NotificationTaskCompleted]
	stateFile, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var edited ducklord.ActivityState
	if err := json.Unmarshal(stateFile, &edited); err != nil {
		t.Fatal(err)
	}
	if level := edited.Sessions[targetRemote.InstanceID+"/"+target.SessionID].NotificationLevels[ducklord.NotificationCompleted]; level != ducklord.NotificationOff {
		t.Fatalf("Session notification level was not saved: %q", level)
	}
	marker := "private-agent-answer-" + stamp
	hook := fmt.Sprintf("ducklion __ducklion_agent_hook_v1 codex '{\"type\":\"agent-turn-complete\",\"last-assistant-message\":\"%s\"}'", marker)
	readState := func() (*ducklord.ActivityState, bool) {
		output, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		if err != nil {
			return nil, false
		}
		var state ducklord.ActivityState
		return &state, json.Unmarshal(output, &state) == nil
	}
	if output, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", inspectorOwner, "send", "client-a", handle, hook,
		"--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("send suppressed shell hook: %v: %s", err, output)
	}
	waitE2E(t, 10*time.Second, func() bool {
		remote, exists := findContainerSessionAs(t, runtime, controller, "client-a", target.SessionID, inspectorOwner)
		return exists && remote.ActivitySequences[model.NotificationTaskCompleted] >= baselineSequence+1
	}, func() string { return "suppressed hook did not reach Ducklion" })
	waitE2E(t, 10*time.Second, func() bool {
		state, exists := readState()
		if !exists {
			return false
		}
		entry := state.Sessions[targetRemote.InstanceID+"/"+target.SessionID]
		return entry.Observed[model.NotificationTaskCompleted] >= baselineSequence+1 && !entry.Unread[model.NotificationTaskCompleted]
	}, func() string { return "Session off did not consume the event without unread" })
	time.Sleep(4 * time.Second) // Exceeds the local notifier's 3 s delivery deadline.
	if output, err := exec.Command(runtime, "exec", controller, "sh", "-c", `if [ -e "$1" ]; then cat "$1"; fi`, "_", home+"/notifications.log").Output(); err != nil || len(strings.TrimSpace(string(output))) != 0 {
		t.Fatalf("Session off emitted a desktop notification or log was unreadable: %v: %s", err, output)
	}
	writePTY(t, terminal, "n")
	capture.waitCurrent(t, "ID "+target.SessionID, 10*time.Second)
	for range len(model.NotificationCategories()) {
		writePTY(t, terminal, "j")
	}
	writePTY(t, terminal, "\rk\rs") // explicit off → inherit Host
	waitSessionList(10 * time.Second)
	writePTY(t, terminal, "/alpha\r") // Browse another Session so fixture completion stays unread.
	capture.waitCurrent(t, "Active · Enter again to focus", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus", 10*time.Second)
	writePTY(t, terminal, "\x1d")
	if output, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", inspectorOwner, "send", "client-a", handle, hook,
		"--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("send inherited shell hook: %v: %s", err, output)
	}
	waitE2E(t, 10*time.Second, func() bool {
		remote, exists := findContainerSessionAs(t, runtime, controller, "client-a", target.SessionID, inspectorOwner)
		return exists && remote.ActivitySequences[model.NotificationTaskCompleted] >= baselineSequence+2
	}, func() string { return "inherited hook did not advance Ducklion sequence" })
	var log string
	waitE2E(t, 15*time.Second, func() bool {
		output, err := exec.Command(runtime, "exec", controller, "cat", home+"/notifications.log").Output()
		log = string(output)
		return err == nil && strings.Count(log, handle) == 1 && strings.Contains(log, "task completed")
	}, func() string { return "Ducklord did not invoke its local desktop notifier" })
	if strings.Contains(log, marker) {
		t.Fatal("private agent answer leaked to desktop notification")
	}
	time.Sleep(4 * time.Second)
	if output, err := exec.Command(runtime, "exec", controller, "cat", home+"/notifications.log").Output(); err != nil || strings.Count(string(output), handle) != 1 {
		t.Fatalf("inherited completion did not produce exactly one desktop notification: %v: %s", err, output)
	}
	waitE2E(t, 10*time.Second, func() bool {
		state, ok := readState()
		if !ok {
			return false
		}
		entry := state.Sessions[targetRemote.InstanceID+"/"+target.SessionID]
		return entry.Observed[model.NotificationTaskCompleted] >= baselineSequence+2 && entry.Unread[model.NotificationTaskCompleted]
	}, func() string { return "completion did not mark the Session unread" })
}
