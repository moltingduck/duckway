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

// Foreground labels are advisory visibility for shell-first Sessions. This
// fixture uses Node with an agent-like argv[0], never real agent credentials.
func TestDucklordForegroundAgentLabelsContainerE2E(t *testing.T) {
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
	owner := fmt.Sprintf("foreground-e2e-%d", stamp)
	cliOwner := fmt.Sprintf("foreground-cli-%d", stamp)
	state := ducklord.NewActivityState()
	projectName := "Foreground Fixture"
	projectID, err := state.ProjectLayout.AddProject(projectName)
	if err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		agent, handle string
		session       ducklord.RemoteSession
	}
	fixtures := []fixture{{agent: "codex", handle: fmt.Sprintf("fgc%x", stamp&0xffffff)},
		{agent: "claude", handle: fmt.Sprintf("fgl%x", (stamp+1)&0xffffff)}}
	for i := range fixtures {
		item := &fixtures[i]
		out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "start", "client-a", "--name", item.handle,
			"--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "bash").CombinedOutput()
		if err != nil {
			t.Fatalf("start %s fixture shell: %v (output bytes=%d)", item.agent, err, len(out))
		}
		handle := item.handle
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "destroy", "client-a", handle,
				"--config", "/root/.ducklord/config.yaml").CombinedOutput()
		})
		waitE2E(t, 15*time.Second, func() bool {
			for _, listed := range listContainerSessions(t, runtime, controller, "client-a") {
				if listed.Handle == handle {
					var found bool
					item.session, found = findContainerSession(t, runtime, controller, "client-a", listed.SessionID)
					return found && item.session.RuntimeGeneration > 0
				}
			}
			return false
		}, func() string { return "foreground fixture shell not listed: " + handle })
		identity, ok := ducklord.IdentityFromSession(item.session)
		if !ok {
			t.Fatal("foreground fixture has no stable identity")
		}
		if _, err := state.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
			t.Fatal(err)
		}
		// Interactive bash starts this Node child in the terminal foreground
		// process group. `exec -a` sets argv[0] without requiring OAuth.
		command := "bash -c 'exec -a " + item.agent + " /usr/bin/node -e \"process.stdin.resume()\"'"
		out, err = exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "send", "client-a", handle, command,
			"--config", "/root/.ducklord/config.yaml").CombinedOutput()
		if err != nil {
			t.Fatalf("launch %s foreground fixture: %v (output bytes=%d)", item.agent, err, len(out))
		}
		waitE2E(t, 15*time.Second, func() bool {
			latest, found := findContainerSession(t, runtime, controller, "client-a", item.session.SessionID)
			return found && latest.DetectedForeground == item.agent
		}, func() string { return item.agent + " foreground detector did not update Ducklion snapshot" })
	}
	home := fmt.Sprintf("/tmp/ducklord-foreground-e2e-%d-%d", os.Getpid(), stamp)
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
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		binary, "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x03"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner, "/root/.ducklord/config.yaml")
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 24, 120)
	capture.waitCurrent(t, projectName, 20*time.Second)
	writePTY(t, terminal, "Pj")
	capture.waitCurrent(t, "› "+projectName, 10*time.Second)
	capture.waitCurrent(t, "client-a/"+fixtures[0].handle+" [codex?]", 15*time.Second)
	writePTY(t, terminal, "]")
	capture.waitCurrent(t, "client-a/"+fixtures[1].handle+" [claude?]", 15*time.Second)
	for _, item := range fixtures {
		latest, found := findContainerSession(t, runtime, controller, "client-a", item.session.SessionID)
		if !found || latest.RuntimeGeneration != item.session.RuntimeGeneration || latest.Kind != item.session.Kind {
			t.Fatal("foreground visibility changed shell Session identity or kind")
		}
	}
	if strings.Contains(capture.currentText(), "task completed") {
		t.Fatal("foreground-only detection claimed an exact agent task completion")
	}
}
