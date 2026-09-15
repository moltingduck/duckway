package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// Native stty measurements and Ducklion inventory are the contract oracle;
// rendered pane dimensions alone cannot prove that a preview did not resize.
func TestDucklordPaneControlContractContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" || os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on a disposable Host")
	}
	for _, managed := range []bool{false, true} {
		name := "shared-shell"
		if managed {
			name = "foreign-managed-owner"
		}
		t.Run(name, func(t *testing.T) { runPaneControlContractE2E(t, managed) })
	}
}

func runPaneControlContractE2E(t *testing.T, managed bool) {
	t.Helper()
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	const hostContainer = "ducklion-client-a"
	const config = "/root/.ducklord/config.yaml"
	stamp := time.Now().UnixNano()
	handle := fmt.Sprintf("contract-%d", stamp)
	creator := fmt.Sprintf("contract-creator-%d", stamp)
	viewer := fmt.Sprintf("contract-viewer-%d", stamp)
	ttyFile := "/tmp/" + handle + ".tty"
	inputFile := "/tmp/" + handle + ".input"
	program := "stty rows 47 cols 151; tty > " + ttyFile
	kindArgs := []string{"--kind", "shell"}
	if managed {
		kindArgs = []string{"--agent", "fixture"}
		program = "stty rows 47 cols 151 -echo; tty > " + ttyFile + "; while IFS= read -r line; do printf '%s\\n' \"$line\" >> " + inputFile + "; done"
	}
	args := []string{"exec", controller, "ducklord", "--name", creator, "start", "client-a", "--name", handle}
	args = append(args, kindArgs...)
	args = append(args, "--cwd", "/home/duck", "--config", config, "--", "sh")
	if managed {
		args = append(args, "-c", program)
	}
	if err := exec.Command(runtime, args...).Run(); err != nil {
		t.Fatalf("start native contract fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", creator, "destroy", "client-a", handle, "--config", config).CombinedOutput()
		_, _ = exec.Command(runtime, "exec", hostContainer, "rm", "-f", ttyFile, inputFile).CombinedOutput()
	})
	if !managed {
		if err := exec.Command(runtime, "exec", controller, "ducklord", "--name", creator, "send", "client-a", handle, program, "--config", config).Run(); err != nil {
			t.Fatalf("initialize native shell dimensions: %v", err)
		}
	}
	var baseline ducklord.RemoteSession
	var ttyPath string
	waitE2E(t, 15*time.Second, func() bool {
		for _, session := range listContainerRemoteSessions(t, runtime, controller, "client-a") {
			if session.Name == handle && session.RuntimeGeneration > 0 {
				baseline = session
			}
		}
		out, err := exec.Command(runtime, "exec", hostContainer, "cat", ttyFile).Output()
		ttyPath = strings.TrimSpace(string(out))
		return baseline.SessionID != "" && err == nil && regexp.MustCompile(`^/dev/pts/[0-9]+$`).MatchString(ttyPath)
	}, func() string { return "native fixture did not publish its PTY and inventory" })
	if managed && (baseline.WriterID != creator || baseline.WriterKind != "terminal") {
		t.Fatalf("managed fixture must belong to its creator, got %s/%s", baseline.WriterKind, baseline.WriterID)
	}
	dimensions := func() string {
		t.Helper()
		out, err := exec.Command(runtime, "exec", hostContainer, "stty", "-F", ttyPath, "size").Output()
		if err != nil {
			t.Fatalf("read native PTY dimensions: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := dimensions(); got != "47 151" {
		t.Fatalf("fixture PTY size = %q, want 47 151", got)
	}
	assertOwner := func() {
		t.Helper()
		got, ok := findContainerSession(t, runtime, controller, "client-a", baseline.SessionID)
		if !ok || got.WriterKind != baseline.WriterKind || got.WriterID != baseline.WriterID || got.OwnershipEpoch != baseline.OwnershipEpoch || got.RuntimeGeneration != baseline.RuntimeGeneration {
			t.Fatal("pane navigation/focus changed remote owner, ownership epoch, or runtime identity")
		}
	}
	assertStable := func(want, stage string) {
		t.Helper()
		// Observe beyond a single frame so queued preview/resize work can finish.
		until := time.Now().Add(time.Second)
		for {
			assertOwner()
			if got := dimensions(); got != want {
				t.Fatalf("%s resized remote PTY: got %s, want %s", stage, got, want)
			}
			if time.Now().After(until) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	identity, ok := ducklord.IdentityFromSession(baseline)
	if !ok {
		t.Fatal("fixture has no stable identity")
	}
	state := ducklord.NewActivityState()
	for _, projectName := range []string{"Contract A", "Contract B"} {
		id, err := state.ProjectLayout.AddProject(projectName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := state.ProjectLayout.Place(id, identity, ducklord.PlaceNewTab, ""); err != nil {
			t.Fatal(err)
		}
	}
	home := "/tmp/ducklord-" + handle
	if err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").Run(); err != nil {
		t.Fatal(err)
	}
	localState := filepath.Join(t.TempDir(), "state.json")
	if err := (ducklord.ActivityStateStore{Path: localState}).Save(state); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(runtime, "cp", localState, controller+":"+home+"/.ducklord/state.json").Run(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", "ducklord", "tui", "--name", viewer, "--config", config)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 28, Cols: 130})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killNamedContainerTUI(runtime, controller, viewer, config)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 28, 130)
	capture.waitCurrent(t, "Session list pane:", 20*time.Second)
	assertStable("47 151", "initial unfocused preview")
	writePTY(t, terminal, "Pj")
	capture.waitCurrent(t, "› Contract A", 10*time.Second)
	assertStable("47 151", "first Project preview")
	writePTY(t, terminal, "j")
	capture.waitCurrent(t, "› Contract B", 10*time.Second)
	assertStable("47 151", "shared Session Project switch")
	writePTY(t, terminal, "D/"+handle)
	capture.waitCurrent(t, "find › "+handle, 10*time.Second)
	writePTY(t, terminal, "\r")
	assertStable("47 151", "detailed search preview")
	writePTY(t, terminal, "\r")
	if managed {
		capture.waitCurrent(t, "read-only", 10*time.Second)
		writePTY(t, terminal, "987654321\r")
		assertStable("47 151", "foreign managed-owner focus/input attempt")
		if err := exec.Command(runtime, "exec", hostContainer, "test", "!", "-e", inputFile).Run(); err != nil {
			t.Fatal("foreign pane input reached managed PTY stdin")
		}
		return
	}
	// A shell accepts this different logical writer without an ownership yield.
	capture.waitCurrent(t, "Session focus:", 20*time.Second)
	waitE2E(t, 10*time.Second, func() bool { return dimensions() != "47 151" }, func() string { return "focused shell did not drive native PTY dimensions" })
	assertOwner()
	marker := fmt.Sprintf("CONTRACT_INPUT_%d", stamp)
	writePTY(t, terminal, fmt.Sprintf("printf '%%s%%s\\n' 'CONTRACT_' 'INPUT_%d'\r", stamp))
	waitE2E(t, 10*time.Second, func() bool {
		out, err := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", baseline.SessionID, "--lines", "80", "--config", "/tmp/e2e-inspector.yaml").Output()
		return err == nil && strings.Contains(string(out), marker)
	}, func() string { return "focused shared-shell input did not reach original remote PTY" })
	focusedSize := dimensions()
	writePTY(t, terminal, "\x1d")
	capture.waitCurrent(t, "Detailed Sessions:", 10*time.Second)
	writePTY(t, terminal, "Dk")
	capture.waitCurrent(t, "› Contract A", 10*time.Second)
	assertStable(focusedSize, "return to unfocused Project navigation")
}
