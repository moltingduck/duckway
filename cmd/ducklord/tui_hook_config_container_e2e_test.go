package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Exercises approval, SSH RPC, and host-side config changes without credentials.
func TestDucklordHostHookConfigContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	if os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("hook configuration E2E requires a disposable host")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	home := fmt.Sprintf("/tmp/ducklord-hook-e2e-%d", time.Now().UnixNano())
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("prepare SSH config: %v: %s", err, out)
	}
	owner := fmt.Sprintf("hook-config-e2e-%d", time.Now().UnixNano())
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		"ducklord", "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
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
	for _, tc := range []struct{ agent, path, selectKeys, marker string }{
		{"codex", "/home/duck/.codex/hooks.json", "", "__ducklion_agent_hook_v1 codex"},
		{"claude", "/home/duck/.claude/settings.json", "jj", "__ducklion_agent_hook_v1 claude"},
	} {
		writePTY(t, terminal, "hjjj\r")
		capture.waitCurrent(t, "Agent notification hooks", 10*time.Second)
		writePTY(t, terminal, tc.selectKeys+"\r")
		capture.waitCurrent(t, "Edit: ~", 10*time.Second)
		writePTY(t, terminal, "\r")
		capture.waitCurrent(t, "Host configuration updated", 20*time.Second)
		out, err := exec.Command(runtime, "exec", "-u", "duck", "ducklion-client-a", "cat", tc.path).CombinedOutput()
		if err != nil || !strings.Contains(string(out), tc.marker) {
			t.Fatalf("%s hook was not installed: %v", tc.agent, err)
		}
		writePTY(t, terminal, "\r")
		writePTY(t, terminal, "hjjj\r")
		capture.waitCurrent(t, "Agent notification hooks", 10*time.Second)
		removeKeys := "j"
		if tc.agent == "claude" {
			removeKeys = "jjj"
		}
		writePTY(t, terminal, removeKeys+"\r")
		capture.waitCurrent(t, "Edit: ~", 10*time.Second)
		writePTY(t, terminal, "\r")
		capture.waitCurrent(t, "Host hook removed", 20*time.Second)
		out, err = exec.Command(runtime, "exec", "-u", "duck", "ducklion-client-a", "cat", tc.path).CombinedOutput()
		if err != nil || strings.Contains(string(out), tc.marker) {
			t.Fatalf("%s hook was not removed cleanly: %v", tc.agent, err)
		}
		writePTY(t, terminal, "\r")
	}
}
