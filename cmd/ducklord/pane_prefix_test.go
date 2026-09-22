package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestPageKeyInputIsAtomicAcrossReads(t *testing.T) {
	for _, key := range []string{"pageup", "pagedown"} {
		sequence := shortcutInput(key)
		for n := 1; n < len(sequence); n++ {
			_, rest, ok := nextInputEvent([]byte(sequence[:n]))
			if ok || string(rest) != sequence[:n] {
				t.Fatalf("partial %s at %d consumed", key, n)
			}
		}
		event, rest, ok := nextInputEvent([]byte(sequence + "x"))
		if !ok || string(event) != sequence || string(rest) != "x" {
			t.Fatalf("%s split incorrectly: %q %q", key, event, rest)
		}
	}
}

func TestPanePrefixCommandsAndCancellation(t *testing.T) {
	for _, key := range []string{"-", "\\", "t", ",", "up", "down", "left", "right", "pageup", "pagedown", "n", "p", "o", "?"} {
		s := &tuiState{workspacePreview: true, cfg: &ducklord.Config{}}
		if consumed, cmd := s.handlePanePrefix([]byte{2}); !consumed || cmd != "" {
			t.Fatal("prefix not armed")
		}
		if consumed, cmd := s.handlePanePrefix([]byte(shortcutInput(key))); !consumed || cmd != key {
			t.Fatalf("command %q not consumed", key)
		}
		if s.panePrefixPending {
			t.Fatal("prefix remained armed")
		}
		if consumed, cmd := s.handlePanePrefix([]byte("\x02" + shortcutInput(key))); !consumed || cmd != key {
			t.Fatalf("combined command %q failed", key)
		}
	}
	s := &tuiState{workspacePreview: true, cfg: &ducklord.Config{Shortcuts: map[string]string{"pane_prefix": "ctrl-a"}}}
	s.handlePanePrefix([]byte{1})
	if consumed, cmd := s.handlePanePrefix([]byte("q")); !consumed || cmd != "" {
		t.Fatal("unknown command leaked")
	}
	s.handlePanePrefix([]byte{1})
	s.clearAttachIdentity()
	if consumed, _ := s.handlePanePrefix([]byte("t")); consumed {
		t.Fatal("stale prefix survived detach")
	}
	s.workspacePaneMode = true
	if consumed, _ := s.handlePanePrefix([]byte{1}); consumed {
		t.Fatal("modal input intercepted")
	}
}

func TestFocusedPanePrefixHelpStaysLocal(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}, workspacePreview: true, focused: true}
	if consumed, command := s.handlePanePrefix([]byte("\x02")); !consumed || command != "" {
		t.Fatalf("prefix start consumed=%v command=%q", consumed, command)
	}
	consumed, command := s.handlePanePrefix([]byte("?"))
	if !consumed || command != "?" {
		t.Fatalf("prefix+? consumed=%v command=%q", consumed, command)
	}
	if s.helpMode {
		t.Fatal("help opened before pane command dispatch")
	}
	if !s.dispatchFocusedPaneCommand(command) {
		t.Fatal("focused Ctrl-B ? was not dispatched locally")
	}
	if !s.helpMode {
		t.Fatal("focused Ctrl-B ? did not open help")
	}
	s.dispatchPaneCommand(command)
	if s.helpMode {
		t.Fatal("help ? command did not toggle help closed")
	}
}

func TestFocusedHelpCloseConsumesBareShortcut(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}, workspacePreview: true, focused: true, activeAttachKey: "host/i/s"}
	s.toggleHelp()
	// A readiness callback can restore focus while Help remains visible. The
	// close key must still belong to Help and leave no stale focus fence.
	s.focused = true
	if !s.dispatchHelpAction("help") || s.helpMode {
		t.Fatalf("focused Help close was not consumed: mode=%v", s.helpMode)
	}
	if !s.focused || s.helpFocusRestorePending || s.helpPendingInputKey != "" {
		t.Fatalf("Help close left stale focus state: focused=%v pending=%v key=%q", s.focused, s.helpFocusRestorePending, s.helpPendingInputKey)
	}
}

