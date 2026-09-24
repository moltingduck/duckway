package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func clickModalSelection(t *testing.T, s *tuiState, selection *int, index int) []byte {
	t.Helper()
	for _, r := range s.modalMouseRegions {
		if r.action.selection == selection && r.action.index == index {
			return s.modalMouseInput(r.left, r.row)
		}
	}
	t.Fatalf("no rendered region for option %d", index)
	return nil
}

func TestModalMouseSharedBoxClipsAndRejectsDisabled(t *testing.T) {
	s := &tuiState{}
	s.resetModalMouse()
	selected := -1
	for i := 0; i < 4; i++ {
		s.modalChoice(i, &selected, i, "\r")
	}
	lines := []modalRenderLine{{"", "visible"}, {modalDisabled, "disabled"}, {"", "visible"}, {"", "clipped"}}
	var out bytes.Buffer
	s.renderModalBox(&out, 20, 5, lines)
	if len(s.modalMouseRegions) != 2 {
		t.Fatalf("regions: %+v", s.modalMouseRegions)
	}
	for _, r := range s.modalMouseRegions {
		if r.row < 2 || r.row > 4 || r.left != 3 || r.right != 18 {
			t.Fatalf("bad rectangle: %+v", r)
		}
		if s.modalMouseInput(r.left-1, r.row) != nil {
			t.Fatal("border activated")
		}
	}
	if key := clickModalSelection(t, s, &selected, 2); string(key) != "\r" || selected != 2 {
		t.Fatal("selection did not translate to Enter")
	}
	s.renderModalBox(io.Discard, 7, 5, lines)
	if len(s.modalMouseRegions) != 0 {
		t.Fatal("invisible modal retained targets")
	}
}

func TestModalMouseCreateSelectsClickedChoiceOverTypedInput(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {18, 6}, {10, 3}} {
		s := &tuiState{newSessionMode: true, newSessionStep: "kind", newSessionLine: "2"}
		s.renderCreateModal(io.Discard, size[0], size[1])
		key := clickModalSelection(t, s, &s.newSessionSelected, 0)
		if s.newSessionLine != "" {
			t.Fatal("typed choice overrode click")
		}
		s.handleCreateInput(key)
		if s.newSessionLine != "1" {
			t.Fatalf("clicked selection not used: %q", s.newSessionLine)
		}
	}
}

func TestModalMouseHostConnectionsToggleWithoutApplying(t *testing.T) {
	s := &tuiState{hostMenuMode: true, hostMenuStep: "connections", cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "one"}, {Name: "two"}}}, hostMenuSelected: map[string]bool{}}
	s.renderHostModal(io.Discard, 80, 24)
	if key := clickModalSelection(t, s, &s.hostMenuIndex, 1); string(key) != " " || s.hostMenuIndex != 1 {
		t.Fatal("connections must use Space, preserving explicit apply")
	}
}

func TestModalMouseHostScopedDisabledRowsHaveNoTargets(t *testing.T) {
	s := &tuiState{hostMenuMode: true, hostScoped: true}
	s.renderHostModal(io.Discard, 80, 24)
	for _, r := range s.modalMouseRegions {
		if r.action.selection == &s.hostMenuIndex && r.action.index >= 5 {
			t.Fatal("disabled Host action clickable")
		}
	}
}

func TestModalMouseNotificationLevelsAndWorkspaceConfirmation(t *testing.T) {
	s, _ := notificationConfigTestState(t)
	s.beginNotificationConfig("host", "host")
	s.notificationConfigStep = "level"
	s.renderNotificationConfigModal(io.Discard, 80, 24)
	s.handleNotificationConfigInput(clickModalSelection(t, s, &s.notificationConfigChoice, 3))
	client, _ := s.notificationConfigDraft.Client("host")
	if client.NotificationLevels[ducklord.NotificationCompleted] != ducklord.NotificationSound {
		t.Fatal("clicked level not staged")
	}
	w, project, _, _ := workspacePaneTestState(t)
	nav, _ := w.workspaceNavigation()
	_ = nav.SelectProject(project)
	w.beginWorkspaceProjectDelete()
	w.renderWorkspacePaneModal(io.Discard, 80, 24)
	key := clickModalSelection(t, w, &w.workspacePaneIndex, 0)
	if w.activity().ProjectLayout.Project(project) == nil {
		t.Fatal("click translation bypassed confirmation handler")
	}
	w.handleWorkspacePaneInput(key)
	if w.activity().ProjectLayout.Project(project) != nil {
		t.Fatal("explicit confirmation click not applied")
	}
}

