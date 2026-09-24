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

// TestDucklordScrollContainerE2E drives the overflow controls through a real
// PTY. Assertions use the emulator's current terminal cells, not old output.
func TestDucklordScrollContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" || os.Getenv("DUCKLORD_E2E_DISPOSABLE_HOST") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on a disposable Host")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	binary := os.Getenv("DUCKLORD_E2E_BINARY")
	if binary == "" {
		binary = "ducklord"
	}
	const config = "/root/.ducklord/config.yaml"
	stamp := time.Now().UnixNano()
	owner := fmt.Sprintf("scroll-e2e-%x", stamp)
	state := ducklord.NewActivityState()
	projectID, err := state.ProjectLayout.AddProject("Scroll active project")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		if _, err := state.ProjectLayout.AddProject(fmt.Sprintf("Scroll project %02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	const sessionCount = 10
	handles := make([]string, sessionCount)
	logs := make([]string, sessionCount)
	var firstPane string
	var firstHandle string
	for i := range handles {
		handles[i] = fmt.Sprintf("scroll-%02d-%x", i, stamp)
		logs[i] = "/tmp/" + handles[i] + ".input"
		handle, log := handles[i], logs[i]
		if out, err := exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "start", "client-a", "--name", handle,
			"--kind", "shell", "--cwd", "/home/duck", "--config", config, "--", "bash").CombinedOutput(); err != nil {
			t.Fatalf("start scroll shell: %v: %s", err, out)
		}
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "destroy", "client-a", handle, "--config", config).CombinedOutput()
			_, _ = exec.Command(runtime, "exec", e2eContainerName("ducklion-client-a"), "rm", "-f", log).CombinedOutput()
		})
		program := "export DUCKWAY_SCROLL_LOG=" + log + "; : > \"$DUCKWAY_SCROLL_LOG\""
		if out, err := exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "send", "client-a", handle, program, "--config", config).CombinedOutput(); err != nil {
			t.Fatalf("initialize scroll shell: %v: %s", err, out)
		}
		var identity ducklord.SessionIdentity
		waitE2E(t, 10*time.Second, func() bool {
			for _, summary := range listContainerSessions(t, runtime, controller, "client-a") {
				if summary.Handle == handle {
					remote, found := findContainerSession(t, runtime, controller, "client-a", summary.SessionID)
					if found {
						identity, _ = ducklord.IdentityFromSession(remote)
						return identity.SessionID != ""
					}
				}
			}
			return false
		}, func() string { return "scroll shell not listed: " + handle })
		if i == 0 {
			firstHandle = handle
			firstPane, err = state.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, "")
		} else {
			_, err = state.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, "")
		}
		if err != nil {
			t.Fatalf("place scroll shell %q: %v", handle, err)
		}
	}
	if firstPane == "" {
		t.Fatal("fixture did not create an active pane")
	}
	home := "/tmp/ducklord-" + owner
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("link SSH configuration: %v: %s", err, out)
	}
	localState := filepath.Join(t.TempDir(), "state.json")
	if err := (ducklord.ActivityStateStore{Path: localState}).Save(state); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(runtime, "cp", localState, controller+":"+home+"/.ducklord/state.json").CombinedOutput(); err != nil {
		t.Fatalf("install scroll layout: %v: %s", err, out)
	}
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", binary, "tui", "--name", owner, "--config", config)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 16, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x1bq"))
		killNamedContainerTUI(runtime, controller, owner, config)
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 16, 100)
	capture.waitCurrent(t, firstHandle, 20*time.Second)
	readLog := func() string {
		out, err := exec.Command(runtime, "exec", e2eContainerName("ducklion-client-a"), "cat", logs[0]).Output()
		if err != nil {
			t.Fatalf("read origin shell log: %v", err)
		}
		return string(out)
	}
	geometry := ducklord.CalculateWorkspaceGeometry(100, 16, 4)
	var expectedShellLog string
	assertScrollbar := func(where string, column int) {
		t.Helper()
		if column <= 0 {
			t.Fatalf("%s scrollbar column is invalid: %d", where, column)
		}
		if where == "Project list" {
			findScrollCellsInRows(capture.currentText(), column, geometry.Projects.Y+1, geometry.Projects.Y+geometry.Projects.Height-1)
		} else if where == "Session list" {
			findScrollCellsInRows(capture.currentText(), column, geometry.Quick.Y+1, geometry.Quick.Y+geometry.Quick.Height-1)
		} else {
			findScrollCellsInColumn(capture.currentText(), column)
		}
	}
	projectHeading := func(screen string) string {
		lines := strings.Split(screen, "\n")
		row := geometry.Terminal.Y - 1
		if row < 0 || row >= len(lines) {
			return ""
		}
		return strings.TrimSpace(lines[row])
	}
	paneHeading := func(screen string) string {
		lines := strings.Split(screen, "\n")
		row := geometry.Terminal.Y
		if row < 0 || row >= len(lines) {
			return ""
		}
		return strings.TrimSpace(lines[row])
	}
	selectedQuickRow := func(screen string) string {
		lines := strings.Split(screen, "\n")
		firstDataRow := max(0, geometry.Quick.Y)
		lastDataRow := min(len(lines), geometry.Quick.Y+geometry.Quick.Height-1)
		for row := firstDataRow; row < lastDataRow; row++ {
			if strings.Contains(lines[row], "›") {
				return strings.TrimSpace(lines[row])
			}
		}
		return ""
	}
	scrollFailure := func(where string) string {
		screen := capture.currentText()
		footer := ""
		for _, line := range strings.Split(screen, "\n") {
			if strings.Contains(line, "Project pane:") || strings.Contains(line, "Session list pane:") || strings.Contains(line, "Session focus: keys go to PTY") {
				footer = strings.TrimSpace(line)
			}
		}
		got := readLog()
		return fmt.Sprintf("%s: heading=%q footer=%q shellLogMatches=%t shellLog=%q want=%q\n%s", where, projectHeading(screen), footer, got == expectedShellLog, got, expectedShellLog, screen)
	}
	helpViewport := func(screen string) string {
		lines := strings.Split(screen, "\n")
		const first, afterLast = 3, 14 // Help results occupy rows 4 through 14.
		if len(lines) < afterLast {
			return ""
		}
		rows := make([]string, 0, afterLast-first)
		for _, line := range lines[first:afterLast] {
			line = strings.ReplaceAll(line, "█", " ")
			line = strings.ReplaceAll(line, "│", " ")
			rows = append(rows, strings.TrimRight(line, " "))
		}
		return strings.Join(rows, "\n")
	}

	// Establish a real Help origin instead of relying on the startup selection.
	// The firstHandle can appear in the quick list while another Project still
	// owns navigation, in which case closing Help correctly returns to that list.
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Project pane:", 5*time.Second)
	selectedTargetProject := false
	for step := 0; step < 24; step++ {
		if strings.Contains(projectHeading(capture.currentText()), "Scroll active project") {
			selectedTargetProject = true
			break
		}
		before := projectHeading(capture.currentText())
		writePTY(t, terminal, "\x1b[B")
		waitE2E(t, 5*time.Second, func() bool {
			heading := projectHeading(capture.currentText())
			return heading != "" && heading != before
		}, func() string { return "Project navigation did not advance while selecting the scroll fixture Project" })
	}
	if !selectedTargetProject && strings.Contains(projectHeading(capture.currentText()), "Scroll active project") {
		selectedTargetProject = true
	}
	if !selectedTargetProject {
		t.Fatalf("could not select Scroll active project before attaching origin PTY: heading=%q\n%s", projectHeading(capture.currentText()), safeTerminalDiagnostic(capture.currentText()))
	}
	writePTY(t, terminal, "b") // return to the Session list for this Project
	capture.waitCurrent(t, "Session list pane:", 5*time.Second)
	writePTY(t, terminal, "/"+firstHandle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus: keys go to PTY", 10*time.Second)
	originMarker := fmt.Sprintf("SCROLL_ORIGIN_%x", stamp)
	writePTY(t, terminal, fmt.Sprintf("printf '%%s\\n' '%s' | tee -a \"$DUCKWAY_SCROLL_LOG\"\r", originMarker))
	waitE2E(t, 10*time.Second, func() bool {
		expectedShellLog = readLog()
		return expectedShellLog == originMarker+"\n" && strings.Contains(capture.currentText(), originMarker)
	}, func() string { return "origin PTY did not execute its setup sentinel before Help" })

	// The Help page has many more rows than this compact terminal. Page keys
	// must move its current viewport, and pending remote output must not take
	// input ownership or close it.
	writePTY(t, terminal, "\x02?")
	capture.waitCurrent(t, "Keyboard shortcuts", 10*time.Second)
	helpColumn := (100-72)/2 + 1 + 72 - 2
	assertScrollbar("Help", helpColumn)
	helpAtStart := helpViewport(capture.currentText())
	writePTY(t, terminal, "\x1b[6~") // PageDown
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, _, _, ready := tryFindScrollCellsInColumn(screen, helpColumn)
		return ready && helpViewport(screen) != helpAtStart
	}, func() string { return "Help PageDown did not change the visible result rows" })
	helpAfterPage := helpViewport(capture.currentText())
	writePTY(t, terminal, "\x1b[5~") // PageUp
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, _, _, ready := tryFindScrollCellsInColumn(screen, helpColumn)
		return ready && helpViewport(screen) != helpAfterPage
	}, func() string { return "Help PageUp did not change the visible result rows" })
	// Move beyond the initial viewport in both directions so this proves the
	// Help-local arrows change the current result viewport, not merely its cursor.
	helpBeforeArrows := helpViewport(capture.currentText())
	writePTY(t, terminal, strings.Repeat("\x1b[B", 12))
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, _, _, ready := tryFindScrollCellsInColumn(screen, helpColumn)
		return ready && helpViewport(screen) != helpBeforeArrows
	}, func() string { return "Help Down did not move its current result viewport" })
	helpAfterDown := helpViewport(capture.currentText())
	writePTY(t, terminal, strings.Repeat("\x1b[A", 12))
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, _, _, ready := tryFindScrollCellsInColumn(screen, helpColumn)
		return ready && helpViewport(screen) != helpAfterDown
	}, func() string { return "Help Up did not move its current result viewport" })
	writePTY(t, terminal, "/session")
	capture.waitCurrent(t, "Search: session", 5*time.Second)
	writePTY(t, terminal, "\r") // pin the search filter
	capture.waitCurrent(t, "Filter: session", 5*time.Second)
	filtered := capture.currentText()
	filteredRowsBefore := helpViewport(filtered)
	writePTY(t, terminal, "\x1b[6~")
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, _, _, ready := tryFindScrollCellsInColumn(screen, helpColumn)
		return ready && helpViewport(screen) != filteredRowsBefore && strings.Contains(screen, "Filter: session")
	}, func() string { return "Help PageDown lost search or failed to change its filtered result rows" })
	// The mouse wheel and thumb drag use the actual current scrollbar cells.
	helpX, helpThumbY, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
	beforeWheelRows := helpViewport(capture.currentText())
	writePTY(t, terminal, string(workspaceMouse(64, helpX-1, helpThumbY, false)))
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, _, _, ready := tryFindScrollCellsInColumn(screen, helpColumn)
		return ready && helpViewport(screen) != beforeWheelRows && strings.Contains(screen, "Filter: session")
	}, func() string { return "Help mouse wheel did not move its thumb while preserving search" })
	_, helpThumbY, helpTrackY := findScrollCellsInColumn(capture.currentText(), helpColumn)
	beforeDragY := helpThumbY
	writePTY(t, terminal, string(workspaceMouse(0, helpX, helpThumbY, false))+string(workspaceMouse(32, helpX, helpTrackY, false))+string(workspaceMouse(0, helpX+4, helpTrackY, true)))
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, y, _, ready := tryFindScrollCellsInColumn(screen, helpColumn)
		return ready && y > beforeDragY && strings.Contains(screen, "Filter: session")
	}, func() string { return "Help thumb drag did not scroll while preserving search" })
	if got := readLog(); got != expectedShellLog {
		t.Fatalf("Help paging leaked bytes to origin shell: got %q want %q", got, expectedShellLog)
	}
	asyncMarker := fmt.Sprintf("SCROLL_ASYNC_%x", stamp)
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "send", "client-a", firstHandle,
		"sleep 1; printf '"+asyncMarker+"\\n' | tee -a \"$DUCKWAY_SCROLL_LOG\"", "--config", config).CombinedOutput(); err != nil {
		t.Fatalf("request asynchronous PTY output: %v: %s", err, out)
	}
	waitE2E(t, 10*time.Second, func() bool {
		return readLog() == expectedShellLog+asyncMarker+"\n" && strings.Contains(capture.currentText(), "Keyboard shortcuts")
	}, func() string { return "async PTY output did not arrive while Help remained open" })
	expectedShellLog += asyncMarker + "\n"
	writePTY(t, terminal, "?")
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return !strings.Contains(screen, "Keyboard shortcuts") && !strings.Contains(screen, "Help ·")
	}, func() string { return "? did not close Help in current terminal cells" })
	// Help can disappear one event before its asynchronous PTY focus lease is
	// restored. Wait for the visible terminal-focus indicator and the unique
	// output marker before sending the restoration sentinel, so a timeout tells
	// us whether focus failed independently of shell input/output.
	var restoreScreen, restoreLog string
	waitE2E(t, 10*time.Second, func() bool {
		restoreScreen = capture.currentText()
		restoreLog = readLog()
		return strings.Contains(restoreScreen, "Session focus: keys go to PTY") &&
			strings.Contains(restoreScreen, asyncMarker) && restoreLog == expectedShellLog
	}, func() string {
		return fmt.Sprintf("Help closed but origin focus/output was not restored: log=%q focused=%t asyncVisible=%t\n%s",
			restoreLog,
			strings.Contains(restoreScreen, "Session focus: keys go to PTY"),
			strings.Contains(restoreScreen, asyncMarker), safeTerminalDiagnostic(restoreScreen))
	})
	marker := fmt.Sprintf("SCROLL_RESTORED_%x", stamp)
	writePTY(t, terminal, fmt.Sprintf("printf '%%s\\n' '%s' | tee -a \"$DUCKWAY_SCROLL_LOG\"\r", marker))
	var sentinelLog string
	waitE2E(t, 10*time.Second, func() bool {
		sentinelLog = readLog()
		return sentinelLog == expectedShellLog+marker+"\n"
	}, func() string {
		return fmt.Sprintf("restored origin PTY did not execute the sentinel: log=%q want=%q\n%s",
			sentinelLog, expectedShellLog+marker+"\\n", safeTerminalDiagnostic(capture.currentText()))
	})
	expectedShellLog += marker + "\n"

	// Project and Session list overflow controls are exercised while their own
	// navigation pane owns the mouse. A scrollbar click scrolls the viewport;
	// it must not enter a Project or transfer terminal control.
	// With terminal focus, hovering the Project list and wheeling over it is a
	// consumed no-op: it must not switch the selected Session or PTY.
	writePTY(t, terminal, string(workspaceMouse(65, geometry.Projects.X+2, geometry.Projects.Y+2, false)))
	waitE2E(t, 3*time.Second, func() bool {
		screen := capture.currentText()
		_, _, _, projectReady := tryFindScrollCellsInRows(screen, geometry.Projects.X+geometry.Projects.Width-1, geometry.Projects.Y+1, geometry.Projects.Y+geometry.Projects.Height-1)
		_, _, _, sessionReady := tryFindScrollCellsInRows(screen, geometry.Quick.X+geometry.Quick.Width-1, geometry.Quick.Y+1, geometry.Quick.Y+geometry.Quick.Height-1)
		return projectReady && sessionReady && strings.Contains(screen, marker) && readLog() == expectedShellLog
	}, func() string { return "terminal focus or exact origin PTY changed after list wheel" })
	if got := readLog(); got != expectedShellLog {
		t.Fatalf("list wheel leaked input to origin shell: %q", got)
	}
	if strings.Contains(capture.currentText(), "Project pane:") || strings.Contains(capture.currentText(), "Session list pane:") {
		t.Fatalf("list wheel changed the terminal owner\n%s", safeTerminalDiagnostic(capture.currentText()))
	}
	writePTY(t, terminal, "\x1d")
	capture.waitCurrent(t, "Session list pane:", 5*time.Second)
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Project pane:", 5*time.Second)
	assertScrollbar("Project list", geometry.Projects.X+geometry.Projects.Width-1)
	if x, _, _ := findScrollCellsInRows(capture.currentText(), geometry.Projects.X+geometry.Projects.Width-1, geometry.Projects.Y+1, geometry.Projects.Y+geometry.Projects.Height-1); x != geometry.Projects.X+geometry.Projects.Width-1 {
		t.Fatalf("Project scrollbar appeared at column %d, want pane edge %d", x, geometry.Projects.X+geometry.Projects.Width-1)
	}
	projectLine := -1
	for row, line := range strings.Split(capture.currentText(), "\n") {
		if strings.Contains(line, "Scroll project 00") {
			projectLine = row
		}
	}
	if projectLine < 0 {
		t.Fatal("first synthetic Project was not visible before scrolling")
	}
	// With Project focus, a wheel over the Project list moves the selected
	// Project and its current terminal heading, while retaining navigation
	// focus and leaving the attached origin PTY untouched.
	writePTY(t, terminal, string(workspaceMouse(65, geometry.Projects.X+2, geometry.Projects.Y+2, false)))
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(projectHeading(screen), "Scroll project 02") &&
			strings.Contains(screen, "Project pane:") &&
			!strings.Contains(screen, "Session focus: keys go to PTY") && readLog() == expectedShellLog
	}, func() string {
		return scrollFailure("Project-list wheel did not advance the selected Project while retaining Project focus")
	})
	if got := readLog(); got != expectedShellLog {
		t.Fatalf("Project-list wheel leaked input to the origin PTY: %q", got)
	}
	writePTY(t, terminal, string(workspaceMouse(64, geometry.Projects.X+2, geometry.Projects.Y+2, false)))
	waitE2E(t, 5*time.Second, func() bool {
		return strings.Contains(projectHeading(capture.currentText()), "Scroll active project")
	}, func() string { return scrollFailure("Project-list wheel did not restore the origin Project selection") })
	projectColumn := geometry.Projects.X + geometry.Projects.Width - 1
	writePTY(t, terminal, "\x1b[6~")
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(projectHeading(screen), "Scroll project 02") && strings.Contains(screen, "Project pane:")
	}, func() string {
		return scrollFailure("Project PageDown did not select the third visible Project while retaining Project focus")
	})
	writePTY(t, terminal, "\x1b[5~")
	waitE2E(t, 5*time.Second, func() bool {
		return strings.Contains(projectHeading(capture.currentText()), "Scroll active project")
	}, func() string { return scrollFailure("Project PageUp did not restore the original selected Project") })
	projectTrackX := projectColumn
	projectTrackY := geometry.Projects.Y + geometry.Projects.Height - 1
	for page := 0; page < 8 && !strings.Contains(capture.currentText(), "Scroll project 15"); page++ {
		beforeHeading := projectHeading(capture.currentText())
		writePTY(t, terminal, string(workspaceMouse(0, projectTrackX, projectTrackY, false))+string(workspaceMouse(0, projectTrackX, projectTrackY, true)))
		waitE2E(t, 5*time.Second, func() bool {
			screen := capture.currentText()
			return projectHeading(screen) != "" && projectHeading(screen) != beforeHeading
		}, func() string {
			return scrollFailure("Project scrollbar track click did not change the current selected Project")
		})
	}
	waitE2E(t, 5*time.Second, func() bool { return strings.Contains(capture.currentText(), "Scroll project 15") }, func() string { return scrollFailure("Project scrollbar track click did not page to the end") })
	if strings.Contains(capture.currentText(), "Session focus: keys go to PTY") || strings.Contains(capture.currentText(), "Add Session pane") {
		t.Fatal("Project scrollbar click activated a Project item")
	}
	projectThumbX, projectThumbY, _ := findScrollCellsInRows(capture.currentText(), geometry.Projects.X+geometry.Projects.Width-1, geometry.Projects.Y+1, geometry.Projects.Y+geometry.Projects.Height-1)
	projectDragEnd := geometry.Projects.Y + 1
	writePTY(t, terminal, string(workspaceMouse(0, projectThumbX, projectThumbY, false))+string(workspaceMouse(32, projectThumbX, projectDragEnd, false))+string(workspaceMouse(0, projectThumbX+3, projectDragEnd, true)))
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, thumbY, _, ready := tryFindScrollCellsInRows(screen, projectColumn, geometry.Projects.Y+1, geometry.Projects.Y+geometry.Projects.Height-1)
		return ready && thumbY == projectDragEnd && strings.Contains(projectHeading(screen), "Default Project")
	}, func() string {
		return scrollFailure("Project thumb drag did not return the viewport and selection to the top")
	})
	if got := readLog(); got != expectedShellLog {
		t.Fatalf("Project scrolling changed the exact active Session: %q", got)
	}
	// The top Project is the default Project, while the fixture Sessions belong
	// to Scroll active project. Return there before opening the Quick list.
	writePTY(t, terminal, "\x1b[B")
	waitE2E(t, 5*time.Second, func() bool {
		return strings.Contains(projectHeading(capture.currentText()), "Scroll active project")
	}, func() string {
		return scrollFailure("could not restore the fixture Project after dragging the Project list to the top")
	})
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Session list pane:", 10*time.Second)
	assertScrollbar("Session list", geometry.Quick.X+geometry.Quick.Width-1)
	if x, _, _ := findScrollCellsInRows(capture.currentText(), geometry.Quick.X+geometry.Quick.Width-1, geometry.Quick.Y+1, geometry.Quick.Y+geometry.Quick.Height-1); x != geometry.Quick.X+geometry.Quick.Width-1 {
		t.Fatalf("Session scrollbar appeared at column %d, want pane edge %d", x, geometry.Quick.X+geometry.Quick.Width-1)
	}
	// The Quick list spans the whole inventory, including Sessions outside the
	// fixture Project. Walk its actual selected row to handle0 instead of
	// assuming the fixture begins at index zero.
	firstHandlePrefix := strings.TrimSuffix(handles[0], fmt.Sprintf("%x", stamp))
	for step := 0; step < 64 && !strings.Contains(selectedQuickRow(capture.currentText()), firstHandlePrefix); step++ {
		before := selectedQuickRow(capture.currentText())
		writePTY(t, terminal, "\x1b[B")
		waitE2E(t, 5*time.Second, func() bool {
			row := selectedQuickRow(capture.currentText())
			return row != "" && row != before
		}, func() string {
			return scrollFailure("Session navigation did not advance while selecting fixture handle0")
		})
	}
	if !strings.Contains(selectedQuickRow(capture.currentText()), firstHandlePrefix) {
		t.Fatalf("could not select fixture handle0 in Quick list: row=%q\n%s", selectedQuickRow(capture.currentText()), safeTerminalDiagnostic(capture.currentText()))
	}
	if !strings.Contains(capture.currentText(), handles[0]) {
		t.Fatal("origin Session was not visible before Session list scrolling")
	}
	if !strings.Contains(paneHeading(capture.currentText()), handles[0]) {
		t.Fatalf("origin Session was not the current preview before Session list scrolling: pane=%q\n%s", paneHeading(capture.currentText()), safeTerminalDiagnostic(capture.currentText()))
	}
	sessionColumn := geometry.Quick.X + geometry.Quick.Width - 1
	writePTY(t, terminal, "\x1b[6~")
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, y, _, ready := tryFindScrollCellsInRows(screen, sessionColumn, geometry.Quick.Y+1, geometry.Quick.Y+geometry.Quick.Height-1)
		return ready && y > geometry.Quick.Y+1 && strings.Contains(paneHeading(screen), handles[sessionCount-2])
	}, func() string {
		return scrollFailure("Session PageDown did not select the last row of the next page and move the viewport")
	})
	writePTY(t, terminal, "\x1b[5~")
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, y, _, ready := tryFindScrollCellsInRows(screen, sessionColumn, geometry.Quick.Y+1, geometry.Quick.Y+geometry.Quick.Height-1)
		return ready && y == geometry.Quick.Y+1 && strings.Contains(paneHeading(screen), handles[0])
	}, func() string { return scrollFailure("Session PageUp did not restore the first Session and viewport") })
	quick := geometry.Quick
	trackX := quick.X + quick.Width - 1
	trackY := quick.Y + quick.Height - 1
	writePTY(t, terminal, string(workspaceMouse(65, trackX-1, quick.Y+2, false)))
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(paneHeading(screen), handles[3]) && strings.Contains(screen, "Session list pane:") && readLog() == expectedShellLog
	}, func() string {
		return scrollFailure("Session list mouse wheel did not select the fourth Session while retaining list focus")
	})
	writePTY(t, terminal, string(workspaceMouse(0, trackX, trackY, false))+string(workspaceMouse(0, trackX, trackY, true)))
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(paneHeading(screen), handles[sessionCount-1]) && strings.Contains(screen, "Session list pane:")
	}, func() string {
		return scrollFailure("Session scrollbar track click did not select the last Session while retaining list focus")
	})
	if strings.Contains(capture.currentText(), "Session focus: keys go to PTY") {
		t.Fatal("Session scrollbar click activated a Session")
	}
	sessionThumbX, sessionThumbY, _ := findScrollCellsInRows(capture.currentText(), sessionColumn, geometry.Quick.Y+1, geometry.Quick.Y+geometry.Quick.Height-1)
	sessionDragEnd := quick.Y + 1
	writePTY(t, terminal, string(workspaceMouse(0, sessionThumbX, sessionThumbY, false))+string(workspaceMouse(32, sessionThumbX, sessionDragEnd, false))+string(workspaceMouse(0, sessionThumbX+3, sessionDragEnd, true)))
	waitE2E(t, 5*time.Second, func() bool {
		screen := capture.currentText()
		_, y, _, ready := tryFindScrollCellsInRows(screen, sessionColumn, geometry.Quick.Y+1, geometry.Quick.Y+geometry.Quick.Height-1)
		lines := strings.Split(screen, "\n")
		firstRow := ""
		if geometry.Quick.Y < len(lines) {
			firstRow = strings.TrimSpace(lines[geometry.Quick.Y])
		}
		return ready && y == sessionDragEnd && selectedQuickRow(screen) == firstRow &&
			strings.Contains(screen, "Session list pane:") && readLog() == expectedShellLog
	}, func() string {
		return scrollFailure("Session thumb drag did not select the first inventory row and return the viewport to the top")
	})
	writePTY(t, terminal, "\x1b[6~")
	if got := readLog(); got != expectedShellLog {
		t.Fatalf("list paging leaked input to origin shell: %q", got)
	}
}

