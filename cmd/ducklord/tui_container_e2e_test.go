package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// TestDucklordCreateTUIContainerE2E retains the old TUI wizard regression as
// an explicit legacy-only test; the production Project TUI has separate E2E.
func TestDucklordCreateTUIContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_LEGACY_TUI_E2E") != "1" {
		t.Skip("legacy TUI wizard is outside the production Project E2E")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	before := listContainerSessions(t, runtime, controller, "client-a")
	if out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", "printf '\nraw_output_subscription_limit: 2\n' >>/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("configure pooled output limit: %v: %s", err, out)
	}
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "send", "client-a", "bash", `printf '\033[31mDUCKLORD_POOLED_RED\033[0m\n'`, "--config", "/tmp/e2e-inspector.yaml").CombinedOutput(); err != nil {
		t.Fatalf("seed colored PTY output: %v: %s", err, out)
	}
	var alpha protocol.SessionSummary
	for _, session := range before {
		if session.Handle == "alpha" {
			alpha = session
		}
	}
	if alpha.SessionID == "" {
		t.Fatal("demo alpha session is missing")
	}
	alphaRemote, ok := findContainerSession(t, runtime, controller, "client-a", alpha.SessionID)
	if !ok || alphaRemote.InstanceID == "" {
		t.Fatal("demo alpha session has no remote instance identity")
	}
	handle := fmt.Sprintf("job-tui-e2e-%d", os.Getpid())
	notificationHandle := fmt.Sprintf("notify-tui-e2e-%d", os.Getpid())
	notifyCommand := `sleep 3; printf '%s\n' '{"kind":"completed","response":"fixture completed"}' >&3; sleep 30`
	if out, startErr := exec.Command(runtime, "exec", controller, "ducklord", "start", "client-a", "--name", notificationHandle,
		"--agent", "fixture", "--cwd", "/home/duck/projects/alpha", "--config", "/tmp/e2e-inspector.yaml", "--", "sh", "-c", notifyCommand).CombinedOutput(); startErr != nil {
		t.Fatalf("start notification fixture: %v: %s", startErr, out)
	}
	var notificationSession protocol.SessionSummary
	waitE2E(t, 5*time.Second, func() bool {
		for _, session := range listContainerSessions(t, runtime, controller, "client-a") {
			if session.Handle == notificationHandle {
				notificationSession = session
				return true
			}
		}
		return false
	}, func() string { return "notification fixture was not listed" })
	command := exec.Command(runtime, "exec", "-it", controller, "env", "TERM=xterm-256color", "DUCKLORD_LEGACY_TUI=1", "ducklord", "tui", "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("start Ducklord TUI: %v", err)
	}
	defer func() {
		_, _ = terminal.Write([]byte("\x1bq"))
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()

	capture := newTUICapture(terminal)
	capture.wait(t, "ducklord remote agents", 20*time.Second)
	capture.wait(t, "client-a alpha tick", 20*time.Second)
	capture.waitCurrent(t, "sessions [custom]", 10*time.Second)
	capture.waitCurrent(t, "[Ungrouped] •", 10*time.Second)
	if screen := capture.currentText(); !strings.Contains(screen, "[Ungrouped] •") || !strings.Contains(screen, "• client-a") {
		t.Fatalf("background agent completion did not render session/group unread: %q", safeTerminalDiagnostic(screen))
	}
	// Notification promotion changes visual row positions, so activate the
	// exact completed session through search instead of relying on row counts.
	writePTY(t, terminal, "/"+notificationHandle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "session focus", 10*time.Second)
	writePTY(t, terminal, "\x1d")
	waitE2E(t, 10*time.Second, func() bool {
		data, readErr := exec.Command(runtime, "exec", controller, "cat", "/root/.ducklord/state.json").Output()
		if readErr != nil {
			return false
		}
		var activity ducklord.ActivityState
		if json.Unmarshal(data, &activity) != nil {
			return false
		}
		entry := activity.Sessions[alphaRemote.InstanceID+"/"+notificationSession.SessionID]
		return entry.Seen[model.NotificationTaskCompleted] == 1 && !entry.Unread[model.NotificationTaskCompleted]
	}, func() string { return "freshly browsing the completed agent did not persist seen state" })
	if screen := capture.currentText(); strings.Contains(screen, "[Ungrouped] •") {
		t.Fatalf("group unread survived browsing completed session: %q", safeTerminalDiagnostic(screen))
	}
	for range 3 {
		writePTY(t, terminal, "k")
		time.Sleep(100 * time.Millisecond)
	}
	capture.waitCurrent(t, "client-a / alpha", 5*time.Second)
	copyStart := capture.position()
	writePTY(t, terminal, "v")
	capture.waitCurrent(t, "COPY MODE", 5*time.Second)
	if raw := capture.since(copyStart); !strings.Contains(raw, "\033[?1002l\033[?1000h\033[?1006h") || !strings.Contains(raw, modalSelected) {
		t.Fatalf("copy mode did not enable local wheel tracking with a visible status: %q", safeTerminalDiagnostic(raw))
	}
	frozen := capture.currentText()
	copyMarker := fmt.Sprintf("DUCKLORD_COPY_MODE_%d", os.Getpid())
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "send", "client-a", alpha.SessionID, "printf "+copyMarker+"\\n\r", "--config", "/tmp/e2e-inspector.yaml").CombinedOutput(); err != nil {
		t.Fatalf("inject output while copy mode is frozen: %v: %s", err, out)
	}
	waitE2E(t, 10*time.Second, func() bool {
		out, readErr := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", alpha.SessionID, "--lines", "20", "--config", "/tmp/e2e-inspector.yaml").CombinedOutput()
		return readErr == nil && bytes.Contains(out, []byte(copyMarker))
	}, func() string { return "copy-mode marker never reached the authoritative remote PTY" })
	// The remote read above proves new PTY output exists before we assert that
	// Ducklord kept the selectable framebuffer stable.
	time.Sleep(1200 * time.Millisecond)
	if current := capture.currentText(); current != frozen {
		t.Fatalf("copy mode redraw disturbed selectable text: before=%q after=%q", safeTerminalDiagnostic(frozen), safeTerminalDiagnostic(current))
	}
	writePTY(t, terminal, "\x1b")
	capture.waitCurrent(t, "sessions [custom]", 5*time.Second)
	capture.waitCurrent(t, copyMarker, 10*time.Second)

	// The unified desired-state form disconnects only Ducklord's transport.
	// Reconnecting the same Ducklion instance must restore raw output without
	// stopping or replacing the remote PTY session.
	writePTY(t, terminal, "h\r")
	capture.waitCurrent(t, "Host connections", 5*time.Second)
	writePTY(t, terminal, " \r")
	capture.waitCurrent(t, "client-a:DISCONNECTED", 5*time.Second)
	if current, ok := findContainerSession(t, runtime, controller, "client-a", alpha.SessionID); !ok || current.Status != string(model.StatusRunning) || current.InstanceID != alphaRemote.InstanceID {
		t.Fatalf("disconnect changed remote session: ok=%v session=%+v", ok, current)
	}
	writePTY(t, terminal, "h\r")
	capture.waitCurrent(t, "Host connections", 5*time.Second)
	writePTY(t, terminal, " \r")
	capture.waitCurrent(t, "client-a:", 5*time.Second)
	reconnectedMarker := fmt.Sprintf("DUCKLORD_RECONNECTED_%d", os.Getpid())
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "send", "client-a", alpha.SessionID, "printf "+reconnectedMarker+"\\n\r", "--config", "/tmp/e2e-inspector.yaml").CombinedOutput(); err != nil {
		t.Fatalf("write after same-instance reconnect: %v: %s", err, out)
	}
	capture.waitCurrent(t, reconnectedMarker, 15*time.Second)
	writePTY(t, terminal, "o")
	capture.waitCurrent(t, "sessions [host]", 2*time.Second)
	writePTY(t, terminal, "o")
	capture.waitCurrent(t, "sessions [type]", 2*time.Second)
	writePTY(t, terminal, "o")
	capture.waitCurrent(t, "sessions [custom]", 2*time.Second)

	var bashSession protocol.SessionSummary
	for _, session := range before {
		if session.Handle == "bash" {
			bashSession = session
		}
	}
	if bashSession.SessionID == "" {
		t.Fatal("demo bash session is missing")
	}
	writePTY(t, terminal, "j\x0b") // select bash, then move it above alpha
	waitE2E(t, 2*time.Second, func() bool {
		out, readErr := exec.Command(runtime, "exec", controller, "cat", "/root/.ducklord/state.json").CombinedOutput()
		if readErr != nil {
			return false
		}
		var localState ducklord.ActivityState
		if json.Unmarshal(out, &localState) != nil || len(localState.Organization.SessionOrder) < 2 {
			return false
		}
		return localState.Organization.Mode == ducklord.OrganizationCustom && localState.Organization.SessionOrder[0].SessionID == bashSession.SessionID && localState.Organization.SessionOrder[1].SessionID == alpha.SessionID
	}, func() string { return "organization mode/manual session order was not persisted" })
	writePTY(t, terminal, "\x0a") // restore alpha above bash via Ctrl-J
	writePTY(t, terminal, "k")    // return selection to alpha
	capture.waitCurrent(t, "client-a alpha tick", 2*time.Second)

	// Custom-group management is entirely local. Exercise Unicode duplicate
	// names, exact membership, rename, ordering, and delete-to-Ungrouped.
	loadGroupState := func() (ducklord.ActivityState, bool) {
		out, readErr := exec.Command(runtime, "exec", controller, "cat", "/root/.ducklord/state.json").CombinedOutput()
		var current ducklord.ActivityState
		if readErr != nil || json.Unmarshal(out, &current) != nil {
			return ducklord.ActivityState{}, false
		}
		return current, true
	}
	writePTY(t, terminal, "g\r正式環境\r")
	waitE2E(t, 2*time.Second, func() bool { state, ok := loadGroupState(); return ok && len(state.Organization.Groups) == 1 }, func() string { return "first custom group was not saved" })
	writePTY(t, terminal, "g\x1b[B\r\x1b[B\r")
	capture.waitCurrent(t, "[正式環境]", 2*time.Second)
	writePTY(t, terminal, "g\r正式環境\r")
	waitE2E(t, 2*time.Second, func() bool { state, ok := loadGroupState(); return ok && len(state.Organization.Groups) == 2 }, func() string { return "duplicate-name custom group was not saved" })
	writePTY(t, terminal, "g\x1b[B\x1b[B\r")
	capture.waitCurrent(t, "正式環境 ·", 2*time.Second)
	writePTY(t, terminal, "\x1b[B\r備援q\r")
	waitE2E(t, 2*time.Second, func() bool {
		state, ok := loadGroupState()
		return ok && len(state.Organization.Groups) == 2 && state.Organization.Groups[1].Name == "備援q"
	}, func() string { return "exact duplicate-name group was not renamed" })
	writePTY(t, terminal, "g\x1b[B\x1b[B\x1b[B\x1b[B\r\x1b[B\r")
	waitE2E(t, 2*time.Second, func() bool {
		state, ok := loadGroupState()
		return ok && len(state.Organization.GroupOrders[ducklord.OrganizationCustom]) >= 3 && state.Organization.GroupOrders[ducklord.OrganizationCustom][1] == state.Organization.Groups[1].ID
	}, func() string { return "custom group order was not saved" })
	writePTY(t, terminal, "g\x1b[B\x1b[B\x1b[B\r\x1b[B\r")
	waitE2E(t, 2*time.Second, func() bool {
		state, ok := loadGroupState()
		return ok && len(state.Organization.Groups) == 1 && len(state.Organization.Membership) == 0
	}, func() string { return "custom group delete did not move the session to Ungrouped" })
	groupStateRaw, err := exec.Command(runtime, "exec", controller, "cat", "/root/.ducklord/state.json").CombinedOutput()
	if err != nil {
		t.Fatalf("read custom-group state: %v: %s", err, groupStateRaw)
	}
	var groupState ducklord.ActivityState
	if err := json.Unmarshal(groupStateRaw, &groupState); err != nil || len(groupState.Organization.Groups) != 1 || groupState.Organization.Groups[0].Name != "備援q" || len(groupState.Organization.Membership) != 0 {
		t.Fatalf("unexpected custom-group state=%+v err=%v", groupState.Organization, err)
	}
	alphaAfterGroups, ok := findContainerSession(t, runtime, controller, "client-a", alpha.SessionID)
	if !ok || alphaAfterGroups.RuntimeGeneration != alpha.RuntimeGeneration {
		t.Fatalf("local group operations mutated remote alpha: before=%+v after=%+v", alpha, alphaAfterGroups)
	}
	start := capture.position()
	writePTY(t, terminal, "j") // alpha -> bash, without focusing the PTY
	capture.waitCurrent(t, "DUCKLORD_POOLED_RED", 10*time.Second)
	if !capture.markerHasForeground("DUCKLORD_POOLED_RED", 2) {
		t.Fatalf("pooled marker is not semantically red on current screen: %q", safeTerminalDiagnostic(capture.currentText()))
	}
	start = capture.position()
	writePTY(t, terminal, "j") // bash -> build; capacity 2 evicts alpha
	capture.waitCurrent(t, "client-a build output", 10*time.Second)
	snapshotPath := fmt.Sprintf("/root/.ducklord/sessions/%s/%s.snapshot", alphaRemote.InstanceID, alpha.SessionID)
	waitE2E(t, 10*time.Second, func() bool {
		return exec.Command(runtime, "exec", controller, "test", "-s", snapshotPath).Run() == nil
	}, func() string { return "LRU eviction did not persist alpha framebuffer snapshot at " + snapshotPath })
	localSnapshots := t.TempDir()
	localInstanceDir := filepath.Join(localSnapshots, alphaRemote.InstanceID)
	if err := os.MkdirAll(localInstanceDir, 0700); err != nil {
		t.Fatal(err)
	}
	localSnapshot := filepath.Join(localInstanceDir, alpha.SessionID+".snapshot")
	if out, err := exec.Command(runtime, "cp", controller+":"+snapshotPath, localSnapshot).CombinedOutput(); err != nil {
		t.Fatalf("copy LRU snapshot: %v: %s", err, out)
	}
	info, err := os.Stat(localSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot mode=%v", info.Mode().Perm())
	}
	snapshot, err := (ducklord.SnapshotStore{Root: localSnapshots}).Load(alphaRemote.InstanceID, alpha.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	renderState, err := ducklord.DecodeTerminalRenderState(snapshot.Payload)
	if err != nil || renderState.Framebuffer == nil || renderState.RuntimeGeneration != alpha.RuntimeGeneration || renderState.OutputOffset == 0 {
		t.Fatalf("invalid LRU snapshot state=%+v err=%v", renderState, err)
	}
	snapshotTerminal, valid := ducklord.NewTerminalFromState(*renderState.Framebuffer, ducklord.DefaultTerminalScrollback)
	if !valid || !strings.Contains(snapshotTerminal.Text(), "client-a alpha tick") || strings.Contains(snapshotTerminal.Text(), "DUCKLORD_POOLED_RED") {
		t.Fatalf("LRU snapshot crossed session identity: %q", safeTerminalDiagnostic(snapshotTerminal.Text()))
	}
	writePTY(t, terminal, "kk") // reactivate alpha from its exact saved cursor
	capture.waitCurrent(t, "client-a alpha tick", 10*time.Second)

	// Drop the real Ducklion daemon while alpha and bash are the two desired
	// framebuffer subscriptions. Selection must remain responsive and the host
	// restore must reconnect both views after the daemon returns.
	if out, err := exec.Command(runtime, "exec", "-u", "duck", "ducklion-client-a", "sh", "-lc", `kill "$(cat $HOME/.duckway/ducklion-daemon.pid)"`).CombinedOutput(); err != nil {
		t.Fatalf("stop Ducklion during TUI reconnect: %v: %s", err, out)
	}
	capture.waitCurrent(t, "RECONNECTING", 10*time.Second)
	writePTY(t, terminal, "j") // selection remains local while the host is down
	if out, err := exec.Command(runtime, "exec", "-d", "-u", "duck", "ducklion-client-a", "sh", "-lc", `nohup ducklion daemon >$HOME/.duckway/ducklion-daemon.log 2>&1 </dev/null & echo $! >$HOME/.duckway/ducklion-daemon.pid`).CombinedOutput(); err != nil {
		t.Fatalf("restart Ducklion during TUI reconnect: %v: %s", err, out)
	}
	capture.waitCurrent(t, "DUCKLORD_POOLED_RED", 20*time.Second)
	writePTY(t, terminal, "k")
	capture.waitCurrent(t, "client-a alpha tick", 20*time.Second)

	start = capture.position()
	writePTY(t, terminal, "c")
	assertCurrentCreateModal(t, capture, start, "new session: choose agent or shell", "Shell session", true)

	start = capture.position()
	writePTY(t, terminal, "\x1b[B\r") // shell, then submit
	assertCurrentCreateModal(t, capture, start, "choose host number/name", "client-a", true)
	start = capture.position()
	writePTY(t, terminal, "\r") // currently selected client-a
	assertCurrentCreateModal(t, capture, start, "choose directory", "alpha-project", true)
	start = capture.position()
	writePTY(t, terminal, "\r") // alpha-project
	assertCurrentCreateModal(t, capture, start, "handle (default alpha)", "handle", false)
	start = capture.position()
	writePTY(t, terminal, handle+"\r")
	capture.waitAfter(t, start, "? help", 20*time.Second)
	start = capture.position()
	writePTY(t, terminal, "?")
	assertCurrentCreateModal(t, capture, start, "Keyboard shortcuts", "SESSION LIST", false)
	writePTY(t, terminal, "?")

	var created protocol.SessionSummary
	waitE2E(t, 15*time.Second, func() bool {
		for _, session := range listContainerSessions(t, runtime, controller, "client-a") {
			if session.Handle == handle {
				created = session
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("created session did not appear in Ducklion inventory; host-a=%s host-b=%s tui=%q",
			sessionInventoryDiagnostic(t, runtime, controller, "client-a"), sessionInventoryDiagnostic(t, runtime, controller, "client-b"), safeTerminalDiagnostic(capture.since(start)))
	})
	if created.SessionID == "" || created.Kind != "shell" || created.CWD != "/home/duck/projects/alpha" || created.ProjectName != "alpha-project" {
		t.Fatalf("created identity = %#v", created)
	}
	assertContainerShellPWD(t, runtime, controller, "ducklord", "", "client-a", handle, "/home/duck/projects/alpha")
	for _, old := range before {
		if old.SessionID == created.SessionID {
			t.Fatalf("create reused existing session ID %q", created.SessionID)
		}
	}
	for _, other := range listContainerSessions(t, runtime, controller, "client-b") {
		if other.Handle == handle {
			t.Fatalf("create escaped selected host: host-b has %#v", other)
		}
	}
	stateBeforeSearch, err := exec.Command(runtime, "exec", controller, "cat", "/root/.ducklord/state.json").CombinedOutput()
	if err != nil {
		t.Fatalf("read state before search: %v: %s", err, stateBeforeSearch)
	}
	writePTY(t, terminal, "/不存在的工作")
	capture.waitCurrent(t, "No matching sessions", 5*time.Second)
	writePTY(t, terminal, "\x1b")
	stateAfterSearch, err := exec.Command(runtime, "exec", controller, "cat", "/root/.ducklord/state.json").CombinedOutput()
	if err != nil || !bytes.Equal(stateBeforeSearch, stateAfterSearch) {
		t.Fatalf("local search changed persistent state: err=%v before=%q after=%q", err, stateBeforeSearch, stateAfterSearch)
	}

	// Search is a local, centered projection. The first Enter switches only the
	// framebuffer and keeps the modal open; the unchanged second Enter focuses
	// the exact PTY. Search command letters are consumed as query text.
	start = capture.position()
	writePTY(t, terminal, "/alpha")
	capture.waitCurrent(t, "Search sessions", 5*time.Second)
	capture.waitCurrent(t, "alpha", 5*time.Second)
	if raw := capture.since(start); !strings.Contains(raw, modalBorder) || !strings.Contains(raw, modalInput) || !strings.Contains(raw, modalSelected) {
		t.Fatalf("search modal lacks semantic colors: %q", safeTerminalDiagnostic(raw))
	}
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 10*time.Second)
	capture.waitCurrent(t, "Search sessions", 2*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "session focus", 10*time.Second)
	writePTY(t, terminal, "\x1d")
	capture.waitCurrent(t, "? help", 5*time.Second)

	start = capture.position()
	writePTY(t, terminal, "/"+handle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 10*time.Second)
	capture.waitCurrent(t, handle, 2*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitAfter(t, start, "session focus", 10*time.Second)
	marker := "DUCKLORD_TUI_INPUT_OK"
	writePTY(t, terminal, "printf "+marker+"\r")
	waitE2E(t, 10*time.Second, func() bool {
		out, runErr := exec.Command(runtime, "exec", controller, "ducklord", "read", "client-a", created.SessionID, "--lines", "20", "--config", "/tmp/e2e-inspector.yaml").CombinedOutput()
		return runErr == nil && bytes.Contains(out, []byte(marker))
	}, func() string { return "focused PTY input did not reach selected remote session" })

	// Return to the list and exercise the state-aware action modal. Backend
	// generation/identity changes, rather than screen coordinates, are the
	// authoritative assertions.
	start = capture.position()
	writePTY(t, terminal, "\x1d") // Ctrl-]
	capture.waitAfter(t, start, "? help", 10*time.Second)
	start = capture.position()
	writePTY(t, terminal, "m")
	capture.waitAfter(t, start, "Session actions", 10*time.Second)
	actionScreen := capture.since(start)
	for _, want := range []string{"Restart shell immediately", "Destroy shell and logs", created.SessionID, "╭"} {
		if !strings.Contains(actionScreen, want) {
			t.Fatalf("action modal missing %q: %q", want, safeTerminalDiagnostic(actionScreen))
		}
	}
	start = capture.position()
	writePTY(t, terminal, "r")
	assertCurrentCreateModal(t, capture, start, "RESTART SESSION", created.SessionID, false)
	writePTY(t, terminal, "\r")
	waitE2E(t, 15*time.Second, func() bool {
		session, ok := findContainerSession(t, runtime, controller, "client-a", created.SessionID)
		return ok && session.RuntimeGeneration == created.RuntimeGeneration+1 && session.Status == string(model.StatusRunning) && session.ProjectName == "alpha-project"
	}, func() string { return "action-menu restart did not advance the selected session generation" })
	// Inventory completion can precede the TUI goroutine consuming its durable
	// completion event by one redraw. Give that local event loop a bounded beat;
	// the generation assertion above remains the authoritative result.
	time.Sleep(750 * time.Millisecond)

	// Notification settings are also a modal and remain staged until Enter.
	start = capture.position()
	writePTY(t, terminal, "n")
	capture.waitCurrent(t, "Notifications", 10*time.Second)
	if raw := capture.since(start); !strings.Contains(raw, modalBorder) || !strings.Contains(raw, modalSelected) {
		t.Fatalf("notification modal lacks frame/color: %q", safeTerminalDiagnostic(raw))
	}
	writePTY(t, terminal, " \x1b") // toggle, then discard
	// Let the standalone Escape cross the input parser's ambiguity window;
	// otherwise one synthetic PTY write can turn Esc+n into an Alt+n chord.
	time.Sleep(50 * time.Millisecond)
	writePTY(t, terminal, "n \r") // reopen, toggle and save
	waitE2E(t, 10*time.Second, func() bool {
		return exec.Command(runtime, "exec", controller, "sh", "-lc", "test -s /root/.ducklord/state.json && grep -q '"+created.SessionID+"' /root/.ducklord/state.json && grep -q '\"terminal_attention\": true' /root/.ducklord/state.json").Run() == nil
	}, func() string { return "notification modal did not persist exact session settings" })

	// Destructive selection still requires a second confirmation, and Escape
	// must leave the exact target untouched.
	start = capture.position()
	writePTY(t, terminal, "m")
	capture.waitAfter(t, start, "Session actions", 10*time.Second)
	writePTY(t, terminal, "x")
	assertCurrentCreateModal(t, capture, start, "DESTROY SESSION", created.SessionID, false)
	writePTY(t, terminal, "\x1b")
	if _, ok := findContainerSession(t, runtime, controller, "client-a", created.SessionID); !ok {
		t.Fatal("canceling destructive confirmation removed the session")
	}

	start = capture.position()
	writePTY(t, terminal, "m")
	capture.waitAfter(t, start, "Session actions", 10*time.Second)
	writePTY(t, terminal, "x")
	assertCurrentCreateModal(t, capture, start, "DESTROY SESSION", created.SessionID, false)
	writePTY(t, terminal, "\r")
	waitE2E(t, 15*time.Second, func() bool {
		_, ok := findContainerSession(t, runtime, controller, "client-a", created.SessionID)
		return !ok
	}, func() string { return "action-menu destroy did not remove the exact selected session" })

	// Add-host uses the same centered modal and persists the selected SSH target.
	// Make client-c's command entrypoint slow so this real PTY path proves that
	// the modal remains responsive and a canceled, late probe cannot commit.
	if out, err := exec.Command(runtime, "exec", "-u", "0", "ducklion-client-c", "sh", "-lc", `mv /usr/local/bin/ducklion /usr/local/bin/ducklion-real && printf '%s\n' '#!/bin/sh' 'if [ -e /tmp/slow-ducklion ]; then sleep 3; fi' 'exec /usr/local/bin/ducklion-real "$@"' >/usr/local/bin/ducklion && chmod 0755 /usr/local/bin/ducklion && touch /tmp/slow-ducklion`).CombinedOutput(); err != nil {
		t.Fatalf("install slow client-c wrapper: %v: %s", err, out)
	}
	start = capture.position()
	writePTY(t, terminal, "a")
	assertCurrentCreateModal(t, capture, start, "Add Ducklion host", "client-c", true)
	writePTY(t, terminal, "client-c\r")
	capture.waitCurrent(t, "probing client-c...", 2*time.Second)
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 500*time.Millisecond, func() bool {
		screen := capture.currentText()
		return !strings.Contains(screen, "Add Ducklion host") && !strings.Contains(screen, "probing client-c...")
	}, func() string {
		return "add-host modal did not dismiss promptly: " + safeTerminalDiagnostic(capture.currentText())
	})
	time.Sleep(3500 * time.Millisecond)
	if out, err := exec.Command(runtime, "exec", controller, "ducklord", "clients", "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil || bytes.Contains(out, []byte("client-c")) {
		t.Fatalf("canceled late add-host changed config: err=%v output=%s", err, out)
	}
	if out, err := exec.Command(runtime, "exec", "-u", "0", "ducklion-client-c", "rm", "-f", "/tmp/slow-ducklion").CombinedOutput(); err != nil {
		t.Fatalf("disable slow client-c wrapper: %v: %s", err, out)
	}
	start = capture.position()
	writePTY(t, terminal, "a")
	assertCurrentCreateModal(t, capture, start, "Add Ducklion host", "client-c", true)
	writePTY(t, terminal, "client-c\r")
	waitE2E(t, 15*time.Second, func() bool {
		out, err := exec.Command(runtime, "exec", controller, "ducklord", "clients", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
		return err == nil && bytes.Contains(out, []byte("client-c"))
	}, func() string { return "add-host modal did not persist client-c" })
	if out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", "sed 's/^name: .*/name: e2e-inspector/' /root/.ducklord/config.yaml >/tmp/e2e-inspector-added.yaml && ducklord start client-c --name gamma-live --kind shell --cwd /home/duck/projects/gamma --config /tmp/e2e-inspector-added.yaml -- bash").CombinedOutput(); err != nil {
		t.Fatalf("create session on dynamically watched host: %v: %s", err, out)
	}
	capture.waitCurrent(t, "gamma-live", 20*time.Second)

	// Leave a non-default mode, stop this Ducklord cleanly, and prove a fresh
	// process restores organization state from the same local state file.
	writePTY(t, terminal, "o")
	capture.waitCurrent(t, "sessions [host]", 2*time.Second)
	writePTY(t, terminal, "q")
	if err := command.Wait(); err != nil {
		t.Fatalf("stop first Ducklord TUI: %v", err)
	}
	_ = terminal.Close()
	restartedCommand := exec.Command(runtime, "exec", "-it", controller, "env", "TERM=xterm-256color", "ducklord", "tui", "--config", "/root/.ducklord/config.yaml")
	restartedTerminal, err := pty.StartWithSize(restartedCommand, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("restart Ducklord TUI: %v", err)
	}
	defer func() {
		_, _ = restartedTerminal.Write([]byte("q"))
		_ = restartedTerminal.Close()
		_ = restartedCommand.Process.Kill()
		_ = restartedCommand.Wait()
	}()
	restartedCapture := newTUICapture(restartedTerminal)
	restartedCapture.waitCurrent(t, "sessions [host]", 20*time.Second)
}

// TestDucklordWorkspacePreviewContainerE2E proves that the new three-region
// renderer is connected to the real TUI event loop, while preview and control
// remain separate across the SSH/Ducklion/PTY boundary.
func TestDucklordWorkspacePreviewContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	handle := fmt.Sprintf("a0ws%d", time.Now().UnixNano())
	start := exec.Command(runtime, "exec", controller, "ducklord", "--name", "workspace-e2e-cli", "start", "client-a", "--name", handle,
		"--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "bash")
	if out, startErr := start.CombinedOutput(); startErr != nil {
		t.Fatalf("start isolated workspace shell: %v: %s", startErr, out)
	}
	defer func() {
		_, _ = exec.Command(runtime, "exec", controller, "ducklord", "--name", "workspace-e2e-cli", "destroy", "client-a", handle,
			"--config", "/root/.ducklord/config.yaml").CombinedOutput()
	}()
	home := fmt.Sprintf("/tmp/ducklord-workspace-e2e-%d", os.Getpid())
	if out, setupErr := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); setupErr != nil {
		t.Fatalf("prepare isolated Ducklord state: %v: %s", setupErr, out)
	}
	if out, setupErr := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); setupErr != nil {
		t.Fatalf("prepare isolated SSH config: %v: %s", setupErr, out)
	}
	defer func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() }()
	binary := os.Getenv("DUCKLORD_E2E_BINARY")
	if binary == "" {
		binary = "ducklord"
	}
	owner := fmt.Sprintf("workspace-e2e-%d", time.Now().UnixNano())
	t.Run("remote bookmarks command", func(t *testing.T) {
		bookmarks, err := exec.Command(runtime, "exec", controller, binary, "bookmarks", "client-a",
			"--config", "/root/.ducklord/config.yaml").CombinedOutput()
		if err != nil {
			t.Fatalf("list remote bookmarks: %v: %s", err, bookmarks)
		}
		found := false
		for _, line := range strings.Split(string(bookmarks), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "alpha-project" && fields[1] == "/home/duck/projects/alpha" {
				found = true
			}
		}
		if !found {
			t.Fatalf("remote bookmark fixture not listed: %s", bookmarks)
		}
		alias, err := exec.Command(runtime, "exec", controller, binary, "projects", "client-a",
			"--config", "/root/.ducklord/config.yaml").CombinedOutput()
		if err != nil || !bytes.Equal(alias, bookmarks) {
			t.Fatalf("projects alias differs from bookmarks: err=%v alias=%s bookmarks=%s", err, alias, bookmarks)
		}
	})
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		binary, "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = terminal.Write([]byte("q"))
		time.Sleep(300 * time.Millisecond)
		if listing, err := exec.Command(runtime, "exec", controller, "ps", "-ef").Output(); err == nil {
			needle := binary + " tui --name " + owner + " --config"
			for _, line := range strings.Split(string(listing), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 3 || !strings.Contains(line, needle) {
					continue
				}
				if pid, parseErr := strconv.Atoi(fields[0]); parseErr == nil {
					_, _ = exec.Command(runtime, "exec", controller, "kill", strconv.Itoa(pid)).CombinedOutput()
				}
			}
		}
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	capture := newTUICapture(terminal)
	for _, heading := range []string{"PROJECTS", "SESSIONS", "Default Project", "client-a/" + handle} {
		capture.waitCurrent(t, heading, 20*time.Second)
	}
	capture.waitCurrent(t, "◇ client-a/"+handle, 10*time.Second)
	// Digits are deliberately unbound list keys; letters could trigger a TUI
	// shortcut and make this a false input-isolation test.
	marker := fmt.Sprintf("%d", time.Now().UnixNano())
	writePTY(t, terminal, marker)
	assertAbsent := func() {
		t.Helper()
		for _, target := range []string{"alpha", handle} {
			out, readErr := exec.Command(runtime, "exec", controller, "ducklord", "--name", "workspace-e2e-cli", "read", "client-a", target,
				"--lines", "50", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
			if readErr != nil || bytes.Contains(out, []byte(marker)) {
				t.Fatalf("preview input reached %s: err=%v output=%q", target, readErr, safeTerminalDiagnostic(string(out)))
			}
		}
	}
	time.Sleep(200 * time.Millisecond)
	assertAbsent()
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "▣ client-a/"+handle, 10*time.Second)
	writePTY(t, terminal, "printf '"+marker+"'\r")
	waitE2E(t, 10*time.Second, func() bool {
		out, readErr := exec.Command(runtime, "exec", controller, "ducklord", "--name", "workspace-e2e-cli", "read", "client-a", handle,
			"--lines", "50", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
		return readErr == nil && bytes.Contains(out, []byte(marker))
	}, func() string { return "focused shell did not receive PTY input" })
	writePTY(t, terminal, "\x1d")
	capture.waitCurrent(t, "◇ client-a/"+handle, 10*time.Second)

	t.Run("create missing directory and save remote bookmark", func(t *testing.T) {
		// Keep this inside the real workspace test so the normal E2E script
		// exercises the complete pane wizard through SSH, not just CLI creation.
		token := strconv.FormatInt(time.Now().UnixNano(), 36)
		newHandle := "wb" + token
		bookmark := "pane-" + token
		remoteRoot := "/tmp/dwb-" + token
		remotePath := remoteRoot + "/nested/leaf"
		run := func(args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return exec.CommandContext(ctx, runtime, append([]string{"exec", controller}, args...)...).CombinedOutput()
		}
		if out, err := run("ssh", "client-a", "test", "!", "-e", remoteRoot); err != nil {
			t.Fatalf("remote test path must be absent: %v: %s", err, out)
		}
		t.Cleanup(func() {
			_, _ = run(binary, "--name", "workspace-e2e-cli", "destroy", "client-a", newHandle,
				"--config", "/root/.ducklord/config.yaml")
			// The registry operation locks and removes only this exact path;
			// never restore a snapshot over other bookmarks added meanwhile.
			_, _ = run("ssh", "client-a", "duckway", "projects", "remove", remotePath)
			// rmdir cannot delete unexpected files, even on a failed test.
			_, _ = run("ssh", "client-a", "rmdir", remotePath, remoteRoot+"/nested", remoteRoot)
		})
		writePTY(t, terminal, "bp")
		capture.waitCurrent(t, "Add Session pane", 10*time.Second)
		writePTY(t, terminal, "\r\r") // new tab, new shell
		capture.waitCurrent(t, "host ›", 10*time.Second)
		writePTY(t, terminal, "\r")
		capture.waitCurrent(t, "choose a directory", 15*time.Second)
		writePTY(t, terminal, "browse\r")
		capture.waitCurrent(t, "type a remote directory path", 10*time.Second)
		writePTY(t, terminal, remotePath+"\r")
		capture.waitCurrent(t, "confirm recursive creation", 15*time.Second)
		if out, err := run("ssh", "client-a", "test", "!", "-e", remoteRoot); err != nil {
			t.Fatalf("path inspection created directories before confirmation: %v: %s", err, out)
		}
		writePTY(t, terminal, "\r")
		capture.waitCurrent(t, "directory created recursively", 15*time.Second)
		if out, err := run("ssh", "client-a", "test", "-d", remotePath); err != nil {
			t.Fatalf("recursive creation did not reach remote filesystem: %v: %s", err, out)
		}
		writePTY(t, terminal, "\r") // save as a remote bookmark
		capture.waitCurrent(t, "bookmark name", 10*time.Second)
		writePTY(t, terminal, bookmark+"\r")
		capture.waitCurrent(t, "bookmark added; press Enter to continue", 15*time.Second)
		writePTY(t, terminal, "\r") // accept the newly selected bookmark
		capture.waitCurrent(t, "handle (default", 10*time.Second)
		writePTY(t, terminal, newHandle+"\r")
		waitE2E(t, 20*time.Second, func() bool {
			out, err := run(binary, "--name", "workspace-e2e-cli", "sessions", "client-a", "--json",
				"--config", "/root/.ducklord/config.yaml")
			var sessions []ducklord.RemoteSession
			if err != nil || json.Unmarshal(out, &sessions) != nil {
				return false
			}
			for _, session := range sessions {
				if session.Name == newHandle && session.Kind == string(model.KindShell) && session.Cwd == remotePath {
					return true
				}
			}
			return false
		}, func() string {
			return "new shell did not start in recursively created directory: " + safeTerminalDiagnostic(capture.currentText())
		})
		capture.waitCurrent(t, "client-a/"+newHandle, 15*time.Second)
		assertContainerShellPWD(t, runtime, controller, binary, owner, "client-a", newHandle, remotePath)
		out, err := run(binary, "bookmarks", "client-a", "--config", "/root/.ducklord/config.yaml")
		if err != nil {
			t.Fatalf("read saved remote bookmarks: %v: %s", err, out)
		}
		found, fixturePreserved := false, false
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 {
				found = found || fields[0] == bookmark && fields[1] == remotePath
				fixturePreserved = fixturePreserved || fields[0] == "alpha-project" && fields[1] == "/home/duck/projects/alpha"
			}
		}
		if !found || !fixturePreserved {
			t.Fatalf("saved bookmark missing or existing fixture changed: %s", out)
		}
	})
	for _, mode := range []string{"existing bookmark", "use path once"} {
		t.Run(mode, func(t *testing.T) {
			run := func(args ...string) ([]byte, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				return exec.CommandContext(ctx, runtime, append([]string{"exec", controller}, args...)...).CombinedOutput()
			}
			before, err := run(binary, "bookmarks", "client-a", "--config", "/root/.ducklord/config.yaml")
			if err != nil {
				t.Fatalf("read initial bookmarks: %v: %s", err, before)
			}
			newHandle := "wd" + strconv.FormatInt(time.Now().UnixNano(), 36)
			remotePath := "/home/duck/projects/alpha"
			if mode == "use path once" {
				remotePath = "/tmp/" + newHandle + "/nested"
				if out, err := run("ssh", "client-a", "test", "!", "-e", "/tmp/"+newHandle); err != nil {
					t.Fatalf("one-time path must be absent: %v: %s", err, out)
				}
				t.Cleanup(func() { _, _ = run("ssh", "client-a", "rmdir", remotePath, "/tmp/"+newHandle) })
			}
			t.Cleanup(func() {
				_, _ = run(binary, "--name", "workspace-e2e-cli", "destroy", "client-a", newHandle,
					"--config", "/root/.ducklord/config.yaml")
			})
			if strings.Contains(capture.currentText(), "Session list pane:") {
				writePTY(t, terminal, "b")
				capture.waitCurrent(t, "Project pane:", 10*time.Second)
			}
			writePTY(t, terminal, "p")
			capture.waitCurrent(t, "Add Session pane", 10*time.Second)
			writePTY(t, terminal, "\r\r")
			capture.waitCurrent(t, "host ›", 10*time.Second)
			writePTY(t, terminal, "client-a\r")
			capture.waitCurrent(t, "choose a directory", 15*time.Second)
			if mode == "existing bookmark" {
				writePTY(t, terminal, "alpha-project\r")
			} else {
				writePTY(t, terminal, "browse\r")
				capture.waitCurrent(t, "type a remote directory path", 10*time.Second)
				writePTY(t, terminal, remotePath+"\r")
				capture.waitCurrent(t, "confirm recursive creation", 15*time.Second)
				writePTY(t, terminal, "\r")
				capture.waitCurrent(t, "directory created recursively", 15*time.Second)
				writePTY(t, terminal, "\x1b[B\r") // Use path once
			}
			capture.waitCurrent(t, "handle (default", 10*time.Second)
			writePTY(t, terminal, newHandle+"\r")
			capture.waitCurrent(t, "client-a/"+newHandle, 20*time.Second)
			assertContainerShellPWD(t, runtime, controller, binary, owner, "client-a", newHandle, remotePath)
			after, err := run(binary, "bookmarks", "client-a", "--config", "/root/.ducklord/config.yaml")
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("%s changed bookmark inventory: err=%v before=%s after=%s", mode, err, before, after)
			}
		})
	}
}

