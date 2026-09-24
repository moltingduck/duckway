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
	assertScrollbar := func(where string, column int) {
		t.Helper()
		if column <= 0 {
			t.Fatalf("%s scrollbar column is invalid: %d", where, column)
		}
		findScrollCellsInColumn(capture.currentText(), column)
	}

	// The Help page has many more rows than this compact terminal. Page keys
	// must move its current viewport, and pending remote output must not take
	// input ownership or close it.
	writePTY(t, terminal, "\x02?")
	capture.waitCurrent(t, "Keyboard shortcuts", 10*time.Second)
	helpColumn := (100-72)/2 + 1 + 72 - 2
	assertScrollbar("Help", helpColumn)
	_, helpThumbStart, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
	writePTY(t, terminal, "\x1b[6~") // PageDown
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
		return y > helpThumbStart
	}, func() string { return "Help PageDown did not move its current scrollbar thumb" })
	_, helpAfterPage, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
	writePTY(t, terminal, "\x1b[5~") // PageUp
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
		return y < helpAfterPage
	}, func() string { return "Help PageUp did not move its current scrollbar thumb" })
	// Move beyond the initial viewport in both directions so this proves the
	// Help-local arrows change the current result viewport, not merely its cursor.
	_, helpBeforeArrows, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
	writePTY(t, terminal, strings.Repeat("\x1b[B", 12))
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
		return y > helpBeforeArrows
	}, func() string { return "Help Down did not move its current result viewport" })
	_, helpAfterDown, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
	writePTY(t, terminal, strings.Repeat("\x1b[A", 12))
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
		return y < helpAfterDown
	}, func() string { return "Help Up did not move its current result viewport" })
	writePTY(t, terminal, "/session")
	capture.waitCurrent(t, "Filter: session", 5*time.Second)
	filtered := capture.currentText()
	_, filteredThumbBefore, _ := findScrollCellsInColumn(filtered, helpColumn)
	writePTY(t, terminal, "\x1b[6~")
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
		return y > filteredThumbBefore && strings.Contains(capture.currentText(), "Filter: session")
	}, func() string { return "Help PageDown lost search or failed to move its filtered viewport" })
	// The mouse wheel and thumb drag use the actual current scrollbar cells.
	helpX, helpThumbY, helpTrackY := findScrollCellsInColumn(capture.currentText(), helpColumn)
	beforeWheelY := helpThumbY
	writePTY(t, terminal, string(workspaceMouse(64, helpX-1, helpThumbY, false)))
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
		return y < beforeWheelY && strings.Contains(capture.currentText(), "Filter: session")
	}, func() string { return "Help mouse wheel did not move its thumb while preserving search" })
	_, helpThumbY, helpTrackY = findScrollCellsInColumn(capture.currentText(), helpColumn)
	beforeDragY := helpThumbY
	writePTY(t, terminal, string(workspaceMouse(0, helpX, helpThumbY, false))+string(workspaceMouse(32, helpX, helpTrackY, false))+string(workspaceMouse(0, helpX+4, helpTrackY, true)))
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), helpColumn)
		return y > beforeDragY && strings.Contains(capture.currentText(), "Filter: session")
	}, func() string { return "Help thumb drag did not scroll while preserving search" })
	if got := readLog(); got != "" {
		t.Fatalf("Help paging leaked bytes to origin shell: %q", got)
	}
	asyncMarker := fmt.Sprintf("SCROLL_ASYNC_%x", stamp)
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", owner+"-cli", "send", "client-a", firstHandle,
		"sleep 1; printf '"+asyncMarker+"\\n' | tee -a \"$DUCKWAY_SCROLL_LOG\"", "--config", config).CombinedOutput(); err != nil {
		t.Fatalf("request asynchronous PTY output: %v: %s", err, out)
	}
	waitE2E(t, 10*time.Second, func() bool {
		return strings.Contains(readLog(), asyncMarker+"\n") && strings.Contains(capture.currentText(), "Keyboard shortcuts")
	}, func() string { return "async PTY output did not arrive while Help remained open" })
	if got := readLog(); got != "" {
		t.Fatalf("Help ownership leaked input during async output: %q", got)
	}
	writePTY(t, terminal, "?")
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return !strings.Contains(screen, "Keyboard shortcuts") && !strings.Contains(screen, "Help ·")
	}, func() string { return "? did not close Help in current terminal cells" })
	marker := fmt.Sprintf("SCROLL_RESTORED_%x", stamp)
	writePTY(t, terminal, fmt.Sprintf("printf '%%s\\n' '%s' | tee -a \"$DUCKWAY_SCROLL_LOG\"\r", marker))
	waitE2E(t, 10*time.Second, func() bool { return strings.Contains(readLog(), marker+"\n") }, func() string { return "Help close did not restore the exact origin shell" })

	// Project and Session list overflow controls are exercised while their own
	// navigation pane owns the mouse. A scrollbar click scrolls the viewport;
	// it must not enter a Project or transfer terminal control.
	geometry := ducklord.CalculateWorkspaceGeometry(100, 16, 4)
	// With terminal focus, hovering the Project list and wheeling over it is a
	// consumed no-op: it must not switch the selected Session or PTY.
	writePTY(t, terminal, string(workspaceMouse(65, geometry.Projects.X+2, geometry.Projects.Y+2, false)))
	waitE2E(t, 3*time.Second, func() bool {
		return strings.Contains(capture.currentText(), marker) && strings.Contains(readLog(), marker+"\n")
	}, func() string { return "terminal focus or exact origin PTY changed after list wheel" })
	if got := readLog(); got != marker+"\n" {
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
	if x, _, _ := findScrollCellsInColumn(capture.currentText(), geometry.Projects.X+geometry.Projects.Width-1); x != geometry.Projects.X+geometry.Projects.Width-1 {
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
	projectColumn := geometry.Projects.X + geometry.Projects.Width - 1
	_, projectThumbStart, _ := findScrollCellsInColumn(capture.currentText(), projectColumn)
	writePTY(t, terminal, "\x1b[6~")
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), projectColumn)
		return y > projectThumbStart
	}, func() string { return "Project PageDown did not move its current viewport" })
	_, projectThumbEnd, _ := findScrollCellsInColumn(capture.currentText(), projectColumn)
	writePTY(t, terminal, "\x1b[5~")
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), projectColumn)
		return y < projectThumbEnd
	}, func() string { return "Project PageUp did not move its current viewport" })
	projectTrackX := projectColumn
	projectTrackY := geometry.Projects.Y + geometry.Projects.Height - 2
	for page := 0; page < 8 && !strings.Contains(capture.currentText(), "Scroll project 15"); page++ {
		writePTY(t, terminal, string(workspaceMouse(0, projectTrackX, projectTrackY, false))+string(workspaceMouse(0, projectTrackX, projectTrackY, true)))
		time.Sleep(120 * time.Millisecond)
	}
	waitE2E(t, 5*time.Second, func() bool { return strings.Contains(capture.currentText(), "Scroll project 15") }, func() string { return "Project scrollbar track click did not page to the end" })
	if strings.Contains(capture.currentText(), "Session focus: keys go to PTY") || strings.Contains(capture.currentText(), "Add Session pane") {
		t.Fatal("Project scrollbar click activated a Project item")
	}
	projectThumbX, projectThumbY, _ := findScrollCellsInColumn(capture.currentText(), geometry.Projects.X+geometry.Projects.Width-1)
	projectDragEnd := geometry.Projects.Y + 2
	writePTY(t, terminal, string(workspaceMouse(0, projectThumbX, projectThumbY, false))+string(workspaceMouse(32, projectThumbX, projectDragEnd, false))+string(workspaceMouse(0, projectThumbX+3, projectDragEnd, true)))
	capture.waitCurrent(t, "Scroll project 00", 5*time.Second)
	if got := readLog(); got != marker+"\n" {
		t.Fatalf("Project scrolling changed the exact active Session: %q", got)
	}
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Session list pane:", 10*time.Second)
	assertScrollbar("Session list", geometry.Quick.X+geometry.Quick.Width-1)
	if x, _, _ := findScrollCellsInColumn(capture.currentText(), geometry.Quick.X+geometry.Quick.Width-1); x != geometry.Quick.X+geometry.Quick.Width-1 {
		t.Fatalf("Session scrollbar appeared at column %d, want pane edge %d", x, geometry.Quick.X+geometry.Quick.Width-1)
	}
	if !strings.Contains(capture.currentText(), handles[0]) {
		t.Fatal("origin Session was not visible before Session list scrolling")
	}
	sessionColumn := geometry.Quick.X + geometry.Quick.Width - 1
	_, sessionPageStart, _ := findScrollCellsInColumn(capture.currentText(), sessionColumn)
	writePTY(t, terminal, "\x1b[6~")
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), sessionColumn)
		return y > sessionPageStart
	}, func() string { return "Session PageDown did not move its current viewport" })
	_, sessionPageEnd, _ := findScrollCellsInColumn(capture.currentText(), sessionColumn)
	writePTY(t, terminal, "\x1b[5~")
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), sessionColumn)
		return y < sessionPageEnd
	}, func() string { return "Session PageUp did not move its current viewport" })
	quick := geometry.Quick
	trackX := quick.X + quick.Width - 1
	trackY := quick.Y + quick.Height - 2
	_, sessionThumbBefore, _ := findScrollCellsInColumn(capture.currentText(), geometry.Quick.X+geometry.Quick.Width-1)
	writePTY(t, terminal, string(workspaceMouse(65, trackX-1, quick.Y+2, false)))
	waitE2E(t, 5*time.Second, func() bool {
		_, y, _ := findScrollCellsInColumn(capture.currentText(), geometry.Quick.X+geometry.Quick.Width-1)
		return y != sessionThumbBefore
	}, func() string { return "Session list mouse wheel did not move its current scrollbar thumb" })
	writePTY(t, terminal, string(workspaceMouse(0, trackX, trackY, false))+string(workspaceMouse(0, trackX, trackY, true)))
	waitE2E(t, 5*time.Second, func() bool { return strings.Contains(capture.currentText(), handles[sessionCount-1]) }, func() string { return "Session scrollbar track click did not page to the end" })
	if strings.Contains(capture.currentText(), "Session focus: keys go to PTY") {
		t.Fatal("Session scrollbar click activated a Session")
	}
	sessionThumbX, sessionThumbY, _ := findScrollCellsInColumn(capture.currentText(), geometry.Quick.X+geometry.Quick.Width-1)
	sessionDragEnd := quick.Y + quick.Height - 2
	writePTY(t, terminal, string(workspaceMouse(0, sessionThumbX, sessionThumbY, false))+string(workspaceMouse(32, sessionThumbX, sessionDragEnd, false))+string(workspaceMouse(0, sessionThumbX+3, sessionDragEnd, true)))
	capture.waitCurrent(t, handles[sessionCount-1], 5*time.Second)
	writePTY(t, terminal, "\x1b[6~")
	if got := readLog(); got != marker+"\n" {
		t.Fatalf("list paging leaked input to origin shell: %q", got)
	}
}

// findScrollCellsInColumn scopes workspace-list assertions to the requested
// pane edge so another overflowing list cannot satisfy the check.
func findScrollCellsInColumn(screen string, column int) (x, thumbY, trackY int) {
	for row, line := range strings.Split(screen, "\n") {
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
	if x == 0 || thumbY == 0 || trackY == 0 {
		panic(fmt.Sprintf("scrollbar not found in column %d of current screen: %q", column, safeTerminalDiagnostic(screen)))
	}
	return x, thumbY, trackY
}
