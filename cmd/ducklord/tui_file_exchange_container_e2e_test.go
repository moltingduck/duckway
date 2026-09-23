package main

// This test drives file exchange through a real PTY and verifies the bytes in
// the two Ducklion containers. A rendered list or mocked copy cannot satisfy
// it.

import (
	"bytes"
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
	"github.com/hackerduck/duckway/internal/ducklord"
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
	notStartedFile := fmt.Sprintf("not-started-%d.txt", stamp)
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

	prepareA := fmt.Sprintf("rm -rf %s %s; mkdir -p %s/%s %s/drop-dir %s/%s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s; printf %q > %s/%s/nested.txt",
		sourceA, targetA, sourceA, bundle, targetA, sourceA, bundle, emptyDir,
		aBytes, sourceA, aFile, multiOneBytes, sourceA, multiOne, multiTwoBytes, sourceA, multiTwo,
		shelfBytes, sourceA, shelfFile, cancelBytes, sourceA, cancelFile, failureBytes, sourceA, failureFile, []byte("not started\n"), sourceA, notStartedFile, nestedBytes, sourceA, bundle)
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
	// A browser opened from the focused client-a shell keeps LOCAL on the left
	// and starts the destination on that session's remote cwd.
	currentEndpoints := [2]string{"LOCAL", endpointConfig.Hosts[clientAEndpoint-1].Name}
	activatePane := func(want int) {
		t.Helper()
		if activePane != want {
			writePTY(t, terminal, "\t")
			activePane = want
		}
		waitE2E(t, 10*time.Second, func() bool {
			return projectFilesPanelContains(capture, want, "ACTIVE") && !projectFilesPanelContains(capture, 1-want, "ACTIVE")
		}, func() string {
			return fmt.Sprintf("Project Files did not acknowledge pane %d activation: %s", want, projectFilesOwnershipDiagnostic(capture))
		})
	}
	paneContains := func(side int, value string) bool {
		return projectFilesPanelContains(capture, side, value)
	}
	endpointLabel := func(endpoint int) string {
		switch {
		case endpoint == 0:
			return "LOCAL"
		case endpoint <= len(endpointConfig.Hosts):
			return endpointConfig.Hosts[endpoint-1].Name
		default:
			return "PROJECT SHELF"
		}
	}
	selectEndpoint := func(active int, endpoint int, path string, listed string) {
		t.Helper()
		currentEndpoints[active] = endpointLabel(endpoint)
		activatePane(active)
		writePTY(t, terminal, "h")
		capture.waitCurrent(t, "Endpoint picker", 10*time.Second)
		writePTY(t, terminal, strings.Repeat("j", endpoint)+"\r")
		if path != "" {
			writePTY(t, terminal, "g")
			capture.waitCurrent(t, "Path:", 10*time.Second)
			writePTY(t, terminal, strings.Repeat("\b", 256)+path+"\r")
		}
		waitE2E(t, 15*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "Tab switch column") &&
				paneContains(active, endpointLabel(endpoint)) &&
				(path == "" || paneContains(active, projectFilesVisiblePath(capture, path))) &&
				!paneContains(active, "Loading…") &&
				(listed == "" || paneContains(active, listed))
		}, func() string {
			return "endpoint pane did not finish loading the requested entry: " + safeTerminalDiagnostic(capture.currentText())
		})
	}
	applyFilter := func(name string) {
		t.Helper()
		writePTY(t, terminal, "/")
		waitE2E(t, 10*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "Filter:") && strings.Contains(screen, "Enter apply · Esc cancel")
		}, func() string {
			return "filter editor did not open: " + projectFilesOwnershipDiagnostic(capture)
		})
		// Browse summaries retain the previous query. Replace it explicitly so
		// selection is independent of whichever item was filtered last.
		writePTY(t, terminal, strings.Repeat("\b", 256)+name+"\r")
		waitE2E(t, 10*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "Tab switch column") &&
				!strings.Contains(screen, "Enter apply · Esc cancel") &&
				paneContains(activePane, name)
		}, func() string {
			return "filter did not resolve the requested active-pane entry: " + projectFilesOwnershipDiagnostic(capture)
		})
	}
	selectOnly := func(name string) {
		t.Helper()
		applyFilter(name)
		writePTY(t, terminal, " ")
		waitE2E(t, 10*time.Second, func() bool {
			return projectFilesPanelRowContains(capture, activePane, name, "[x]")
		}, func() string {
			return "source entry was not selected in its active pane: " + projectFilesOwnershipDiagnostic(capture)
		})
	}
	selectAdditional := func(name, alreadySelected string) {
		t.Helper()
		applyFilter(name)
		writePTY(t, terminal, " ")
		waitE2E(t, 10*time.Second, func() bool {
			return projectFilesPanelRowContains(capture, activePane, name, "[x]")
		}, func() string {
			return "additional source entry was not selected: " + projectFilesOwnershipDiagnostic(capture)
		})
		// Filtering hides unrelated rows, so clear the query before proving that
		// both the newly selected item and the prior selection remain checked.
		writePTY(t, terminal, "/")
		waitE2E(t, 10*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "Filter:") && strings.Contains(screen, "Enter apply · Esc cancel")
		}, func() string {
			return "filter editor did not reopen to clear query: " + projectFilesOwnershipDiagnostic(capture)
		})
		writePTY(t, terminal, strings.Repeat("\b", 256)+"\r")
		waitE2E(t, 10*time.Second, func() bool {
			screen := capture.currentText()
			return projectFilesPanelContains(capture, 0, "LEFT") &&
				projectFilesPanelContains(capture, 1, "RIGHT") &&
				strings.Contains(screen, "Tab switch column") &&
				!strings.Contains(screen, "Enter apply · Esc cancel") &&
				!paneContains(activePane, "Filter:")
		}, func() string {
			return "cleared source filter did not return to browse mode: " + projectFilesOwnershipDiagnostic(capture)
		})
		waitE2E(t, 10*time.Second, func() bool {
			return projectFilesPanelRowContains(capture, activePane, name, "[x]") &&
				projectFilesPanelRowContains(capture, activePane, alreadySelected, "[x]")
		}, func() string {
			return "clearing the filter did not preserve both source selections: " + projectFilesOwnershipDiagnostic(capture)
		})
	}
	hasCopyResult := func(screen string) bool {
		for _, line := range strings.Split(screen, "\n") {
			start := strings.Index(line, "Copied ")
			if start < 0 {
				continue
			}
			var copied, skipped int
			if _, err := fmt.Sscanf(line[start:], "Copied %d, skipped %d", &copied, &skipped); err == nil {
				return true
			}
		}
		return false
	}
	copyPreview := func(policy string) {
		t.Helper()
		writePTY(t, terminal, "c")
		// The terminal renderer emits deltas, so a raw-output search can miss a
		// frame that remains visible. The preview deliberately does not render
		// the prior browse status; waiting for its complete screen also makes a
		// stale successful-copy status unable to satisfy the next operation.
		waitE2E(t, 10*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, "Copy preview") && !strings.Contains(screen, "Copied ")
		}, func() string {
			return "copy preview did not open: " + safeTerminalDiagnostic(capture.currentText())
		})
		if policy != "" {
			writePTY(t, terminal, policy)
		}
		writePTY(t, terminal, "\r")
		waitE2E(t, 20*time.Second, func() bool {
			screen := capture.currentText()
			// Completion must be the current browse view. In particular, a
			// historical "Copied" delta cannot pass while the preview is open,
			// while the operation is still running, or after the modal has closed.
			// A partial result is an E2E failure even if some files copied.
			return strings.Contains(screen, "Tab switch column") &&
				!strings.Contains(screen, "Copy preview") &&
				!strings.Contains(screen, "Copying") &&
				hasCopyResult(screen) &&
				!strings.Contains(screen, "; partial:")
		}, func() string {
			return "copy preview did not complete successfully: " + safeTerminalDiagnostic(capture.currentText())
		})
	}
	assertRenderedTransfer := func(from, sourcePath, to, destinationPath string) {
		t.Helper()
		name := filepath.Base(sourcePath)
		sourceSide, destinationSide := -1, -1
		for side := 0; side < 2; side++ {
			if projectFilesPanelContains(capture, side, from) {
				sourceSide = side
			}
			if projectFilesPanelContains(capture, side, to) {
				destinationSide = side
			}
		}
		waitE2E(t, 10*time.Second, func() bool {
			return sourceSide >= 0 && destinationSide >= 0 && sourceSide != destinationSide &&
				projectFilesPanelRowContains(capture, sourceSide, name, "Sent") &&
				projectFilesPanelRowContains(capture, destinationSide, name, "Received")
		}, func() string {
			return fmt.Sprintf("browse rows did not mark %s sent from %s or received at %s (endpoints %s -> %s): %s", name, sourcePath, destinationPath, from, to, projectFilesOwnershipDiagnostic(capture))
		})
	}
	waitProjectFiles := func() {
		t.Helper()
		// Path is a permanent browse header now; LEFT and RIGHT are the browse
		// identity, while editor-only prompts distinguish path/filter/picker.
		waitE2E(t, 15*time.Second, func() bool {
			screen := capture.currentText()
			return projectFilesPanelContains(capture, 0, "LEFT") &&
				projectFilesPanelContains(capture, 1, "RIGHT") &&
				strings.Contains(screen, "Tab switch column") &&
				!strings.Contains(screen, "Enter apply · Esc cancel") &&
				!strings.Contains(screen, "Endpoint picker")
		}, func() string {
			return "Project Files did not reach browse mode: " + projectFilesOwnershipDiagnostic(capture)
		})
	}
	assertHistoryItem := func(sourcePath, destinationPath, sourceName, destinationName, outcome string) {
		t.Helper()
		writePTY(t, terminal, "l")
		capture.waitCurrent(t, "Transfer history", 10*time.Second)
		itemVisible := func(selected bool) bool {
			rows := projectFilesStyledRows(capture)
			for i, row := range rows {
				if (selected && !strings.Contains(row.text, "›")) || !strings.Contains(row.text, outcome) || !strings.Contains(row.text, sourceName) {
					continue
				}
				// A narrow history modal may wrap the destination basename onto
				// the following row. Keep item identity separate from full paths.
				visibleText := row.text
				if i+1 < len(rows) && strings.HasPrefix(strings.TrimSpace(rows[i+1].text), "→") {
					visibleText += " " + rows[i+1].text
				}
				if strings.Contains(visibleText, destinationName) {
					return true
				}
			}
			return false
		}
		selectedHistoryRow := func() string {
			for _, row := range projectFilesStyledRows(capture) {
				if strings.Contains(row.text, "›") {
					return row.text
				}
			}
			return ""
		}
		for i := 0; i < 32 && !itemVisible(true); i++ {
			if i == 31 {
				t.Fatalf("history did not select %s item %q → %q: %s", outcome, sourceName, destinationName, projectFilesOwnershipDiagnostic(capture))
			}
			previous := selectedHistoryRow()
			writePTY(t, terminal, "j")
			waitE2E(t, 3*time.Second, func() bool { return selectedHistoryRow() != previous }, func() string {
				return "history selection did not advance: " + projectFilesOwnershipDiagnostic(capture)
			})
		}
		waitE2E(t, 10*time.Second, func() bool {
			screen := capture.currentText()
			compact := strings.ReplaceAll(screen, "\n", "")
			return itemVisible(true) && strings.Contains(screen, "Source:") && strings.Contains(screen, "Destination:") &&
				strings.Contains(compact, sourcePath) && strings.Contains(compact, destinationPath)
		}, func() string {
			return fmt.Sprintf("selected %s history item did not expose full source/actual destination paths %q → %q: %s", outcome, sourcePath, destinationPath, projectFilesOwnershipDiagnostic(capture))
		})
		writePTY(t, terminal, "\x1b")
		waitProjectFiles()
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
	if !paneContains(0, "LOCAL") || !paneContains(1, endpointConfig.Hosts[clientAEndpoint-1].Name) {
		t.Fatalf("wide file exchange did not render the documented list-origin endpoints (LOCAL -> %s): %s", endpointConfig.Hosts[clientAEndpoint-1].Name, projectFilesOwnershipDiagnostic(capture))
	}
	resizeTUICapture(t, terminal, capture, 32, 50)
	waitProjectFiles()

	// Client A -> client B file, directory drag/drop, and a two-file selection.
	selectEndpoint(0, clientAEndpoint, sourceA, aFile)
	selectEndpoint(1, clientBEndpoint, targetB, "drop-dir")
	if paneContains(0, "ACTIVE") || !paneContains(1, "ACTIVE") {
		t.Fatalf("narrow stacked panels did not preserve the right pane as active: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	activatePane(0)
	if !paneContains(0, "ACTIVE") || paneContains(1, "ACTIVE") {
		t.Fatalf("narrow stacked panels did not switch active ownership to the left: %s", safeTerminalDiagnostic(capture.currentText()))
	}
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

	applyFilter(bundle)
	// The filter editor also renders its query. Wait for browse mode before
	// resolving a mouse row so the drag starts on the entry region, not text
	// that briefly matched while the query was being submitted.
	waitProjectFiles()
	writePTY(t, terminal, "i")
	waitE2E(t, 5*time.Second, func() bool {
		return paneContains(0, "[D] "+bundle)
	}, func() string {
		return "ASCII folder icon toggle did not label the source directory: " + safeTerminalDiagnostic(capture.currentText())
	})
	writePTY(t, terminal, "i")
	waitE2E(t, 5*time.Second, func() bool {
		return paneContains(0, "📁 "+bundle)
	}, func() string {
		return "folder icon toggle did not restore the folder glyph: " + safeTerminalDiagnostic(capture.currentText())
	})
	leftX, leftY, ok := projectFilesScreenPointAny(capture, "📁 "+bundle)
	if !ok {
		t.Fatalf("could not locate source directory %q: %s", bundle, safeTerminalDiagnostic(capture.currentText()))
	}
	rightX, rightY, ok := projectFilesScreenPointAny(capture, "📁 drop-dir")
	if !ok {
		t.Fatalf("could not locate destination directory: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	writePTY(t, terminal, fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", leftX, leftY, rightX, rightY))
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	capture.waitCurrent(t, "From: client-a:", 10*time.Second)
	capture.waitCurrent(t, "To: client-b:", 10*time.Second)
	capture.waitCurrent(t, bundle, 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Copied", 20*time.Second)
	assertRemoteText(clientB, targetB+"/drop-dir/"+bundle+"/nested.txt", nestedBytes, "dragged directory copy failed")
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientB, "test", "-d", targetB+"/drop-dir/"+bundle+"/"+emptyDir).CombinedOutput(); err != nil {
		t.Fatalf("dragged directory copy omitted empty directory: %v: %s; screen: %s", err, out, safeTerminalDiagnostic(capture.currentText()))
	}
	resizeTUICapture(t, terminal, capture, 32, 150)
	assertRenderedTransfer("client-a", sourceA+"/"+bundle, "client-b", targetB+"/drop-dir/"+bundle)
	waitProjectFiles()
	// The narrow drag commits drop-dir as the visible destination path. Return
	// the right pane to targetB before the wide drag so the child row exists as
	// a hit target again.
	selectEndpoint(1, clientBEndpoint, targetB, "drop-dir")
	activatePane(0)
	// Resolve the same drag targets again after widening. This exercises mouse
	// hit testing on each layout without relying on fixed pane header offsets.
	leftX, leftY, ok = projectFilesScreenPointAny(capture, "📁 "+bundle)
	if !ok {
		t.Fatalf("could not locate source directory after widening: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	rightX, rightY, ok = projectFilesScreenPointAny(capture, "📁 drop-dir")
	if !ok {
		t.Fatalf("could not locate destination directory after widening: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	writePTY(t, terminal, fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", leftX, leftY, rightX, rightY))
	capture.waitCurrent(t, "Copy preview", 10*time.Second)
	capture.waitCurrent(t, "From: client-a: "+sourceA, 10*time.Second)
	capture.waitCurrent(t, "To: client-b: "+targetB+"/drop-dir", 10*time.Second)
	writePTY(t, terminal, "\x1b")
	waitProjectFiles()

	// Dragging onto drop-dir makes that child the pending destination; reset the
	// target pane before the following root-level multiselect transfer.
	selectEndpoint(1, clientBEndpoint, targetB, "drop-dir")
	selectEndpoint(0, clientAEndpoint, sourceA, multiOne)
	selectOnly(multiOne)
	selectAdditional(multiTwo, multiOne)
	copyPreview("")
	assertRemoteText(clientB, targetB+"/"+multiOne, multiOneBytes, "first multiselect file was not copied")
	assertRemoteText(clientB, targetB+"/"+multiTwo, multiTwoBytes, "second multiselect file was not copied")
	assertRenderedTransfer("client-a", sourceA+"/"+multiOne, "client-b", targetB+"/"+multiOne)
	assertRenderedTransfer("client-a", sourceA+"/"+multiTwo, "client-b", targetB+"/"+multiTwo)
	// History navigation must leave focus in the modal while terminal output is
	// arriving, and Esc must return to this browser with its active side intact.
	writePTY(t, terminal, "l")
	capture.waitCurrent(t, "Transfer history", 10*time.Second)
	capture.waitCurrent(t, multiOne, 5*time.Second)
	capture.waitCurrent(t, multiTwo, 5*time.Second)
	writePTY(t, terminal, "\x1b[D\x1b[C\x1b[A\x1b[B")
	asyncHistory := fmt.Sprintf("ASYNC_EXCHANGE_HISTORY_%d", stamp)
	asyncHistoryPath := sourceA + "/" + asyncHistory
	asyncCommand := fmt.Sprintf("printf %q > %q; printf '%s\\n'", asyncHistory+"\n", asyncHistoryPath, asyncHistory)
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "--name", owner+"-output", "send", "client-a", handle, asyncCommand, "--config", configPath).CombinedOutput(); err != nil {
		t.Fatalf("send async shell output during transfer history: %v: %s", err, out)
	}
	assertRemoteText(clientA, asyncHistoryPath, asyncHistory+"\n", "async history shell output did not execute")
	if strings.Contains(capture.currentText(), asyncHistory) {
		t.Fatalf("transfer history lost modal ownership to async terminal output: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	writePTY(t, terminal, "\x1b")
	waitProjectFiles()
	if !paneContains(0, "ACTIVE") || paneContains(1, "ACTIVE") {
		t.Fatalf("history Esc did not restore the same active browser side: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	// Closing history only reveals the Project Files modal. Close that modal as
	// well, refocus the owning shell, and prove its queued output is delivered
	// only after terminal ownership is restored. Then reopen the browser for the
	// remaining transfer checks.
	writePTY(t, terminal, "\x03")
	capture.waitCurrent(t, "SESSIONS", 10*time.Second)
	writePTY(t, terminal, "/"+handle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 10*time.Second)
	capture.waitCurrent(t, asyncHistory, 15*time.Second)
	// Re-enter through the Session list so the later close assertion exercises
	// the same list-origin route. A fresh browser starts on the left, with the
	// active session host as its destination.
	writePTY(t, terminal, "\x1df")
	waitProjectFiles()
	activePane = 0
	currentEndpoints = [2]string{"LOCAL", endpointConfig.Hosts[clientAEndpoint-1].Name}
	if !paneContains(0, "LOCAL") || !paneContains(1, endpointConfig.Hosts[clientAEndpoint-1].Name) {
		t.Fatalf("reopened list-origin Project Files did not start with documented endpoints (LOCAL -> %s): %s", endpointConfig.Hosts[clientAEndpoint-1].Name, projectFilesOwnershipDiagnostic(capture))
	}

	// Client B -> client A is a separate direction with different bytes.
	selectEndpoint(0, clientBEndpoint, sourceB, bFile)
	selectEndpoint(1, clientAEndpoint, targetA, "drop-dir")
	activatePane(0)
	selectOnly(bFile)
	copyPreview("")
	assertRemoteText(clientA, targetA+"/"+bFile, bBytes, "client-b -> client-a copy failed")
	assertRenderedTransfer("client-b", sourceB+"/"+bFile, "client-a", targetA+"/"+bFile)

	// The per-project shelf survives closing and reopening the modal.
	selectEndpoint(0, clientAEndpoint, sourceA, shelfFile)
	selectOnly(shelfFile)
	selectEndpoint(1, shelfEndpoint, "", "")
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
			!strings.Contains(screen, "Tab switch column")
	}, func() string {
		return "Project Files did not fully close to the Session list: " + safeTerminalDiagnostic(capture.currentText())
	})
	writePTY(t, terminal, "f")
	waitProjectFiles()
	activePane = 0 // every new Project files modal starts on the left.
	currentEndpoints = [2]string{"LOCAL", endpointConfig.Hosts[clientAEndpoint-1].Name}
	// History belongs to the project, not the modal instance. Reopen it before
	// starting another copy so this proves the previous modal's transfer remains.
	writePTY(t, terminal, "l")
	capture.waitCurrent(t, "Transfer history", 10*time.Second)
	capture.waitCurrent(t, shelfFile, 5*time.Second)
	reopenedShelfOutcome := false
	for _, row := range projectFilesStyledRows(capture) {
		if strings.Contains(row.text, "Copied") && strings.Contains(row.text, shelfFile) {
			reopenedShelfOutcome = true
			break
		}
	}
	if !strings.Contains(capture.currentText(), "Copied") || !strings.Contains(capture.currentText(), "PROJECT SHELF") ||
		!strings.Contains(capture.currentText(), sourceA) || !strings.Contains(capture.currentText(), home) ||
		strings.Contains(capture.currentText(), targetB) || !reopenedShelfOutcome {
		t.Fatalf("reopened transfer history did not show the latest client-a -> project shelf batch: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	writePTY(t, terminal, "\x1b")
	waitProjectFiles()
	if !paneContains(0, "ACTIVE") || paneContains(1, "ACTIVE") {
		t.Fatalf("history Esc after modal reopen did not restore the left browser side: %s", safeTerminalDiagnostic(capture.currentText()))
	}
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
	if projectFilesPanelRowContains(capture, 0, aFile, "Sent") || projectFilesPanelRowContains(capture, 1, aFile, "Received") {
		t.Fatalf("skipped file was marked as transferred in browse rows: %s", projectFilesOwnershipDiagnostic(capture))
	}
	assertHistoryItem(sourceA+"/"+aFile, targetB+"/"+aFile, aFile, aFile, "Skipped")
	copyPreview("r")
	assertRemoteText(clientB, targetB+"/"+aFile+" (1)", aBytes, "rename policy did not create suffixed file")
	assertHistoryItem(sourceA+"/"+aFile, targetB+"/"+aFile+" (1)", aFile, aFile+" (1)", "Copied")
	copyPreview("o")
	assertRemoteText(clientB, targetB+"/"+aFile, aBytes, "overwrite policy did not replace destination bytes")
	assertHistoryItem(sourceA+"/"+aFile, targetB+"/"+aFile, aFile, aFile, "Copied")

	// Make one source read block before it emits an archive, then cancel from
	// the busy state. The destination writer has already opened its isolated
	// staging directory, so this proves Ctrl-C joins both sides without exposing
	// a partial destination or leaving staging behind.
	installControlledReadFailures()
	selectEndpoint(0, clientAEndpoint, sourceA, cancelFile)
	selectEndpoint(1, clientBEndpoint, targetB, "drop-dir")
	activatePane(0)
	selectOnly(cancelFile)
	selectAdditional(notStartedFile, cancelFile)
	writePTY(t, terminal, "c\r")
	capture.waitCurrent(t, "Copying", 10*time.Second)
	waitExchangeStage(clientB, targetB, "cancelled transfer never created destination staging")
	writePTY(t, terminal, "\x03")
	capture.waitCurrent(t, "Cancelled: copied 0, skipped 0", 15*time.Second)
	waitProjectFiles()
	assertHistoryItem(sourceA+"/"+cancelFile, targetB+"/"+cancelFile, cancelFile, cancelFile, "Cancelled")
	assertHistoryItem(sourceA+"/"+notStartedFile, targetB+"/"+notStartedFile, notStartedFile, notStartedFile, "Not started")
	assertRemoteText(clientA, sourceA+"/"+cancelFile, cancelBytes, "cancelled transfer modified source bytes")
	assertRemoteMissing(clientB, targetB+"/"+cancelFile, "cancelled transfer committed destination file")
	assertNoExchangeStage(clientB, targetB, "cancelled transfer left destination staging")

	// A source-side remote read failure follows the same two-phase protocol.
	// It must return to browsing, preserve the source, and never commit either
	// the final destination name or its temporary staging directory.
	selectEndpoint(0, clientAEndpoint, sourceA, failureFile)
	activatePane(0)
	selectOnly(failureFile)
	selectAdditional(notStartedFile, failureFile)
	writePTY(t, terminal, "c\r")
	capture.waitCurrent(t, "Copying", 10*time.Second)
	waitExchangeStage(clientB, targetB, "failed transfer never created destination staging")
	if out, err := exec.Command(runtime, "exec", "-u", "duck", clientA, "touch", failureRelease).CombinedOutput(); err != nil {
		t.Fatalf("release controlled failed source read: %v: %s", err, out)
	}
	capture.waitCurrent(t, "Copied 0, skipped 0; partial:", 15*time.Second)
	waitProjectFiles()
	assertHistoryItem(sourceA+"/"+failureFile, targetB+"/"+failureFile, failureFile, failureFile, "Failed")
	assertHistoryItem(sourceA+"/"+notStartedFile, targetB+"/"+notStartedFile, notStartedFile, notStartedFile, "Not started")
	assertRemoteText(clientA, sourceA+"/"+failureFile, failureBytes, "failed transfer modified source bytes")
	assertRemoteMissing(clientB, targetB+"/"+failureFile, "failed transfer committed destination file")
	assertNoExchangeStage(clientB, targetB, "failed transfer left destination staging")
	restoreControlledReadFailures()

	// Clearing history is scoped to the project record and does not undo any
	// already verified destination bytes.
	writePTY(t, terminal, "l")
	capture.waitCurrent(t, "Transfer history", 10*time.Second)
	writePTY(t, terminal, "x")
	capture.waitCurrent(t, "No batches for this Project", 10*time.Second)
	writePTY(t, terminal, "\x1b")
	waitProjectFiles()
	if !paneContains(0, "ACTIVE") || paneContains(1, "ACTIVE") {
		t.Fatalf("clearing history did not restore the active browser side: %s", safeTerminalDiagnostic(capture.currentText()))
	}

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

func projectFilesScreenPointAny(capture *tuiCapture, label string) (int, int, bool) {
	capture.mu.Lock()
	screen := strings.Join(capture.screen.RenderLines(capture.rows, capture.cols), "\n")
	cols := capture.cols
	capture.mu.Unlock()
	return workspaceScreenPoint(screen, label, 1, cols+1)
}

func projectFilesPanelContains(capture *tuiCapture, side int, value string) bool {
	if side < 0 || side > 1 {
		return false
	}
	rows := projectFilesStyledRows(capture)
	anchorStyles := projectFilesPanelStyles(rows, side)
	if len(anchorStyles) == 0 {
		return false
	}
	for _, row := range rows {
		for _, match := range row.matches(value) {
			if anchorStyles[match.style] {
				return true
			}
		}
	}
	return false
}

func projectFilesOwnershipDiagnostic(capture *tuiCapture) string {
	capture.mu.Lock()
	screen := visibleTerminalText(strings.Join(capture.screen.RenderLines(capture.rows, capture.cols), "\n"))
	rows, cols := capture.rows, capture.cols
	capture.mu.Unlock()
	styledRows := projectFilesStyledRows(capture)
	panelText := func(side int) string {
		styles := projectFilesPanelStyles(styledRows, side)
		var lines []string
		for _, row := range styledRows {
			for _, style := range row.styles {
				if styles[style] {
					lines = append(lines, strings.TrimRight(row.text, " "))
					break
				}
			}
		}
		return safeTerminalDiagnostic(strings.Join(lines, "\n"))
	}
	return fmt.Sprintf("size=%dx%d LEFT-anchor=%t RIGHT-anchor=%t left-panel=%s right-panel=%s screen=%s",
		rows, cols, strings.Contains(screen, "LEFT"), strings.Contains(screen, "RIGHT"), panelText(0), panelText(1), safeTerminalDiagnostic(screen))
}

func projectFilesPanelRowContains(capture *tuiCapture, side int, label, value string) bool {
	if side < 0 || side > 1 {
		return false
	}
	rows := projectFilesStyledRows(capture)
	anchorStyles := projectFilesPanelStyles(rows, side)
	for _, row := range rows {
		for _, labelMatch := range row.matches(label) {
			if !anchorStyles[labelMatch.style] {
				continue
			}
			for _, valueMatch := range row.matches(value) {
				if valueMatch.style == labelMatch.style {
					return true
				}
			}
		}
	}
	return false
}

func projectFilesPanelStyles(rows []projectFilesStyledRow, side int) map[string]bool {
	heading := "LEFT"
	if side == 1 {
		heading = "RIGHT"
	}
	styles := map[string]bool{}
	for _, row := range rows {
		for _, match := range row.matches(heading) {
			if match.style != "" {
				styles[match.style] = true
			}
		}
	}
	return styles
}

func projectFilesVisiblePath(capture *tuiCapture, path string) string {
	capture.mu.Lock()
	cols := capture.cols
	capture.mu.Unlock()
	if cols < 58 {
		// The stacked panel clips long paths. Keep enough of the root-specific
		// prefix to distinguish source and destination, rather than accepting
		// the shared /home/duck/ prefix for either endpoint.
		return path[:min(len(path), 32)]
	}
	return path
}

type projectFilesStyledMatch struct {
	style string
}

type projectFilesStyledRow struct {
	text   string
	styles map[int]string
}

func (row projectFilesStyledRow) matches(label string) []projectFilesStyledMatch {
	var matches []projectFilesStyledMatch
	for start := 0; start < len(row.text); {
		offset := strings.Index(row.text[start:], label)
		if offset < 0 {
			break
		}
		offset += start
		cell := modalCellWidth(row.text[:offset])
		matches = append(matches, projectFilesStyledMatch{style: row.styles[cell]})
		start = offset + len(label)
	}
	return matches
}

func projectFilesStyledRows(capture *tuiCapture) []projectFilesStyledRow {
	capture.mu.Lock()
	lines := capture.screen.RenderLines(capture.rows, capture.cols)
	capture.mu.Unlock()
	ansi := regexp.MustCompile(`\x1b\[([0-9;:]*)m`)
	rows := make([]projectFilesStyledRow, 0, len(lines))
	for _, raw := range lines {
		row := projectFilesStyledRow{styles: make(map[int]string)}
		style := ""
		cell := 0
		for len(raw) > 0 {
			loc := ansi.FindStringSubmatchIndex(raw)
			chunk := raw
			if loc != nil {
				chunk = raw[:loc[0]]
			}
			for _, r := range chunk {
				row.text += string(r)
				width := modalCellWidth(string(r))
				for n := 0; n < width; n++ {
					row.styles[cell+n] = style
				}
				cell += width
			}
			if loc == nil {
				break
			}
			params := strings.Split(raw[loc[2]:loc[3]], ";")
			for i := 0; i < len(params); i++ {
				code, _ := strconv.Atoi(params[i])
				if code == 0 || code == 49 {
					style = ""
				}
				if code == 48 && i+4 < len(params) && params[i+1] == "2" {
					style = strings.Join(params[i:i+5], ";")
					i += 4
				}
			}
			raw = raw[loc[1]:]
		}
		rows = append(rows, row)
	}
	return rows
}

func TestProjectFilesPanelRenderedOwnership(t *testing.T) {
	for _, cols := range []int{150, 50} {
		t.Run(fmt.Sprintf("%d-columns", cols), func(t *testing.T) {
			state := &tuiState{projectFiles: projectFilesState{
				open: true, step: "browse", active: 0,
				left: projectFilesPane{
					label: "LOCAL", endpoint: ducklord.FileEndpoint{Path: "/left/root"},
					entries: []ducklord.FileEntry{
						{Name: "left-only.txt"},
						{Name: "selected-file.txt"},
						{Name: "selected-dir", IsDir: true},
					},
					marked: map[string]bool{"selected-file.txt": true, "selected-dir": true},
				},
				right: projectFilesPane{label: "LOCAL", endpoint: ducklord.FileEndpoint{Path: "/right/root"}, entries: []ducklord.FileEntry{{Name: "right-only.txt"}}},
			}}
			var rendered bytes.Buffer
			state.renderProjectFilesModal(&rendered, cols, 32)
			screen := ducklord.NewTerminal(32, cols, 0)
			screen.Write(rendered.Bytes())
			capture := &tuiCapture{screen: screen, rows: 32, cols: cols}
			if !projectFilesPanelContains(capture, 0, "visible") || !projectFilesPanelContains(capture, 1, "visible") {
				t.Fatalf("production renderer did not preserve both panel identities: %s", projectFilesOwnershipDiagnostic(capture))
			}
			if !projectFilesPanelContains(capture, 0, "left-only.txt") || projectFilesPanelContains(capture, 0, "right-only.txt") ||
				!projectFilesPanelContains(capture, 1, "right-only.txt") || projectFilesPanelContains(capture, 1, "left-only.txt") {
				t.Fatalf("production render lost independent panel ownership: %s", projectFilesOwnershipDiagnostic(capture))
			}
			if !projectFilesPanelRowContains(capture, 0, "left-only.txt", "[ ]") ||
				projectFilesPanelRowContains(capture, 0, "left-only.txt", "right-only.txt") {
				t.Fatal("panel row assertion did not require values on the same rendered row")
			}
			if !projectFilesPanelRowContains(capture, 0, "selected-file.txt", "[x]") ||
				!projectFilesPanelRowContains(capture, 0, "selected-dir", "[x]") {
				t.Fatalf("selected file and directory marks did not render on their entry rows: %s", projectFilesOwnershipDiagnostic(capture))
			}
		})
	}
}