// TestDucklordWorkspaceTwoLivePanesContainerE2E proves that split panes keep
// separate, concurrent SSH/Ducklion raw subscriptions without list selection.
func TestDucklordWorkspaceTwoLivePanesContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	binary := os.Getenv("DUCKLORD_E2E_BINARY")
	if binary == "" {
		binary = "ducklord"
	}
	var sessions []protocol.SessionSummary
	for _, suffix := range []string{"a", "b"} {
		handle := fmt.Sprintf("ws2%s%x", suffix, time.Now().UnixNano()&0xffffff)
		out, err := exec.Command(runtime, "exec", controller, binary, "--name", "workspace-two-cli", "start", "client-a", "--name", handle,
			"--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "bash").CombinedOutput()
		if err != nil {
			t.Fatalf("start %s: %v: %s", handle, err, out)
		}
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", controller, binary, "--name", "workspace-two-cli", "destroy", "client-a", handle,
				"--config", "/root/.ducklord/config.yaml").CombinedOutput()
		})
		for _, session := range listContainerSessions(t, runtime, controller, "client-a") {
			if session.Handle == handle {
				sessions = append(sessions, session)
				break
			}
		}
	}
	if len(sessions) != 2 {
		t.Fatalf("dedicated sessions missing: %+v", sessions)
	}
	remoteA, ok := findContainerSession(t, runtime, controller, "client-a", sessions[0].SessionID)
	if !ok {
		t.Fatal("first session identity missing")
	}
	remoteB, ok := findContainerSession(t, runtime, controller, "client-a", sessions[1].SessionID)
	if !ok {
		t.Fatal("second session identity missing")
	}
	a, _ := ducklord.IdentityFromSession(remoteA)
	state := ducklord.NewActivityState()
	projectID, err := state.ProjectLayout.AddProject("Live split")
	if err != nil {
		t.Fatal(err)
	}
	_, err = state.ProjectLayout.Place(projectID, a, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	home := fmt.Sprintf("/tmp/ducklord-two-pane-e2e-%d", os.Getpid())
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("link isolated SSH: %v: %s", err, out)
	}
	localState := filepath.Join(t.TempDir(), "state.json")
	if err := (ducklord.ActivityStateStore{Path: localState}).Save(state); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(runtime, "cp", localState, controller+":"+home+"/.ducklord/state.json").CombinedOutput(); err != nil {
		t.Fatalf("install isolated layout: %v: %s", err, out)
	}
	owner := fmt.Sprintf("workspace-two-%d", time.Now().UnixNano())
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		binary, "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1bq"))
		time.Sleep(300 * time.Millisecond)
		if listing, err := exec.Command(runtime, "exec", controller, "ps", "-ef").Output(); err == nil {
			needle := binary + " tui --name " + owner + " --config"
			for _, line := range strings.Split(string(listing), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 3 || !strings.Contains(line, needle) {
					continue
				}
				if pid, parseErr := strconv.Atoi(fields[0]); parseErr == nil {
					_, _ = exec.Command(runtime, "exec", controller, "kill", strconv.Itoa(pid)).CombinedOutput()
				}
			}
		}
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 24, 120)
	capture.waitCurrent(t, sessions[0].Handle, 20*time.Second)
	writePTY(t, terminal, "/"+sessions[0].Handle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 20*time.Second)
	writePTY(t, terminal, "\x1d")
	capture.waitCurrent(t, "client-a/"+sessions[0].Handle, 20*time.Second)
	writePTY(t, terminal, "bp")
	capture.waitCurrent(t, "Add Session pane", 10*time.Second)
	writePTY(t, terminal, "j\rj\r"+sessions[1].Handle)
	capture.waitCurrent(t, "find › "+sessions[1].Handle, 10*time.Second)
	writePTY(t, terminal, "\r")
	for _, handle := range []string{sessions[0].Handle, sessions[1].Handle} {
		capture.waitCurrent(t, "client-a/"+handle, 20*time.Second)
	}
	oldBPaneID := ""
	if data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output(); err != nil {
		t.Fatalf("read persisted pane layout: %v", err)
	} else {
		var persisted ducklord.ActivityState
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Fatal(err)
		}
		identity, _ := ducklord.IdentityFromSession(remoteB)
		if ids := persisted.ProjectLayout.ProjectsFor(identity); len(ids) != 1 || ids[0] != projectID {
			t.Fatalf("TUI pane placement did not persist: %v", ids)
		}
		for _, tab := range persisted.ProjectLayout.Project(projectID).Tabs {
			if id := paneIDForSession(tab.Root, identity); id != "" {
				oldBPaneID = id
			}
		}
	}
	if oldBPaneID == "" {
		t.Fatal("placed B pane ID missing")
	}
	writePTY(t, terminal, "(") // select A so B can be re-split beside it
	writePTY(t, terminal, "pj\rj\r"+sessions[1].Handle+"\r")
	capture.waitCurrent(t, "move its pane, not duplicate it", 10*time.Second)
	writePTY(t, terminal, "\r") // Cancel is the safe default
	writePTY(t, terminal, "pj\rj\r"+sessions[1].Handle+"\r")
	capture.waitCurrent(t, "move its pane, not duplicate it", 10*time.Second)
	writePTY(t, terminal, "\x1b[A\r") // Move explicitly
	moveData, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
	if err != nil {
		t.Fatalf("read moved layout: %v", err)
	}
	var movedState ducklord.ActivityState
	if err := json.Unmarshal(moveData, &movedState); err != nil {
		t.Fatal(err)
	}
	identityB, _ := ducklord.IdentityFromSession(remoteB)
	if _, ok := movedState.ProjectLayout.PaneSession(projectID, oldBPaneID); ok {
		t.Fatal("old B pane survived confirmed move")
	}
	if ids := movedState.ProjectLayout.ProjectsFor(identityB); len(ids) != 1 || ids[0] != projectID {
		t.Fatalf("B move duplicated membership: %v", ids)
	}
	if _, ok := findContainerSession(t, runtime, controller, "client-a", sessions[1].SessionID); !ok {
		t.Fatal("local pane move stopped remote B")
	}
	writePTY(t, terminal, "b") // the original keyboard move must still stream both panes
	var preDragMarkers [2]string
	for i, session := range sessions {
		marker := fmt.Sprintf("BEFOREDRAG%d-%d", i, os.Getpid())
		preDragMarkers[i] = marker
		if out, err := exec.Command(runtime, "exec", controller, binary, "--name", "workspace-two-cli", "send", "client-a", session.Handle,
			"printf '"+marker+"\\n'", "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
			t.Fatalf("seed pre-drag marker: %v: %s", err, out)
		}
		capture.waitCurrent(t, marker, 20*time.Second)
	}
	writePTY(t, terminal, "b") // Project focus for the drag target
	// Drag an already-placed Session from the quick list onto A's live pane.
	// The centered move confirmation must avoid duplicating B in this Project.
	oldDraggedPaneID := ""
	for _, tab := range movedState.ProjectLayout.Project(projectID).Tabs {
		if id := paneIDForSession(tab.Root, identityB); id != "" {
			oldDraggedPaneID = id
		}
	}
	if oldDraggedPaneID == "" {
		t.Fatal("moved B pane missing before drag")
	}
	geometry := ducklord.CalculateWorkspaceGeometry(120, 24, 4)
	sourceX, sourceY, sourceFound := workspaceScreenPoint(capture.currentText(), sessions[1].Handle, 1, geometry.Terminal.X-1)
	targetX, targetY, targetFound := workspaceScreenPoint(capture.currentText(), "client-a/"+sessions[0].Handle, geometry.Terminal.X, 120)
	if !sourceFound || !targetFound {
		t.Fatal("drag source or target not visible in the workspace TUI")
	}
	writePTY(t, terminal, fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<32;1;1M\x1b[<0;1;1m", sourceX, sourceY))
	time.Sleep(200 * time.Millisecond)
	if screen := capture.currentText(); strings.Contains(screen, "Place dragged Session pane") {
		t.Fatal("drop outside Terminal area opened a placement modal")
	}
	if data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output(); err != nil {
		t.Fatal("read layout after invalid drop: ", err)
	} else {
		var unchanged ducklord.ActivityState
		if json.Unmarshal(data, &unchanged) != nil {
			t.Fatal("decode layout after invalid drop")
		}
		if _, still := unchanged.ProjectLayout.PaneSession(projectID, oldDraggedPaneID); !still {
			t.Fatal("drop outside Terminal area changed pane placement")
		}
	}
	writePTY(t, terminal, fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<32;%d;%dM\x1b[<0;%d;%dm", sourceX, sourceY, targetX, targetY, targetX, targetY))
	capture.waitCurrent(t, "Place dragged Session pane", 10*time.Second)
	writePTY(t, terminal, "jj\r") // split horizontally
	capture.waitCurrent(t, "move its pane, not duplicate it", 10*time.Second)
	writePTY(t, terminal, "\x1b[A\r") // explicitly move B
	waitE2E(t, 10*time.Second, func() bool {
		data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		if err != nil {
			return false
		}
		var latest ducklord.ActivityState
		if json.Unmarshal(data, &latest) != nil {
			return false
		}
		if _, still := latest.ProjectLayout.PaneSession(projectID, oldDraggedPaneID); still {
			return false
		}
		project := latest.ProjectLayout.Project(projectID)
		return project != nil && len(project.Tabs) == 1 && project.Tabs[0].Root != nil && project.Tabs[0].Root.Direction == ducklord.SplitHorizontal &&
			paneIDForSession(project.Tabs[0].Root, a) != "" && paneIDForSession(project.Tabs[0].Root, identityB) != "" &&
			len(latest.ProjectLayout.ProjectsFor(identityB)) == 1 && latest.ProjectLayout.ProjectsFor(identityB)[0] == projectID
	}, func() string { return "TUI drag did not atomically move B's pane without duplication" })
	// Drag starts by focusing the Session list. Explicitly establish Project
	// focus before toggling back, rather than relying on the pre-drag focus.
	writePTY(t, terminal, fmt.Sprintf("\x1b[<0;1;%dM\x1b[<0;1;%dm", geometry.Projects.Y, geometry.Projects.Y))
	capture.waitCurrent(t, "Project pane:", 10*time.Second)
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Session list pane:", 10*time.Second)
	waitE2E(t, 20*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, preDragMarkers[0]) && strings.Contains(screen, preDragMarkers[1])
	}, func() string { return "dragged layout did not restore both pre-existing PTY views" })
	for round := 1; round <= 2; round++ {
		markers := []string{fmt.Sprintf("LIVEA%d-%d", round, os.Getpid()), fmt.Sprintf("LIVEB%d-%d", round, os.Getpid())}
		for i, session := range sessions {
			out, err := exec.Command(runtime, "exec", controller, binary, "--name", "workspace-two-cli", "send", "client-a", session.Handle,
				"printf '"+markers[i]+"\\n'", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
			if err != nil {
				t.Fatalf("send marker %s: %v: %s", markers[i], err, out)
			}
		}
		waitE2E(t, 20*time.Second, func() bool {
			screen := capture.currentText()
			return strings.Contains(screen, markers[0]) && strings.Contains(screen, markers[1])
		}, func() string {
			screen := capture.currentText()
			remoteSeen := [2]bool{}
			for i, session := range sessions {
				out, err := exec.Command(runtime, "exec", controller, binary, "--name", "workspace-two-cli", "read", "client-a", session.Handle,
					"--lines", "30", "--config", "/root/.ducklord/config.yaml").Output()
				remoteSeen[i] = err == nil && strings.Contains(string(out), markers[i])
			}
			return fmt.Sprintf("split pane markers not simultaneously live (remote=%v stale=%t unavailable=%t loading=%t A=%t B=%t): %s",
				remoteSeen, strings.Contains(screen, "PTY output unavailable"), strings.Contains(screen, "PTY output is unavailable"),
				strings.Contains(screen, "loading live PTY output"), strings.Contains(screen, markers[0]), strings.Contains(screen, markers[1]), safeTerminalDiagnostic(screen))
		})
	}
	writePTY(t, terminal, "bk") // Project pane: Live split -> Default Project
	capture.waitCurrent(t, "› Default Project", 10*time.Second)
	if screen := capture.currentText(); strings.Contains(screen, "client-a/"+sessions[0].Handle) {
		t.Fatalf("Project navigation kept the previous Terminal area: %q", safeTerminalDiagnostic(screen))
	}
	writePTY(t, terminal, "j") // Project pane: Default -> Live split
	capture.waitCurrent(t, "› Live split", 10*time.Second)
	capture.waitCurrent(t, "client-a/"+sessions[1].Handle, 10*time.Second)
	projectMarker := fmt.Sprintf("PROJECTB3-%d", os.Getpid())
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", "workspace-two-cli", "send", "client-a", sessions[1].Handle,
		"printf '"+projectMarker+"\\n'", "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("send Project-only pane marker: %v: %s", err, out)
	}
	capture.waitCurrent(t, projectMarker, 10*time.Second)
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Session list pane:", 10*time.Second)
	// Create a shell-first Session pane through the same Project modal. The
	// first remote directory choice must be this host's home, not Ducklion's
	// own working directory or a previous bookmark.
	newHandle := fmt.Sprintf("wsh%x", time.Now().UnixNano()&0xffffff)
	t.Cleanup(func() {
		_, _ = exec.Command(runtime, "exec", controller, binary, "--name", "workspace-two-cli", "destroy", "client-a", newHandle,
			"--config", "/root/.ducklord/config.yaml").CombinedOutput()
	})
	writePTY(t, terminal, "b")
	waitE2E(t, 10*time.Second, func() bool { return strings.Contains(capture.currentText(), "Project pane:") }, func() string {
		return "Project focus header: " + safeTerminalDiagnostic(strings.Join(strings.Split(capture.currentText(), "\n")[:3], "\n"))
	})
	writePTY(t, terminal, "p")
	capture.waitCurrent(t, "Add Session pane", 10*time.Second)
	writePTY(t, terminal, "\r\r") // new tab, new shell
	capture.waitCurrent(t, "host ›", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "choose a directory", 15*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "handle (default", 10*time.Second)
	writePTY(t, terminal, newHandle+"\r")
	waitE2E(t, 20*time.Second, func() bool {
		for _, session := range listContainerSessions(t, runtime, controller, "client-a") {
			if session.Handle != newHandle || session.Kind != model.KindShell || session.CWD != "/home/duck" {
				continue
			}
			data, readErr := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
			if readErr != nil {
				return false
			}
			var persisted ducklord.ActivityState
			if json.Unmarshal(data, &persisted) != nil {
				return false
			}
			remote, found := findContainerSession(t, runtime, controller, "client-a", session.SessionID)
			if !found {
				return false
			}
			identity, ok := ducklord.IdentityFromSession(remote)
			if !ok {
				return false
			}
			ids := persisted.ProjectLayout.ProjectsFor(identity)
			return len(ids) == 1 && ids[0] == projectID
		}
		return false
	}, func() string {
		return "new shell pane did not start in host home and persist in selected Project: screen=" +
			safeTerminalDiagnostic(capture.currentText()) + fmt.Sprintf(" sessions=%+v", listContainerSessions(t, runtime, controller, "client-a"))
	})
	assertContainerShellPWD(t, runtime, controller, binary, owner, "client-a", newHandle, "/home/duck")
}

// TestDucklordWorkspaceDefaultNewShellSplitContainerE2E catches discovery of a
// newly created shell becoming a separate Default tab before placement finishes.
func TestDucklordWorkspaceDefaultNewShellSplitContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	binary := os.Getenv("DUCKLORD_E2E_BINARY")
	if binary == "" {
		binary = "ducklord"
	}
	cliOwner := fmt.Sprintf("default-split-cli-%d", time.Now().UnixNano())
	targetHandle := fmt.Sprintf("dsta%x", time.Now().UnixNano()&0xffffff)
	newHandle := fmt.Sprintf("dstb%x", time.Now().UnixNano()&0xffffff)
	for _, handle := range []string{targetHandle, newHandle} {
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "destroy", "client-a", handle,
				"--config", "/root/.ducklord/config.yaml").CombinedOutput()
		})
	}
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "start", "client-a", "--name", targetHandle,
		"--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "bash").CombinedOutput(); err != nil {
		t.Fatalf("start Default target shell: %v: %s", err, out)
	}
	var targetIdentity ducklord.SessionIdentity
	waitE2E(t, 10*time.Second, func() bool {
		for _, session := range listContainerSessions(t, runtime, controller, "client-a") {
			if session.Handle == targetHandle {
				remote, found := findContainerSession(t, runtime, controller, "client-a", session.SessionID)
				if found {
					var ok bool
					targetIdentity, ok = ducklord.IdentityFromSession(remote)
					return ok
				}
			}
		}
		return false
	}, func() string { return "Default target shell identity not listed: " + targetHandle })
	state := ducklord.NewActivityState()
	if err := state.ProjectLayout.Discover(targetIdentity); err != nil {
		t.Fatal(err)
	}
	targetTab := state.ProjectLayout.Project(ducklord.DefaultProjectID).Tabs[0]
	targetPaneID := targetTab.Root.ID
	home := fmt.Sprintf("/tmp/ducklord-default-split-e2e-%d-%d", os.Getpid(), time.Now().UnixNano())
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("link isolated SSH: %v: %s", err, out)
	}
	localState := filepath.Join(t.TempDir(), "state.json")
	if err := (ducklord.ActivityStateStore{Path: localState}).Save(state); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(runtime, "cp", localState, controller+":"+home+"/.ducklord/state.json").CombinedOutput(); err != nil {
		t.Fatalf("install Default layout: %v: %s", err, out)
	}
	owner := fmt.Sprintf("default-split-%d", time.Now().UnixNano())
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color",
		binary, "tui", "--name", owner, "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x1bq"))
		time.Sleep(300 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner, "/root/.ducklord/config.yaml")
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 24, 120)
	capture.waitCurrent(t, targetHandle, 20*time.Second)
	writePTY(t, terminal, "/"+targetHandle+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 20*time.Second)
	// Prefix commands must leave the focused remote writer before opening a
	// local modal; neither the prefix nor its command may reach the shell.
	writePTY(t, terminal, "\x02,")
	capture.waitCurrent(t, "Rename Terminal tab", 10*time.Second)
	writePTY(t, terminal, "Shell work\r")
	capture.waitCurrent(t, "Shell work", 10*time.Second)
	waitE2E(t, 10*time.Second, func() bool {
		data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
		var persisted ducklord.ActivityState
		if err != nil || json.Unmarshal(data, &persisted) != nil {
			return false
		}
		project := persisted.ProjectLayout.Project(ducklord.DefaultProjectID)
		if project == nil {
			return false
		}
		for _, tab := range project.Tabs {
			if tab.ID == targetTab.ID {
				return tab.Name == "Shell work"
			}
		}
		return false
	}, func() string { return "renamed tab was not persisted" })
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 10*time.Second)
	writePTY(t, terminal, "\x02\\")
	capture.waitCurrent(t, "New shell session", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "host ›", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "choose a directory", 15*time.Second)
	writePTY(t, terminal, "\r") // selected host's home
	capture.waitCurrent(t, "handle (default", 10*time.Second)
	writePTY(t, terminal, newHandle+"\r")
	var lastLayout []byte
	waitE2E(t, 20*time.Second, func() bool {
		for _, session := range listContainerSessions(t, runtime, controller, "client-a") {
			if session.Handle != newHandle || session.Kind != model.KindShell || session.CWD != "/home/duck" {
				continue
			}
			remote, found := findContainerSession(t, runtime, controller, "client-a", session.SessionID)
			identity, ok := ducklord.IdentityFromSession(remote)
			if !found || !ok {
				return false
			}
			var err error
			lastLayout, err = exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
			var persisted ducklord.ActivityState
			if err != nil || json.Unmarshal(lastLayout, &persisted) != nil {
				return false
			}
			project := persisted.ProjectLayout.Project(ducklord.DefaultProjectID)
			ids := persisted.ProjectLayout.ProjectsFor(identity)
			if project == nil || len(ids) != 1 || ids[0] != ducklord.DefaultProjectID {
				return false
			}
			for _, tab := range project.Tabs {
				if tab.ID != targetTab.ID {
					continue
				}
				root := tab.Root
				return root != nil && root.Direction == ducklord.SplitVertical && root.First != nil && root.Second != nil &&
					root.First.ID == targetPaneID && root.First.Session != nil && *root.First.Session == targetIdentity &&
					root.Second.Session != nil && *root.Second.Session == identity && persisted.ProjectLayout.Validate() == nil
			}
		}
		return false
	}, func() string {
		return "new shell did not persist as a Default vertical split: screen=" + safeTerminalDiagnostic(capture.currentText()) +
			" layout=" + string(lastLayout)
	})
	assertContainerShellPWD(t, runtime, controller, binary, owner, "client-a", newHandle, "/home/duck")
	// Both headers must be visible in the selected tab, not merely discoverable
	// as separate Default tabs in the saved layout.
	waitE2E(t, 10*time.Second, func() bool {
		screen := capture.currentText()
		return strings.Contains(screen, "client-a/"+targetHandle) && strings.Contains(screen, "client-a/"+newHandle)
	}, func() string {
		return "Default split panes not both visible: " + safeTerminalDiagnostic(capture.currentText())
	})
	// The add button is a real mouse target and enters the new-tab source
	// chooser directly, without asking for a split placement first.
	x, y, foundAdd := workspaceScreenPoint(capture.currentText(), "[+]", 1, 120)
	if !foundAdd {
		t.Fatal("tab add button was not visible")
	}
	writePTY(t, terminal, fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", x+1, y, x+1, y))
	capture.waitCurrent(t, "New shell session", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "host ›", 10*time.Second)
	writePTY(t, terminal, "\x1b") // cancel before creating another remote session
}