func TestModalMouseHelpUsesConfiguredShortcutsAndIgnoresExamples(t *testing.T) {
	s, _, a, _ := workspacePaneTestState(t)
	s.focused = true
	s.activeAttachKey = sessionKey(a)
	s.cfg.Shortcuts = map[string]string{"pane_prefix": "ctrl+g", "help": "F1"}
	s.toggleHelp()
	// Selection may advance while the overlay is open; close must restore the
	// Session that owned focus when Help was opened.
	s.selected = 1
	s.activeAttachKey = sessionKey(s.sessions[1])
	s.helpSearchQuery = "Open local help"
	s.renderHelpModal(io.Discard, 100, 80)
	foundPrefix := false
	prefixHelp := shortcutInput(s.cfg.Shortcut("pane_prefix")) + "?"
	for _, r := range s.modalMouseRegions {
		if r.action.key == "\r" && r.action.before == nil {
			t.Fatal("explanatory Enter hint must not activate underlying Session")
		}
		if r.action.key == prefixHelp {
			foundPrefix = true
			key := s.modalMouseInput(r.left, r.row)
			if string(key) != r.action.key {
				t.Fatal("configured prefix shortcut was not translated")
			}
			consumed, command := s.handlePanePrefix(key)
			if !consumed || command != "?" {
				t.Fatalf("clicked prefix sequence did not reach command routing: consumed=%v command=%q", consumed, command)
			}
			focus := &recordingWorkspaceInputFocus{}
			control := &ducklord.ControlSession{ClientKey: a.Client, InstanceID: a.InstanceID, SessionID: a.SessionID, RuntimeGeneration: a.RuntimeGeneration}
			dispatchPaneCommandAndRestoreHelpFocus(s, command, focus, true, control, nil)
			if s.helpMode || !s.focused || s.helpFocusRestorePending || s.helpPendingInputKey != "" {
				t.Fatalf("clicked Help command did not restore originating Session focus: help=%v focused=%v pending=%v key=%q", s.helpMode, s.focused, s.helpFocusRestorePending, s.helpPendingInputKey)
			}
			want := ducklord.OutputKey{ClientKey: a.Client, InstanceID: a.InstanceID, SessionID: a.SessionID}
			if len(focus.keys) != 1 || focus.keys[0] != want {
				t.Fatalf("Help click restored wrong PTY focus: got=%+v want=%+v", focus.keys, want)
			}
		}
	}
	if !foundPrefix {
		t.Fatal("configured prefix shortcut row has no mouse target")
	}
	withoutWorkspace := &tuiState{helpMode: true, cfg: s.cfg, helpSearchQuery: "Open local help"}
	withoutWorkspace.renderHelpModal(io.Discard, 100, 80)
	for _, r := range withoutWorkspace.modalMouseRegions {
		if r.action.key == prefixHelp {
			t.Fatal("prefix shortcut unavailable outside workspace must not be clickable")
		}
	}
}

