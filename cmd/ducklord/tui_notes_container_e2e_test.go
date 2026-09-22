package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// TestDucklordNotesTUIContainerE2E exercises the live Notes path when the
// container fixture is enabled. It is opt-in because the fixture owns the
// controller and clipboard environment.
func TestDucklordNotesTUIContainerE2E(t *testing.T) {
	if !notesTUIE2EAllowed(os.Getenv, inspectNotesE2EContainer) {
		t.Skip("run through scripts/ducklord-tui-e2e.sh on a disposable host")
	}
	runtime := requiredE2EEnv(t, "DUCKLORD_E2E_RUNTIME")
	controller := requiredE2EEnv(t, "DUCKLORD_E2E_CONTROLLER")
	prefix := os.Getenv("DUCKLORD_E2E_CONTAINER_PREFIX")
	var safePrefix strings.Builder
	for _, r := range prefix {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			safePrefix.WriteRune(r)
		} else {
			safePrefix.WriteByte('_')
		}
	}
	prefix = safePrefix.String()
	if prefix == "" {
		t.Skip("set DUCKLORD_E2E_CONTAINER_PREFIX to isolate the Notes fixture")
	}
	fixtureRoot := fmt.Sprintf("/tmp/ducklord-notes-%s-%d", prefix, os.Getpid())
	countFile := fixtureRoot + "-editor-count"
	editorPath := fixtureRoot + "-editor"
	backupRoot := fixtureRoot + "-backup"
	backupReady := backupRoot + "/ready"
	projectDir := "/root/.ducklord/projects/default"
	projectNotes := "/root/.ducklord/projects/default/notes.md"
	globalNotes := "/root/.ducklord/notes.md"
	editorScript := "#!/bin/sh\nset -eu\ncount_file=" + countFile + "\ncount=0\nif [ -f \"$count_file\" ]; then count=$(cat \"$count_file\"); fi\ncount=$((count + 1))\nprintf '%s' \"$count\" > \"$count_file\"\ncase \"$count\" in\n1) printf '%s\\n' '## First' 'final alpha' '## Added' 'final added body' '## Second' 'final beta' > \"$1\" ;;\n*) exit 1 ;;\nesac\n"
	editorEncoded := base64.StdEncoding.EncodeToString([]byte(editorScript))
	seed := "set -eu; rm -rf " + fixtureRoot + " " + backupRoot + " " + editorPath + " " + countFile + "; mkdir -p " + backupRoot + "; " +
		"if [ -d " + projectDir + " ]; then cp -a " + projectDir + " " + backupRoot + "/project-dir; else : > " + backupRoot + "/project-dir.missing; fi; " +
		"mkdir -p " + projectDir + "; " +
		"if [ -e " + globalNotes + " ]; then cp -p " + globalNotes + " " + backupRoot + "/global; else : > " + backupRoot + "/global.missing; fi; " +
		"touch " + backupReady + "; " +
		"printf '%s\\n' '## First' 'body only alpha' '## Second' 'body only beta' > " + projectNotes + "; printf '%s\\n' '## Global' 'global body' > " + globalNotes + "; " +
		"printf '%s' '" + editorEncoded + "' | base64 -d > " + editorPath + "; chmod 700 " + editorPath + "; rm -f " + countFile
	restore := "set +e; restore_status=0; " +
		"if [ -f " + backupReady + " ]; then " +
		"rm -rf " + projectDir + "; status=$?; if [ $status -ne 0 ] && [ $restore_status -eq 0 ]; then restore_status=$status; fi; " +
		"if [ -d " + backupRoot + "/project-dir ]; then cp -a " + backupRoot + "/project-dir " + projectDir + "; status=$?; if [ $status -ne 0 ] && [ $restore_status -eq 0 ]; then restore_status=$status; fi; fi; " +
		"if [ -f " + backupRoot + "/global.missing ]; then rm -f " + globalNotes + "; status=$?; elif [ -f " + backupRoot + "/global ]; then cp -p " + backupRoot + "/global " + globalNotes + "; status=$?; else status=0; fi; if [ $status -ne 0 ] && [ $restore_status -eq 0 ]; then restore_status=$status; fi; fi; " +
		"rm -rf " + fixtureRoot + " " + backupRoot + " " + editorPath + " " + countFile + "; cleanup_status=$?; " +
		"if [ $cleanup_status -ne 0 ] && [ $restore_status -eq 0 ]; then restore_status=$cleanup_status; fi; " +
		"exit $restore_status"
	defer func() {
		if out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", restore).CombinedOutput(); err != nil {
			t.Errorf("restore Notes fixture: %v: %s", err, out)
		}
	}()
	if out, err := exec.Command(runtime, "exec", controller, "sh", "-lc", seed).CombinedOutput(); err != nil {
		t.Fatalf("seed Notes fixture: %v: %s", err, out)
	}
	cmd := exec.Command(runtime, "exec", "-it", controller, "env", "TERM=xterm-256color", "EDITOR="+editorPath, "ducklord", "tui", "--config", "/root/.ducklord/config.yaml")
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = terminal.Write([]byte("\x1bq"))
		_ = terminal.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	capture := newTUICapture(terminal)
	capture.wait(t, "Default Project", 20*time.Second)
	// Establish Project-pane focus through the configured default shortcut
	// before opening Notes. The status line is the observable focus indicator;
	// this avoids mistaking the session list or a terminal pane for Project
	// focus. The fixture uses the default config, where project_focus is b.
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Project pane:", 10*time.Second)
	writePTY(t, terminal, "o")
	capture.waitCurrent(t, "✦ Notes Codex ✦", 10*time.Second)
	capture.waitCurrent(t, "Project default", 5*time.Second)
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool {
		return !strings.Contains(capture.currentText(), "✦ Notes Codex ✦")
	}, func() string {
		return "Project Notes modal did not close: " + safeTerminalDiagnostic(capture.currentText())
	})

	// Prove Session Notes while the workspace has its session-list focus, before
	// the focused-terminal route exercises focus restoration.
	geometry := ducklord.CalculateWorkspaceGeometry(80, 24, 4)
	quick := geometry.Quick
	writePTY(t, terminal, string(workspaceMouse(0, quick.X+1, quick.Y+1, false)))
	writePTY(t, terminal, string(workspaceMouse(0, quick.X+1, quick.Y+1, true)))
	capture.waitCurrent(t, "Session list pane:", 10*time.Second)
	writePTY(t, terminal, "/alpha\r")
	capture.waitCurrent(t, "alpha @client-a", 10*time.Second)
	// The search overlay owns input until it is closed; direct Notes routing
	// starts from the resulting Session list focus.
	writePTY(t, terminal, "\x03")
	waitE2E(t, 10*time.Second, func() bool {
		return !strings.Contains(capture.currentText(), "search sessions")
	}, func() string {
		return "Session search did not close before direct Notes route: " + safeTerminalDiagnostic(capture.currentText())
	})
	writePTY(t, terminal, "o")
	capture.waitCurrent(t, "✦ Notes Codex ✦", 10*time.Second)
	capture.waitCurrent(t, "Session ", 5*time.Second)
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool {
		return !strings.Contains(capture.currentText(), "✦ Notes Codex ✦")
	}, func() string {
		return "Session-list Notes modal did not close: " + safeTerminalDiagnostic(capture.currentText())
	})
	// Notes restores the Session list origin. Leave that navigation state with
	// its documented Esc key before exercising terminal focus.
	writePTY(t, terminal, "\x1b")
	capture.waitCurrent(t, "Session list pane:", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus: keys go to PTY", 20*time.Second)
	// A plain lowercase o belongs to the focused terminal. Include it as the
	// first shell command and require the following sentinel; if routing
	// opened Notes instead, the shell never receives the command sequence.
	const plainOSentinel = "DUCKLORD_PLAIN_O_7f3c"
	plainOStart := capture.position()
	writePTY(t, terminal, "o; printf '"+plainOSentinel+"\\n'\r")
	waitE2E(t, 10*time.Second, func() bool {
		return strings.Contains(capture.since(plainOStart), plainOSentinel)
	}, func() string {
		return "focused terminal did not receive plain lowercase o: " + safeTerminalDiagnostic(capture.currentText())
	})
	const asyncMarker = "DUCKLORD_NOTES_ASYNC_7f3c"
	writePTY(t, terminal, "for i in 1 2 3 4 5 6 7 8 9 10 11 12; do printf '"+asyncMarker+"-%s\\n' \"$i\"; sleep 0.1; done\r")
	capture.waitCurrent(t, "NOTES_ASYNC_7f3c", 10*time.Second)
	writePTY(t, terminal, "\x02O")
	capture.waitCurrent(t, "✦ Notes Codex ✦", 10*time.Second)
	// Hold the modal while the finite PTY output continues arriving.
	time.Sleep(1500 * time.Millisecond)
	if !strings.Contains(capture.currentText(), "✦ Notes Codex ✦") {
		t.Fatalf("Session Notes lost ownership during asynchronous PTY output: %s", safeTerminalDiagnostic(capture.currentText()))
	}
	waitE2E(t, 10*time.Second, func() bool {
		return !strings.Contains(capture.currentText(), "waiting for Session pane output")
	}, func() string { return "Session Notes did not become ready for modal input" })
	const sessionNoteTitle = "Modal session ownership 7f3c"
	const sessionNoteBody = "saved while Notes owned input 7f3c"
	// The first byte can coincide with the final async frame; the repeated
	// command makes form activation deterministic while the modal owns input.
	writePTY(t, terminal, "aee")
	capture.waitCurrent(t, "Editing Title", 5*time.Second)
	writePTY(t, terminal, "\x0b")
	writePTY(t, terminal, sessionNoteTitle)
	writePTY(t, terminal, "\t")
	writePTY(t, terminal, sessionNoteBody)
	writePTY(t, terminal, "\x13")
	capture.waitCurrent(t, sessionNoteTitle, 10*time.Second)
	capture.waitCurrent(t, sessionNoteBody, 10*time.Second)
	// Reopen the same Session scope and verify the saved body through the UI.
	// This avoids depending on the container's host-side storage mount.
	writePTY(t, terminal, "\x1b")
	waitE2E(t, 10*time.Second, func() bool { return !strings.Contains(capture.currentText(), "✦ Notes Codex ✦") }, func() string { return "Notes modal did not close before persistence reload" })
	writePTY(t, terminal, "\x02O")
	capture.waitCurrent(t, sessionNoteBody, 10*time.Second)
	writePTY(t, terminal, "\x1b")
	// Return to the Session pane first, then use the configured prefix route.
	// This is the regression path: an unhandled prefix here used to leave Notes closed.
	waitE2E(t, 10*time.Second, func() bool {
		return !strings.Contains(capture.currentText(), "✦ Notes Codex ✦")
	}, func() string {
		return "Notes modal did not close: " + safeTerminalDiagnostic(capture.currentText())
	})
	// Closing Session Notes must hand input straight back to the session that
	// opened it. Send a unique command before any navigation or re-selection so
	// the visible terminal output is causal evidence of that restoration.
	sessionCapturePosition := capture.position()
	const sessionNotesSentinel = "DUCKLORD_SESSION_NOTES_FOCUS_RETURN_7f3c"
	capture.waitCurrent(t, "Session focus: keys go to PTY", 10*time.Second)
	capture.waitCurrent(t, "▣ client-a/alpha", 10*time.Second)
	writePTY(t, terminal, "printf '"+sessionNotesSentinel+"\\n'\r")
	waitE2E(t, 10*time.Second, func() bool {
		return strings.Contains(capture.since(sessionCapturePosition), sessionNotesSentinel)
	}, func() string { return "Session Notes did not return input to alpha" })
	closeStart := capture.position()
	writePTY(t, terminal, "\x1d")
	capture.waitAfter(t, closeStart, "Session list pane:", 10*time.Second)
	// Establish Project focus so the seeded Project Notes entries are visible;
	// the earlier focused-terminal Ctrl-B O path covers prefix routing.
	writePTY(t, terminal, "b")
	capture.waitCurrent(t, "Project pane:", 10*time.Second)
	writePTY(t, terminal, "o")
	capture.waitCurrent(t, "✦ Notes Codex ✦", 10*time.Second)
	writePTY(t, terminal, "j")
	capture.waitCurrent(t, "Second", 5*time.Second)
	copyStart := capture.position()
	writePTY(t, terminal, "\r")
	waitE2E(t, 5*time.Second, func() bool {
		return strings.Contains(capture.since(copyStart), "\033]52;c;")
	}, func() string { return "Notes did not emit OSC52 clipboard data" })
	raw := capture.since(copyStart)
	const marker = "\033]52;c;"
	start := strings.Index(raw, marker) + len(marker)
	end := strings.Index(raw[start:], "\a")
	if start < len(marker) || end < 0 {
		t.Fatalf("Notes did not emit a complete OSC52 payload: %q", safeTerminalDiagnostic(raw))
	}
	payload, err := base64.StdEncoding.DecodeString(raw[start : start+end])
	if err != nil || string(payload) != "body only beta" {
		t.Fatalf("Notes clipboard payload=%q err=%v", payload, err)
	}
	// Add and edit use the built-in title/content form.
	writePTY(t, terminal, "a")
	writePTY(t, terminal, "Added")
	writePTY(t, terminal, "\t")
	writePTY(t, terminal, "added body")
	writePTY(t, terminal, "\x13")
	capture.waitCurrent(t, "Added", 10*time.Second)
	writePTY(t, terminal, "e")
	writePTY(t, terminal, "\t")
	writePTY(t, terminal, "\x0b")
	writePTY(t, terminal, "edited added body")
	writePTY(t, terminal, "\x13")
	capture.waitCurrent(t, "Added", 10*time.Second)
	// E is the explicit full-notebook editor and the only route using $EDITOR.
	writePTY(t, terminal, "E")
	capture.waitCurrent(t, "Added", 10*time.Second)
	check, err := exec.Command(runtime, "exec", controller, "cat", "/root/.ducklord/projects/default/notes.md").CombinedOutput()
	if err != nil {
		t.Fatalf("reload persisted Notes: %v: %s", err, check)
	}
	if got := string(check); got != "## First\nfinal alpha\n## Added\nfinal added body\n## Second\nfinal beta\n" {
		t.Fatalf("persisted Notes=%q", got)
	}
	// Scope routing remains visible in the book header. Search is entered in
	// the selected scope and can be cleared before moving to another book.
	writePTY(t, terminal, "g")
	capture.waitCurrent(t, "  global", 5*time.Second)
	// A parent-scope query includes descendant entries and renders their origin.
	writePTY(t, terminal, "/beta")
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Second", 5*time.Second)
	capture.waitCurrent(t, "Project default", 5*time.Second)
	// Reopen search and clear the retained query before changing scope.
	writePTY(t, terminal, "/")
	writePTY(t, terminal, "\x7f\x7f\x7f\x7f\r")
	writePTY(t, terminal, "p")
	capture.waitCurrent(t, "project", 5*time.Second)
	writePTY(t, terminal, "k")
	capture.waitCurrent(t, "First", 5*time.Second)
	writePTY(t, terminal, "s")
	capture.waitCurrent(t, "Session ", 5*time.Second)
	// Close the Notes modal before leaving the workspace.
	writePTY(t, terminal, "\x1b")
	// The focused-session route must return terminal input ownership after Notes
	// closes. Inject a unique command after a fresh capture position so the
	// sentinel proves the command reached alpha's PTY rather than the UI.
	geometry = ducklord.CalculateWorkspaceGeometry(80, 24, 4)
	quick = geometry.Quick
	writePTY(t, terminal, string(workspaceMouse(0, quick.X+1, quick.Y+1, false)))
	writePTY(t, terminal, string(workspaceMouse(0, quick.X+1, quick.Y+1, true)))
	capture.waitCurrent(t, "Session list pane:", 10*time.Second)
	writePTY(t, terminal, "/alpha\r")
	capture.waitCurrent(t, "alpha @client-a", 10*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Active · Enter again to focus", 20*time.Second)
	writePTY(t, terminal, "\r")
	capture.waitCurrent(t, "Session focus: keys go to PTY", 20*time.Second)
	writePTY(t, terminal, "\x02O")
	capture.waitCurrent(t, "✦ Notes Codex ✦", 10*time.Second)
	writePTY(t, terminal, "\x1b")
	// Wait for the observable modal-close transition before sending the command.
	// readInput keeps a lone ESC ambiguous briefly so split Alt chords remain
	// intact; sending the next bytes before that transition can join them to Esc.
	waitE2E(t, 10*time.Second, func() bool {
		current := capture.currentText()
		return !strings.Contains(current, "✦ Notes Codex ✦") &&
			!strings.Contains(current, "Create project") &&
			!strings.Contains(current, "Create tab")
	}, func() string {
		return "Notes or create modal remained open before post-focus input: " + safeTerminalDiagnostic(capture.currentText())
	})
	capture.waitCurrent(t, "Session focus: keys go to PTY", 10*time.Second)
	capture.waitCurrent(t, "▣ client-a/alpha", 10*time.Second)
	// Sending a fresh, unique command is the durable proof that input returned
	// to the original PTY.
	capturePosition := capture.position()
	const sentinel = "DUCKLORD_NOTES_FOCUS_RESTORED_7f3c"
	writePTY(t, terminal, "printf '"+sentinel+"\\n'\r")
	waitE2E(t, 10*time.Second, func() bool {
		return strings.Contains(capture.since(capturePosition), sentinel)
	}, func() string { return "focused session did not receive post-Notes sentinel" })
	if !strings.Contains(capture.currentText(), sentinel) {
		t.Fatalf("current terminal screen lacks post-Notes sentinel: %s", safeTerminalDiagnostic(capture.currentText()))
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

type notesE2EContainerIdentity struct {
	id    string
	owner string
}

func inspectNotesE2EContainer(runtime, controller string) (notesE2EContainerIdentity, error) {
	out, err := exec.Command(runtime, "container", "inspect", "-f", "{{.Id}}\t{{index .Config.Labels \"ducklord.demo.owner\"}}", controller).CombinedOutput()
	if err != nil {
		return notesE2EContainerIdentity{}, err
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(fields) != 2 {
		return notesE2EContainerIdentity{}, fmt.Errorf("unexpected container identity %q", strings.TrimSpace(string(out)))
	}
	return notesE2EContainerIdentity{id: fields[0], owner: fields[1]}, nil
}

func notesTUIE2EAllowed(getenv func(string) string, inspect func(string, string) (notesE2EContainerIdentity, error)) bool {
	if getenv("DUCKLORD_NOTES_TUI_E2E") == "1" &&
		getenv("DUCKLORD_TUI_CONTAINER_E2E") == "1" &&
		getenv("DUCKLORD_E2E_DISPOSABLE_HOST") == "1" {
		runtime := getenv("DUCKLORD_E2E_RUNTIME")
		controller := getenv("DUCKLORD_E2E_CONTROLLER")
		token := getenv("DUCKLORD_E2E_OWNER_TOKEN")
		if runtime == "" || controller == "" || token == "" {
			return false
		}
		identity, err := inspect(runtime, controller)
		return err == nil && identity.id != "" && identity.owner == token
	}
	return false
}

func TestNotesTUIE2ERequiresDisposableHost(t *testing.T) {
	base := map[string]string{
		"DUCKLORD_NOTES_TUI_E2E":       "1",
		"DUCKLORD_TUI_CONTAINER_E2E":   "1",
		"DUCKLORD_E2E_DISPOSABLE_HOST": "1",
		"DUCKLORD_E2E_RUNTIME":         "podman",
		"DUCKLORD_E2E_CONTROLLER":      "fixture-controller",
		"DUCKLORD_E2E_OWNER_TOKEN":     "wrapper-token",
	}
	inspect := func(string, string) (notesE2EContainerIdentity, error) {
		return notesE2EContainerIdentity{id: "fixture-id", owner: "wrapper-token"}, nil
	}
	if !notesTUIE2EAllowed(func(name string) string { return base[name] }, inspect) {
		t.Fatal("the wrapper environment should enable the Notes fixture")
	}
	for _, name := range []string{"DUCKLORD_NOTES_TUI_E2E", "DUCKLORD_TUI_CONTAINER_E2E", "DUCKLORD_E2E_DISPOSABLE_HOST"} {
		env := make(map[string]string, len(base))
		for key, value := range base {
			env[key] = value
		}
		delete(env, name)
		if notesTUIE2EAllowed(func(key string) string { return env[key] }, inspect) {
			t.Errorf("fixture allowed without %s", name)
		}
	}
}

func TestNotesTUIE2ERejectsFakeEnvironment(t *testing.T) {
	env := map[string]string{
		"DUCKLORD_NOTES_TUI_E2E":       "1",
		"DUCKLORD_TUI_CONTAINER_E2E":   "1",
		"DUCKLORD_E2E_DISPOSABLE_HOST": "1",
		"DUCKLORD_E2E_RUNTIME":         "podman",
		"DUCKLORD_E2E_CONTROLLER":      "claimed-controller",
		"DUCKLORD_E2E_OWNER_TOKEN":     "claimed-token",
	}
	if notesTUIE2EAllowed(func(name string) string { return env[name] }, func(string, string) (notesE2EContainerIdentity, error) {
		return notesE2EContainerIdentity{}, fmt.Errorf("no wrapper-created container")
	}) {
		t.Fatal("directly declared environment must not enable the fixture")
	}
}