// TestDucklordWorkspaceProjectEnterFocusContainerE2E keeps quick-list selection
// independent from the Project pane's focused PTY across a real SSH bridge.
func TestDucklordWorkspaceProjectEnterFocusContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	binary := os.Getenv("DUCKLORD_E2E_BINARY")
	if binary == "" {
		binary = "ducklord"
	}
	cliOwner := fmt.Sprintf("project-focus-cli-%d", time.Now().UnixNano())
	handles := []string{
		fmt.Sprintf("pfa%x", time.Now().UnixNano()&0xffffff),
		fmt.Sprintf("pfb%x", (time.Now().UnixNano()+1)&0xffffff),
	}
	sessions := make([]ducklord.RemoteSession, 2)
	for i, handle := range handles {
		out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "start", "client-a", "--name", handle,
			"--kind", "shell", "--cwd", "/home/duck", "--config", "/root/.ducklord/config.yaml", "--", "bash").CombinedOutput()
		if err != nil {
			t.Fatalf("start isolated shell %s: %v: %s", handle, err, out)
		}
		t.Cleanup(func() {
			_, _ = exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "destroy", "client-a", handle,
				"--config", "/root/.ducklord/config.yaml").CombinedOutput()
		})
		waitE2E(t, 10*time.Second, func() bool {
			for _, session := range listContainerSessions(t, runtime, controller, "client-a") {
				if session.Handle == handle {
					var ok bool
					sessions[i], ok = findContainerSession(t, runtime, controller, "client-a", session.SessionID)
					return ok && sessions[i].InstanceID != ""
				}
			}
			return false
		}, func() string { return "isolated shell not listed: " + handle })
	}
	state := ducklord.NewActivityState()
	for i, name := range []string{"Focus A", "Focus B"} {
		projectID, err := state.ProjectLayout.AddProject(name)
		if err != nil {
			t.Fatal(err)
		}
		identity, ok := ducklord.IdentityFromSession(sessions[i])
		if !ok {
			t.Fatalf("missing Session identity for %s", handles[i])
		}
		if _, err := state.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			other, _ := ducklord.IdentityFromSession(sessions[0])
			if _, err := state.ProjectLayout.Place(projectID, other, ducklord.PlaceNewTab, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	home := fmt.Sprintf("/tmp/ducklord-project-focus-e2e-%d-%d", os.Getpid(), time.Now().UnixNano())
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/.ducklord").CombinedOutput(); err != nil {
		t.Fatalf("prepare isolated home: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = exec.Command(runtime, "exec", controller, "rm", "-r", home).CombinedOutput() })
	if out, err := exec.Command(runtime, "exec", controller, "ln", "-s", "/root/.ssh", home+"/.ssh").CombinedOutput(); err != nil {
		t.Fatalf("link isolated SSH: %v: %s", err, out)
	}
	localState := filepath.Join(t.TempDir(), "state.json")
	if err := (ducklord.ActivityStateStore{Path: localState}).Save(state); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(runtime, "cp", localState, controller+":"+home+"/.ducklord/state.json").CombinedOutput(); err != nil {
		t.Fatalf("install isolated layout: %v: %s", err, out)
	}
	// Keep only one raw subscription so Project navigation must promote the
	// selected pane rather than the first visible/quick-list pane.
	soundPath := home + "/" + handles[1] + ".wav"
	if out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", "cp /root/.ducklord/config.yaml "+home+"/.ducklord/limited.yaml && printf '\nraw_output_subscription_limit: 1\nnotification_levels:\n  task_completed: sound\nnotification_sounds:\n  task_completed: "+soundPath+"\n' >>"+home+"/.ducklord/limited.yaml").CombinedOutput(); err != nil {
		t.Fatalf("prepare limited subscription config: %v: %s", err, out)
	}
	if out, err := exec.Command(runtime, "exec", controller, "sh", "-c", `printf RIFF > "$1"`, "_", soundPath).CombinedOutput(); err != nil {
		t.Fatalf("prepare sound fixture: %v: %s", err, out)
	}
	if out, err := exec.Command(runtime, "exec", controller, "mkdir", "-p", home+"/bin").CombinedOutput(); err != nil {
		t.Fatalf("prepare notifier fixture directory: %v: %s", err, out)
	}
	notifier := filepath.Join(t.TempDir(), "ffplay")
	if err := os.WriteFile(notifier, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DUCKLORD_NOTIFY_E2E_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(runtime, "cp", notifier, controller+":"+home+"/bin/ffplay").CombinedOutput(); err != nil {
		t.Fatalf("install local notifier fixture: %v: %s", err, out)
	}
	if out, err := exec.Command(runtime, "exec", controller, "chmod", "700", home+"/bin/ffplay").CombinedOutput(); err != nil {
		t.Fatalf("make notifier fixture executable: %v: %s", err, out)
	}
	owner := fmt.Sprintf("project-focus-%d", time.Now().UnixNano())
	command := exec.Command(runtime, "exec", "-it", controller, "env", "HOME="+home, "TERM=xterm-256color", "PATH="+home+"/bin:/usr/local/bin:/usr/bin:/bin", "DUCKLORD_NOTIFY_E2E_LOG="+home+"/notifications.log",
		binary, "tui", "--name", owner, "--config", home+"/.ducklord/limited.yaml")
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = terminal.Write([]byte("\x1d\x1bq"))
		time.Sleep(300 * time.Millisecond)
		killNamedContainerTUI(runtime, controller, owner, home+"/.ducklord/limited.yaml")
		_ = terminal.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	capture := newSizedTUICapture(terminal, 24, 120)
	capture.waitCurrent(t, "Focus A", 20*time.Second)
	writePTY(t, terminal, "/"+handles[0]+"\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus:", 20*time.Second)
	writePTY(t, terminal, "\x1d")
	capture.waitCurrent(t, "› Focus A", 20*time.Second)
	capture.waitCurrent(t, "› "+handles[0]+" @client-a", 20*time.Second)
	writePTY(t, terminal, "bj")
	capture.waitCurrent(t, "› Focus B", 10*time.Second)
	capture.waitCurrent(t, "◇ client-a/"+handles[1], 10*time.Second)
	capture.waitCurrent(t, "› "+handles[0]+" @client-a", 10*time.Second)
	writePTY(t, terminal, "\x1b[6~")
	capture.waitCurrent(t, "◇ client-a/"+handles[0], 10*time.Second)
	// The one-slot output pool must reopen A after B was visible. A title
	// alone does not prove its resumed PTY stream is live.
	returnedMarker := fmt.Sprintf("RETURNED-A-%d", time.Now().UnixNano())
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "send", "client-a", handles[0],
		"printf '"+returnedMarker+"\\n'", "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("send resumed A marker: %v: %s", err, out)
	}
	capture.waitCurrent(t, returnedMarker, 10*time.Second)
	writePTY(t, terminal, "\x1b[5~")
	capture.waitCurrent(t, "◇ client-a/"+handles[1], 10*time.Second)
	projectMarker := fmt.Sprintf("PROJECTONLY-%d", time.Now().UnixNano())
	if out, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "send", "client-a", handles[1],
		"printf '"+projectMarker+"\\n'", "--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
		t.Fatalf("send Project-only output marker: %v: %s", err, out)
	}
	capture.waitCurrent(t, projectMarker, 10*time.Second)
	noRoute := fmt.Sprintf("%d", time.Now().UnixNano())
	writePTY(t, terminal, noRoute)
	time.Sleep(200 * time.Millisecond)
	for _, handle := range handles {
		out, readErr := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "read", "client-a", handle,
			"--lines", "50", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
		if readErr != nil || bytes.Contains(out, []byte(noRoute)) {
			t.Fatalf("Project-list input reached %s: err=%v output=%q", handle, readErr, safeTerminalDiagnostic(string(out)))
		}
	}
	// A pane click uses the same owner-gated focus path as Enter.
	paneX, paneY, found := workspaceScreenPoint(capture.currentText(), "client-a/"+handles[1], 32, 120)
	if !found {
		t.Fatal("mouse focus target not visible")
	}
	writePTY(t, terminal, fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", paneX, paneY, paneX, paneY))
	capture.waitCurrent(t, "▣ client-a/"+handles[1], 20*time.Second)
	capture.waitCurrent(t, "› "+handles[0]+" @client-a", 10*time.Second)
	toB := fmt.Sprintf("TOB-%d", time.Now().UnixNano())
	writePTY(t, terminal, "printf '"+toB+"\\n'\r")
	waitE2E(t, 10*time.Second, func() bool {
		out, readErr := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "read", "client-a", handles[1],
			"--lines", "50", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
		return readErr == nil && bytes.Contains(out, []byte(toB))
	}, func() string { return "Project B shell did not receive focused PTY input" })
	out, readErr := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "read", "client-a", handles[0],
		"--lines", "50", "--config", "/root/.ducklord/config.yaml").CombinedOutput()
	if readErr != nil || bytes.Contains(out, []byte(toB)) {
		t.Fatalf("focused input reached quick-selected A: err=%v output=%q", readErr, safeTerminalDiagnostic(string(out)))
	}
	writePTY(t, terminal, "\x1d")
	capture.waitCurrent(t, "Project pane:", 10*time.Second)
	writePTY(t, terminal, "k") // Focus A contains only A; B is outside it.
	capture.waitCurrent(t, "› Focus A", 10*time.Second)
	writePTY(t, terminal, "i")
	capture.waitCurrent(t, "Focus: Focus A", 10*time.Second)
	baseline, ok := findContainerSession(t, runtime, controller, "client-a", sessions[1].SessionID)
	if !ok {
		t.Fatal("Focus B Session vanished before notification test")
	}
	baselineSeq := baseline.ActivitySequences[model.NotificationTaskCompleted]
	insideBaseline, ok := findContainerSession(t, runtime, controller, "client-a", sessions[0].SessionID)
	if !ok {
		t.Fatal("Focus A Session vanished before notification test")
	}
	insideSeq := insideBaseline.ActivitySequences[model.NotificationTaskCompleted]
	hook := fmt.Sprintf("ducklion __ducklion_agent_hook_v1 codex '{\"type\":\"agent-turn-complete\",\"last-assistant-message\":\"private-focus-%d\"}'", time.Now().UnixNano())
	for turn := 1; turn <= 2; turn++ {
		if turn == 2 {
			writePTY(t, terminal, "i") // Clear focus; the same outside Project can now deliver.
			waitE2E(t, 10*time.Second, func() bool {
				return !strings.Contains(capture.currentText(), "Focus:")
			}, func() string { return "Project notification focus did not clear" })
		}
		if output, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "send", "client-a", handles[1], hook,
			"--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
			t.Fatalf("send Project focus hook %d: %v: %s", turn, err, output)
		}
		waitE2E(t, 10*time.Second, func() bool {
			remote, exists := findContainerSession(t, runtime, controller, "client-a", sessions[1].SessionID)
			return exists && remote.ActivitySequences[model.NotificationTaskCompleted] >= baselineSeq+uint64(turn)
		}, func() string { return "Project focus hook did not reach Ducklion" })
		waitE2E(t, 10*time.Second, func() bool {
			data, err := exec.Command(runtime, "exec", controller, "cat", home+"/.ducklord/state.json").Output()
			if err != nil {
				return false
			}
			var current ducklord.ActivityState
			if json.Unmarshal(data, &current) != nil {
				return false
			}
			identity, _ := ducklord.IdentityFromSession(sessions[1])
			entry := current.Sessions[identity.Key()]
			return entry.Observed[model.NotificationTaskCompleted] >= baselineSeq+uint64(turn) && entry.Unread[model.NotificationTaskCompleted]
		}, func() string { return "outside Project completion was not retained as unread" })
		if turn == 1 {
			time.Sleep(4 * time.Second)
			output, err := exec.Command(runtime, "exec", controller, "sh", "-c", `if [ -e "$1" ]; then cat "$1"; fi`, "_", home+"/notifications.log").Output()
			if err != nil || len(strings.TrimSpace(string(output))) != 0 {
				t.Fatalf("focused Project played sound for outside completion: %v: %s", err, output)
			}
			if output, err := exec.Command(runtime, "exec", controller, binary, "--name", cliOwner, "send", "client-a", handles[0], hook,
				"--config", "/root/.ducklord/config.yaml").CombinedOutput(); err != nil {
				t.Fatalf("send in-focus hook: %v: %s", err, output)
			}
			waitE2E(t, 10*time.Second, func() bool {
				remote, exists := findContainerSession(t, runtime, controller, "client-a", sessions[0].SessionID)
				return exists && remote.ActivitySequences[model.NotificationTaskCompleted] >= insideSeq+1
			}, func() string { return "in-focus hook did not reach Ducklion" })
			waitE2E(t, 10*time.Second, func() bool {
				output, err := exec.Command(runtime, "exec", controller, "cat", home+"/notifications.log").Output()
				return err == nil && strings.Count(string(output), soundPath) == 1
			}, func() string { return "focused Project completion did not play sound" })
		}
	}
	waitE2E(t, 10*time.Second, func() bool {
		output, err := exec.Command(runtime, "exec", controller, "cat", home+"/notifications.log").Output()
		return err == nil && strings.Count(string(output), soundPath) == 2
	}, func() string { return "completion sound did not play after clearing Project focus" })
	if output, err := exec.Command(runtime, "exec", controller, "cat", home+"/notifications.log").Output(); err != nil || strings.Contains(string(output), "private-focus-") {
		t.Fatalf("notifier log unavailable or leaked agent output: %v", err)
	}
}

