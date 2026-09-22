package main

import (
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestTerminalToolModalOwnsInputAndRestoresExactTerminal(t *testing.T) {
	terminal := ducklord.NewTerminal(8, 80, 64)
	terminal.Write([]byte("local terminal output\r\n"))
	state := &tuiState{focused: true, terminal: terminal, activeAttachKey: "client/11111111-1111-4111-8111-111111111111/ABC123", activityState: ducklord.NewActivityState(), sessions: []ducklord.RemoteSession{{Client: "client", InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}}}
	if !state.openTerminalTool("/") {
		t.Fatal("search modal did not open for focused terminal")
	}
	if !state.handleTerminalToolInput([]byte("secret")) {
		t.Fatal("search modal did not claim query input")
	}
	if state.terminalSearchQuery != "secret" {
		t.Fatalf("query = %q", state.terminalSearchQuery)
	}
	if !state.handleTerminalToolInput([]byte("\x1b")) {
		t.Fatal("Esc was not claimed by search modal")
	}
	if state.terminalSearchMode || state.activeAttachKey != "client/11111111-1111-4111-8111-111111111111/ABC123" {
		t.Fatalf("modal close did not restore terminal lease: mode=%v key=%q", state.terminalSearchMode, state.activeAttachKey)
	}
}

func TestTerminalToolRenderMarksMissingBookmarkWithRetainedHistoryFallback(t *testing.T) {
	terminal := ducklord.NewTerminal(8, 80, 64)
	terminal.Write([]byte("current output\r\n"))
	identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
	bookmark, err := ducklord.NewTerminalBookmark(identity, "old output", "old output")
	if err != nil {
		t.Fatal(err)
	}
	state := &tuiState{focused: true, workspacePreview: true, terminal: terminal, activeAttachKey: "client/" + identity.Key(), activityState: ducklord.NewActivityState(), sessions: []ducklord.RemoteSession{{Client: "client", InstanceID: identity.InstanceID, SessionID: identity.SessionID}}}
	if err := state.activityState.AddTerminalBookmark(bookmark); err != nil {
		t.Fatal(err)
	}
	state.terminalBookmarkListMode = true
	var rendered strings.Builder
	state.renderTerminalTool(&rendered, 80, 24)
	if !strings.Contains(rendered.String(), "old output [unavailable; Enter shows retained history]") {
		t.Fatalf("bookmark status did not show retained-history fallback: %q", rendered.String())
	}
	if !state.handleTerminalToolInput([]byte("\r")) || state.terminalBookmarkListMode {
		t.Fatal("bookmark selection did not close the list")
	}
	if !strings.Contains(state.outputErr, "showing current retained history") || state.ptyScrollOffset != 0 {
		t.Fatalf("bookmark fallback did not select deterministic retained history: error=%q offset=%d", state.outputErr, state.ptyScrollOffset)
	}
}

func TestTerminalToolSearchSelectionMovesPTYViewport(t *testing.T) {
	terminal := ducklord.NewTerminal(3, 40, 64)
	terminal.Write([]byte("first target\r\nsecond line\r\nsecond target\r\n"))
	state := &tuiState{focused: true, terminal: terminal, activeAttachKey: "instance/session", activityState: ducklord.NewActivityState()}
	state.openTerminalTool("/")
	state.handleTerminalToolInput([]byte("target"))
	if state.ptyScrollOffset == 0 {
		t.Fatal("initial search did not position the terminal")
	}
	state.handleTerminalToolInput([]byte("\x1b[B"))
	var rendered strings.Builder
	state.renderTerminalTool(&rendered, 80, 24)
	if !strings.Contains(rendered.String(), "match 2/2") {
		t.Fatalf("next match did not select the next retained line: %q", rendered.String())
	}
}

func TestTerminalToolBookmarkUsesRetainedTailCoordinates(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
	current := strings.Repeat("filler\n", 12000) + strings.Repeat("later\n", 1000) + "saved anchor\n"
	bookmark, err := ducklord.NewTerminalBookmark(identity, "saved", current)
	if err != nil {
		t.Fatal(err)
	}
	line, ok := terminalBookmarkLineInCurrent(current, bookmark)
	if !ok || line != 13000 {
		t.Fatalf("bookmark line = %d, ok=%v; want retained absolute line 13000", line, ok)
	}
}

func TestTerminalToolAccumulatesSplitUTF8Label(t *testing.T) {
	state := &tuiState{focused: true, terminal: ducklord.NewTerminal(8, 80, 64), activeAttachKey: "instance/session", activityState: ducklord.NewActivityState()}
	state.openTerminalTool("m")
	state.handleTerminalToolInput([]byte{0xe3})
	state.handleTerminalToolInput([]byte{0x83, 0x86})
	if state.terminalBookmarkLabel != "テ" {
		t.Fatalf("label = %q, want split UTF-8 rune", state.terminalBookmarkLabel)
	}
}

func TestTerminalToolBookmarkFailureKeepsModalOpen(t *testing.T) {
	state := &tuiState{focused: true, terminal: ducklord.NewTerminal(8, 80, 64), activeAttachKey: "missing/session", activityState: ducklord.NewActivityState()}
	state.openTerminalTool("m")
	state.handleTerminalToolInput([]byte("label"))
	state.handleTerminalToolInput([]byte("\r"))
	if !state.terminalBookmarkMode || state.outputErr != "bookmark: no active session" {
		t.Fatalf("bookmark failure closed modal or omitted error: mode=%v error=%q", state.terminalBookmarkMode, state.outputErr)
	}
}

func TestTerminalToolCloseFallsBackWhenOriginSessionDisappears(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
	state := &tuiState{focused: true, workspacePreview: true, terminal: ducklord.NewTerminal(8, 80, 64), activeAttachKey: "client/" + identity.Key(),
		activityState: ducklord.NewActivityState(), sessions: []ducklord.RemoteSession{{Client: "client", InstanceID: identity.InstanceID, SessionID: identity.SessionID}}}
	state.openTerminalTool("/")
	state.sessions = []ducklord.RemoteSession{{Client: "client", InstanceID: identity.InstanceID, SessionID: "OTHER"}}
	state.closeTerminalTool()
	if state.focused || !state.workspaceProjectFocus || state.activeAttachKey != "" {
		t.Fatalf("stale terminal origin restored: focused=%v projectFocus=%v key=%q", state.focused, state.workspaceProjectFocus, state.activeAttachKey)
	}
}

func TestTerminalToolCloseFallsBackWithNoSessions(t *testing.T) {
	state := &tuiState{focused: true, terminal: ducklord.NewTerminal(8, 80, 64), activeAttachKey: "client/ABC123", activityState: ducklord.NewActivityState()}
	state.openTerminalTool("/")
	state.closeTerminalTool()
	if state.focused || !state.workspaceProjectFocus || state.activeAttachKey != "" {
		t.Fatalf("zero-session origin restored: focused=%v projectFocus=%v key=%q", state.focused, state.workspaceProjectFocus, state.activeAttachKey)
	}
}
