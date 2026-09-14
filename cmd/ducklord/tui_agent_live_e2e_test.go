package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// TestDucklordInteractiveAgentLiveTUIContainerE2E is opt-in because it uses
// real OAuth credentials. Never include a live PTY screen in failure output.
func TestDucklordInteractiveAgentLiveTUIContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_AGENT_LIVE_TUI_E2E") != "1" {
		t.Skip("run through scripts/ducklord-agent-tui-live-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	agent := requiredE2EEnv(t, "DUCKLORD_E2E_AGENT")
	if agent != "codex" && agent != "claude" {
		t.Fatalf("unsupported agent type %q", agent)
	}
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	handle, owner := "live-tui-"+agent+"-"+stamp, "live-tui-owner-"+stamp
	args := []string{"--name", owner, "start", "client-a", "--name", handle, "--kind", "shell", "--cwd", "/home/duck/projects/alpha", "--config", "/root/.ducklord/config.yaml", "--", "bash"}
	if out, err := exec.Command(runtime, append([]string{"exec", controller, "ducklord"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("start %s shell: %v; status=%d", agent, err, len(out))
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "destroy", "client-a", handle, "--config", "/root/.ducklord/config.yaml").CombinedOutput()
	})
	var session ducklord.RemoteSession
	waitE2E(t, 15*time.Second, func() bool {
		for _, item := range listContainerSessions(t, runtime, controller, "client-a") {
			if item.Handle == handle {
				var ok bool
				session, ok = findContainerSession(t, runtime, controller, "client-a", item.SessionID)
				return ok && session.RuntimeGeneration > 0
			}
		}
		return false
	}, func() string { return agent + " shell was not listed" })
	identity, ok := ducklord.IdentityFromSession(session)
	if !ok {
		t.Fatal("shell has no stable identity")
	}
	activity := ducklord.NewActivityState()
	projectID, err := activity.ProjectLayout.AddProject("Live " + agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	home := "/tmp/ducklord-live-tui-" + stamp
	if err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").Run(); err != nil {
		t.Fatal("prepare isolated TUI home: ", err)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").Run(); err != nil {
		t.Fatal("link isolated SSH: ", err)
	}
	localState := filepath.Join(t.TempDir(), "state.json")
	if err := (ducklord.ActivityStateStore{Path: localState}).Save(activity); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(runtime, "cp", localState, controller+":"+home+"/.ducklord/state.json").Run(); err != nil {
		t.Fatal("install isolated Project layout: ", err)
	}
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", "DUCKLORD_WORKSPACE_PREVIEW=1",
		"ducklord", "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal("start TUI: ", err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x1bq"))
		time.Sleep(200 * time.Millisecond)
		if listing, listErr := exec.Command(runtime, "exec", controller, "ps", "-ef").Output(); listErr == nil {
			for _, line := range strings.Split(string(listing), "\n") {
				if !strings.Contains(line, "ducklord tui --name "+owner+" --config") {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) > 0 {
					if pid, parseErr := strconv.Atoi(fields[0]); parseErr == nil {
						_, _ = exec.Command(runtime, "exec", controller, "kill", strconv.Itoa(pid)).CombinedOutput()
					}
				}
			}
		}
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	waitLiveAgentScreen(t, capture, "Live "+agent, 20*time.Second)
	writePTY(t, terminal, "/"+handle+"\r")
	waitLiveAgentScreen(t, capture, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	waitLiveAgentScreen(t, capture, "Session focus:", 20*time.Second)
	precheck := "SHELL_INPUT_" + stamp
	writePTY(t, terminal, "printf 'SHELL_%s_"+stamp+"\\n' INPUT\r")
	waitLiveAgentScreen(t, capture, precheck, 10*time.Second)
	launch := "codex --no-alt-screen --sandbox read-only\r"
	ready := "OpenAI Codex"
	if agent == "claude" {
		launch, ready = "claude --permission-mode plan\r", "Claude"
	}
	writePTY(t, terminal, launch)
	waitLiveAgentScreen(t, capture, ready, 45*time.Second)
	t.Log("interactive agent launched in Ducklord TUI")
	if agent == "codex" {
		output, readErr := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", session.SessionID,
			"--lines", "200", "--config", "/root/.ducklord/config.yaml").Output()
		if readErr != nil {
			t.Fatal("read Codex startup state: ", readErr)
		}
		if strings.Contains(string(output), "Update available!") || strings.Contains(string(output), "Yes, continue") {
			t.Fatal("Codex startup requires an unconfigured prompt; no automatic choice made")
		}
	}
	for turn := 1; turn <= 2; turn++ {
		prefix := fmt.Sprintf("LIVE_%s_%s_%d_", strings.ToUpper(agent), stamp, turn)
		response := prefix + "OK"
		writePTY(t, terminal, "Join "+prefix+" and OK without spaces. Reply with only the joined text.")
		time.Sleep(150 * time.Millisecond) // Codex treats a prompt and Enter in one PTY write as a paste.
		writePTY(t, terminal, "\r")
		if agent == "codex" {
			time.Sleep(time.Second)
			// Some Codex terminal modes accept the first Enter as paste completion.
			// A second Enter submits the now-visible prompt; a completed turn
			// treats it as an empty input and does not start another task.
			writePTY(t, terminal, "\r")
		}
		turnTimeout := 180 * time.Second
		if os.Getenv("DUCKLORD_LIVE_TUI_DEBUG_TIMEOUT") == "1" {
			turnTimeout = 45 * time.Second
		}
		var screenSeen, remoteSeen, remoteTrust, remotePrompt, remoteContinue bool
		waitE2E(t, turnTimeout, func() bool {
			screenSeen = strings.Contains(capture.currentText(), response)
			output, readErr := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", session.SessionID,
				"--lines", "200", "--config", "/root/.ducklord/config.yaml").Output()
			remoteSeen = readErr == nil && strings.Contains(string(output), response)
			if readErr == nil {
				remoteTrust = strings.Contains(string(output), "Yes, continue") || strings.Contains(string(output), "Do you trust")
				remotePrompt = strings.Contains(string(output), "Join") || strings.Contains(string(output), prefix)
				remoteContinue = strings.Contains(string(output), "Press enter to continue")
			}
			return screenSeen && remoteSeen
		}, func() string {
			return fmt.Sprintf("%s turn %d missing answer (screen=%t remote=%t remote-trust=%t remote-prompt=%t remote-continue=%t); no PTY content logged", agent, turn, screenSeen, remoteSeen, remoteTrust, remotePrompt, remoteContinue)
		})
		if turn == 1 {
			writePTY(t, terminal, "\x1d")
			waitLiveAgentScreen(t, capture, "Preview:", 20*time.Second)
			writePTY(t, terminal, "P")
			waitLiveAgentScreen(t, capture, "Project pane:", 20*time.Second)
			writePTY(t, terminal, "k")
			waitLiveAgentScreen(t, capture, "› Default Project", 20*time.Second)
			writePTY(t, terminal, "j")
			waitLiveAgentScreen(t, capture, "› Live "+agent, 20*time.Second)
			waitLiveAgentScreen(t, capture, response, 20*time.Second)
			writePTY(t, terminal, "P")
			waitLiveAgentScreen(t, capture, "Preview:", 20*time.Second)
			writePTY(t, terminal, "/"+handle+"\r")
			waitLiveAgentScreen(t, capture, "Active · Enter again to focus", 20*time.Second)
			writePTY(t, terminal, "\r")
			waitLiveAgentScreen(t, capture, "Session focus:", 20*time.Second)
		}
	}
	if latest, found := findContainerSession(t, runtime, controller, "client-a", session.SessionID); !found || latest.RuntimeGeneration != session.RuntimeGeneration {
		t.Fatal("interactive agent shell changed identity during TUI navigation")
	}
}

func waitLiveAgentScreen(t *testing.T, capture *tuiCapture, marker string, timeout time.Duration) {
	t.Helper()
	waitE2E(t, timeout, func() bool { return strings.Contains(capture.currentText(), marker) }, func() string {
		screen := capture.currentText()
		return fmt.Sprintf("live agent TUI phase timed out (trust=%t, trust-choice=%t, codex=%t, claude=%t, join-input=%t, login=%t, invalid-frame=%t, loading=%t, unavailable=%t, focused=%t, project=%t); no PTY content logged",
			strings.Contains(screen, "Do you trust"), strings.Contains(screen, "Yes, continue"), strings.Contains(screen, "Codex"),
			strings.Contains(screen, "Claude"), strings.Contains(screen, "Join LIVE_"), strings.Contains(strings.ToLower(screen), "login"),
			strings.Contains(screen, "framebuffer is invalid"), strings.Contains(screen, "loading live PTY"), strings.Contains(screen, "PTY output unavailable"),
			strings.Contains(screen, "Session focus"), strings.Contains(screen, "PROJECTS"))
	})
}
