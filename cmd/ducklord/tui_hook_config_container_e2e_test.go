package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklion/daemon"
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
	// Never print a live screen: this disposable host can also contain real
	// agent sessions from the broader suite.
	waitScreen := func(needle string, timeout time.Duration) {
		t.Helper()
		waitE2E(t, timeout, func() bool { return strings.Contains(capture.currentText(), needle) },
			func() string { return "hook configuration screen missing " + needle + "; screen suppressed" })
	}
	waitScreen("PROJECTS", 20*time.Second)
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
		before, err := readStatus(tc.agent)
		if err != nil {
			t.Fatalf("read %s hook baseline: %v", tc.agent, err)
		}
		t.Cleanup(func() {
			action := "remove"
			if before.Installed {
				action = "install"
			}
			if err := restoreContainerHostHook(runtime, owner, tc.agent, action); err != nil {
				t.Errorf("restore %s hook installed=%t: %v (remote output suppressed)", tc.agent, before.Installed, err)
			}
		})
		writePTY(t, terminal, "hjjj\r")
		waitScreen("Agent notification hooks", 10*time.Second)
		waitE2E(t, 15*time.Second, func() bool {
			return strings.Contains(capture.currentText(), strings.ToUpper(tc.agent[:1])+tc.agent[1:]+":")
		}, func() string {
			screen := capture.currentText()
			return fmt.Sprintf("%s Host status missing (loading=%t unavailable=%t disconnected=%t); screen suppressed", tc.agent,
				strings.Contains(screen, "Reading Host hook status"), strings.Contains(screen, "Host hook status unavailable"), strings.Contains(screen, "Host is disconnected"))
		})
		writePTY(t, terminal, tc.selectKeys+"\r")
		waitE2E(t, 10*time.Second, func() bool {
			return strings.Contains(capture.currentText(), "Edit: ~")
		}, func() string {
			screen := capture.currentText()
			return fmt.Sprintf("%s hook review missing (select=%t confirm=%t saving=%t done=%t error=%t); screen suppressed", tc.agent,
				strings.Contains(screen, "Enter review"), strings.Contains(screen, "Enter confirm"),
				strings.Contains(screen, "Updating Host hook"), strings.Contains(screen, "Host configuration updated"),
				strings.Contains(screen, "Enter retry"))
		})
		writePTY(t, terminal, "\r")
		waitE2E(t, 20*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "Host configuration updated") || strings.Contains(screen, "Already in the requested state")
		}, func() string {
			screen := capture.currentText()
			return fmt.Sprintf("%s hook install not confirmed (previously_installed=%t modal=%t saving=%t disconnected=%t); screen suppressed",
				tc.agent, before.Installed, strings.Contains(screen, "Agent notification hooks"),
				strings.Contains(screen, "Updating Host hook configuration"), strings.Contains(screen, "Host is disconnected"))
		})
		waitScreen(strings.ToUpper(tc.agent[:1])+tc.agent[1:]+": installed ·", 15*time.Second)
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
			return err == nil && status.Installed && status.Activation == "operational" && status.CallbackObserved && status.CallbackSessionID == hookSession.SessionID &&
				status.CallbackGeneration == hookSession.RuntimeGeneration && status.CallbackUpdatedAtMS > baseline.CallbackUpdatedAtMS
		}, func() string { return tc.agent + " callback did not reach persistent Host status" })
		writePTY(t, terminal, "hjjj\r")
		waitScreen("Agent notification hooks", 10*time.Second)
		waitScreen(strings.ToUpper(tc.agent[:1])+tc.agent[1:]+": installed · operational (advisory)", 15*time.Second)
		removeKeys := "j"
		if tc.agent == "claude" {
			removeKeys = "jjj"
		}
		writePTY(t, terminal, removeKeys+"\r")
		waitScreen("Edit: ~", 10*time.Second)
		writePTY(t, terminal, "\r")
		waitScreen("Host hook removed", 20*time.Second)
		waitScreen(strings.ToUpper(tc.agent[:1])+tc.agent[1:]+": not installed · advisory callback observed", 15*time.Second)
		out, err = exec.Command(runtime, "exec", "-u", "duck", "ducklion-client-a", "cat", tc.path).CombinedOutput()
		if err != nil || strings.Contains(string(out), tc.marker) {
			t.Fatalf("%s hook was not removed cleanly: %v", tc.agent, err)
		}
		writePTY(t, terminal, "\r")
		// A historical callback must not verify a fresh installation. Reopen
		// the TUI status after reinstalling through the same structured RPC.
		if err := restoreContainerHostHook(runtime, owner, tc.agent, "install"); err != nil {
			t.Fatalf("reinstall %s hook: %v; remote output suppressed", tc.agent, err)
		}
		status, err := readStatus(tc.agent)
		if err != nil || !status.Installed || status.Activation != "pending" || !status.CallbackObserved {
			t.Fatalf("%s reinstall did not invalidate historical activation; err=%v", tc.agent, err)
		}
		writePTY(t, terminal, "hjjj\r")
		waitScreen(strings.ToUpper(tc.agent[:1])+tc.agent[1:]+": installed · pending activation", 15*time.Second)
		if tc.agent == "codex" {
			waitScreen("Review Codex /hooks", 10*time.Second)
		}
		writePTY(t, terminal, "\x03")
		waitE2E(t, 10*time.Second, func() bool {
			return !strings.Contains(capture.currentText(), "Agent notification hooks")
		}, func() string { return "hook status modal did not close; screen suppressed" })
	}
}

// Restore only Ducklion's integration entry, preserving unrelated host settings.
// Cleanup uses the native bridge so it still works if the TUI failed mid-modal.
func restoreContainerHostHook(runtime, owner, agent, action string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtime, "exec", "-i", "-u", "duck", "ducklion-client-a",
		"env", "HOME=/home/duck", "ducklion", "bridge", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	client, err := daemon.ConnectContext(ctx, &hostHookBridgePipes{ReadCloser: stdout, WriteCloser: stdin}, owner+"-restore")
	if err != nil {
		return fmt.Errorf("connect cleanup bridge: %w", err)
	}
	defer client.Close()
	if _, err := client.ConfigureHostAgentHook(ctx, agent, action); err != nil {
		return fmt.Errorf("restore hook configuration: %w", err)
	}
	status, err := client.HostAgentHookStatus(ctx, agent)
	if err != nil || status.Installed != (action == "install") {
		return fmt.Errorf("restored hook state mismatch: installed=%t err=%v", status.Installed, err)
	}
	return nil
}

type hostHookBridgePipes struct {
	io.ReadCloser
	io.WriteCloser
}

func (p *hostHookBridgePipes) Close() error {
	_ = p.WriteCloser.Close()
	return p.ReadCloser.Close()
}
