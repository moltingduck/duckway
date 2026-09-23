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
	s := &tuiState{workspacePreview: true, focused: true, activeAttachKey: "host/session", cfg: &ducklord.Config{Shortcuts: map[string]string{"pane_prefix": "ctrl+g", "help": "F1"}}}
	s.toggleHelp()
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
			s.dispatchPaneCommand(command)
			if s.helpMode || !s.helpFocusRestorePending {
				t.Fatalf("clicked Help command did not close overlay while retaining origin restoration: help=%v pending=%v", s.helpMode, s.helpFocusRestorePending)
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