func TestHelpCloseClearsAlreadyRestoredFocusFence(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}, workspacePreview: true, focused: true, activeAttachKey: "host/i/s"}
	s.toggleHelp()
	if !s.helpMode || s.focused || !s.helpFocusRestorePending {
		t.Fatalf("help open did not fence focus: mode=%v focused=%v pending=%v", s.helpMode, s.focused, s.helpFocusRestorePending)
	}
	// The output-ready callback can restore focus before the closing '?' is
	// dispatched. Closing Help must retire that already-satisfied fence.
	s.focused = true
	s.toggleHelp()
	if s.helpMode || !s.focused || s.helpFocusRestorePending || s.helpPendingInputKey != "" {
		t.Fatalf("help close retained stale focus fence: mode=%v focused=%v pending=%v key=%q", s.helpMode, s.focused, s.helpFocusRestorePending, s.helpPendingInputKey)
	}
}

func TestHelpSearchCloseRetiresPendingFocusFence(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}, workspacePreview: true, focused: true, activeAttachKey: "host/i/s"}
	s.toggleHelp()
	s.helpSearchActive = true
	if s.focused || !s.helpFocusRestorePending {
		t.Fatalf("help open did not defer focus: focused=%v pending=%v", s.focused, s.helpFocusRestorePending)
	}
	// Search mode uses its own close path. It must preserve the pending lease
	// while focus is still unavailable, then retire it once the lease wins.
	s.closeHelp()
	if s.helpMode || s.helpSearchActive || !s.helpFocusRestorePending {
		t.Fatalf("search close lost deferred lease: mode=%v search=%v pending=%v", s.helpMode, s.helpSearchActive, s.helpFocusRestorePending)
	}
	s.focused = true
	s.closeHelp()
	if s.helpFocusRestorePending || s.helpPendingInputKey != "" {
		t.Fatalf("search close retained restored lease: pending=%v key=%q", s.helpFocusRestorePending, s.helpPendingInputKey)
	}
}

func TestFocusedPanePrefixNotesStaysLocal(t *testing.T) {
	for _, key := range []string{"o", "O"} {
		t.Run(key, func(t *testing.T) {
			s, projectID, _, session := workspacePaneTestState(t)
			s.focused = true
			s.activeAttachKey = sessionKey(session)
			s.notesProjectID = projectID
			if consumed, command := s.handlePanePrefix([]byte("\x02")); !consumed || command != "" {
				t.Fatalf("prefix start consumed=%v command=%q", consumed, command)
			}
			consumed, command := s.handlePanePrefix([]byte(key))
			if !consumed || command != key || !s.dispatchFocusedPaneCommand(command) {
				t.Fatalf("focused Ctrl-B %s was not dispatched locally: consumed=%v command=%q", key, consumed, command)
			}
			if !s.workspacePaneMode || s.workspacePaneStep != "notes" || s.focused {
				t.Fatalf("focused Ctrl-B %s changed wrong state: pane=%v step=%q focused=%v", key, s.workspacePaneMode, s.workspacePaneStep, s.focused)
			}
			// A Notes interaction can change the active attachment; closing it must
			// return to the session that was focused when Notes opened.
			s.activeAttachKey = "changed-while-notes-was-open"
			// Notes owns keys while open, then Esc returns the exact terminal focus.
			if !s.handleNotesInput([]byte("j")) || s.workspacePaneIndex != 0 {
				t.Fatalf("Notes did not consume navigation input: index=%d", s.workspacePaneIndex)
			}
			if !s.handleCentralModalCancel([]byte("\x1b")) {
				t.Fatal("Esc did not close Notes modal")
			}
			if s.workspacePaneMode || s.focused || !s.notesFocusRestorePending || s.activeAttachKey != sessionKey(session) {
				t.Fatalf("closing Notes did not defer terminal focus restoration: mode=%v focused=%v pending=%v key=%q", s.workspacePaneMode, s.focused, s.notesFocusRestorePending, s.activeAttachKey)
			}
		})
	}
}

