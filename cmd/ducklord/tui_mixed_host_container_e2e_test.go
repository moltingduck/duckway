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

// The split spans separate Ducklion instances, not two Sessions on one Host.
// Native per-Host files prove where TUI input executed; command echo alone is
// not an input-routing oracle. Project navigation deliberately leaves the
// quick-list cursor on A while the second phase sends input to B.
func TestDucklordMixedHostProjectContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" || os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on disposable Hosts")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	binary := os.Getenv("DUCKLORD_E2E_BINARY")
	if binary == "" {
		binary = "ducklord"
	}
	const config = "/root/.ducklord/config.yaml"
	stamp := time.Now().UnixNano()
	creator := fmt.Sprintf("mixed-creator-%d", stamp)
	viewer := fmt.Sprintf("mixed-viewer-%d", stamp)
	inputFile := fmt.Sprintf("/tmp/ducklord-mixed-%d.input", stamp)
	hosts := []string{"client-a", "client-b"}
	containers := []string{"ducklion-client-a", "ducklion-client-b"}
	handles := []string{fmt.Sprintf("mha%x", stamp&0xffffff), fmt.Sprintf("mhb%x", stamp&0xffffff)}
	sessions := make([]ducklord.RemoteSession, 2)
	identities := make([]ducklord.SessionIdentity, 2)
	for i, host := range hosts {
		if out, err := exec.Command(runtime, "exec", controller, binary, "--name", creator, "start", host, "--name", handles[i],
			"--kind", "shell", "--cwd", "/home/duck", "--config", config, "--", "bash").CombinedOutput(); err != nil {
			t.Fatalf("start %s fixture: %v (output bytes=%d)", host, err, len(out))
		}
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", controller, binary, "--name", creator, "destroy", host, handles[i], "--config", config).CombinedOutput()
			_, _ = exec.Command(runtime, "exec", containers[i], "rm", "-f", inputFile).CombinedOutput()
		})
		waitE2E(t, 15*time.Second, func() bool {
			for _, candidate := range listContainerRemoteSessions(t, runtime, controller, host) {
				if candidate.Name == handles[i] && candidate.RuntimeGeneration > 0 {
					identity, ok := ducklord.IdentityFromSession(candidate)
					if ok {
						sessions[i], identities[i] = candidate, identity
						return true
					}
				}
			}
			return false
		}, func() string { return "mixed-host fixture missing on " + host })
		if err := exec.Command(runtime, "exec", controller, binary, "--name", creator, "send", host, handles[i],
			": > "+inputFile, "--config", config).Run(); err != nil {
			t.Fatalf("initialize %s input oracle: %v", host, err)
		}
		waitE2E(t, 10*time.Second, func() bool {
			return exec.Command(runtime, "exec", containers[i], "test", "-f", inputFile).Run() == nil
		}, func() string { return "native input oracle not initialized on " + host })
	}
	if identities[0].InstanceID == identities[1].InstanceID {
		t.Fatal("mixed-host fixture requires distinct Ducklion instances")
	}
	state := ducklord.NewActivityState()
	projectID, err := state.ProjectLayout.AddProject("Mixed Hosts")
	if err != nil {
		t.Fatal(err)
	}
	first, err := state.ProjectLayout.Place(projectID, identities[0], ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ProjectLayout.Place(projectID, identities[1], ducklord.PlaceHorizontal, first); err != nil {
		t.Fatal(err)
	}
	home := fmt.Sprintf("/tmp/ducklord-mixed-%d", stamp)
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
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		binary, "tui", "--name", viewer, "--config", config)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 36, Cols: 150})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killNamedContainerTUI(runtime, controller, viewer, config)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 36, 150)
	capture.waitCurrent(t, "Mixed Hosts", 20*time.Second)
	writePTY(t, terminal, "/"+handles[0]+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool { return !strings.Contains(capture.currentText(), "search ›") },
		func() string { return "search did not close before Project navigation" })
	writePTY(t, terminal, "P")
	capture.waitCurrent(t, "Project pane:", 10*time.Second)
	capture.waitCurrent(t, "› Mixed Hosts", 10*time.Second)
	// Refresh both panes twice after selection: a cached snapshot or a single
	// selected Host stream cannot satisfy simultaneous new markers each round.
	for round := 1; round <= 2; round++ {
		markers := []string{fmt.Sprintf("MIX_A_%d_%x", round, stamp&0xffffff), fmt.Sprintf("MIX_B_%d_%x", round, stamp&0xffffff)}
		for i, host := range hosts {
			program := fmt.Sprintf("printf '%%s%%s\\n' 'MIX_' '%s'", strings.TrimPrefix(markers[i], "MIX_"))
			if err := exec.Command(runtime, "exec", controller, binary, "--name", creator, "send", host, handles[i], program, "--config", config).Run(); err != nil {
				t.Fatalf("send fresh output to %s: %v", host, err)
			}
		}
		waitE2E(t, 20*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, markers[0]) && strings.Contains(screen, markers[1])
		}, func() string {
			return "mixed-host split did not display both fresh markers: " + safeTerminalDiagnostic(capture.currentText())
		})
	}
	want := []string{"", ""}
	readInput := func(i int) string {
		t.Helper()
		out, err := exec.Command(runtime, "exec", containers[i], "cat", inputFile).Output()
		if err != nil {
			t.Fatalf("read native %s input oracle: %v", hosts[i], err)
		}
		return string(out)
	}
	for phase, target := range []int{0, 1, 0} {
		if phase > 0 {
			writePTY(t, terminal, "\x1d")
			capture.waitCurrent(t, "Project pane:", 10*time.Second)
			key := "L"
			if target == 0 {
				key = "H"
			}
			writePTY(t, terminal, key)
		}
		writePTY(t, terminal, "\r")
		capture.waitCurrent(t, "▣ "+hosts[target]+"/"+handles[target], 20*time.Second)
		capture.waitCurrent(t, "› "+handles[0]+" @client-a", 10*time.Second)
		marker := fmt.Sprintf("ROUTE_%d_%x", phase, stamp&0xffffff)
		// The complete marker never occurs in the echoed command; tee both
		// renders real output and records actual execution on the remote Host.
		writePTY(t, terminal, fmt.Sprintf("printf '%%s%%s\\n' 'ROUTE_' '%d_%x' | tee -a %s\r", phase, stamp&0xffffff, inputFile))
		want[target] += marker + "\n"
		waitE2E(t, 15*time.Second, func() bool { return readInput(target) == want[target] },
			func() string { return "focused input did not execute exactly once on " + hosts[target] })
		capture.waitCurrent(t, marker, 15*time.Second)
		// Poll the exact contents of BOTH files beyond the output barrier to
		// catch duplicate/broadcast routing, including delayed writes.
		until := time.Now().Add(time.Second)
		for {
			for i := range hosts {
				if got := readInput(i); got != want[i] {
					t.Fatalf("phase %d: %s input log = %q, want %q", phase, hosts[i], got, want[i])
				}
			}
			if time.Now().After(until) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	for i, host := range hosts {
		current, ok := findContainerSession(t, runtime, controller, host, sessions[i].SessionID)
		if !ok || current.InstanceID != sessions[i].InstanceID || current.RuntimeGeneration != sessions[i].RuntimeGeneration ||
			current.WriterKind != sessions[i].WriterKind || current.WriterID != sessions[i].WriterID || current.OwnershipEpoch != sessions[i].OwnershipEpoch {
			t.Fatalf("mixed-host navigation/input changed %s remote identity or writer ownership", host)
		}
	}
}
