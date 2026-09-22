package main

// This test deliberately drives file exchange through a real PTY.  The files
// are created in the two Ducklion containers and are checked from the
// controller, so a rendered list or a mocked copy cannot satisfy the test.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestDucklordFileExchangeContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	prefix := requiredE2EEnv(t, "DUCKLORD_E2E_CONTAINER_PREFIX")
	clientA, clientB := prefix+"-ducklion-client-a", prefix+"-ducklion-client-b"
	stamp := time.Now().UnixNano()
	home := fmt.Sprintf("/tmp/ducklord-file-exchange-e2e-%d", stamp)
	owner, handle := fmt.Sprintf("file-exchange-%d", stamp), fmt.Sprintf("exchange-%x", stamp&0xffffff)
	sourceDir, targetDir := fmt.Sprintf("/home/duck/exchange-source-%d", stamp), fmt.Sprintf("/home/duck/exchange-target-%d", stamp)
	dropDir := targetDir + "/drop-dir"
	source, nested := sourceDir+"/exchange.txt", sourceDir+"/nested/nested.txt"
	prepare := fmt.Sprintf("rm -rf %s %s %s; mkdir -p %s/nested %s %s; printf 'exchange bytes %d\\n' > %s; printf 'nested bytes %d\\n' > %s", sourceDir, targetDir, sourceDir, sourceDir, targetDir, dropDir, stamp, source, stamp, nested)
	for _, client := range []string{clientA, clientB} {
		if out, err := exec.Command(runtime, "exec", "-u", "duck", client, "sh", "-lc", prepare).CombinedOutput(); err != nil {
			t.Fatalf("prepare %s fixture: %v: %s", client, err, out)
		}
		// Register cleanup immediately after each fixture is created so every
		// later failure path still removes the files it owns.
		client := client
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", "-u", "duck", client, "rm", "-rf", sourceDir, targetDir).CombinedOutput()
		})
	}
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientB, "sh", "-lc", "rm -rf "+targetDir+"/*").CombinedOutput(); err != nil {
		t.Fatalf("clear destination fixture: %v: %s", err, out)
	}

	// Give the TUI a live terminal with an owner-specific HOME. The route is
	// opened from this terminal, then its close path is proved by delivering a
	// unique sentinel to the PTY.
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated HOME: %v: %s", err, out)
	}
	configPath := home + "/.ducklord/config.yaml"
	if out, err := exec.Command(runtime, "exec", controller, "cp", "/root/.ducklord/config.yaml", configPath).CombinedOutput(); err != nil {
		t.Fatalf("copy isolated config: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-rf", home).CombinedOutput() })
	start := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "start", "client-a", "--name", handle, "--kind", "shell", "--cwd", "/home/duck", "--config", configPath, "--", "sh")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start exchange shell: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", owner, "destroy", "client-a", handle, "--config", configPath).CombinedOutput()
	})
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", "ducklord", "tui", "--name", owner+"-tui", "--config", configPath)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 32, Cols: 150})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x03"))
		time.Sleep(200 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner+"-tui")
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 32, 150)
	capture.waitCurrent(t, handle, 20*time.Second)

	// The workspace/session-list opener must work before a terminal receives
	// focus. Close it with Esc, then exercise the command-palette action too.
	writePTY(t, terminal, "f")
	capture.waitCurrent(t, "Project files", 10*time.Second)
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool { return !strings.Contains(capture.currentText(), "Project files") }, func() string { return "Project-list f did not close" })
	writePTY(t, terminal, "\x02 ")
	capture.waitCurrent(t, "Command palette", 10*time.Second)
	writePTY(t, terminal, "Project files\r")
	capture.waitCurrent(t, "Project files", 10*time.Second)
	writePTY(t, terminal, "\x1b")

	// Focus the terminal, establish the sentinel, and open the route through
	// the focused-terminal shortcut.  The palette route is checked separately.
	writePTY(t, terminal, "\r")
	writePTY(t, terminal, "printf FILE_EXCHANGE_SENTINEL\n")
	capture.waitCurrent(t, "FILE_EXCHANGE_SENTINEL", 10*time.Second)
	writePTY(t, terminal, "\x02f")
	capture.waitCurrent(t, "Project files", 10*time.Second)
	current := strings.ToLower(capture.currentText())
	if !strings.Contains(current, "local") || !strings.Contains(current, "project") {
		t.Fatalf("file exchange did not render endpoint choices: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner+"-output", "send", "client-a", handle, "printf ASYNC_EXCHANGE_OUTPUT\n", "--config", configPath).CombinedOutput(); err != nil {
		t.Fatalf("send async shell output: %v: %s", err, out)
	}
	capture.waitCurrent(t, "Project files", 10*time.Second)

	// Choose Hosts explicitly, then choose client-a from the host list. The
	// endpoint picker is a real modal: h opens it, j selects Hosts, and Enter
	// confirms each level. This keeps host choice deterministic when more than
	// one configured host is present.
	writePTY(t, terminal, "hj\r")
	capture.waitCurrent(t, "client-a", 10*time.Second)
	writePTY(t, terminal, "g"+strings.Repeat("\b", 256)+sourceDir+"\r")
	capture.waitCurrent(t, "exchange.txt", 15*time.Second)
	if !strings.Contains(capture.currentText(), "nested") {
		t.Fatalf("source directory was not listed: %s", safeTerminalDiagnostic(capture.currentText()))
	}

	// Search is scoped to the current directory. Select the source file, then
	// make the destination column active and choose client-b explicitly.
	writePTY(t, terminal, "/exchange.txt\r")
	capture.waitCurrent(t, "exchange.txt", 10*time.Second)
	writePTY(t, terminal, " \thjj\r")
	capture.waitCurrent(t, "client-b", 10*time.Second)
	writePTY(t, terminal, "g"+strings.Repeat("\b", 256)+targetDir+"\r")
	capture.waitCurrent(t, "Copy", 10*time.Second)
	// c is owned by the left/source column. Return focus there before opening
	// the preview, otherwise a destination key must remain a no-op.
	writePTY(t, terminal, "\tc")
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	if !strings.Contains(strings.ToLower(capture.currentText()), "skip") || !strings.Contains(strings.ToLower(capture.currentText()), "rename") || !strings.Contains(strings.ToLower(capture.currentText()), "overwrite") {
		t.Fatalf("copy preview omitted conflict policies: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	writePTY(t, terminal, "\r")
	waitE2E(t, 20*time.Second, func() bool {
		out, err := exec.Command(runtime, "exec", "-u", "duck", clientB, "cat", targetDir+"/exchange.txt").Output()
		return err == nil && strings.Contains(string(out), fmt.Sprintf("exchange bytes %d", stamp))
	}, func() string {
		return "client-a -> client-b copy did not produce source bytes: " + safeTerminalDiagnostic(capture.currentText())
	})
	assertRemoteText := func(client, path, want, failure string) {
		t.Helper()
		waitE2E(t, 20*time.Second, func() bool {
			out, err := exec.Command(runtime, "exec", "-u", "duck", client, "cat", path).Output()
			return err == nil && strings.Contains(string(out), want)
		}, func() string { return failure + ": " + safeTerminalDiagnostic(capture.currentText()) })
	}

	// The Project shelf is persistent for this Project. Select it in the
	// destination column, then return to the source column before copying.
	writePTY(t, terminal, "\thjjj\r")
	capture.waitCurrent(t, "PROJECT SHELF", 10*time.Second)
	writePTY(t, terminal, "\tc")
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	writePTY(t, terminal, "\r")
	waitE2E(t, 20*time.Second, func() bool {
		out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", "find "+home+"/.ducklord/exchange -type f -name exchange.txt -print -quit 2>/dev/null | xargs -r cat").Output()
		return err == nil && strings.Contains(string(out), fmt.Sprintf("exchange bytes %d", stamp))
	}, func() string {
		return "Project shelf copy did not produce source bytes: " + safeTerminalDiagnostic(capture.currentText())
	})

	// Clear the current-directory query, select the directory by keyboard, and
	// copy it to client-b. No fixed mouse coordinate is used: a rendered row is
	// not evidence that a drag changed the transfer source.
	writePTY(t, terminal, "/\r")
	writePTY(t, terminal, "j k") // mark exchange.txt, then return to nested for a multi-select drag
	writePTY(t, terminal, "\thjj\r")
	writePTY(t, terminal, "g"+strings.Repeat("\b", 256)+targetDir+"\r")
	capture.waitCurrent(t, "drop-dir", 10*time.Second)
	// At 150x32 the first source and destination rows are y=17. The columns
	// occupy x=41..74 and x=77..110; press the nested directory, then release
	// on the visible drop-dir row. This exercises drag/drop onto a directory.
	writePTY(t, terminal, "\t")
	writePTY(t, terminal, "\x1b[<0;65;16M\x1b[<0;95;16m")
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	writePTY(t, terminal, "\r")
	assertRemoteText(clientB, dropDir+"/nested/nested.txt", fmt.Sprintf("nested bytes %d", stamp), "directory copy did not preserve nested bytes")
	assertRemoteText(clientA, source, fmt.Sprintf("exchange bytes %d", stamp), "source was modified by copy")

	// Reverse the direction: choose client-b as the source and local client-a
	// as the destination. This catches one-way controller routing and proves
	// that endpoint changes do not leave the other column's stale listing active.
	writePTY(t, terminal, "hjj\r")
	writePTY(t, terminal, "g"+strings.Repeat("\b", 256)+sourceDir+"\r")
	capture.waitCurrent(t, "exchange.txt", 10*time.Second)
	writePTY(t, terminal, "\th\r")
	writePTY(t, terminal, "g"+strings.Repeat("\b", 256)+targetDir+"\r")
	capture.waitCurrent(t, "drop-dir", 10*time.Second)
	writePTY(t, terminal, "\tj ")
	writePTY(t, terminal, "c")
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	writePTY(t, terminal, "\r")
	assertRemoteText(clientA, targetDir+"/exchange.txt", fmt.Sprintf("exchange bytes %d", stamp), "reverse client-b -> client-a copy failed")

	// Return to client-a -> client-b for the conflict policy checks.
	writePTY(t, terminal, "hj\r")
	writePTY(t, terminal, "g"+strings.Repeat("\b", 256)+sourceDir+"\r")
	writePTY(t, terminal, "\thjj\r")
	writePTY(t, terminal, "g"+strings.Repeat("\b", 256)+targetDir+"\r")

	// Make a conflicting destination deliberately, then prove each policy by
	// inspecting bytes and the rename result, rather than only closing the UI.
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientB, "sh", "-lc", "printf 'conflict bytes\\n' > "+targetDir+"/exchange.txt").CombinedOutput(); err != nil {
		t.Fatalf("seed conflict: %v: %s", err, out)
	}
	writePTY(t, terminal, " k ") // unmark nested, select and mark exchange.txt
	writePTY(t, terminal, "\tc")
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	writePTY(t, terminal, "s\r")
	assertRemoteText(clientB, targetDir+"/exchange.txt", "conflict bytes", "skip policy changed the destination")
	writePTY(t, terminal, "c")
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	writePTY(t, terminal, "r\r")
	assertRemoteText(clientB, targetDir+"/exchange.txt (1)", fmt.Sprintf("exchange bytes %d", stamp), "rename policy did not create a suffixed file")
	writePTY(t, terminal, "c")
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	writePTY(t, terminal, "o\r")
	assertRemoteText(clientB, targetDir+"/exchange.txt", fmt.Sprintf("exchange bytes %d", stamp), "overwrite policy did not replace the destination")

	// Esc closes the browse modal and restores the original terminal. Assert
	// that closure before sending the sentinel, so a focused modal cannot mask
	// a failed focus restoration.
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool { return !strings.Contains(capture.currentText(), "Project files") }, func() string {
		return "Esc did not close file exchange: " + safeTerminalDiagnostic(capture.currentText())
	})
	closed := fmt.Sprintf("CLOSED_%d", stamp)
	writePTY(t, terminal, fmt.Sprintf("printf 'FILE_%%s\\n' '%s'\n", closed))
	capture.waitCurrent(t, "FILE_"+closed, 15*time.Second)

}
