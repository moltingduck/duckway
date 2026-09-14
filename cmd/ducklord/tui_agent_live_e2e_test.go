package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklion/model"
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
	split := os.Getenv("DUCKLORD_AGENT_LIVE_TUI_SPLIT") == "1"
	cols := uint16(130)
	if split {
		cols = 180
	}
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
	agentPaneID, err := activity.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	var sidecar ducklord.RemoteSession
	sidecarHandle := fmt.Sprintf("side-%x", time.Now().UnixNano()&0xffffff)
	if split {
		sidecarArgs := []string{"--name", owner, "start", "client-a", "--name", sidecarHandle, "--kind", "shell", "--cwd", "/home/duck/projects/alpha", "--config", "/root/.ducklord/config.yaml", "--", "bash"}
		if out, err := exec.Command(runtime, append([]string{"exec", controller, "ducklord"}, sidecarArgs...)...).CombinedOutput(); err != nil {
			t.Fatalf("start split sidecar shell: %v; status=%d", err, len(out))
		}
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "destroy", "client-a", sidecarHandle, "--config", "/root/.ducklord/config.yaml").CombinedOutput()
		})
		waitE2E(t, 15*time.Second, func() bool {
			for _, item := range listContainerSessions(t, runtime, controller, "client-a") {
				if item.Handle == sidecarHandle {
					var ok bool
					sidecar, ok = findContainerSession(t, runtime, controller, "client-a", item.SessionID)
					return ok && sidecar.RuntimeGeneration > 0
				}
			}
			return false
		}, func() string { return "split sidecar shell was not listed" })
		sidecarIdentity, ok := ducklord.IdentityFromSession(sidecar)
		if !ok {
			t.Fatal("split sidecar has no stable identity")
		}
		if _, err := activity.ProjectLayout.Place(projectID, sidecarIdentity, ducklord.PlaceVertical, agentPaneID); err != nil {
			t.Fatal(err)
		}
	}
	home := "/tmp/ducklord-live-tui-" + stamp
	if err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord", home+"/bin").Run(); err != nil {
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
	configPath := home + "/.ducklord/config.yaml"
	if output, err := exec.Command(runtime, "exec", controller, "sh", "-c", `cp "$1" "$2" && printf '\nnotification_levels:\n  task_completed: system\n' >> "$2"`,
		"_", "/root/.ducklord/config.yaml", configPath).CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated notification policy: %v (output bytes=%d)", err, len(output))
	}
	notifier := filepath.Join(t.TempDir(), "notify-send")
	if err := os.WriteFile(notifier, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DUCKLORD_NOTIFY_E2E_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(runtime, "cp", notifier, controller+":"+home+"/bin/notify-send").CombinedOutput(); err != nil {
		t.Fatalf("install local notifier fixture: %v (output bytes=%d)", err, len(output))
	}
	if err := exec.Command(runtime, "exec", controller, "chmod", "700", home+"/bin/notify-send").Run(); err != nil {
		t.Fatal("make notifier fixture executable: ", err)
	}
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", "PATH="+home+"/bin:/usr/local/bin:/usr/bin:/bin",
		"DUCKLORD_NOTIFY_E2E_LOG="+home+"/notifications.log", "ducklord", "tui", "--name", owner, "--config", configPath)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: cols})
	if err != nil {
		t.Fatal("start TUI: ", err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x1bq"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner, configPath)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, int(cols))
	waitLiveAgentScreen(t, capture, "Live "+agent, 20*time.Second)
	// Install through the same explicit Host confirmation flow operators use.
	writePTY(t, terminal, "hjjj\r")
	waitLiveAgentScreen(t, capture, "Agent notification hooks", 10*time.Second)
	if agent == "claude" {
		writePTY(t, terminal, "jj")
	}
	writePTY(t, terminal, "\r")
	waitLiveAgentScreen(t, capture, "Edit: ~", 10*time.Second)
	writePTY(t, terminal, "\r")
	waitLiveAgentScreen(t, capture, "Host configuration updated", 20*time.Second)
	writePTY(t, terminal, "\r")
	waitLiveAgentScreen(t, capture, "Session list pane:", 10*time.Second)
	writePTY(t, terminal, "/"+handle)
	waitLiveAgentScreen(t, capture, "search › "+handle, 10*time.Second)
	writePTY(t, terminal, "\r")
	waitLiveAgentScreen(t, capture, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	waitLiveAgentScreen(t, capture, "Session focus:", 20*time.Second)
	if split {
		waitLiveAgentScreen(t, capture, "client-a/"+sidecarHandle, 20*time.Second)
	}
	precheck := "SHELL_INPUT_" + stamp
	writePTY(t, terminal, "printf 'SHELL_%s_"+stamp+"\\n' INPUT\r")
	waitLiveAgentScreen(t, capture, precheck, 10*time.Second)
	launch := "codex --no-alt-screen --sandbox read-only\r"
	ready := "OpenAI Codex"
	if agent == "claude" {
		launch = "claude --permission-mode plan\r"
	}
	writePTY(t, terminal, launch)
	if agent == "claude" {
		// The disposable Project is new to Claude. The first screen is a
		// safety decision; its ❯ points to "No, exit", not the input prompt.
		// Approve only this isolated E2E workspace, never a user's directory.
		waitLiveAgentScreen(t, capture, "Quick safety check", 45*time.Second)
		time.Sleep(350 * time.Millisecond) // Wait until the visible choice is accepting input.
		writePTY(t, terminal, "\x1b[B")
		time.Sleep(250 * time.Millisecond)
		writePTY(t, terminal, "\r")
		var safetyVisible, inputPromptVisible, remoteShell, remoteApp bool
		waitE2E(t, 45*time.Second, func() bool {
			screen := capture.currentText()
			safetyVisible = strings.Contains(screen, "Quick safety check")
			inputPromptVisible = strings.Contains(screen, "❯")
			out, err := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", session.SessionID,
				"--lines", "40", "--config", "/root/.ducklord/config.yaml").Output()
			remoteShell = err == nil && strings.Contains(string(out), "client-a:~/projects/alpha$")
			remoteApp = err == nil && (strings.Contains(string(out), "shift+tab") || strings.Contains(string(out), "Auto-update"))
			return !safetyVisible && inputPromptVisible && remoteApp
		}, func() string {
			return fmt.Sprintf("Claude did not reach its interactive input after disposable Project trust (safety=%t prompt=%t remote-shell=%t remote-app=%t); output suppressed", safetyVisible, inputPromptVisible, remoteShell, remoteApp)
		})
	} else {
		waitLiveAgentScreen(t, capture, ready, 45*time.Second)
	}
	if agent == "codex" {
		// Fresh Codex profiles require a separate, agent-owned trust decision.
		// Approve only the hook we just installed in this disposable Host.
		waitLiveAgentScreen(t, capture, "Hooks need review", 20*time.Second)
		waitLiveAgentScreen(t, capture, "Trust all and continue", 20*time.Second)
		time.Sleep(250 * time.Millisecond) // Let the interactive choice become input-ready.
		writePTY(t, terminal, "\x1b[B\r")  // Trust all and continue.
		waitE2E(t, 20*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "Codex") && strings.Contains(screen, "Session focus:") && !strings.Contains(screen, "Hooks need review")
		}, func() string {
			screen := capture.currentText()
			return fmt.Sprintf("Codex hook review did not return to the interactive prompt (review=%t choice=%t loading=%t login=%t); PTY content suppressed",
				strings.Contains(screen, "Hooks need review"), strings.Contains(screen, "Trust all and continue"),
				strings.Contains(screen, "loading live PTY"), strings.Contains(strings.ToLower(screen), "login"))
		})
	}
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
	baselineSession, found := findContainerSession(t, runtime, controller, "client-a", session.SessionID)
	if !found {
		t.Fatal("live agent Session disappeared before first prompt")
	}
	baselineSequence := baselineSession.ActivitySequences[model.NotificationTaskCompleted]
	baselineOutput, baselineErr := exec.Command(runtime, "exec", controller, "sh", "-c", `if [ -e "$1" ]; then cat "$1"; fi`, "_", home+"/notifications.log").Output()
	if baselineErr != nil {
		t.Fatal("read baseline notifier state: ", baselineErr)
	}
	baselineNotifications := strings.Count(string(baselineOutput), handle)
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
		var remoteAuth, remoteError, screenError, screenPrompt, focusVisible, inputBlocked, controlChanged bool
		waitE2E(t, turnTimeout, func() bool {
			screen := capture.currentText()
			screenSeen = strings.Contains(screen, response)
			screenError = strings.Contains(strings.ToLower(screen), "error")
			screenPrompt = strings.Contains(screen, prefix)
			focusVisible = strings.Contains(screen, "Session focus:")
			inputBlocked = strings.Contains(screen, "waiting for Session pane output") || strings.Contains(screen, "input was not sent")
			controlChanged = strings.Contains(screen, "PTY control changed") || strings.Contains(screen, "host reconnecting")
			output, readErr := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", session.SessionID,
				"--lines", "200", "--config", "/root/.ducklord/config.yaml").Output()
			remoteSeen = readErr == nil && strings.Contains(string(output), response)
			if readErr == nil {
				remote := string(output)
				remoteTrust = strings.Contains(remote, "Yes, continue") || strings.Contains(remote, "Do you trust")
				remotePrompt = strings.Contains(remote, "Join") || strings.Contains(remote, prefix)
				remoteContinue = strings.Contains(remote, "Press enter to continue")
				remoteAuth = strings.Contains(strings.ToLower(remote), "login") || strings.Contains(strings.ToLower(remote), "authentication")
				remoteError = strings.Contains(strings.ToLower(remote), "error")
			}
			return screenSeen && remoteSeen
		}, func() string {
			return fmt.Sprintf("%s turn %d missing answer (screen=%t remote=%t screen-prompt=%t screen-error=%t focused=%t input-blocked=%t control-changed=%t remote-trust=%t remote-prompt=%t remote-continue=%t remote-auth=%t remote-error=%t); no PTY content logged", agent, turn, screenSeen, remoteSeen, screenPrompt, screenError, focusVisible, inputBlocked, controlChanged, remoteTrust, remotePrompt, remoteContinue, remoteAuth, remoteError)
		})
		waitE2E(t, 25*time.Second, func() bool {
			latest, found := findContainerSession(t, runtime, controller, "client-a", session.SessionID)
			return found && latest.ActivitySequences[model.NotificationTaskCompleted] >= baselineSequence+uint64(turn)
		}, func() string {
			return fmt.Sprintf("%s turn %d produced an answer but no native Stop hook completion", agent, turn)
		})
		waitE2E(t, 25*time.Second, func() bool {
			log, err := exec.Command(runtime, "exec", controller, "cat", home+"/notifications.log").Output()
			return err == nil && strings.Count(string(log), handle) >= baselineNotifications+turn && strings.Count(string(log), "task completed") >= baselineNotifications+turn
		}, func() string {
			return fmt.Sprintf("%s turn %d reached Ducklion but not Ducklord desktop delivery; notification log content suppressed", agent, turn)
		})
		if turn == 1 {
			writePTY(t, terminal, "\x1d")
			waitLiveAgentScreen(t, capture, "Session list pane:", 20*time.Second)
			writePTY(t, terminal, "P")
			waitLiveAgentScreen(t, capture, "Project pane:", 20*time.Second)
			writePTY(t, terminal, "k")
			waitLiveAgentScreen(t, capture, "› Default Project", 20*time.Second)
			writePTY(t, terminal, "j")
			waitLiveAgentScreen(t, capture, "› Live "+agent, 20*time.Second)
			waitLiveAgentScreen(t, capture, response, 20*time.Second)
			writePTY(t, terminal, "P")
			waitLiveAgentScreen(t, capture, "Session list pane:", 20*time.Second)
			writePTY(t, terminal, "/")
			waitLiveAgentScreen(t, capture, "Search sessions", 10*time.Second)
			writePTY(t, terminal, handle)
			waitLiveAgentScreen(t, capture, "search › "+handle, 10*time.Second)
			writePTY(t, terminal, "\r")
			waitLiveAgentScreen(t, capture, "Active · Enter again to focus", 20*time.Second)
			writePTY(t, terminal, "\r")
			waitLiveAgentScreen(t, capture, "Session focus:", 20*time.Second)
		}
	}
	if split {
		// A live agent must keep rendering while the adjacent shell receives
		// input, and switching back must restore the same agent PTY.
		writePTY(t, terminal, "\x1dP")
		waitLiveAgentScreen(t, capture, "Project pane:", 20*time.Second)
		writePTY(t, terminal, "L\r")
		waitLiveAgentScreen(t, capture, "Session focus:", 20*time.Second)
		marker := "SPLIT_INPUT_" + stamp
		writePTY(t, terminal, "printf '"+marker+"\\n'\r")
		waitLiveAgentScreen(t, capture, marker, 20*time.Second)
		sidecarOutput, sidecarErr := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", sidecar.SessionID,
			"--lines", "80", "--config", "/root/.ducklord/config.yaml").Output()
		agentOutput, agentErr := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", session.SessionID,
			"--lines", "80", "--config", "/root/.ducklord/config.yaml").Output()
		if sidecarErr != nil || agentErr != nil || !strings.Contains(string(sidecarOutput), marker) || strings.Contains(string(agentOutput), marker) {
			t.Fatalf("split input did not stay in selected sidecar PTY (sidecar=%t agent=%t read-errors=%t/%t); output suppressed",
				strings.Contains(string(sidecarOutput), marker), strings.Contains(string(agentOutput), marker), sidecarErr != nil, agentErr != nil)
		}
		writePTY(t, terminal, "\x1d")
		waitLiveAgentScreen(t, capture, "Project pane:", 20*time.Second)
		writePTY(t, terminal, "H\r")
		waitLiveAgentScreen(t, capture, "Session focus:", 20*time.Second)
		waitLiveAgentScreen(t, capture, "client-a/"+sidecarHandle, 20*time.Second)
	}
	backgroundPrefix := fmt.Sprintf("LIVE_%s_%s_BACKGROUND_", strings.ToUpper(agent), stamp)
	backgroundResponse := backgroundPrefix + "OK"
	backgroundPrompt := "Join " + backgroundPrefix + " and OK without spaces. Reply with only the joined text."
	if agent == "codex" {
		// Keep the turn active while Ducklord leaves the focused pane; a
		// trivially fast answer could otherwise be correctly marked seen.
		backgroundPrompt = "First run `sleep 3` in the shell. Then " + backgroundPrompt
	}
	writePTY(t, terminal, backgroundPrompt)
	time.Sleep(150 * time.Millisecond)
	writePTY(t, terminal, "\r")
	if agent == "codex" {
		time.Sleep(time.Second)
		writePTY(t, terminal, "\r")
	}
	// The TUI may still be flushing this input to the remote PTY. Observe the
	// prompt there before navigating away; otherwise Ctrl-] can race the
	// submission and leave a partially entered Claude prompt behind.
	waitE2E(t, 10*time.Second, func() bool {
		output, err := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", session.SessionID,
			"--lines", "100", "--config", "/root/.ducklord/config.yaml").Output()
		return err == nil && strings.Contains(string(output), backgroundPrefix)
	}, func() string { return agent + " background prompt never reached the remote PTY" })
	time.Sleep(200 * time.Millisecond)
	writePTY(t, terminal, "\x1d")
	if !split {
		waitLiveAgentScreen(t, capture, "Session list pane:", 20*time.Second)
		writePTY(t, terminal, "P")
	}
	waitLiveAgentScreen(t, capture, "Project pane:", 20*time.Second)
	writePTY(t, terminal, "k")
	waitLiveAgentScreen(t, capture, "› Default Project", 20*time.Second)
	if latest, exists := findContainerSession(t, runtime, controller, "client-a", session.SessionID); !exists || latest.ActivitySequences[model.NotificationTaskCompleted] != baselineSequence+2 {
		t.Fatal(agent + " background task finished before Ducklord left the focused Session; unread precondition was not met")
	}
	backgroundTimeout := 180 * time.Second
	if os.Getenv("DUCKLORD_LIVE_TUI_DEBUG_TIMEOUT") == "1" {
		backgroundTimeout = 45 * time.Second
	}
	var backgroundPromptVisible, backgroundAgentBusy, backgroundAuth, backgroundError bool
	var backgroundRateLimit, backgroundAPIError, backgroundNetworkError, backgroundPermission, backgroundSubmitHint bool
	backgroundAPIStatus := "none"
	apiStatusPattern := regexp.MustCompile(`(?i)api error[^0-9]{0,20}([0-9]{3})`)
	waitE2E(t, backgroundTimeout, func() bool {
		output, err := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", session.SessionID,
			"--lines", "200", "--config", "/root/.ducklord/config.yaml").Output()
		if err != nil {
			return false
		}
		remote := string(output)
		backgroundPromptVisible = strings.Contains(remote, backgroundPrefix)
		if index := strings.Index(remote, backgroundPrefix); index >= 0 {
			remote = remote[index:]
		}
		lower := strings.ToLower(remote)
		backgroundAgentBusy = strings.Contains(lower, "esc to interrupt")
		backgroundAuth = strings.Contains(lower, "login") || strings.Contains(lower, "authentication")
		backgroundError = strings.Contains(lower, "error")
		backgroundRateLimit = strings.Contains(lower, "rate limit") || strings.Contains(lower, "429")
		backgroundAPIError = strings.Contains(lower, "api error") || strings.Contains(lower, "overloaded")
		if match := apiStatusPattern.FindStringSubmatch(remote); len(match) == 2 {
			backgroundAPIStatus = match[1]
		}
		backgroundNetworkError = strings.Contains(lower, "network error") || strings.Contains(lower, "connection error")
		backgroundPermission = strings.Contains(lower, "permission") || strings.Contains(lower, "approve")
		backgroundSubmitHint = strings.Contains(lower, "press enter") || strings.Contains(lower, "return to submit")
		return strings.Contains(remote, backgroundResponse)
	}, func() string {
		completed, failed := uint64(0), uint64(0)
		if latest, exists := findContainerSession(t, runtime, controller, "client-a", session.SessionID); exists {
			completed = latest.ActivitySequences[model.NotificationTaskCompleted]
			failed = latest.ActivitySequences[model.NotificationTaskFailed]
		}
		return fmt.Sprintf("%s background turn missing remote answer (prompt=%t busy=%t auth=%t error=%t rate=%t api=%t api-status=%s network=%t permission=%t submit-hint=%t completed=%d failed=%d baseline=%d); PTY content suppressed",
			agent, backgroundPromptVisible, backgroundAgentBusy, backgroundAuth, backgroundError,
			backgroundRateLimit, backgroundAPIError, backgroundAPIStatus, backgroundNetworkError, backgroundPermission, backgroundSubmitHint, completed, failed, baselineSequence)
	})
	waitE2E(t, 25*time.Second, func() bool {
		latest, exists := findContainerSession(t, runtime, controller, "client-a", session.SessionID)
		return exists && latest.ActivitySequences[model.NotificationTaskCompleted] >= baselineSequence+3
	}, func() string { return agent + " background turn produced an answer but no native Stop event" })
	waitE2E(t, 25*time.Second, func() bool {
		data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		if err != nil {
			return false
		}
		var current ducklord.ActivityState
		if json.Unmarshal(data, &current) != nil {
			return false
		}
		entry := current.Sessions[identity.Key()]
		return entry.Observed[model.NotificationTaskCompleted] >= baselineSequence+3 && entry.Unread[model.NotificationTaskCompleted]
	}, func() string { return agent + " native background completion did not mark its Session unread" })
	var projectRow, projectMarker, sessionMarker, quickBullet, quickHeader bool
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		geometry := ducklord.CalculateWorkspaceGeometry(int(cols), 28, 4)
		quick := liveScreenQuickColumn(screen)
		projectRow = liveScreenHasRow(screen, "Live "+agent, geometry.Projects)
		projectMarker = liveScreenHasMarkedRow(screen, "Live "+agent, true, geometry.Projects)
		sessionMarker = liveScreenHasMarkedRow(screen, "", true, quick)
		quickBullet = liveScreenHasRow(screen, "•", quick)
		quickHeader = quick.Width > 0
		return projectMarker && sessionMarker
	}, func() string {
		return fmt.Sprintf("%s background unread marker absent (project-row=%t project-marker=%t session-marker=%t quick-bullet=%t quick-header=%t); screen suppressed", agent, projectRow, projectMarker, sessionMarker, quickBullet, quickHeader)
	})
	writePTY(t, terminal, "P")
	waitLiveAgentScreen(t, capture, "Session list pane:", 20*time.Second)
	writePTY(t, terminal, "/"+handle+"\r")
	waitLiveAgentScreen(t, capture, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	waitLiveAgentScreen(t, capture, "Session focus:", 20*time.Second)
	waitE2E(t, 15*time.Second, func() bool {
		data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		if err != nil {
			return false
		}
		var current ducklord.ActivityState
		if json.Unmarshal(data, &current) != nil {
			return false
		}
		entry := current.Sessions[identity.Key()]
		return entry.Seen[model.NotificationTaskCompleted] >= baselineSequence+3 && !entry.Unread[model.NotificationTaskCompleted]
	}, func() string { return agent + " unread Session did not clear after browsing its live pane" })
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		geometry := ducklord.CalculateWorkspaceGeometry(int(cols), 28, 4)
		quick := liveScreenQuickColumn(screen)
		return quick.Width > 0 && liveScreenHasMarkedRow(screen, "Live "+agent, false, geometry.Projects) && !liveScreenHasMarkedRow(screen, "", true, quick)
	}, func() string {
		return agent + " unread marker remained visible after browsing the Session pane; screen suppressed"
	})
	if latest, found := findContainerSession(t, runtime, controller, "client-a", session.SessionID); !found || latest.RuntimeGeneration != session.RuntimeGeneration {
		t.Fatal("interactive agent shell changed identity during TUI navigation")
	}
	time.Sleep(4 * time.Second) // Exceeds the notifier deadline; catch duplicate delayed deliveries.
	log, err := exec.Command(runtime, "exec", controller, "cat", home+"/notifications.log").Output()
	if err != nil || strings.Count(string(log), handle) != baselineNotifications+3 || strings.Contains(string(log), "LIVE_"+strings.ToUpper(agent)+"_") {
		t.Fatalf("%s native turns did not produce exactly three private-safe notifications: %v (log content suppressed)", agent, err)
	}
}

