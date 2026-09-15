package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestWorkspaceTabRenamePersistsAndCancels(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	nav, _ := s.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	s.beginWorkspaceTabRename()
	s.handleWorkspacePaneInput([]byte("工作"))
	var out bytes.Buffer
	s.renderWorkspacePaneModal(&out, 100, 30)
	if !strings.Contains(out.String(), "Rename Terminal tab") || !strings.Contains(out.String(), "工作") {
		t.Fatal("rename editor not rendered")
	}
	s.handleWorkspacePaneInput([]byte("\r"))
	loaded, err := s.activityStore.Load()
	if err != nil || loaded.ProjectLayout.Project(projectID).Tabs[0].Name != "工作" || s.workspacePaneMode {
		t.Fatalf("tab name not saved: %v", err)
	}
	s.beginWorkspaceTabRename()
	s.handleWorkspacePaneInput([]byte(" extra"))
	s.handleWorkspacePaneInput([]byte("\x1b"))
	if s.activity().ProjectLayout.Project(projectID).Tabs[0].Name != "工作" {
		t.Fatal("cancel changed tab name")
	}
	s.beginWorkspaceTabRename()
	s.handleWorkspacePaneInput([]byte("\x7f"))
	s.handleWorkspacePaneInput([]byte("\x7f"))
	s.handleWorkspacePaneInput([]byte("\r"))
	if s.workspacePaneMode || s.activity().ProjectLayout.Project(projectID).Tabs[0].Name != "" {
		t.Fatal("empty name did not restore tab number")
	}
}

func TestWorkspaceTabRenameSaveFailureDoesNotMutateState(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	nav, _ := s.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	s.beginWorkspaceTabRename()
	s.handleWorkspacePaneInput([]byte("Unsaved"))
	s.activityStore.Path = filepath.Join(s.activityStore.Path, "not-a-directory", "state.json")
	s.handleWorkspacePaneInput([]byte("\r"))
	if !s.workspacePaneMode || s.workspacePaneErr == "" || s.activity().ProjectLayout.Project(projectID).Tabs[0].Name != "" {
		t.Fatal("failed save changed live state or closed modal")
	}
}

func TestWorkspaceTabRenameRejectsRemovedTab(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	nav, _ := s.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	s.beginWorkspaceTabRename()
	s.handleWorkspacePaneInput([]byte("Stale"))
	s.activity().ProjectLayout.Project(projectID).Tabs = nil
	s.handleWorkspacePaneInput([]byte("\r"))
	if !s.workspacePaneMode || s.workspacePaneErr == "" || len(s.activity().ProjectLayout.Project(projectID).Tabs) != 0 {
		t.Fatal("rename did not reject removed tab")
	}
}

func TestWorkspaceTabRenameInputRejectsTerminalControls(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	nav, _ := s.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	s.beginWorkspaceTabRename()
	s.handleWorkspacePaneInput([]byte("\x1b]52;c;clipboard\a"))
	s.handleWorkspacePaneInput([]byte("a\u202eb\x00"))
	if s.workspacePaneName != "ab" {
		t.Fatalf("unexpected tab input: %q", s.workspacePaneName)
	}
}

func TestWorkspaceMouseNamedTabsAndPlus(t *testing.T) {
	s, projectID, _, b := workspacePaneTestState(t)
	project := s.activity().ProjectLayout.Project(projectID)
	project.Tabs[0].Name = "工作"
	identity, _ := ducklord.IdentityFromSession(b)
	_, err := s.activity().ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	nav, _ := s.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	width, height := terminalSize()
	rect := ducklord.CalculateWorkspaceGeometry(width, height, 4).Terminal
	x := rect.X + modalCellWidth(" "+project.Name+"  ") + modalCellWidth(ducklord.WorkspaceTabLabel(project.Tabs[0], 0, true))
	handled, changed := s.handleWorkspaceMouse(workspaceMouse(0, x+1, rect.Y, false))
	if !handled || !changed || nav.CurrentTabID() != project.Tabs[1].ID {
		t.Fatal("named tab widths did not match mouse targets")
	}
	x += modalCellWidth(ducklord.WorkspaceTabLabel(project.Tabs[1], 1, true))
	s.handleWorkspaceMouse(workspaceMouse(0, x+2, rect.Y, false))
	if !s.workspacePaneMode || s.workspacePaneStep != "source" || s.workspacePaneIntent.placement != ducklord.PlaceNewTab {
		t.Fatal("plus did not open new tab session source modal")
	}
	if !s.handleWorkspacePaneInput([]byte("\r")) || s.workspaceNewSessionIntent == nil || s.workspaceNewSessionIntent.projectID != projectID {
		t.Fatal("new tab did not hand off to Session creation")
	}
}

func TestWorkspaceMouseEmptyProjectPlus(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	projectID, _ := s.activity().ProjectLayout.AddProject("Empty")
	nav, _ := s.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	width, height := terminalSize()
	rect := ducklord.CalculateWorkspaceGeometry(width, height, 4).Terminal
	s.handleWorkspaceMouse(workspaceMouse(0, rect.X+modalCellWidth(" Empty  ")+2, rect.Y, false))
	if !s.workspacePaneMode || s.workspacePaneStep != "source" || s.workspacePaneIntent.projectID != projectID {
		t.Fatal("empty Project plus unavailable")
	}
}