type tuiCapture struct {
	mu     sync.Mutex
	data   []byte
	screen *ducklord.Terminal
	rows   int
	cols   int
	watch  map[string]bool
}

func newTUICapture(terminal *os.File) *tuiCapture {
	return newSizedTUICapture(terminal, 24, 80)
}

func newSizedTUICapture(terminal *os.File, rows, cols int) *tuiCapture {
	capture := &tuiCapture{screen: ducklord.NewTerminal(rows, cols, 0), rows: rows, cols: cols}
	go func() {
		buffer := make([]byte, 8192)
		for {
			n, err := terminal.Read(buffer)
			if n > 0 {
				capture.mu.Lock()
				capture.data = append(capture.data, buffer[:n]...)
				capture.screen.Write(buffer[:n])
				if len(capture.watch) != 0 {
					visible := strings.Join(capture.screen.RenderLines(capture.rows, capture.cols), "\n")
					for marker, seen := range capture.watch {
						if !seen && strings.Contains(visible, marker) {
							capture.watch[marker] = true
						}
					}
				}
				capture.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return capture
}

func (c *tuiCapture) watchCurrent(marker string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.watch == nil {
		c.watch = make(map[string]bool)
	}
	c.watch[marker] = strings.Contains(strings.Join(c.screen.RenderLines(c.rows, c.cols), "\n"), marker)
}

func (c *tuiCapture) everCurrent(marker string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watch[marker]
}

func (c *tuiCapture) currentText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.screen.RenderLines(c.rows, c.cols), "\n")
}

func (c *tuiCapture) waitCurrent(t *testing.T, needle string, timeout time.Duration) {
	t.Helper()
	waitE2E(t, timeout, func() bool { return strings.Contains(c.currentText(), needle) }, func() string {
		return fmt.Sprintf("current TUI screen did not contain %q; screen=%q", needle, safeTerminalDiagnostic(c.currentText()))
	})
}

func (c *tuiCapture) markerHasForeground(marker string, foreground int32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.screen.SnapshotState()
	screen := state.Primary
	if state.UseAlternate {
		screen = state.Alternate
	}
	for _, line := range screen.Lines {
		for start := 0; start < len(line.Cells); start++ {
			var text strings.Builder
			valid := true
			for index := start; index < len(line.Cells) && text.Len() < len(marker); index++ {
				cell := line.Cells[index]
				if cell.Width == 0 {
					continue
				}
				if cell.Style.Foreground != foreground {
					valid = false
				}
				text.WriteRune(cell.Rune)
			}
			if valid && text.String() == marker {
				return true
			}
		}
	}
	return false
}

func assertCurrentCreateModal(t *testing.T, capture *tuiCapture, start int, title, choice string, selected bool) {
	t.Helper()
	capture.waitCurrent(t, title, 15*time.Second)
	capture.waitCurrent(t, choice, 15*time.Second)
	screen := capture.currentText()
	lines := strings.Split(screen, "\n")
	for top, line := range lines {
		runes := []rune(line)
		left, right := -1, -1
		for index, r := range runes {
			if r == '╭' && left < 0 {
				left = index
			}
			if r == '╮' {
				right = index
			}
		}
		if left < 0 || right <= left {
			continue
		}
		bottom := -1
		for index := top + 1; index < len(lines); index++ {
			if strings.ContainsRune(lines[index], '╰') && strings.ContainsRune(lines[index], '╯') {
				bottom = index
				break
			}
		}
		if bottom < 0 || top != (24-(bottom-top+1))/2 {
			t.Fatalf("create modal is not vertically centered: %q", safeTerminalDiagnostic(screen))
		}
		raw := capture.since(start)
		if !strings.Contains(raw, ";5H"+modalBorder+"╭") {
			t.Fatalf("create modal is not horizontally centered: %q", safeTerminalDiagnostic(raw))
		}
		if !strings.Contains(raw, modalBorder) || !strings.Contains(raw, modalInput) || selected && !strings.Contains(raw, modalSelected) {
			t.Fatalf("create modal lacks semantic colors: %q", safeTerminalDiagnostic(raw))
		}
		return
	}
	t.Fatalf("current create modal has no frame: %q", safeTerminalDiagnostic(screen))
}

func (c *tuiCapture) position() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.data)
}

