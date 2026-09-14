package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestWorkspacePreviewRendersLiveSelectedSessionWithoutGrantingFocus(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "ABC123"}
	activity := ducklord.NewActivityState()
	if err := activity.ProjectLayout.Discover(identity); err != nil {
		t.Fatal(err)
	}
	session := ducklord.RemoteSession{Client: "host-a", InstanceID: identity.InstanceID, SessionID: identity.SessionID,
		Name: "codex-shell", Kind: "shell", Status: "running", RuntimeGeneration: 1}
	terminal := ducklord.NewTerminal(15, 60, 16)
	terminal.Write([]byte("\033[31mRED_FRAME\033[0m"))
	state := &tuiState{ownerName: "local", activityState: activity, sessions: []ducklord.RemoteSession{session}, terminal: terminal,
		outputForKey: sessionKey(session), outputFresh: true}
	var out bytes.Buffer
	state.renderWorkspacePreviewAt(&out, 120, 20)
	got := out.String()
	for _, want := range []string{"PROJECTS", "SESSIONS", "Default Project", "codex-shell", "\033[0;31mRED_FRAME"} {
		if !strings.Contains(got, want) {
			t.Fatalf("workspace missing %q: %q", want, got)
		}
	}
	if !strings.Contains(got, "◇ host-a/codex-shell") || strings.Contains(got, "▣ host-a/codex-shell") {
		t.Fatalf("preview was presented as focused: %q", got)
	}
	if state.focused || state.activeAttachKey != "" {
		t.Fatal("rendering granted PTY focus")
	}
	state.workspacePreview = true
	out.Reset()
	state.render(&out)
	if !strings.Contains(out.String(), "PROJECTS") || !strings.Contains(out.String(), "SESSIONS") {
		t.Fatalf("live TUI render path did not select workspace: %q", out.String())
	}
	state.focused = true
	state.activeAttachKey = sessionKey(session)
	out.Reset()
	state.renderWorkspacePreviewAt(&out, 120, 20)
	if !strings.Contains(out.String(), "▣ host-a/codex-shell") {
		t.Fatalf("focused session was not marked: %q", out.String())
	}
}

func TestWorkspacePreviewPreflightUsesVisibleLeafNotWholeTerminal(t *testing.T) {
	a := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: a.InstanceID, SessionID: "BBB222"}
	activity := ducklord.NewActivityState()
	first, err := activity.ProjectLayout.Place(ducklord.DefaultProjectID, a, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(ducklord.DefaultProjectID, b, ducklord.PlaceVertical, first); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{activityState: activity, selected: 1, sessions: []ducklord.RemoteSession{
		{Client: "host", InstanceID: a.InstanceID, SessionID: a.SessionID, Kind: "shell"},
		{Client: "host", InstanceID: b.InstanceID, SessionID: b.SessionID, Kind: "shell"},
	}}
	rect, err := state.workspacePaneRectAt(120, 20)
	if err != nil || rect.X != 88 || rect.Width != 33 || rect.Height != 16 {
		t.Fatalf("selected right pane rect=%+v err=%v", rect, err)
	}
	state.focused = true
	state.terminal = ducklord.NewTerminal(15, 33, 0)
	var rendered bytes.Buffer
	state.renderWorkspacePreviewAt(&rendered, 120, 20)
	if !strings.Contains(rendered.String(), "\033[6;88H\033[?25h") {
		t.Fatalf("focused cursor was not placed inside right pane: %q", rendered.String())
	}
	activity.ProjectLayout = ducklord.NewProjectLayout()
	first, err = activity.ProjectLayout.Place(ducklord.DefaultProjectID, a, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(ducklord.DefaultProjectID, b, ducklord.PlaceHorizontal, first); err != nil {
		t.Fatal(err)
	}
	if _, err := state.workspacePaneRectAt(120, 5); err == nil {
		t.Fatal("hidden lower pane passed control preflight")
	}
}