func TestHelpOwnsMouseReportsAcrossPendingAndRestoredPTYFocus(t *testing.T) {
	for _, restored := range []bool{false, true} {
		name := "pending focus"
		if restored {
			name = "restored focus"
		}
		t.Run(name, func(t *testing.T) {
			s, _, session, _ := workspacePaneTestState(t)
			s.focused = true
			s.activeAttachKey = sessionKey(session)
			s.toggleHelp()
			focus := &recordingWorkspaceInputFocus{}
			control := &ducklord.ControlSession{ClientKey: session.Client, InstanceID: session.InstanceID, SessionID: session.SessionID, RuntimeGeneration: session.RuntimeGeneration}
			if restored && !restorePendingHelpFocusWithAttach(s, focus, true, control, nil) {
				t.Fatal("expected ready PTY control to restore focus while Help remained open")
			}
			wantKey := sessionKey(session)
			if s.activeAttachKey != wantKey {
				t.Fatalf("Help lost originating attach identity before click: got=%q want=%q", s.activeAttachKey, wantKey)
			}
			s.helpSearchQuery = "Open local help"
			s.renderHelpModal(io.Discard, 100, 80)
			var action modalMouseRegion
			found := false
			for _, region := range s.modalMouseRegions {
				if region.action.key == shortcutInput(s.cfg.Shortcut("pane_prefix"))+"?" {
					action, found = region, true
					break
				}
			}
			if !found {
				t.Fatal("search result has no actionable Help shortcut row")
			}
			key, owned := s.handleHelpMouseReport(0, action.left, action.row, true)
			if !owned || string(key) != action.action.key {
				t.Fatalf("Help action click was not routed as owned input: owned=%v key=%q want=%q", owned, key, action.action.key)
			}
			if s.activeAttachKey != wantKey || s.focused != restored || s.helpFocusRestorePending != !restored {
				t.Fatalf("Help mouse dispatch changed PTY lease before routing: active=%q focused=%v pending=%v", s.activeAttachKey, s.focused, s.helpFocusRestorePending)
			}
			consumed, command := s.handlePanePrefix(key)
			if !consumed || command != "?" {
				t.Fatalf("clicked Help action did not reach prefix dispatcher: consumed=%v command=%q", consumed, command)
			}
			if restored {
				if !s.dispatchFocusedPaneCommand(command) {
					t.Fatal("focused prefix dispatcher did not handle the Help close action")
				}
			} else {
				dispatchPaneCommandAndRestoreHelpFocus(s, command, focus, true, nil, nil)
			}
			if s.helpMode || s.activeAttachKey != wantKey {
				t.Fatalf("Help close lost its origin: help=%v active=%q", s.helpMode, s.activeAttachKey)
			}
			if restored {
				if !s.focused || s.helpFocusRestorePending || s.helpPendingInputKey != "" {
					t.Fatalf("restored PTY focus was not preserved after click: focused=%v pending=%v key=%q", s.focused, s.helpFocusRestorePending, s.helpPendingInputKey)
				}
				if len(focus.keys) != 1 || focus.keys[0] != (ducklord.OutputKey{ClientKey: session.Client, InstanceID: session.InstanceID, SessionID: session.SessionID}) {
					t.Fatalf("wrong output focus restored before click: %+v", focus.keys)
				}
			} else if s.focused || !s.helpFocusRestorePending || s.helpPendingInputKey != wantKey {
				t.Fatalf("pending origin lease should stay fenced until ready: focused=%v pending=%v key=%q", s.focused, s.helpFocusRestorePending, s.helpPendingInputKey)
			}
		})
	}
}

