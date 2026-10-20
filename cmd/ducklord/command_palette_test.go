package main

import (
	"errors"
	"path/filepath"
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

func TestCommandPaletteDeleteKeysRemoveOneUnicodeRune(t *testing.T) {
	for _, key := range [][]byte{{0x08}, {0x7f}} {
		s := &tuiState{}
		s.openCommandPalette()
		s.commandPaletteQuery = "a猫"
		s.commandPaletteIndex = 2
		s.handleCommandPaletteInput(key)
		if s.commandPaletteQuery != "a" || s.commandPaletteIndex != 0 {
			t.Fatalf("delete %x left query=%q index=%d", key, s.commandPaletteQuery, s.commandPaletteIndex)
		}
		s.handleCommandPaletteInput(key)
		if s.commandPaletteQuery != "" {
			t.Fatalf("delete %x on one rune left query=%q", key, s.commandPaletteQuery)
		}
		s.handleCommandPaletteInput(key)
		if s.commandPaletteQuery != "" {
			t.Fatalf("delete %x on empty query changed it to %q", key, s.commandPaletteQuery)
		}
	}
}

func TestCommandPaletteOperationalActionsAreRealAndSearchable(t *testing.T) {
	want := []struct {
		kind, label, query string
	}{
		{"host-add", "Add host configuration", "addhost"},
		{"create-session", "Create session", "createsession"},
		{"refresh", "Refresh sessions and hosts", "refresh"},
	}
	for _, tc := range want {
		s := &tuiState{}
		if tc.kind == "host-add" {
			identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
			s.focused = true
			s.activeAttachKey = "client/" + identity.Key()
			s.sessions = []ducklord.RemoteSession{{Client: "client", InstanceID: identity.InstanceID, SessionID: identity.SessionID, RuntimeGeneration: 5}}
		}
		s.openCommandPalette()
		s.commandPaletteQuery = tc.query
		items := s.commandPaletteItems()
		found := false
		for i, item := range items {
			if item.kind == tc.kind && item.label == tc.label {
				s.commandPaletteIndex = i
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("query %q did not find operational action %q (%q): %#v", tc.query, tc.kind, tc.label, items)
		}
		if !s.handleCommandPaletteInput([]byte("\r")) || s.commandPaletteAction != tc.kind {
			t.Fatalf("dispatch for %q = %q", tc.kind, s.commandPaletteAction)
		}
		if tc.kind == "host-add" {
			// The event loop starts the existing Add host flow after palette dispatch.
			s.beginAddClientWithHosts(nil, nil)
			if !s.addClientMode || s.focused || s.activeAttachKey == "" {
				t.Fatalf("Add host dispatch did not take keyboard ownership from the focused PTY: modal=%v focused=%v key=%q", s.addClientMode, s.focused, s.activeAttachKey)
			}
			s.cancelAddClient()
			if !s.focused || s.activeAttachKey == "" {
				t.Fatalf("Add host cancel did not restore focused PTY: focused=%v key=%q", s.focused, s.activeAttachKey)
			}
		}
	}

	s := &tuiState{}
	s.openCommandPalette()
	s.commandPaletteQuery = "zz-no-match"
	if got := s.commandPaletteItems(); len(got) != 0 {
		t.Fatalf("no-match query returned %#v", got)
	}
	if !s.handleCommandPaletteInput([]byte("j")) || s.commandPaletteIndex != 0 {
		t.Fatalf("navigation moved selection with no matches: %d", s.commandPaletteIndex)
	}

	s.hostScoped = true
	s.commandPaletteQuery = ""
	for _, item := range s.commandPaletteItems() {
		if item.kind == "host-add" || item.kind == "create-session" {
			t.Fatalf("host-scoped palette exposes unavailable action %q", item.kind)
		}
	}
}

func TestAddClientModalOwnsWorkspaceKeyboardAndControl(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
	key := "client/" + identity.Key()
	s := &tuiState{focused: true, activeAttachKey: key, sessions: []ducklord.RemoteSession{{Client: "client", InstanceID: identity.InstanceID, SessionID: identity.SessionID, RuntimeGeneration: 5}}}
	s.beginAddClientWithHosts(nil, nil)
	if s.focused {
		t.Fatal("Add host kept keyboard ownership with the focused PTY")
	}
	if !s.addClientMode {
		t.Fatal("Add host modal did not open")
	}
	if s.workspaceControlMayAccept() {
		t.Fatal("workspace control accepted focus while Add host modal owns input")
	}
	s.cancelAddClient()
	if !s.focused || s.activeAttachKey != key {
		t.Fatalf("Add host cancel did not restore original PTY focus: focused=%v key=%q", s.focused, s.activeAttachKey)
	}
}

func TestAddClientLoadErrorPreservesFocusedPTY(t *testing.T) {
	s := &tuiState{focused: true, activeAttachKey: "client/session"}
	s.beginAddClientWithHosts(nil, errors.New("bad ssh config"))
	if !s.focused || s.activeAttachKey != "client/session" || s.addClientMode {
		t.Fatalf("SSH config error changed focus/modal state: focused=%v key=%q modal=%v", s.focused, s.activeAttachKey, s.addClientMode)
	}
}

func TestAddClientCancelDoesNotRestoreReplacedPTYGeneration(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
	key := "client/" + identity.Key()
	s := &tuiState{focused: true, activeAttachKey: key, sessions: []ducklord.RemoteSession{{Client: "client", InstanceID: identity.InstanceID, SessionID: identity.SessionID, RuntimeGeneration: 5}}}
	s.beginAddClientWithHosts(nil, nil)
	s.sessions[0].RuntimeGeneration = 6
	s.cancelAddClient()
	if s.focused || !s.workspaceProjectFocus || s.activeAttachKey != "" {
		t.Fatalf("replacement runtime inherited stale PTY focus: focused=%v workspace=%v key=%q", s.focused, s.workspaceProjectFocus, s.activeAttachKey)
	}
}

func TestAddClientCompletionRestoresFocusedPTY(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
	key := "client/" + identity.Key()
	s := &tuiState{
		focused:         true,
		activeAttachKey: key,
		sessions:        []ducklord.RemoteSession{{Client: "client", InstanceID: identity.InstanceID, SessionID: identity.SessionID, RuntimeGeneration: 5}},
		cfg:             &ducklord.Config{},
		cfgPath:         filepath.Join(t.TempDir(), "config.yaml"),
	}
	s.beginAddClientWithHosts(nil, nil)
	err := s.applyAddClientResult(addClientDoneEvent{client: ducklord.Client{Name: "new", Host: "new.example"}})
	if err != nil {
		t.Fatalf("complete Add host: %v", err)
	}
	if s.addClientMode || !s.focused || s.activeAttachKey != key {
		t.Fatalf("Add host completion did not restore originating PTY: modal=%v focused=%v key=%q", s.addClientMode, s.focused, s.activeAttachKey)
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
