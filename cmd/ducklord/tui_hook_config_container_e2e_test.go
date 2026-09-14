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
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
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
	hookHandle := fmt.Sprintf("hook-status-%d", time.Now().UnixNano())
	if output, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner+"-shell", "start", "client-a", "--name", hookHandle,
		"--kind", "shell", "--cwd", "/home/duck/projects/alpha", "--config", "/tmp/e2e-inspector.yaml", "--", "sh").CombinedOutput(); err != nil {
		t.Fatalf("start disposable callback shell: %v (output bytes=%d)", err, len(output))
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", owner+"-shell", "destroy", "client-a", hookHandle,
			"--config", "/tmp/e2e-inspector.yaml").CombinedOutput()
	})
	sessionsOutput, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner+"-inspect", "sessions", "client-a",
		"--json", "--config", "/tmp/e2e-inspector.yaml").Output()
	if err != nil {
		t.Fatalf("list disposable callback shell: %v", err)
	}
	var sessions []ducklord.RemoteSession
	if err := json.Unmarshal(sessionsOutput, &sessions); err != nil {
		t.Fatalf("decode disposable callback shell: %v", err)
	}
	var hookSession ducklord.RemoteSession
	for _, session := range sessions {
		if session.Name == hookHandle {
			hookSession = session
			break
		}
	}
	if hookSession.SessionID == "" || hookSession.RuntimeGeneration == 0 {
		t.Fatal("disposable callback shell has no session identity")
	}
	readStatus := func(agent string) (protocol.HostAgentHookStatus, error) {
		output, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner+"-inspect", "hook-status", "client-a", agent,
			"--config", "/tmp/e2e-inspector.yaml").Output()
		if err != nil {
			return protocol.HostAgentHookStatus{}, err
		}
		var status protocol.HostAgentHookStatus
		if err := json.Unmarshal(output, &status); err != nil {
			return protocol.HostAgentHookStatus{}, err
		}
		return status, nil
	}
	for _, tc := range []struct{ agent, path, selectKeys, marker string }{
		{"codex", "/home/duck/.codex/hooks.json", "", "__ducklion_agent_hook_v1 codex"},
		{"claude", "/home/duck/.claude/settings.json", "jj", "__ducklion_agent_hook_v1 claude"},
	} {
		writePTY(t, terminal, "hjjj\r")
		capture.waitCurrent(t, "Agent notification hooks", 10*time.Second)
		waitE2E(t, 15*time.Second, func() bool {
			return strings.Contains(capture.currentText(), strings.ToUpper(tc.agent[:1])+tc.agent[1:]+":")
		}, func() string {
			screen := capture.currentText()
			return fmt.Sprintf("%s Host status missing (loading=%t unavailable=%t disconnected=%t); screen suppressed", tc.agent,
				strings.Contains(screen, "Reading Host hook status"), strings.Contains(screen, "Host hook status unavailable"), strings.Contains(screen, "Host is disconnected"))
		})
		writePTY(t, terminal, tc.selectKeys+"\r")
		capture.waitCurrent(t, "Edit: ~", 10*time.Second)
		writePTY(t, terminal, "\r")
		capture.waitCurrent(t, "Host configuration updated", 20*time.Second)
		capture.waitCurrent(t, strings.ToUpper(tc.agent[:1])+tc.agent[1:]+": installed ·", 15*time.Second)
		out, err := exec.Command(runtime, "exec", "-u", "duck", "ducklion-client-a", "cat", tc.path).CombinedOutput()
		if err != nil || !strings.Contains(string(out), tc.marker) {
			t.Fatalf("%s hook was not installed: %v", tc.agent, err)
		}
		writePTY(t, terminal, "\r")
		baseline, err := readStatus(tc.agent)
		if err != nil {
			t.Fatalf("read %s callback baseline: %v", tc.agent, err)
		}
		// The persisted callback timestamp has millisecond precision. Keep a
		// prior callback from sharing the same tick as this invocation.
		if baseline.CallbackUpdatedAtMS > 0 {
			time.Sleep(2 * time.Millisecond)
		}
		payload := `{"type":"agent-turn-complete","last-assistant-message":"private answer"}`
		if tc.agent == "claude" {
			payload = `{"hook_event_name":"Stop","last_assistant_message":"private answer"}`
		}
		callbackCommand := "ducklion __ducklion_agent_hook_v1 " + tc.agent + " '" + payload + "'"
		if output, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner+"-shell", "send", "client-a", hookHandle,
			callbackCommand, "--config", "/tmp/e2e-inspector.yaml").CombinedOutput(); err != nil {
			t.Fatalf("send %s advisory callback: %v (output bytes=%d)", tc.agent, err, len(output))
		}
		waitE2E(t, 10*time.Second, func() bool {
			status, err := readStatus(tc.agent)
			return err == nil && status.Installed && status.CallbackObserved && status.CallbackSessionID == hookSession.SessionID &&
				status.CallbackGeneration == hookSession.RuntimeGeneration && status.CallbackUpdatedAtMS > baseline.CallbackUpdatedAtMS
		}, func() string { return tc.agent + " callback did not reach persistent Host status" })
		writePTY(t, terminal, "hjjj\r")
		capture.waitCurrent(t, "Agent notification hooks", 10*time.Second)
		capture.waitCurrent(t, strings.ToUpper(tc.agent[:1])+tc.agent[1:]+": installed · advisory callback observed", 15*time.Second)
		removeKeys := "j"
		if tc.agent == "claude" {
			removeKeys = "jjj"
		}
		writePTY(t, terminal, removeKeys+"\r")
		capture.waitCurrent(t, "Edit: ~", 10*time.Second)
		writePTY(t, terminal, "\r")
		capture.waitCurrent(t, "Host hook removed", 20*time.Second)
		capture.waitCurrent(t, strings.ToUpper(tc.agent[:1])+tc.agent[1:]+": not installed · advisory callback observed", 15*time.Second)
		out, err = exec.Command(runtime, "exec", "-u", "duck", "ducklion-client-a", "cat", tc.path).CombinedOutput()
		if err != nil || strings.Contains(string(out), tc.marker) {
			t.Fatalf("%s hook was not removed cleanly: %v", tc.agent, err)
		}
		writePTY(t, terminal, "\r")
	}
}