func TestClosedProjectFilesRendererPreservesHelpMouseAction(t *testing.T) {
	s, _, session, _ := workspacePaneTestState(t)
	s.focused = true
	s.activeAttachKey = sessionKey(session)
	s.toggleHelp()
	s.helpSearchQuery = "Open local help"
	// Exercise the actual renderer ordering: Help registers its action, then
	// the deferred Project Files renderer runs last while that modal is closed.
	s.renderWorkspacePreviewAt(io.Discard, 100, 80)

	prefixHelp := shortcutInput(s.cfg.Shortcut("pane_prefix")) + "?"
	var helpRegion modalMouseRegion
	for _, region := range s.modalMouseRegions {
		if region.action.key == prefixHelp {
			helpRegion = region
			break
		}
	}
	if helpRegion.row == 0 {
		t.Fatal("Help action row was not registered before the later closed Project Files renderer")
	}

	// The full workspace renderer invokes Project Files after Help even when
	// that modal is closed. That inactive renderer must not erase Help's target.
	s.renderProjectFilesModal(io.Discard, 100, 80)
	key, owned := s.handleHelpMouseReport(0, helpRegion.left, helpRegion.row, true)
	if !owned || string(key) != prefixHelp {
		t.Fatalf("closed Project Files renderer erased Help click target: owned=%v key=%q", owned, key)
	}
	consumed, command := s.handlePanePrefix(key)
	if !consumed || command != "?" {
		t.Fatalf("preserved Help click did not reach prefix dispatcher: consumed=%v command=%q", consumed, command)
	}
	dispatchPaneCommandAndRestoreHelpFocus(s, command, &recordingWorkspaceInputFocus{}, true, nil, nil)
	if s.helpMode {
		t.Fatal("preserved Help action did not close the overlay")
	}

	// Once Project Files is active, it owns the shared mouse targets and must
	// clear stale Help targets before drawing its own controls.
	s.helpMode = true
	s.helpSearchQuery = "Open local help"
	s.renderHelpModal(io.Discard, 100, 80)
	s.projectFiles.open = true
	s.projectFiles.step = "path"
	s.projectFiles.editorOriginal = "remote/path"
	s.renderProjectFilesModal(io.Discard, 100, 80)
	for _, region := range s.modalMouseRegions {
		if region.action.key == prefixHelp {
			t.Fatal("active Project Files modal retained stale Help click target")
		}
	}
}

func TestHelpConsumesNonActionMouseReportsBeforeFocusedPaneRouting(t *testing.T) {
	s, _, session, _ := workspacePaneTestState(t)
	s.focused = true
	s.activeAttachKey = sessionKey(session)
	s.toggleHelp()
	control := &ducklord.ControlSession{ClientKey: session.Client, InstanceID: session.InstanceID, SessionID: session.SessionID, RuntimeGeneration: session.RuntimeGeneration}
	focus := &recordingWorkspaceInputFocus{}
	if !restorePendingHelpFocusWithAttach(s, focus, true, control, nil) {
		t.Fatal("expected PTY focus restoration while Help remains open")
	}
	s.renderHelpModal(io.Discard, 100, 80)
	blankX, blankY := 0, 0
	for _, region := range s.modalMouseRegions {
		for _, y := range []int{region.row - 1, region.row + 1} {
			x := (region.left + region.right) / 2
			if x >= 14 && x <= 85 && y > 0 && y < 79 && !s.modalMouseHit(x, y) {
				blankX, blankY = x, y
				break
			}
		}
		if blankX != 0 {
			break
		}
	}
	if blankX == 0 {
		t.Fatal("could not locate blank interior Help cell")
	}
	for _, report := range []struct {
		name   string
		button int
		x, y   int
		press  bool
	}{
		{name: "outside click", button: 0, x: 1, y: 1, press: true},
		{name: "blank interior click", button: 0, x: blankX, y: blankY, press: true},
		{name: "release", button: 0, x: 1, y: 1, press: false},
		{name: "right click", button: 2, x: 1, y: 1, press: true},
		{name: "wheel", button: 64, x: 1, y: 1, press: true},
	} {
		t.Run(report.name, func(t *testing.T) {
			key, owned := s.handleHelpMouseReport(report.button, report.x, report.y, report.press)
			if !owned || len(key) != 0 {
				t.Fatalf("non-action report escaped Help ownership: owned=%v key=%q", owned, key)
			}
			if !s.focused || s.activeAttachKey != sessionKey(session) {
				t.Fatalf("non-action report changed focused PTY lease: focused=%v attach=%q", s.focused, s.activeAttachKey)
			}
		})
	}
}

type recordingWorkspaceInputFocus struct {
	keys []ducklord.OutputKey
}

func (r *recordingWorkspaceInputFocus) SetInputFocus(key ducklord.OutputKey) error {
	r.keys = append(r.keys, key)
	return nil
}

