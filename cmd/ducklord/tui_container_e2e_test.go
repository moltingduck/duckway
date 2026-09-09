package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// TestDucklordCreateTUIContainerE2E is driven by scripts/ducklord-tui-e2e.sh.
// It uses a real terminal, Ducklord process, SSH stdio bridge, Ducklion daemon,
// and native remote PTY. The ordinary unit suite remains container-free.
func TestDucklordCreateTUIContainerE2E(t *testing.T) {
	if os.Getenv("DUCKLORD_TUI_CONTAINER_E2E") != "1" {
		t.Skip("run through scripts/ducklord-tui-e2e.sh")
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
	command := exec.Command(runtime, "exec", "-it", controller, "env", "TERM=xterm-256color", "ducklord", "tui", "--config", "/root/.ducklord/config.yaml")
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
	assertCurrentCreateModal(t, capture, start, "choose configured project", "alpha-project", true)
	start = capture.position()
	writePTY(t, terminal, "\r") // alpha-project
	assertCurrentCreateModal(t, capture, start, "handle (default alpha)", "handle", false)
	start = capture.position()
	writePTY(t, terminal, handle+"\r")
	capture.waitAfter(t, start, "j/k move", 20*time.Second)

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
	if created.SessionID == "" || created.Kind != "shell" || created.CWD != "/home/duck/projects/alpha" {
		t.Fatalf("created identity = %#v", created)
	}
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

	start = capture.position()
	writePTY(t, terminal, "\r") // focus the newly created PTY
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
	capture.waitAfter(t, start, "m actions", 10*time.Second)
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
		return ok && session.RuntimeGeneration == created.RuntimeGeneration+1 && session.Status == string(model.StatusRunning)
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
	writePTY(t, terminal, "n \r")  // reopen, toggle and save
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
}

type tuiCapture struct {
	mu     sync.Mutex
	data   []byte
	screen *ducklord.Terminal
}

func newTUICapture(terminal *os.File) *tuiCapture {
	capture := &tuiCapture{screen: ducklord.NewTerminal(24, 80, 0)}
	go func() {
		buffer := make([]byte, 8192)
		for {
			n, err := terminal.Read(buffer)
			if n > 0 {
				capture.mu.Lock()
				capture.data = append(capture.data, buffer[:n]...)
				capture.screen.Write(buffer[:n])
				capture.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return capture
}

func (c *tuiCapture) currentText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.screen.RenderLines(24, 80), "\n")
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
		result = append(result, protocol.SessionSummary{SessionID: session.SessionID, Handle: session.Name, Kind: model.SessionKind(session.Kind), CWD: session.Cwd,
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
		t.Fatalf("list sessions on %s: %v: %s", host, err, safeTerminalDiagnostic(string(out)))
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