func (c *tuiCapture) since(position int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if position > len(c.data) {
		position = len(c.data)
	}
	return string(append([]byte(nil), c.data[position:]...))
}

func (c *tuiCapture) wait(t *testing.T, needle string, timeout time.Duration) {
	t.Helper()
	c.waitAfter(t, 0, needle, timeout)
}

func (c *tuiCapture) waitAfter(t *testing.T, position int, needle string, timeout time.Duration) {
	t.Helper()
	waitE2E(t, timeout, func() bool { return strings.Contains(c.since(position), needle) }, func() string {
		return fmt.Sprintf("TUI did not render %q; tail=%q", needle, safeTerminalDiagnostic(c.since(position)))
	})
}

// Read pwd from the live shell, independently of the session's reported CWD.
// The expected path never appears in the command, so terminal input echo cannot
// satisfy this assertion without the shell actually executing pwd.
func assertContainerShellPWD(t *testing.T, runtime, controller, binary, owner, host, handle, want string) {
	t.Helper()
	// Shell sessions support independent writers. Never reuse the connected
	// TUI's name for this short-lived inspection connection.
	owner = "pwd-check-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := []string{"exec", controller, binary}
		if owner != "" {
			command = append(command, "--name", owner)
		}
		return exec.CommandContext(ctx, runtime, append(command, args...)...).CombinedOutput()
	}
	marker := "PWD" + strconv.FormatInt(time.Now().UnixNano(), 36)
	command := fmt.Sprintf("printf '\\n%%s%%s%%s\\n' '%s:' \"$(pwd -P)\" ':%s'", marker, marker)
	if out, err := run("send", host, handle, command, "--config", "/root/.ducklord/config.yaml"); err != nil {
		t.Fatalf("request live shell pwd for %s: %v: %s", handle, err, safeTerminalDiagnostic(string(out)))
	}
	var output []byte
	var readErr error
	waitE2E(t, 15*time.Second, func() bool {
		output, readErr = run("read", host, handle, "--lines", "100", "--config", "/root/.ducklord/config.yaml")
		return readErr == nil && bytes.Contains(output, []byte(marker+":"+want+":"+marker))
	}, func() string {
		return fmt.Sprintf("shell %s pwd did not equal %q: err=%v output=%s", handle, want, readErr, safeTerminalDiagnostic(string(output)))
	})
}

