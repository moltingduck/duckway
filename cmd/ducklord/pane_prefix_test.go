package main

import (
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
	for _, key := range []string{"-", "\\", "t", ",", "up", "down", "left", "right", "pageup", "pagedown", "n", "p"} {
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
