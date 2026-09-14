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
	home, handle, owner := "/tmp/ducklord-notify-e2e-"+stamp, "notify-e2e-"+stamp, "notify-owner-"+stamp
	start := exec.Command(runtime, "exec", controller, "ducklord", "start", "client-a", "--name", handle, "--kind", "shell",
		"--cwd", "/home/duck/projects/alpha", "--config", "/root/.ducklord/config.yaml", "--", "sh")
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start notification shell: %v: %s", err, output)
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "destroy", "client-a", handle,
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
		killNamedContainerTUI(runtime, controller, owner)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	capture.waitCurrent(t, "PROJECTS", 20*time.Second)
	capture.waitCurrent(t, "notify-e2e", 20*time.Second)
	writePTY(t, terminal, "\x1d") // Leave any focused PTY before opening a global modal.
	writePTY(t, terminal, "O")    // Global notification settings.
	capture.waitCurrent(t, "Global notification settings", 10*time.Second)
	writePTY(t, terminal, "s")
	capture.waitCurrent(t, "Restart Ducklord TUI to load settings?", 10*time.Second)
	writePTY(t, terminal, "n") // Explicitly defer restart without Esc-chord ambiguity.
	capture.waitCurrent(t, "PROJECTS", 10*time.Second)
	writePTY(t, terminal, "h")
	capture.waitCurrent(t, "Host actions", 10*time.Second)
	writePTY(t, terminal, "jjjj\r") // Host actions → Notification defaults.
	capture.waitCurrent(t, "Host notification defaults", 10*time.Second)
	writePTY(t, terminal, "j")      // task failed
	writePTY(t, terminal, "\rjj\r") // inherit → indicator
	writePTY(t, terminal, "s")
	capture.waitCurrent(t, "Restart Ducklord TUI to load settings?", 10*time.Second)
	writePTY(t, terminal, "n")
	configAfter, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/config.yaml").Output()
	if err != nil || !strings.Contains(string(configAfter), "task_failed: indicator") {
		t.Fatalf("Host notification override was not persisted: %v: %s", err, configAfter)
	}
	alphaSessions := listContainerSessions(t, runtime, controller, "client-a")
	var alpha protocol.SessionSummary
	for _, candidate := range alphaSessions {
		if candidate.Handle == "alpha" {
			alpha = candidate
		}
	}
	if alpha.SessionID == "" {
		t.Fatal("alpha Session missing before notification edit")
	}
	writePTY(t, terminal, "n")
	capture.waitCurrent(t, "Notifications ·", 10*time.Second)
	capture.waitCurrent(t, "ID "+alpha.SessionID, 10*time.Second)
	for range len(model.NotificationCategories()) {
		writePTY(t, terminal, "j")
	}
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Notification delivery", 10*time.Second)
	writePTY(t, terminal, "j\rs") // inherit Host → off, then save the Session state.
	capture.waitCurrent(t, "PROJECTS", 10*time.Second)
	alphaRemote, ok := findContainerSession(t, runtime, controller, "client-a", alpha.SessionID)
	if !ok {
		t.Fatal("alpha Session identity missing after notification edit")
	}
	stateFile, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var edited ducklord.ActivityState
	if err := json.Unmarshal(stateFile, &edited); err != nil {
		t.Fatal(err)
	}
	if level := edited.Sessions[alphaRemote.InstanceID+"/"+alpha.SessionID].NotificationLevels[ducklord.NotificationCompleted]; level != ducklord.NotificationOff {
		t.Fatalf("Session notification level was not saved: %q", level)
	}
	marker := "private-agent-answer-" + stamp
	hook := fmt.Sprintf("ducklion __ducklion_agent_hook_v1 codex '{\"type\":\"agent-turn-complete\",\"last-assistant-message\":\"%s\"}'", marker)
	if output, err := exec.Command(runtime, "exec", controller, "ducklord", "send", "client-a", handle, hook,
		"--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("send shell hook: %v: %s", err, output)
	}
	var log string
	waitE2E(t, 15*time.Second, func() bool {
		output, err := exec.Command(runtime, "exec", controller, "cat", home+"/notifications.log").Output()
		log = string(output)
		return err == nil && strings.Contains(log, handle) && strings.Contains(log, "task completed")
	}, func() string { return "Ducklord did not invoke its local desktop notifier" })
	if strings.Contains(log, marker) {
		t.Fatal("private agent answer leaked to desktop notification")
	}
	readState := func() (*ducklord.ActivityState, bool) {
		output, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		if err != nil {
			return nil, false
		}
		var state ducklord.ActivityState
		return &state, json.Unmarshal(output, &state) == nil
	}
	waitE2E(t, 10*time.Second, func() bool {
		state, ok := readState()
		if !ok {
			return false
		}
		for _, entry := range state.Sessions {
			if entry.Unread[model.NotificationTaskCompleted] {
				return true
			}
		}
		return false
	}, func() string { return "completion did not mark the Session unread" })
}