func requiredE2EEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func listContainerSessions(t *testing.T, runtime, controller, host string) []protocol.SessionSummary {
	t.Helper()
	sessions := listContainerRemoteSessions(t, runtime, controller, host)
	result := make([]protocol.SessionSummary, 0, len(sessions))
	for _, session := range sessions {
		result = append(result, protocol.SessionSummary{SessionID: session.SessionID, Handle: session.Name, Kind: model.SessionKind(session.Kind), ProjectName: session.ProjectName, CWD: session.Cwd,
			OwnershipEpoch: session.OwnershipEpoch, RuntimeGeneration: session.RuntimeGeneration})
	}
	return result
}

func sessionInventoryDiagnostic(t *testing.T, runtime, controller, host string) string {
	t.Helper()
	sessions := listContainerSessions(t, runtime, controller, host)
	values := make([]string, 0, len(sessions))
	for _, session := range sessions {
		values = append(values, fmt.Sprintf("%s/%s/%s/%s", session.SessionID, session.Handle, session.Kind, session.CWD))
	}
	return strings.Join(values, ",")
}

func findContainerSession(t *testing.T, runtime, controller, host, sessionID string) (ducklord.RemoteSession, bool) {
	t.Helper()
	for _, session := range listContainerRemoteSessions(t, runtime, controller, host) {
		if session.SessionID == sessionID {
			return session, true
		}
	}
	return ducklord.RemoteSession{}, false
}

