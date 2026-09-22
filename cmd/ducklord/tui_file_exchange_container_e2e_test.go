package main

// This test drives file exchange through a real PTY and verifies the bytes in
// the two Ducklion containers. A rendered list or mocked copy cannot satisfy
// it.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"gopkg.in/yaml.v3"
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

	sourceA := fmt.Sprintf("/home/duck/exchange-a-source-%d", stamp)
	sourceB := fmt.Sprintf("/home/duck/exchange-b-source-%d", stamp)
	targetA := fmt.Sprintf("/home/duck/exchange-a-target-%d", stamp)
	targetB := fmt.Sprintf("/home/duck/exchange-b-target-%d", stamp)
	aFile, bFile := fmt.Sprintf("a-file-%d.txt", stamp), fmt.Sprintf("b-file-%d.txt", stamp)
	cancelFile, failureFile := fmt.Sprintf("cancel-%d.txt", stamp), fmt.Sprintf("failure-%d.txt", stamp)
	multiOne, multiTwo := fmt.Sprintf("multi-one-%d.txt", stamp), fmt.Sprintf("multi-two-%d.txt", stamp)
	shelfFile, bundle, emptyDir := fmt.Sprintf("shelf-%d.txt", stamp), fmt.Sprintf("bundle-%d", stamp), fmt.Sprintf("empty-%d", stamp)
	terminalMarker := fmt.Sprintf("/home/duck/terminal-ready-%d", stamp)
	failureRelease := fmt.Sprintf("/tmp/duckway-file-exchange-release-%d", stamp)
	aBytes := fmt.Sprintf("a bytes %d\n", stamp)
	bBytes := fmt.Sprintf("b bytes %d\n", stamp)
	multiOneBytes := fmt.Sprintf("multi one bytes %d\n", stamp)
	multiTwoBytes := fmt.Sprintf("multi two bytes %d\n", stamp)
	shelfBytes := fmt.Sprintf("shelf bytes %d\n", stamp)
	nestedBytes := fmt.Sprintf("nested bytes %d\n", stamp)
	cancelBytes := fmt.Sprintf("cancel bytes %d\n", stamp)
	failureBytes := fmt.Sprintf("failure bytes %d\n", stamp)

	prepareA := fmt.Sprintf("rm -rf %s %s; mkdir -p %s/%s %s/drop-dir %s/%s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s/nested.txt",
		sourceA, targetA, sourceA, bundle, targetA, sourceA, bundle, emptyDir,
		aBytes, sourceA, aFile, multiOneBytes, sourceA, multiOne, multiTwoBytes, sourceA, multiTwo,
		shelfBytes, sourceA, shelfFile, cancelBytes, sourceA, cancelFile, failureBytes, sourceA, failureFile, nestedBytes, sourceA, bundle)
	prepareB := fmt.Sprintf("rm -rf %s %s; mkdir -p %s %s/drop-dir; printf %q > %s/%s",
		sourceB, targetB, sourceB, targetB, bBytes, sourceB, bFile)
	for client, command := range map[string]string{clientA: prepareA, clientB: prepareB} {
		if out, err := exec.Command(runtime, "exec", "-u", "duck", client, "sh", "-lc", command).CombinedOutput(); err != nil {
			t.Fatalf("prepare %s fixture: %v: %s", client, err, out)
		}
		client := client
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", "-u", "duck", client, "sh", "-lc", "rm -rf "+sourceA+" "+sourceB+" "+targetA+" "+targetB).CombinedOutput()
		})
	}
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", "-u", "duck", clientA, "rm", "-f", terminalMarker, failureRelease).CombinedOutput()
	})

	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated HOME: %v: %s", err, out)
	}
	configPath := home + "/.ducklord/config.yaml"
	if out, err := exec.Command(runtime, "exec", controller, "cp", "/root/.ducklord/config.yaml", configPath).CombinedOutput(); err != nil {
		t.Fatalf("copy isolated config: %v: %s", err, out)
	}
	configBytes, err := exec.Command(runtime, "exec", controller, "cat", configPath).Output()
	if err != nil {
		t.Fatalf("read isolated config: %v", err)
	}
	var endpointConfig struct {
		Hosts []struct {
			Name string `yaml:"name"`
		} `yaml:"hosts"`
	}
	if err := yaml.Unmarshal(configBytes, &endpointConfig); err != nil {
		t.Fatalf("decode isolated config: %v", err)
	}
	endpointIndex := func(name string) int {
		t.Helper()
		for i, host := range endpointConfig.Hosts {
			if host.Name == name {
				return i + 1 // Local is the first picker entry.
			}
		}
		t.Fatalf("test config has no endpoint %q", name)
		return 0
	}
	clientAEndpoint, clientBEndpoint := endpointIndex("client-a"), endpointIndex("client-b")
	shelfEndpoint := len(endpointConfig.Hosts) + 1
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

	assertRemoteText := func(client, path, want, failure string) {
		t.Helper()
		waitE2E(t, 20*time.Second, func() bool {
			out, err := exec.Command(runtime, "exec", "-u", "duck", client, "cat", path).Output()
			return err == nil && string(out) == want
		}, func() string { return failure + ": " + safeTerminalDiagnostic(capture.currentText()) })
	}
	activePane := 0
	activatePane := func(want int) {
		t.Helper()
		if activePane != want {
			writePTY(t, terminal, "\t")
			activePane = want
		}
	}
	selectEndpoint := func(active int, endpoint int, path string, listed string) {
		t.Helper()
		activatePane(active)
		writePTY(t, terminal, "h"+strings.Repeat("j", endpoint)+"\r")
		if path != "" {
			writePTY(t, terminal, "g"+strings.Repeat("\b", 256)+path+"\r")
		}
		capture.waitCurrent(t, listed, 15*time.Second)
	}
	selectOnly := func(name string) {
		t.Helper()
		writePTY(t, terminal, "/"+name+"\r")
		capture.waitCurrent(t, name, 10*time.Second)
		writePTY(t, terminal, " ")
	}
	copyPreview := func(policy string) {
		t.Helper()
		start := capture.position()
		writePTY(t, terminal, "c")
		capture.waitAfter(t, start, "Copy preview", 10*time.Second)
		if policy != "" {
			writePTY(t, terminal, policy)
		}
		start = capture.position()
		writePTY(t, terminal, "\r")
		capture.waitAfter(t, start, "Copied", 20*time.Second)
	}
	waitProjectFiles := func() {
		t.Helper()
		// The palette retains its selected "Project files" command while it is
		// still open. Filter, path, and endpoint editors share the footer, so
		// wait until the actual browse view has returned before using entry rows.
		waitE2E(t, 15*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "Tab switch column") &&
				!strings.Contains(screen, "Filter:") &&
				!strings.Contains(screen, "Path:") &&
				!strings.Contains(screen, "Endpoint picker")
		}, func() string {
			return "Project Files did not reach browse mode: " + safeTerminalDiagnostic(capture.currentText())
		})
	}
	assertRemoteMissing := func(client, path, failure string) {
		t.Helper()
		out, err := exec.Command(runtime, "exec", "-u", "duck", client, "sh", "-lc", "test ! -e "+path).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s; screen: %s", failure, err, out, safeTerminalDiagnostic(capture.currentText()))
		}
	}
	assertNoExchangeStage := func(client, path, failure string) {
		t.Helper()
		command := fmt.Sprintf("test -z \"$(find %q -mindepth 1 -maxdepth 1 -name '.exchange-*' -print -quit)\"", path)
		out, err := exec.Command(runtime, "exec", "-u", "duck", client, "sh", "-lc", command).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s; screen: %s", failure, err, out, safeTerminalDiagnostic(capture.currentText()))
		}
	}
	waitExchangeStage := func(client, path, failure string) {
		t.Helper()
		command := fmt.Sprintf("find %q -mindepth 1 -maxdepth 1 -name '.exchange-*' -print -quit", path)
		waitE2E(t, 15*time.Second, func() bool {
			out, err := exec.Command(runtime, "exec", "-u", "duck", client, "sh", "-lc", command).Output()
			return err == nil && strings.TrimSpace(string(out)) != ""
		}, func() string { return failure + "; screen: " + safeTerminalDiagnostic(capture.currentText()) })
	}
	controlledReadFailuresInstalled := false
	restoreControlledReadFailures := func() {
		t.Helper()
		if !controlledReadFailuresInstalled {
			return
		}
		if out, err := exec.Command(runtime, "exec", "-u", "0", clientA, "sh", "-lc", "if test -e /usr/local/bin/ducklion-real; then mv -f /usr/local/bin/ducklion-real /usr/local/bin/ducklion; fi").CombinedOutput(); err != nil {
			t.Fatalf("restore controlled Ducklion read wrapper: %v: %s", err, out)
		}
		controlledReadFailuresInstalled = false
	}
	installControlledReadFailures := func() {
		t.Helper()
		// The real Ducklion binary still handles every operation except these two
		// fixture reads. Sleeping before the cancel file's archive begins makes the
		// busy Ctrl-C path observable without relying on transfer throughput. The
		// failure file exits before emitting an archive, which must leave the
		// destination's staging directory uncommitted and cleaned up.
		// Register restoration before changing the client binary. If constructing
		// the wrapper fails after the move, Cleanup still puts Ducklion back.
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", "-u", "0", clientA, "sh", "-lc", "if test -e /usr/local/bin/ducklion-real; then mv -f /usr/local/bin/ducklion-real /usr/local/bin/ducklion; fi").CombinedOutput()
		})
		wrapper := fmt.Sprintf(`set -eu
mv /usr/local/bin/ducklion /usr/local/bin/ducklion-real
cat >/usr/local/bin/ducklion <<'EOF'
#!/bin/sh
case "$1:$2:$3" in
files:read:%s) sleep 30 ;;
files:read:%s) while test ! -e %q; do sleep 1; done; exit 17 ;;
esac
exec /usr/local/bin/ducklion-real "$@"
EOF
chmod 0755 /usr/local/bin/ducklion`, sourceA+"/"+cancelFile, sourceA+"/"+failureFile, failureRelease)
		if out, err := exec.Command(runtime, "exec", "-u", "0", clientA, "sh", "-lc", wrapper).CombinedOutput(); err != nil {
			t.Fatalf("install controlled Ducklion read wrapper: %v: %s", err, out)
		}
		controlledReadFailuresInstalled = true
	}

	// The quick list may initially select a demo session. Select and focus the
	// shell created for this exchange so prefix, async-output, and close checks
	// all exercise its actual PTY.
	writePTY(t, terminal, "/"+handle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 15*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 15*time.Second)

	// The palette opens from the focused exchange shell and returns there when
	// closed, proving the palette route independently of the terminal prefix.
	writePTY(t, terminal, "\x02 ")
	capture.waitCurrent(t, "Command palette", 10*time.Second)
	writePTY(t, terminal, "Project files\r")
	waitProjectFiles()
	writePTY(t, terminal, "\x03")
	capture.waitCurrent(t, "Session focus:", 10*time.Second)

	// A terminal prefix opens the same modal. Output arriving while it is open
	// must remain hidden by the modal, then appear after closing it.
	ready := fmt.Sprintf("TERMINAL_READY_%d", stamp)
	writePTY(t, terminal, fmt.Sprintf("printf %q > %s; printf '%s\\n'\r", ready+"\n", terminalMarker, ready))
	capture.waitCurrent(t, ready, 10*time.Second)
	assertRemoteText(clientA, terminalMarker, ready+"\n", "terminal sentinel did not execute in the selected exchange shell")
	writePTY(t, terminal, "\x02f")
	waitProjectFiles()
	async := fmt.Sprintf("ASYNC_EXCHANGE_OUTPUT_%d", stamp)
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner+"-output", "send", "client-a", handle, fmt.Sprintf("printf '%s\\n'", async), "--config", configPath).CombinedOutput(); err != nil {
		t.Fatalf("send async shell output: %v: %s", err, out)
	}
	waitProjectFiles()
	writePTY(t, terminal, "\x03")
	capture.waitCurrent(t, async, 15*time.Second)

	// Ctrl-] returns to the list and f opens the documented list-level route.
	// Send them in one PTY write: the input decoder must split both key events.
	writePTY(t, terminal, "\x1df")
	waitProjectFiles()

	// Client A -> client B file, directory drag/drop, and a two-file selection.
	selectEndpoint(0, clientAEndpoint, sourceA, aFile)
	selectEndpoint(1, clientBEndpoint, targetB, "drop-dir")
	activatePane(0)
	selectOnly(aFile)
	// Cancelling a preview must leave the destination untouched; this covers
	// the modal cancellation path without making timing-dependent claims about
	// cancellation after a real copy has already started.
	writePTY(t, terminal, "c")
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	writePTY(t, terminal, "\x1b")
	waitProjectFiles()
	assertRemoteMissing(clientB, targetB+"/"+aFile, "cancelled preview wrote a destination file")
	copyPreview("")
	assertRemoteText(clientB, targetB+"/"+aFile, aBytes, "client-a -> client-b file copy failed")
	assertRemoteText(clientA, sourceA+"/"+aFile, aBytes, "copy modified client-a source")

	writePTY(t, terminal, "/"+bundle+"\r")
	capture.waitCurrent(t, bundle, 10*time.Second)
	// The filter editor also renders its query. Wait for browse mode before
	// resolving a mouse row so the drag starts on the entry region, not text
	// that briefly matched while the query was being submitted.
	waitProjectFiles()
	leftX, leftY, ok := projectFilesScreenPoint(capture, bundle, 0, 74)
	if !ok {
		t.Fatalf("could not locate source directory %q: %s", bundle, safeTerminalDiagnostic(capture.currentText()))
	}
	rightX, rightY, ok := projectFilesScreenPoint(capture, "drop-dir", 75, 150)
	if !ok {
		t.Fatalf("could not locate destination directory: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	writePTY(t, terminal, fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", leftX, leftY, rightX, rightY))
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	capture.waitCurrent(t, "From: client-a: "+sourceA, 10*time.Second)
	capture.waitCurrent(t, "To: client-b: "+targetB+"/drop-dir", 10*time.Second)
	capture.waitCurrent(t, bundle, 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Copied", 20*time.Second)
	assertRemoteText(clientB, targetB+"/drop-dir/"+bundle+"/nested.txt", nestedBytes, "dragged directory copy failed")
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientB, "test", "-d", targetB+"/drop-dir/"+bundle+"/"+emptyDir).CombinedOutput(); err != nil {
		t.Fatalf("dragged directory copy omitted empty directory: %v: %s; screen: %s", err, out, safeTerminalDiagnostic(capture.currentText()))
	}

	// Dragging onto drop-dir makes that child the pending destination; reset the
	// target pane before the following root-level multiselect transfer.
	selectEndpoint(1, clientBEndpoint, targetB, "drop-dir")
	selectEndpoint(0, clientAEndpoint, sourceA, multiOne)
	selectOnly(multiOne)
	writePTY(t, terminal, "/"+multiTwo+"\r")
	capture.waitCurrent(t, multiTwo, 10*time.Second)
	waitProjectFiles()
	writePTY(t, terminal, " ")
	copyPreview("")
	assertRemoteText(clientB, targetB+"/"+multiOne, multiOneBytes, "first multiselect file was not copied")
	assertRemoteText(clientB, targetB+"/"+multiTwo, multiTwoBytes, "second multiselect file was not copied")

	// Client B -> client A is a separate direction with different bytes.
	selectEndpoint(0, clientBEndpoint, sourceB, bFile)
	selectEndpoint(1, clientAEndpoint, targetA, "drop-dir")
	activatePane(0)
	selectOnly(bFile)
	copyPreview("")
	assertRemoteText(clientA, targetA+"/"+bFile, bBytes, "client-b -> client-a copy failed")

	// The per-project shelf survives closing and reopening the modal.
	selectEndpoint(0, clientAEndpoint, sourceA, shelfFile)
	selectOnly(shelfFile)
	selectEndpoint(1, shelfEndpoint, "", "PROJECT SHELF")
	activatePane(0)
	copyPreview("")
	waitE2E(t, 20*time.Second, func() bool {
		out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", fmt.Sprintf("find %q -type f -name %q -exec cat {} \\;", home, shelfFile)).Output()
		return err == nil && string(out) == shelfBytes
	}, func() string {
		return "project shelf did not retain exact copied bytes: " + safeTerminalDiagnostic(capture.currentText())
	})
	// Project Files was opened from the Session list, so close to that same
	// route and reopen through its list-level binding. A terminal prefix here
	// would be delivered to the list rather than opening Project Files.
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "Session list pane:") &&
			strings.Contains(screen, "Active · Enter again to focus") &&
			!strings.Contains(screen, "Tab switch column")
	}, func() string {
		return "Project Files did not fully close to the Session list: " + safeTerminalDiagnostic(capture.currentText())
	})
	writePTY(t, terminal, "f")
	waitProjectFiles()
	activePane = 0 // every new Project files modal starts on the left.
	selectEndpoint(0, clientBEndpoint, targetB, "drop-dir")
	selectEndpoint(1, shelfEndpoint, "", shelfFile)
	activatePane(1)
	selectOnly(shelfFile)
	copyPreview("")
	assertRemoteText(clientB, targetB+"/"+shelfFile, shelfBytes, "project shelf -> client-b file copy failed")

	// Seed one real destination conflict, then prove all policies by bytes.
	selectEndpoint(0, clientAEndpoint, sourceA, aFile)
	selectOnly(aFile)
	selectEndpoint(1, clientBEndpoint, targetB, "drop-dir")
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientB, "sh", "-lc", fmt.Sprintf("printf %q > %s/%s", "conflict bytes\n", targetB, aFile)).CombinedOutput(); err != nil {
		t.Fatalf("seed conflict: %v: %s", err, out)
	}
	activatePane(0)
	copyPreview("s")
	assertRemoteText(clientB, targetB+"/"+aFile, "conflict bytes\n", "skip policy changed destination bytes")
	copyPreview("r")
	assertRemoteText(clientB, targetB+"/"+aFile+" (1)", aBytes, "rename policy did not create suffixed file")
	copyPreview("o")
	assertRemoteText(clientB, targetB+"/"+aFile, aBytes, "overwrite policy did not replace destination bytes")

	// Make one source read block before it emits an archive, then cancel from
	// the busy state. The destination writer has already opened its isolated
	// staging directory, so this proves Ctrl-C joins both sides without exposing
	// a partial destination or leaving staging behind.
	installControlledReadFailures()
	selectEndpoint(0, clientAEndpoint, sourceA, cancelFile)
	selectEndpoint(1, clientBEndpoint, targetB, "drop-dir")
	activatePane(0)
	selectOnly(cancelFile)
	writePTY(t, terminal, "c\r")
	capture.waitCurrent(t, "Copying", 10*time.Second)
	waitExchangeStage(clientB, targetB, "cancelled transfer never created destination staging")
	writePTY(t, terminal, "\x03")
	capture.waitCurrent(t, "Cancelled: copied 0, skipped 0", 15*time.Second)
	waitProjectFiles()
	assertRemoteText(clientA, sourceA+"/"+cancelFile, cancelBytes, "cancelled transfer modified source bytes")
	assertRemoteMissing(clientB, targetB+"/"+cancelFile, "cancelled transfer committed destination file")
	assertNoExchangeStage(clientB, targetB, "cancelled transfer left destination staging")

	// A source-side remote read failure follows the same two-phase protocol.
	// It must return to browsing, preserve the source, and never commit either
	// the final destination name or its temporary staging directory.
	selectEndpoint(0, clientAEndpoint, sourceA, failureFile)
	activatePane(0)
	selectOnly(failureFile)
	writePTY(t, terminal, "c\r")
	capture.waitCurrent(t, "Copying", 10*time.Second)
	waitExchangeStage(clientB, targetB, "failed transfer never created destination staging")
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientA, "touch", failureRelease).CombinedOutput(); err != nil {
		t.Fatalf("release controlled failed source read: %v: %s", err, out)
	}
	capture.waitCurrent(t, "Copied 0, skipped 0; partial:", 15*time.Second)
	waitProjectFiles()
	assertRemoteText(clientA, sourceA+"/"+failureFile, failureBytes, "failed transfer modified source bytes")
	assertRemoteMissing(clientB, targetB+"/"+failureFile, "failed transfer committed destination file")
	assertNoExchangeStage(clientB, targetB, "failed transfer left destination staging")
	restoreControlledReadFailures()

	// This modal was opened from list focus, so close must return to the list.
	// Re-enter the selected shell explicitly before proving terminal ownership.
	writePTY(t, terminal, "\x03")
	capture.waitCurrent(t, "SESSIONS", 10*time.Second)
	writePTY(t, terminal, "/"+handle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 10*time.Second)
	closed := fmt.Sprintf("CLOSED_%d", stamp)
	startAt := capture.position()
	writePTY(t, terminal, fmt.Sprintf("printf %q > %s; printf 'FILE_%%s\\n' '%s'\r", "FILE_"+closed+"\n", terminalMarker, closed))
	capture.waitAfter(t, startAt, "FILE_"+closed, 15*time.Second)
	assertRemoteText(clientA, terminalMarker, "FILE_"+closed+"\n", "terminal sentinel did not execute after returning from list-origin Project files")
}

func projectFilesScreenPoint(capture *tuiCapture, label string, minX, maxX int) (int, int, bool) {
	capture.mu.Lock()
	screen := strings.Join(capture.screen.RenderLines(capture.rows, capture.cols), "\n")
	capture.mu.Unlock()
	return workspaceScreenPoint(screen, label, minX, maxX)
}