func TestDirectNotesRoutesFollowNavigationFocus(t *testing.T) {
	t.Run("project", func(t *testing.T) {
		s, _, _, _ := workspacePaneTestState(t)
		s.workspaceProjectFocus = true
		if !s.handleDirectNotesInput([]byte("o")) || !s.workspacePaneMode || s.notesScope != ducklord.NotesProject {
			t.Fatalf("direct project Notes route: mode=%v scope=%v err=%q", s.workspacePaneMode, s.notesScope, s.outputErr)
		}
	})
	t.Run("session-list", func(t *testing.T) {
		s, _, _, _ := workspacePaneTestState(t)
		s.workspaceProjectFocus = false
		expected := s.currentSession()
		if expected.SessionID == "" {
			t.Fatal("fixture has no selected session")
		}
		if !s.handleDirectNotesInput([]byte("o")) || !s.workspacePaneMode || s.notesScope != ducklord.NotesSession || s.notesSessionID != expected.SessionID {
			t.Fatalf("direct session Notes route: mode=%v scope=%v session=%q want=%q err=%q", s.workspacePaneMode, s.notesScope, s.notesSessionID, expected.SessionID, s.outputErr)
		}
	})
	t.Run("session-list-detail-search", func(t *testing.T) {
		s, _, _, _ := workspacePaneTestState(t)
		s.workspaceProjectFocus = false
		if !s.enterDetailedMode() {
			t.Fatal("failed to enter Session list detail view")
		}
		expected := s.detailSelected
		if expected.Key() == "" {
			t.Fatal("detail view has no selected session")
		}
		if !s.handleDirectNotesInput([]byte("o")) || !s.workspacePaneMode || s.notesScope != ducklord.NotesSession || s.notesSessionID != expected.SessionID {
			t.Fatalf("direct detail Session Notes route: mode=%v scope=%v session=%q want=%q err=%q", s.workspacePaneMode, s.notesScope, s.notesSessionID, expected.SessionID, s.outputErr)
		}
	})
	t.Run("session-list-search-modal-owns-o", func(t *testing.T) {
		s, _, _, _ := workspacePaneTestState(t)
		s.workspaceProjectFocus = false
		expected := s.currentSession()
		s.beginSearch()
		s.searchQuery = expected.Name
		s.syncSearchSelection()
		if s.handleDirectNotesInput([]byte("o")) || !s.searchMode || s.workspacePaneMode {
			t.Fatalf("search did not retain plain o input: search=%v mode=%v scope=%v session=%q err=%q", s.searchMode, s.workspacePaneMode, s.notesScope, s.notesSessionID, s.outputErr)
		}
	})
	t.Run("focused-terminal-plain-o", func(t *testing.T) {
		s, _, _, _ := workspacePaneTestState(t)
		s.focused = true
		if s.handleDirectNotesInput([]byte("o")) || s.workspacePaneMode {
			t.Fatal("plain o was consumed locally while terminal was focused")
		}
	})
}

func TestFocusedCtrlCBypassesPanePrefix(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}, workspacePreview: true, focused: true, panePrefixPending: true}
	consumed, command := s.handlePanePrefix([]byte("\x03"))
	if consumed || command != "" {
		t.Fatalf("focused Ctrl-C was consumed as pane input: consumed=%t command=%q", consumed, command)
	}
	if s.panePrefixPending {
		t.Fatal("focused Ctrl-C left pane prefix armed")
	}
}

func TestPanePrefixPlacement(t *testing.T) {
	for key, placement := range map[string]ducklord.PanePlacement{"-": ducklord.PlaceHorizontal, "\\": ducklord.PlaceVertical, "t": ducklord.PlaceNewTab} {
		s, _, _, _ := workspacePaneTestState(t)
		s.openPrefixPane(key)
		if !s.workspacePaneMode || s.workspacePaneStep != "source" || s.workspacePaneIntent.placement != placement {
			t.Fatalf("wrong placement for %s: %+v", key, s.workspacePaneIntent)
		}
	}
}

func TestWorkspaceArrowsCrossStackedLists(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.workspaceProjectFocus = false
	if handled, changed := s.handleWorkspaceProjectInput([]byte("\x1b[A")); !handled || !changed || !s.workspaceProjectFocus {
		t.Fatal("up did not enter Project pane")
	}
	if handled, changed := s.handleWorkspaceProjectInput([]byte("\x1b[B")); !handled || !changed || s.workspaceProjectFocus || s.selected != 0 {
		t.Fatal("down did not return to Session list")
	}
	if s.focused {
		t.Fatal("navigation acquired writer")
	}
}