// findScrollCellsInColumn scopes workspace-list assertions to the requested
// pane edge so another overflowing list cannot satisfy the check.
func findScrollCellsInColumn(screen string, column int) (x, thumbY, trackY int) {
	x, thumbY, trackY, ok := tryFindScrollCellsInColumn(screen, column)
	if !ok {
		panic(fmt.Sprintf("scrollbar not found in column %d of current screen: %q", column, safeTerminalDiagnostic(screen)))
	}
	return x, thumbY, trackY
}

func tryFindScrollCellsInColumn(screen string, column int) (x, thumbY, trackY int, ok bool) {
	lines := strings.Split(screen, "\n")
	return tryFindScrollCellsInRows(screen, column, 1, len(lines))
}

func findScrollCellsInRows(screen string, column, firstRow, lastRow int) (x, thumbY, trackY int) {
	x, thumbY, trackY, ok := tryFindScrollCellsInRows(screen, column, firstRow, lastRow)
	if !ok {
		panic(fmt.Sprintf("scrollbar not found in column %d rows %d..%d of current screen: %q", column, firstRow, lastRow, safeTerminalDiagnostic(screen)))
	}
	return x, thumbY, trackY
}

// Row bounds are inclusive, one-based terminal coordinates.
func tryFindScrollCellsInRows(screen string, column, firstRow, lastRow int) (x, thumbY, trackY int, ok bool) {
	lines := strings.Split(screen, "\n")
	for row := max(0, firstRow-1); row < min(len(lines), lastRow); row++ {
		line := lines[row]
		for start := 0; start < len(line); {
			next := strings.IndexAny(line[start:], "█│")
			if next < 0 {
				break
			}
			at := start + next
			cell := modalCellWidth(line[:at]) + 1
			glyph := line[at : at+len("█")]
			if cell == column {
				x = cell
				if glyph == "█" && thumbY == 0 {
					thumbY = row + 1
				}
				if glyph == "│" {
					trackY = row + 1
				}
			}
			start = at + len(glyph)
		}
	}
	return x, thumbY, trackY, x != 0 && thumbY != 0 && trackY != 0
}

func TestScrollContainerScrollbarFinderScopesSameColumnPanes(t *testing.T) {
	screen := strings.Join([]string{
		"",
		"    █", // Project thumb, row 2.
		"    │",
		"    │",
		"",
		"    │", // Quick track begins lower in the same column.
		"    █", // Quick thumb, row 7.
		"    │",
	}, "\n")
	_, projectThumb, _, ok := tryFindScrollCellsInRows(screen, 5, 2, 4)
	if !ok || projectThumb != 2 {
		t.Fatalf("Project scan got thumb row %d (ok=%t), want row 2", projectThumb, ok)
	}
	_, quickThumb, _, ok := tryFindScrollCellsInRows(screen, 5, 6, 8)
	if !ok || quickThumb != 7 {
		t.Fatalf("Quick scan got thumb row %d (ok=%t), want row 7", quickThumb, ok)
	}
}