func listContainerRemoteSessions(t *testing.T, runtime, controller, host string) []ducklord.RemoteSession {
	t.Helper()
	out, err := exec.Command(runtime, "exec", controller, "ducklord", "sessions", host, "--json", "--config", "/tmp/e2e-inspector.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("list sessions on %s: %v (remote output suppressed)", host, err)
	}
	var sessions []ducklord.RemoteSession
	if err := json.Unmarshal(out, &sessions); err != nil {
		t.Fatalf("decode session inventory from %s: %v", host, err)
	}
	return sessions
}

func writePTY(t *testing.T, terminal *os.File, value string) {
	t.Helper()
	if _, err := terminal.Write([]byte(value)); err != nil {
		t.Fatalf("write TUI input: %v", err)
	}
}

func TestWorkspaceScreenPointIgnoresStylesAndFindsRepeatedLabels(t *testing.T) {
	x, y, ok := workspaceScreenPoint("\x1b[38;2;1;2;3mpane\x1b[0m   pane", "pane", 7, 20)
	if !ok || x != 8 || y != 1 {
		t.Fatalf("styled repeated label: (%d, %d, %t)", x, y, ok)
	}
}

func workspaceScreenPoint(screen, label string, minX, maxX int) (x, y int, found bool) {
	// RenderLines includes generated SGR styles; they occupy no screen cells.
	screen = regexp.MustCompile(`\x1b\[[0-9;:]*m`).ReplaceAllString(screen, "")
	for row, line := range strings.Split(screen, "\n") {
		for start := 0; start < len(line); {
			offset := strings.Index(line[start:], label)
			if offset < 0 {
				break
			}
			offset += start
			column := modalCellWidth(line[:offset]) + 1
			if column >= minX && column < maxX {
				return column, row + 1, true
			}
			start = offset + len(label)
		}
	}
	return 0, 0, false
}

func waitE2E(t *testing.T, timeout time.Duration, ready func() bool, diagnostic func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(diagnostic())
}

func safeTerminalDiagnostic(value string) string {
	runes := []rune(value)
	if len(runes) > 800 {
		runes = runes[len(runes)-800:]
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, string(runes))
}