func liveScreenHasMarkedRow(screen, label string, marked bool, column ducklord.WorkspaceRect) bool {
	return liveScreenHasRowMatching(screen, label, column, func(cell string) bool { return strings.HasSuffix(cell, "•") == marked })
}

func liveScreenQuickColumn(screen string) ducklord.WorkspaceRect {
	for _, line := range strings.Split(screen, "\n") {
		if index := strings.Index(line, " SESSIONS "); index >= 0 {
			width := 30
			if index < 21 { // 80-column fallback has a 24-cell Session list.
				width = 24
			}
			return ducklord.WorkspaceRect{X: index + 1, Width: width}
		}
	}
	return ducklord.WorkspaceRect{}
}

func liveScreenHasRow(screen, label string, column ducklord.WorkspaceRect) bool {
	return liveScreenHasRowMatching(screen, label, column, func(string) bool { return true })
}

func liveScreenHasRowMatching(screen, label string, column ducklord.WorkspaceRect, matches func(string) bool) bool {
	for _, line := range strings.Split(screen, "\n") {
		runes := []rune(line)
		start, end := column.X-1, column.X-1+column.Width
		if start < 0 || end > len(runes) {
			continue
		}
		cell := strings.TrimSpace(string(runes[start:end]))
		if strings.Contains(cell, label) && matches(cell) {
			return true
		}
	}
	return false
}

func killNamedContainerTUI(runtime, controller, owner string, configPaths ...string) {
	listing, err := exec.Command(runtime, "exec", controller, "ps", "-eo", "pid=,args=").Output()
	if err != nil {
		return
	}
	configPath := "/root/.ducklord/config.yaml"
	if len(configPaths) > 0 {
		configPath = configPaths[0]
	}
	want := "ducklord tui --name " + owner + " --config " + configPath
	for _, line := range strings.Split(string(listing), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 7 || strings.Join(fields[1:], " ") != want {
			continue
		}
		if pid, parseErr := strconv.Atoi(fields[0]); parseErr == nil {
			_, _ = exec.Command(runtime, "exec", controller, "kill", strconv.Itoa(pid)).CombinedOutput()
		}
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