func TestPanePrefixLetterTabNavigation(t *testing.T) {
	s, projectID, _, b := workspacePaneTestState(t)
	identity, _ := ducklord.IdentityFromSession(b)
	if _, err := s.activity().ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	nav, _ := s.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	project := s.activity().ProjectLayout.Project(projectID)
	for _, step := range []struct {
		key string
		tab int
	}{{"n", 1}, {"n", 0}, {"p", 1}, {"p", 0}} {
		consumed, command := s.handlePanePrefix([]byte("\x02" + step.key))
		if !consumed || !paneNavigationCommand(command) || !s.navigatePrefixPane(command) {
			t.Fatalf("prefix+%s did not navigate: %s", step.key, s.outputErr)
		}
		if nav.CurrentTabID() != project.Tabs[step.tab].ID || s.workspacePaneMode {
			t.Fatalf("prefix+%s selected wrong tab or opened modal", step.key)
		}
	}
}

func TestFocusedPaneNavigationHandsSyntheticEnterToSelectedSession(t *testing.T) {
	s, projectID, _, b := workspacePaneTestState(t)
	identity, _ := ducklord.IdentityFromSession(b)
	if _, err := s.activity().ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	s.focused = true
	s.workspaceProjectFocus = false
	if !s.beginFocusedPaneNavigation("n") {
		t.Fatal("focused tab navigation failed")
	}
	if s.workspaceProjectFocus || !s.workspaceAttachFromProject || !s.workspaceFocusFromProject {
		t.Fatalf("focused navigation left synthetic Enter aimed at Project list: projectFocus=%v attach=%v fromProject=%v", s.workspaceProjectFocus, s.workspaceAttachFromProject, s.workspaceFocusFromProject)
	}
}

func TestWorkspaceEmptyProjectEnterCreatesPaneInNewTab(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	projectID, err := s.activity().ProjectLayout.AddProject("Empty")
	if err != nil {
		t.Fatal(err)
	}
	nav, _ := s.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	s.outputErr = "old error"
	handled, _ := s.handleWorkspaceProjectInput([]byte("\r"))
	if !handled || !s.workspacePaneMode || s.workspacePaneStep != "source" ||
		s.workspacePaneIntent.projectID != projectID || s.workspacePaneIntent.placement != ducklord.PlaceNewTab || s.outputErr != "" {
		t.Fatalf("Enter did not open empty Project new-tab flow: %+v", s.workspacePaneIntent)
	}
	if !s.handleWorkspacePaneInput([]byte("\r")) || s.workspaceNewSessionIntent == nil || s.workspaceNewSessionIntent.projectID != projectID {
		t.Fatal("new tab did not hand off to Session creation")
	}
}

