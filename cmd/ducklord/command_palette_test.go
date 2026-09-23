package main

import (
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestCommandPaletteStateRouter(t *testing.T) {
	s := &tuiState{focused: true, workspacePreview: true, workspaceProjectFocus: true}
	s.openCommandPalette()
	if !s.commandPaletteMode || s.focused {
		t.Fatal("palette did not take local focus")
	}
	if !s.handleCommandPaletteInput([]byte("x")) || s.commandPaletteQuery != "x" {
		t.Fatal("query not owned by palette")
	}
	if !s.handleCommandPaletteInput([]byte("\x1b")) || s.commandPaletteMode || !s.focused || !s.workspaceProjectFocus {
		t.Fatal("cancel did not restore focus")
	}
}

func TestPanePrefixSpaceRoutesOnlyToCommandPalette(t *testing.T) {
	s := &tuiState{
		focused:          true,
		workspacePreview: true,
		cfg:              &ducklord.Config{Shortcuts: map[string]string{"pane_prefix": "ctrl-b"}},
	}
	if consumed, command := s.handlePanePrefix([]byte("\x02")); !consumed || command != "" {
		t.Fatalf("prefix = (%v, %q), want consumed with no command", consumed, command)
	}
	consumed, command := s.handlePanePrefix([]byte(" "))
	if !consumed || command != " " {
		t.Fatalf("prefix+space = (%v, %q), want local space command", consumed, command)
	}
	if len(s.takePanePrefixReplay()) != 0 {
		t.Fatal("prefix+space queued input for PTY replay")
	}
	s.dispatchFocusedPaneCommand(command)
	if !s.commandPaletteMode || s.focused {
		t.Fatal("prefix+space did not transfer ownership to the palette")
	}
	if s.handleCommandPaletteInput([]byte("q")) == false || s.commandPaletteQuery != "q" {
		t.Fatal("palette did not own query input")
	}
}

func TestCommandPaletteRejectsStaleWorkspaceTarget(t *testing.T) {
	s := &tuiState{activityState: ducklord.NewActivityState()}
	_, err := s.activity().ProjectLayout.AddProject("old")
	if err != nil {
		t.Fatal(err)
	}
	s.openCommandPalette()
	if _, err := s.activity().ProjectLayout.AddProject("new"); err != nil {
		t.Fatal(err)
	}
	s.commandPaletteQuery = "old"
	if !s.handleCommandPaletteInput([]byte("\r")) {
		t.Fatal("palette did not consume stale target")
	}
	if s.outputErr != "command palette target is stale" {
		t.Fatalf("stale target error = %q", s.outputErr)
	}
}

func TestCommandPaletteAccumulatesSplitUTF8Input(t *testing.T) {
	s := &tuiState{}
	s.openCommandPalette()
	s.handleCommandPaletteInput([]byte{0xe3})
	s.handleCommandPaletteInput([]byte{0x83, 0x86}) // テ
	if s.commandPaletteQuery != "テ" {
		t.Fatalf("query = %q, want split UTF-8 rune", s.commandPaletteQuery)
	}
}

func TestCommandPaletteFuzzyMatchesLabels(t *testing.T) {
	tests := []struct {
		query, want string
	}{
		{query: "hlp", want: "Help"},
		{query: "ntes", want: "Notes"},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			s := &tuiState{}
			s.commandPaletteQuery = tc.query
			items := s.commandPaletteItems()
			if len(items) != 1 || items[0].label != tc.want {
				t.Fatalf("items for %q = %#v, want only %q", tc.query, items, tc.want)
			}
		})
	}
}

func TestCommandPaletteCloseFallsBackWhenOriginPaneDisappears(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
	activity := ducklord.NewActivityState()
	projectID, err := activity.ProjectLayout.AddProject("Work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	s := &tuiState{focused: true, workspacePreview: true, activeAttachKey: "client/" + identity.Key(), activityState: activity,
		sessions: []ducklord.RemoteSession{{Client: "client", InstanceID: identity.InstanceID, SessionID: identity.SessionID}}}
	s.openCommandPalette()
	activity.ProjectLayout.Destroy(identity)
	s.closeCommandPalette()
	if s.focused || !s.workspaceProjectFocus || s.activeAttachKey != "" {
		t.Fatalf("stale palette origin restored: focused=%v projectFocus=%v key=%q", s.focused, s.workspaceProjectFocus, s.activeAttachKey)
	}
}

func TestCommandPaletteProjectFilesRestoresFocusedShellWithoutLayout(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
	session := ducklord.RemoteSession{Client: "client", InstanceID: identity.InstanceID, SessionID: identity.SessionID}
	s := &tuiState{
		focused:               true,
		activeAttachKey:       sessionKey(session),
		workspaceProjectFocus: false,
		cfg:                   &ducklord.Config{Clients: []ducklord.Client{{Name: "client"}}},
		sessions:              []ducklord.RemoteSession{session},
	}
	s.openCommandPalette()
	s.commandPaletteIndex = 3 // Project files
	if !s.handleCommandPaletteInput([]byte("\r")) || !s.projectFiles.open {
		t.Fatal("palette did not open Project files")
	}
	if !s.projectFiles.originFocused || s.projectFiles.originAttachKey != sessionKey(session) {
		t.Fatalf("Project files lost focused-shell origin: %#v", s.projectFiles)
	}
	s.closeProjectFiles()
	if !s.focused || s.workspaceProjectFocus || s.activeAttachKey != sessionKey(session) {
		t.Fatalf("close did not restore focused shell without a layout: focused=%v projectFocus=%v key=%q", s.focused, s.workspaceProjectFocus, s.activeAttachKey)
	}
}

func TestCommandPaletteFooterMatchesHandledKeys(t *testing.T) {
	s := &tuiState{}
	s.openCommandPalette()
	var rendered strings.Builder
	s.renderCommandPalette(&rendered, 100, 24)
	want := "↑/↓ or j/k choose · Enter run · Esc/Ctrl+C close"
	if !strings.Contains(rendered.String(), want) {
		t.Fatalf("footer missing %q in %q", want, rendered.String())
	}
	if !s.handleCommandPaletteInput([]byte("j")) || s.commandPaletteIndex != 1 {
		t.Fatalf("j did not select next item: index=%d", s.commandPaletteIndex)
	}
	if !s.handleCommandPaletteInput([]byte("\x03")) || s.commandPaletteMode {
		t.Fatal("Ctrl+C did not close palette")
	}
}
