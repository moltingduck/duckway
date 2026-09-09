package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	start := capture.position()
	writePTY(t, terminal, "c")
	capture.waitAfter(t, start, "new session: choose agent or shell", 10*time.Second)
	modal := capture.since(start)
	if !strings.Contains(modal, "\x1b[") || !strings.Contains(modal, "╭") || !strings.Contains(modal, "Shell session") {
		t.Fatalf("create modal lacks frame/color/choices: %q", safeTerminalDiagnostic(modal))
	}

	start = capture.position()
	writePTY(t, terminal, "\x1b[B\r") // shell, then submit
	capture.waitAfter(t, start, "choose host number/name", 10*time.Second)
	start = capture.position()
	writePTY(t, terminal, "\r") // currently selected client-a
	capture.waitAfter(t, start, "choose configured project", 15*time.Second)
	start = capture.position()
	writePTY(t, terminal, "\r") // alpha-project
	capture.waitAfter(t, start, "handle (default alpha)", 15*time.Second)
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
	capture.waitAfter(t, start, "RESTART SESSION", 10*time.Second)
	writePTY(t, terminal, "\r")
	waitE2E(t, 15*time.Second, func() bool {
		session, ok := findContainerSession(t, runtime, controller, "client-a", created.SessionID)
		return ok && session.RuntimeGeneration == created.RuntimeGeneration+1 && session.Status == string(model.StatusRunning)
	}, func() string { return "action-menu restart did not advance the selected session generation" })
	// Inventory completion can precede the TUI goroutine consuming its durable
	// completion event by one redraw. Give that local event loop a bounded beat;
	// the generation assertion above remains the authoritative result.
	time.Sleep(750 * time.Millisecond)

	// Destructive selection still requires a second confirmation, and Escape
	// must leave the exact target untouched.
	start = capture.position()
	writePTY(t, terminal, "m")
	capture.waitAfter(t, start, "Session actions", 10*time.Second)
	writePTY(t, terminal, "x")
	capture.waitAfter(t, start, "DESTROY SESSION", 10*time.Second)
	writePTY(t, terminal, "\x1b")
	if _, ok := findContainerSession(t, runtime, controller, "client-a", created.SessionID); !ok {
		t.Fatal("canceling destructive confirmation removed the session")
	}

	start = capture.position()
	writePTY(t, terminal, "m")
	capture.waitAfter(t, start, "Session actions", 10*time.Second)
	writePTY(t, terminal, "x")
	capture.waitAfter(t, start, "DESTROY SESSION", 10*time.Second)
	writePTY(t, terminal, "\r")
	waitE2E(t, 15*time.Second, func() bool {
		_, ok := findContainerSession(t, runtime, controller, "client-a", created.SessionID)
		return !ok
	}, func() string { return "action-menu destroy did not remove the exact selected session" })
}

type tuiCapture struct {
	mu   sync.Mutex
	data []byte
}

func newTUICapture(terminal *os.File) *tuiCapture {
	capture := &tuiCapture{}
	go func() {
		buffer := make([]byte, 8192)
		for {
			n, err := terminal.Read(buffer)
			if n > 0 {
				capture.mu.Lock()
				capture.data = append(capture.data, buffer[:n]...)
				capture.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return capture
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