// TestPaneRouteContract is the executable index for the route IDs documented
// in docs/pane-routing.md. Each case enters through the same event-loop
// handler used by the TUI and then exercises its local return path.
func TestPaneRouteContract(t *testing.T) {
	tests := []struct {
		id  string
		run func(*tuiState) error
	}{
		{"route.quick-list", func(s *tuiState) error {
			nav, err := s.workspaceNavigation()
			if err != nil {
				return err
			}
			beforePane := nav.CurrentPaneID()
			s.workspaceProjectFocus = true
			handled, _ := s.handleWorkspaceProjectInput([]byte("\x1b[B"))
			if !handled || s.workspaceProjectFocus || nav.CurrentPaneID() != beforePane || s.focused {
				return errRoute("down did not return to quick list")
			}
			return nil
		}},
		{"route.detail-list", func(s *tuiState) error {
			if got := s.handleDetailedInput([]byte(s.cfg.Shortcut("detail_list"))); got != "changed" {
				return errRoute("detail open=%q", got)
			}
			nav, err := s.workspaceNavigation()
			if err != nil {
				return err
			}
			if !nav.InDetailMode() {
				return errRoute("detail mode not entered")
			}
			s.handleDetailedInput([]byte("\x1b"))
			if nav.InDetailMode() {
				return errRoute("escape did not close detail")
			}
			return nil
		}},
		{"route.project", func(s *tuiState) error {
			nav, err := s.workspaceNavigation()
			if err != nil {
				return err
			}
			beforeProject, beforePane := nav.CurrentProjectID(), nav.CurrentPaneID()
			s.workspaceProjectFocus = false
			handled, _ := s.handleWorkspaceProjectInput([]byte("\x1b[A"))
			if !handled || !s.workspaceProjectFocus {
				return errRoute("up did not enter Project")
			}
			handled, changed := s.handleWorkspaceProjectInput([]byte("\x03"))
			if !handled || changed {
				return errRoute("Ctrl-C was not consumed as Project cancellation")
			}
			if s.workspaceProjectFocus {
				return errRoute("Ctrl-C did not leave Project")
			}
			if nav.CurrentProjectID() != beforeProject || nav.CurrentPaneID() != beforePane || s.focused {
				return errRoute("Project close changed origin or owner")
			}
			return nil
		}},
		{"route.tab", func(s *tuiState) error {
			nav, err := s.workspaceNavigation()
			if err != nil {
				return err
			}
			identity, ok := ducklord.IdentityFromSession(s.sessions[1])
			if !ok {
				return errRoute("missing session identity")
			}
			projectID := nav.CurrentProjectID()
			if _, err := s.activity().ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
				return err
			}
			project := s.activity().ProjectLayout.Project(projectID)
			if project == nil || len(project.Tabs) < 2 {
				return errRoute("tab route fixture has fewer than two tabs")
			}
			originalTab := project.Tabs[0].ID
			if !s.navigatePrefixPane("n") || nav.CurrentTabID() == "" {
				return errRoute("next tab did not focus")
			}
			if !s.navigatePrefixPane("p") || nav.CurrentTabID() != originalTab {
				return errRoute("previous tab did not return: got=%q want=%q", nav.CurrentTabID(), originalTab)
			}
			if s.focused || s.workspacePaneMode {
				return errRoute("tab navigation changed owner/modal state")
			}
			return nil
		}},
		{"route.session-pane", func(s *tuiState) error {
			nav, err := s.workspaceNavigation()
			if err != nil {
				return err
			}
			identity, ok := ducklord.IdentityFromSession(s.sessions[1])
			if !ok {
				return errRoute("missing session identity")
			}
			projectID, beforePane := nav.CurrentProjectID(), nav.CurrentPaneID()
			if _, err := s.activity().ProjectLayout.Place(projectID, identity, ducklord.PlaceVertical, beforePane); err != nil {
				return err
			}
			if !s.navigatePrefixPane("right") || nav.CurrentPaneID() == beforePane {
				return errRoute("prefix+right did not focus another Session pane")
			}
			if s.focused || s.workspacePaneMode {
				return errRoute("session pane navigation changed owner/modal state")
			}
			if !s.navigatePrefixPane("left") || nav.CurrentPaneID() != beforePane {
				return errRoute("back did not restore session pane origin")
			}
			return nil
		}},
		{"route.terminal", func(s *tuiState) error {
			_, cancel := context.WithCancel(context.Background())
			defer cancel()
			openCancel := context.CancelFunc(cancel)
			var control *ducklord.ControlSession
			controlID := 0
			if !handlePendingPTYInput(s, []byte(shortcutInput(s.cfg.Shortcut("pty_unfocus"))), true, &control, &openCancel, &controlID) {
				return errRoute("terminal close key was not handled")
			}
			if openCancel != nil || controlID != 1 {
				return errRoute("terminal close did not cancel pending focus")
			}
			return nil
		}},
		{"route.notes", func(s *tuiState) error {
			nav, err := s.workspaceNavigation()
			if err != nil {
				return err
			}
			beforeProject, beforePane := nav.CurrentProjectID(), nav.CurrentPaneID()
			if consumed, command := s.handlePanePrefix([]byte(shortcutInput(s.cfg.Shortcut("pane_prefix")))); !consumed || command != "" {
				return errRoute("pane prefix was not armed")
			} else if consumed, command = s.handlePanePrefix([]byte("o")); !consumed || command != "o" {
				return errRoute("prefix+o was not routed")
			} else {
				s.dispatchPaneCommand(command)
			}
			if !s.workspacePaneMode || s.workspacePaneStep != "notes" {
				return errRoute("Notes modal was not opened: mode=%v step=%q", s.workspacePaneMode, s.workspacePaneStep)
			}
			if s.focused || !s.handleNotesInput([]byte("j")) {
				return errRoute("Notes did not own navigation input")
			}
			if !s.handleCentralModalCancel([]byte("\x1b")) || s.workspacePaneMode {
				return errRoute("escape did not close Notes modal")
			}
			if nav.CurrentProjectID() != beforeProject || nav.CurrentPaneID() != beforePane {
				return errRoute("escape returned to %s/%s, want %s/%s", nav.CurrentProjectID(), nav.CurrentPaneID(), beforeProject, beforePane)
			}
			if consumed, command := s.handlePanePrefix([]byte(shortcutInput(s.cfg.Shortcut("pane_prefix")))); !consumed || command != "" {
				return errRoute("pane prefix was not re-armed")
			} else if consumed, command = s.handlePanePrefix([]byte("o")); !consumed || command != "o" {
				return errRoute("second prefix+o was not routed")
			} else {
				s.dispatchPaneCommand(command)
			}
			if !s.workspacePaneMode || s.workspacePaneStep != "notes" {
				return errRoute("Notes modal did not reopen")
			}
			if consumed, command := s.handlePanePrefix([]byte(shortcutInput(s.cfg.Shortcut("pane_prefix")))); !consumed || command != "" {
				return errRoute("pane prefix was not armed for toggle")
			} else if consumed, command = s.handlePanePrefix([]byte("o")); !consumed || command != "o" {
				return errRoute("toggle prefix+o was not routed")
			} else {
				s.dispatchPaneCommand(command)
			}
			if s.workspacePaneMode {
				return errRoute("prefix+o did not toggle Notes modal closed")
			}
			return nil
		}},
		{"route.session-notes", func(s *tuiState) error {
			beforeAttach := s.activeAttachKey
			s.focused = true
			if consumed, command := s.handlePanePrefix([]byte(shortcutInput(s.cfg.Shortcut("pane_prefix")))); !consumed || command != "" {
				return errRoute("pane prefix was not armed")
			} else if consumed, command = s.handlePanePrefix([]byte("O")); !consumed || command != "O" {
				return errRoute("prefix+O was not routed")
			} else {
				s.dispatchPaneCommand(command)
			}
			if !s.workspacePaneMode || s.notesScope != ducklord.NotesSession {
				return errRoute("Session Notes modal was not opened")
			}
			s.closeNotesModal()
			if s.workspacePaneMode || s.focused || !s.notesFocusRestorePending || s.activeAttachKey != beforeAttach {
				return errRoute("closing Session Notes did not defer terminal focus restoration: mode=%v focused=%v pending=%v key=%q want=%q", s.workspacePaneMode, s.focused, s.notesFocusRestorePending, s.activeAttachKey, beforeAttach)
			}
			return nil
		}},
		{"route.context-modal", func(s *tuiState) error {
			nav, err := s.workspaceNavigation()
			if err != nil {
				return err
			}
			width, height := terminalSize()
			geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
			// Use the same right-click event that the central event loop receives.
			if handled, _ := s.handleWorkspaceMouse(workspaceMouse(2, geometry.Projects.X+1, geometry.Projects.Y+1, false)); !handled {
				return errRoute("context click was not handled")
			}
			if s.workspacePaneStep != "context-config" || s.focused {
				return errRoute("context modal not opened")
			}
			modalProject, modalPane := nav.CurrentProjectID(), nav.CurrentPaneID()
			s.handleWorkspaceAreaConfig([]byte("\x1b"))
			if s.workspacePaneMode || nav.CurrentProjectID() != modalProject || nav.CurrentPaneID() != modalPane {
				return errRoute("escape did not close context modal")
			}
			return nil
		}},
		{"route.help", func(s *tuiState) error {
			nav, err := s.workspaceNavigation()
			if err != nil {
				return err
			}
			beforePane := nav.CurrentPaneID()
			helpKey := []byte(s.cfg.Shortcut("help"))
			if s.handleInput(helpKey) != "help" || !s.dispatchHelpAction("help") {
				return errRoute("help open failed")
			}
			if !s.helpMode {
				return errRoute("help did not open")
			}
			if s.focused || nav.CurrentPaneID() != beforePane {
				return errRoute("help changed owner or origin")
			}
			if s.handleCentralModalCancel([]byte("\x1b")) || !s.helpMode {
				return errRoute("escape unexpectedly closed help overlay")
			}
			if !s.dispatchHelpAction("help") || s.helpMode {
				return errRoute("configured help key did not close help overlay")
			}
			return nil
		}},
		{"route.create", func(s *tuiState) error {
			nav, err := s.workspaceNavigation()
			if err != nil {
				return err
			}
			beforeProject, beforePane := nav.CurrentProjectID(), nav.CurrentPaneID()
			if handled, _ := s.handleWorkspaceProjectInput([]byte(s.cfg.Shortcut("project_create"))); !handled {
				return errRoute("project create command was not handled")
			}
			if !s.workspacePaneMode {
				return errRoute("create flow not opened")
			}
			s.handleCentralModalCancel([]byte("\x1b"))
			if s.workspacePaneMode || nav.CurrentProjectID() != beforeProject || nav.CurrentPaneID() != beforePane || s.focused {
				return errRoute("escape did not close create flow")
			}
			return nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			s, _, _, _ := workspacePaneTestState(t)
			if err := tc.run(s); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFocusedTerminalPrefixDDetachesNonDefaultProjectPane(t *testing.T) {
	s, projectID, a, _ := workspacePaneTestState(t)
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	s.focused = true
	s.activeAttachKey = sessionKey(a)
	if consumed, command := s.handlePanePrefix([]byte(shortcutInput(s.cfg.Shortcut("pane_prefix")))); !consumed || command != "" {
		t.Fatalf("prefix was not armed: consumed=%v command=%q", consumed, command)
	}
	consumed, command := s.handlePanePrefix([]byte("d"))
	if !consumed || command != "d" {
		t.Fatalf("detach command was not routed: consumed=%v command=%q", consumed, command)
	}
	if !s.dispatchFocusedPaneCommand(command) {
		t.Fatal("detach command was not handled")
	}
	if s.activity().ProjectLayout.Project(projectID) == nil || len(s.activity().ProjectLayout.SessionsForProject(projectID)) != 0 {
		t.Fatalf("focused Session pane remained attached: %+v", s.activity().ProjectLayout.Project(projectID))
	}
	if s.activity().ProjectLayout.Project(ducklord.DefaultProjectID) == nil {
		t.Fatal("Default Project disappeared")
	}
}

func TestFocusedTerminalPrefixDOpensDefaultDetachModal(t *testing.T) {
	s, _, a, _ := workspacePaneTestState(t)
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(ducklord.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	s.focused = true
	s.activeAttachKey = sessionKey(a)
	if _, command := s.handlePanePrefix([]byte("\x02")); command != "" {
		t.Fatalf("prefix command=%q", command)
	}
	if _, command := s.handlePanePrefix([]byte("d")); command != "d" {
		t.Fatalf("detach command=%q", command)
	}
	if !s.dispatchFocusedPaneCommand("d") {
		t.Fatal("focused detach was not handled")
	}
	if !s.workspacePaneMode || s.workspacePaneStep != "detach-confirm" {
		t.Fatalf("focused default detach did not open confirmation modal: mode=%v step=%q", s.workspacePaneMode, s.workspacePaneStep)
	}
}

func TestFocusedDefaultDetachModalOwnsNavigationAndRestoresFocus(t *testing.T) {
	s, _, a, _ := workspacePaneTestState(t)
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(ducklord.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	s.focused = true
	s.activeAttachKey = sessionKey(a)
	if !s.dispatchFocusedPaneCommand("d") {
		t.Fatal("focused detach was not handled")
	}
	if !s.workspacePaneMode || !s.focused || s.workspacePaneIndex != 1 {
		t.Fatalf("detach modal did not retain focused origin: mode=%v focused=%v index=%d", s.workspacePaneMode, s.focused, s.workspacePaneIndex)
	}

	// This is the central dispatcher ordering contract: modal navigation is
	// handled while the originating terminal remains focused.
	if s.handleCentralModalCancel([]byte("\x1b[A")) {
		t.Fatal("modal arrow was treated as cancellation")
	}
	s.handleWorkspacePaneInput([]byte("\x1b[A"))
	if s.workspacePaneIndex != 0 || !s.workspacePaneMode || !s.focused {
		t.Fatalf("modal arrow changed the wrong state: mode=%v focused=%v index=%d", s.workspacePaneMode, s.focused, s.workspacePaneIndex)
	}

	// Esc cancels locally and restores the exact terminal focus lease.
	if !s.handleCentralModalCancel([]byte("\x1b")) || s.workspacePaneMode || !s.focused || s.activeAttachKey != sessionKey(a) {
		t.Fatalf("modal cancel did not restore terminal focus: mode=%v focused=%v key=%q", s.workspacePaneMode, s.focused, s.activeAttachKey)
	}
	// A transient session inventory loss must still resolve the saved focused
	// restore session while the attach identity remains active.
	saved := s.workspacePaneFocusRestoreSession
	s.sessions = nil
	if restored := s.activePTYSession(); sessionKey(restored) != s.activeAttachKey || sessionKey(restored) != sessionKey(saved) {
		t.Fatalf("active PTY session was not recovered from focused restore snapshot: got=%q want=%q", sessionKey(restored), s.activeAttachKey)
	}
	s.sessions = []ducklord.RemoteSession{a}

	// Reopen, move to Detach, and confirm through the same modal handler.
	if !s.dispatchFocusedPaneCommand("d") {
		t.Fatal("focused detach did not reopen modal")
	}
	s.handleWorkspacePaneInput([]byte("\x1b[A"))
	s.handleWorkspacePaneInput([]byte("\r"))
	if s.workspacePaneMode || !s.workspacePaneChanged {
		t.Fatalf("detach action left modal or change fence: mode=%v changed=%v", s.workspacePaneMode, s.workspacePaneChanged)
	}
}

func TestFocusedTerminalPrefixDDetachesNonDefaultProjectFromAtomicPrefix(t *testing.T) {
	s, projectID, a, _ := workspacePaneTestState(t)
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	s.focused = true
	s.activeAttachKey = sessionKey(a)
	if _, command := s.handlePanePrefix([]byte("\x02d")); command != "d" {
		t.Fatalf("detach command=%q", command)
	}
	if !s.dispatchFocusedPaneCommand("d") {
		t.Fatal("focused detach was not handled")
	}
	if s.workspacePaneMode {
		t.Fatalf("focused non-default detach unexpectedly opened confirmation modal: step=%q", s.workspacePaneStep)
	}
	if len(s.activity().ProjectLayout.SessionsForProject(projectID)) != 0 {
		t.Fatal("focused non-default Session pane remained attached")
	}
}

func TestFocusedTerminalPrefixDSelectsSurvivingPaneForHandoff(t *testing.T) {
	s, projectID, a, b := workspacePaneTestState(t)
	bIdentity, _ := ducklord.IdentityFromSession(b)
	if _, err := s.activity().ProjectLayout.Place(projectID, bIdentity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	s.focused = true
	s.activeAttachKey = sessionKey(a)
	if !s.dispatchFocusedPaneCommand("d") {
		t.Fatal("focused detach was not handled")
	}
	if len(s.activity().ProjectLayout.SessionsForProject(projectID)) != 1 {
		t.Fatal("detach removed more than the focused pane")
	}
	if !s.workspaceAttachFromProject || s.workspaceFocusFromProject || s.workspaceProjectFocus {
		t.Fatalf("detach did not request surviving-pane handoff: attach=%v fromProject=%v projectFocus=%v", s.workspaceAttachFromProject, s.workspaceFocusFromProject, s.workspaceProjectFocus)
	}
	if identity, ok := s.activity().ProjectLayout.PaneSession(projectID, nav.CurrentPaneID()); !ok || identity.SessionID != b.SessionID {
		t.Fatalf("navigation did not select surviving pane: ok=%v identity=%+v", ok, identity)
	}
}

func TestPendingHelpFocusKeepsDefaultDetachLocal(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(ducklord.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	s.helpFocusRestorePending = true
	s.dispatchPaneCommand("d")
	if !s.workspacePaneMode || s.workspacePaneStep != "detach-confirm" {
		t.Fatalf("pending Help detach did not open modal: mode=%v step=%q", s.workspacePaneMode, s.workspacePaneStep)
	}
}

func errRoute(format string, args ...interface{}) error { return fmt.Errorf(format, args...) }
