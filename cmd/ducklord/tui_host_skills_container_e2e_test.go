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

// TestDucklordHostSkillsContainerE2E proves the complete host-centred route
// and actual SSH transfer: Host list -> Host settings -> Skills -> agent ->
// skills. The fixture starts with Ducklord's quack skill locally and the
// client's meow skill remotely.
func TestDucklordHostSkillsContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	clientA := requiredE2EEnv(t, "DUCKLORD_E2E_CONTAINER_PREFIX") + "-ducklion-client-a"
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	home := "/tmp/ducklord-host-skills-e2e-" + stamp
	owner, handle := "skills-ui-"+stamp, "skills-"+stamp[len(stamp)-6:]
	config := home + "/.ducklord/config.yaml"
	repoMeow := home + "/.ducklord/skills/client-a-meow"
	repoPurr := home + "/.ducklord/skills/client-a-purr"
	remoteDir := "/home/duck/.codex/skills"
	remoteQuack, remoteMeow, remotePurr := remoteDir+"/ducklord-quack", remoteDir+"/client-a-meow", remoteDir+"/client-a-purr"
	prepare := fmt.Sprintf("mkdir -p %s/.ducklord/skills/ducklord-quack && cat > %s/.ducklord/config.yaml <<'YAML'\nname: skills-e2e\nhosts:\n  - name: client-a\n    host: client-a\n    user: duck\n    skill_targets:\n      - id: codex\n        path: /home/duck/.codex/skills\n        skills:\n          - id: ducklord-quack\n            management: push\nYAML\nprintf '%s\\n' > %s/.ducklord/skills/ducklord-quack/SKILL.md", home, home, "# Ducklord quack\n\nReply: 呱呱", home)
	if out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", prepare).CombinedOutput(); err != nil {
		t.Fatalf("prepare fixture: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-rf", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("SSH config: %v: %s", err, out)
	}
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientA, "rm", "-rf", remoteQuack).CombinedOutput(); err != nil {
		t.Fatalf("clear remote push target: %v: %s", err, out)
	}
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientA, "sh", "-lc", "test -f "+remoteMeow+"/SKILL.md && grep -F '喵喵' "+remoteMeow+"/SKILL.md").CombinedOutput(); err != nil {
		t.Fatalf("client meow fixture missing: %v: %s", err, out)
	}
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientA, "sh", "-lc", "rm -rf "+remotePurr+" && mkdir -p "+remotePurr+" && printf '%s\\n' '# Client purr\\n\\nReply: 呼嚕' > "+remotePurr+"/SKILL.md").CombinedOutput(); err != nil {
		t.Fatalf("prepare client purr fixture: %v: %s", err, out)
	}
	start := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "start", "client-a", "--name", handle, "--kind", "shell", "--cwd", "/tmp", "--config", config, "--", "sh")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start shell: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "destroy", "client-a", handle, "--config", config).CombinedOutput()
	})
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", "ducklord", "tui", "--name", owner+"-tui", "--config", config)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x03"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner+"-tui", config)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	capture.waitCurrent(t, handle, 20*time.Second)

	writePTY(t, terminal, "h")
	capture.waitCurrent(t, "Hosts", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Host actions · client-a", 10*time.Second)
	// Exercise the host resource route and verify Esc restores the action focus.
	for range []int{0, 1, 2, 3, 4, 5, 6, 7} {
		writePTY(t, terminal, "j")
	}
	waitE2E(t, 3*time.Second, func() bool { return strings.Contains(capture.currentText(), "› Resources") }, func() string { return safeTerminalDiagnostic(capture.currentText()) })
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Host resources · client-a", 15*time.Second)
	capture.waitCurrent(t, "OS / arch:", 15*time.Second)
	writePTY(t, terminal, "r")
	capture.waitCurrent(t, "OS / arch:", 15*time.Second)
	writePTY(t, terminal, "\x1b")
	capture.waitCurrent(t, "Host actions · client-a", 10*time.Second)
	if !strings.Contains(capture.currentText(), "› Resources") {
		t.Fatalf("resource Esc did not restore focus: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	for range []int{0, 1, 2, 3} {
		writePTY(t, terminal, "k")
	}
	waitE2E(t, 3*time.Second, func() bool { return strings.Contains(capture.currentText(), "› Skills") }, func() string { return safeTerminalDiagnostic(capture.currentText()) })
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Ducklord Managed Repository", 15*time.Second)
	capture.waitCurrent(t, "ducklord-quack", 15*time.Second)

	// Remote discovery runs in the background. While it is in flight, the
	// dashboard must retain both its route and the left-pane selection.
	writePTY(t, terminal, "\t")
	capture.waitCurrent(t, "Ducklord Managed Repository", 15*time.Second)
	if text := capture.currentText(); !strings.Contains(text, "ducklord-quack") {
		t.Fatalf("background remote discovery changed dashboard selection: %s", safeTerminalDiagnostic(text))
	}
	writePTY(t, terminal, "\t")

	// Ctrl-C from a live preview must discard its private staging directory and
	// close the whole nested route back to the workspace that opened it.
	writePTY(t, terminal, "\t")
	capture.waitCurrent(t, "client-a-purr", 15*time.Second)
	writePTY(t, terminal, "jjr")
	capture.waitCurrent(t, "Preview download for client-a-purr", 15*time.Second)
	if out, err := exec.Command(runtime, "exec", controller, "test", "!", "-e", repoPurr).CombinedOutput(); err != nil {
		t.Fatalf("preview wrote purr to local repo: %v: %s", err, out)
	}
	writePTY(t, terminal, "\x03")
	capture.waitCurrent(t, handle, 10*time.Second)
	if out, err := exec.Command(runtime, "exec", controller, "test", "!", "-e", repoPurr).CombinedOutput(); err != nil {
		t.Fatalf("Ctrl-C did not discard purr preview: %v: %s", err, out)
	}

	// Reopen the same route after cancellation, proving focus and navigation
	// ownership returned to the workspace before the real transfer proceeds.
	writePTY(t, terminal, "h")
	capture.waitCurrent(t, "Hosts", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Host actions · client-a", 10*time.Second)
	for range []int{0, 1, 2, 3} {
		writePTY(t, terminal, "j")
	}
	waitE2E(t, 3*time.Second, func() bool { return strings.Contains(capture.currentText(), "› Skills") }, func() string { return safeTerminalDiagnostic(capture.currentText()) })
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Ducklord Managed Repository", 15*time.Second)

	// d uploads only this agent target's push-managed skills.
	writePTY(t, terminal, "d")
	capture.waitCurrent(t, "Working on Host Skills", 10*time.Second)
	capture.waitCurrent(t, "Deployed 1 selected skill(s)", 15*time.Second)
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientA, "sh", "-lc", "test -f "+remoteQuack+"/SKILL.md && grep -F '呱呱' "+remoteQuack+"/SKILL.md").CombinedOutput(); err != nil {
		t.Fatalf("push did not upload quack: %v: %s", err, out)
	}
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Ducklord Managed Repository", 10*time.Second)

	// Delete the remote-only purr skill from the right pane and confirm with y.
	capture.waitCurrent(t, "client-a-purr", 15*time.Second)
	writePTY(t, terminal, "\tjjx")
	writePTY(t, terminal, "y")
	capture.waitCurrent(t, "Deleted client-a-purr", 15*time.Second)
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientA, "test", "!", "-e", remotePurr).CombinedOutput(); err != nil {
		t.Fatalf("remote delete did not remove purr: %v: %s", err, out)
	}
	writePTY(t, terminal, "\t")

	// Rename the managed local repository entry and verify both its directory
	// and the saved host configuration migrate together.
	writePTY(t, terminal, "m")
	capture.waitCurrent(t, "Rename managed skill", 5*time.Second)
	for range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14} {
		writePTY(t, terminal, "\x7f")
	}
	writePTY(t, terminal, "ducklord-quack-renamed\r")
	capture.waitCurrent(t, "Confirm managed skill rename", 5*time.Second)
	writePTY(t, terminal, "y")
	capture.waitCurrent(t, "Renamed ducklord-quack to ducklord-quack-renamed", 15*time.Second)
	if out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", "test -f "+home+"/.ducklord/skills/ducklord-quack-renamed/SKILL.md && test ! -e "+home+"/.ducklord/skills/ducklord-quack/SKILL.md && grep -F 'id: ducklord-quack-renamed' "+config).CombinedOutput(); err != nil {
		t.Fatalf("managed rename did not migrate repository/config: %v: %s", err, out)
	}

	// The right-pane leaf action pulls a remote-only skill, shows a staged
	// preview, then records pull only after explicit confirmation.
	writePTY(t, terminal, "\tkr")
	capture.waitCurrent(t, "Preview download for client-a-meow", 15*time.Second)
	if out, err := exec.Command(runtime, "exec", controller, "test", "!", "-e", repoMeow).CombinedOutput(); err != nil {
		t.Fatalf("preview wrote local repo: %v: %s", err, out)
	}
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Applied client-a-meow", 15*time.Second)
	if out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", "test -f "+repoMeow+"/SKILL.md && grep -F '喵喵' "+repoMeow+"/SKILL.md").CombinedOutput(); err != nil {
		t.Fatalf("pull did not import meow: %v: %s", err, out)
	}
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "[pull] client-a-meow", 10*time.Second)

	// Ctrl-C closes the complete nested route through the main dispatcher and
	// restores the workspace that owned input before h opened Host list.
	writePTY(t, terminal, "\x03")
	capture.waitCurrent(t, handle, 10*time.Second)
	// This route was opened while the workspace was unfocused, so Ctrl-C must
	// return ownership to navigation. Verify that with the navigation shortcut.
	writePTY(t, terminal, "?")
	capture.waitCurrent(t, "Keyboard shortcuts", 10*time.Second)
	writePTY(t, terminal, "?")
	capture.waitCurrent(t, handle, 10*time.Second)
}