func TestModalMouseActionMenuOmitsDisabledOptions(t *testing.T) {
	s := &tuiState{actionMenu: true, actionTarget: ducklord.RemoteSession{Kind: "shell", InstanceID: "instance", SessionID: "session"}}
	s.renderActionModal(io.Discard, 80, 24)
	actions := s.sessionActions(s.actionTarget)
	for _, r := range s.modalMouseRegions {
		if r.action.selection == &s.actionIndex && !actions[r.action.index].Enabled {
			t.Fatal("disabled Session action clickable")
		}
	}
	if key := clickModalSelection(t, s, &s.actionIndex, 2); string(key) != "\r" {
		t.Fatal("enabled local notification action missing")
	}
}

func TestHelpScrollMouseDispatcherWheelTrackDragAndRelease(t *testing.T) {
	s := &tuiState{helpMode: true}
	width, height := terminalSize()
	s.renderHelpModal(io.Discard, width, height)
	if s.helpResultTotal <= height-5 {
		t.Skip("terminal is too tall to overflow Help")
	}
	key, owned := s.handleHelpMouseReport(65, 1, 1, true)
	if !owned || len(key) != 0 || s.helpOffset != 3 {
		t.Fatalf("Help wheel dispatch: owned=%v key=%q offset=%d", owned, key, s.helpOffset)
	}
	visible := max(1, height-5)
	boxWidth := min(72, max(8, width-2))
	left := max(1, (width-boxWidth)/2+1)
	boxTop := max(1, (height-min(visible+3, height-2)-2)/2+1)
	trackX, trackY := left+boxWidth-2, boxTop+3
	// A track press pages while a thumb press captures without jumping.
	key, owned = s.handleHelpMouseReport(0, trackX, trackY+visible-1, true)
	if !owned || len(key) != 0 || s.helpOffset <= 3 {
		t.Fatalf("Help track did not page: owned=%v offset=%d", owned, s.helpOffset)
	}
	thumbHeight := max(1, visible*visible/s.helpResultTotal)
	maxOffset := s.helpResultTotal - visible
	thumbY := trackY + (visible-thumbHeight)*s.helpOffset/maxOffset
	offset := s.helpOffset
	grab := min(1, thumbHeight-1)
	key, owned = s.handleHelpMouseReport(0, trackX, thumbY+grab, true)
	if !owned || len(key) != 0 || !s.helpScrollbarDrag || s.helpOffset != offset {
		t.Fatalf("Help thumb press moved/capture failed: offset=%d before=%d", s.helpOffset, offset)
	}
	_, owned = s.handleHelpMouseReport(32, trackX, trackY+visible-1, true)
	if !owned || s.helpOffset <= offset {
		t.Fatal("Help held drag did not scroll")
	}
	_, owned = s.handleHelpMouseReport(0, 1, 1, false)
	if !owned || s.helpScrollbarDrag {
		t.Fatal("Help release outside track did not clear drag")
	}
}

func TestCloseHelpCancelsScrollbarCapture(t *testing.T) {
	s := &tuiState{helpMode: true, helpScrollbarDrag: true, helpScrollbarDragGrab: 2, helpOffset: 17, helpSearchActive: true, helpSearchQuery: "needle"}
	s.closeHelp()
	if s.helpMode || s.helpScrollbarDrag || s.helpScrollbarDragGrab != 0 || s.helpOffset != 0 || s.helpSearchActive || s.helpSearchQuery != "" {
		t.Fatalf("closing Help retained modal state: mode=%t drag=%t grab=%d offset=%d search=%t query=%q", s.helpMode, s.helpScrollbarDrag, s.helpScrollbarDragGrab, s.helpOffset, s.helpSearchActive, s.helpSearchQuery)
	}
	// Reopening and receiving the old release must not inherit its capture.
	s.helpMode = true
	_, owned := s.handleHelpMouseReport(0, 1, 1, false)
	if !owned || s.helpScrollbarDrag || s.helpScrollbarDragGrab != 0 {
		t.Fatalf("stale release after reopen inherited capture: owned=%t drag=%t grab=%d", owned, s.helpScrollbarDrag, s.helpScrollbarDragGrab)
	}
}
