package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func workspaceMouse(button, x, y int, release bool) []byte {
	suffix := "M"
	if release {
		suffix = "m"
	}
	return []byte(fmt.Sprintf("\x1b[<%d;%d;%d%s", button, x, y, suffix))
}

func TestWorkspaceMouseDragPlacesExistingSessionWithoutYield(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	panes := ducklord.WorkspaceVisiblePaneRects(&state.activity().ProjectLayout, nav, geometry)
	if len(panes) != 1 {
		t.Fatalf("expected one visible drop target, got %d", len(panes))
	}
	quickX, quickY := geometry.Quick.X+1, geometry.Quick.Y+2 // B is second rendered row.
	targetX, targetY := panes[0].Rect.X+1, panes[0].Rect.Y+1
	state.handleWorkspaceMouse(workspaceMouse(0, quickX, quickY, false))
	// Some terminals emit press/release without an intermediate drag report.
	state.handleWorkspaceMouse(workspaceMouse(0, targetX, targetY, true))
	if !state.workspacePaneMode || state.workspacePaneStep != "drop-placement" || len(state.workspacePaneChoices()) != 3 {
		t.Fatalf("drag did not open placement modal: mode=%t step=%q choices=%v", state.workspacePaneMode, state.workspacePaneStep, state.workspacePaneChoices())
	}
	state.handleWorkspacePaneInput([]byte("j")) // split vertically
	state.handleWorkspacePaneInput([]byte("\r"))
	identity, _ := ducklord.IdentityFromSession(b)
	if state.workspacePaneMode || state.workspacePaneErr != "" || state.selected != 0 || state.focused || state.currentSession().SessionID != a.SessionID {
		t.Fatal("local drag changed quick selection, PTY focus, or left modal open")
	}
	if projects := state.activity().ProjectLayout.ProjectsFor(identity); len(projects) != 1 || projects[0] != projectID {
		t.Fatalf("drag did not place B in Project: %v", projects)
	}
	if state.workspacePaneCandidate.Client != "" || state.workspaceDragSession.Client != "" {
		t.Fatal("drag candidate survived modal completion")
	}
}

func TestWorkspaceMouseDragIntoEmptyProjectOffersNewTab(t *testing.T) {
	state, _, _, _ := workspacePaneTestState(t)
	projectID, err := state.activity().ProjectLayout.AddProject("Empty")
	if err != nil {
		t.Fatal(err)
	}
	nav, _ := state.workspaceNavigation()
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	state.handleWorkspaceMouse(workspaceMouse(0, geometry.Quick.X+1, geometry.Quick.Y+1, false))
	state.handleWorkspaceMouse(workspaceMouse(32, geometry.Terminal.X+1, geometry.Terminal.Y+2, false))
	state.handleWorkspaceMouse(workspaceMouse(0, geometry.Terminal.X+1, geometry.Terminal.Y+2, true))
	if choices := state.workspacePaneChoices(); len(choices) != 1 || choices[0] != "New Terminal tab" {
		t.Fatalf("empty Project offered invalid split target: %v", choices)
	}
	state.handleWorkspacePaneInput([]byte("\r"))
	if project := state.activity().ProjectLayout.Project(projectID); project == nil || len(project.Tabs) != 1 {
		t.Fatal("empty Project did not receive a new tab")
	}
}

func TestWorkspaceMouseDragRejectsChangedSessionAtCommit(t *testing.T) {
	state, projectID, _, b := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	target := ducklord.WorkspaceVisiblePaneRects(&state.activity().ProjectLayout, nav, geometry)[0].Rect
	state.handleWorkspaceMouse(workspaceMouse(0, geometry.Quick.X+1, geometry.Quick.Y+2, false))
	state.handleWorkspaceMouse(workspaceMouse(0, target.X+1, target.Y+1, true))
	state.sessions[1].RuntimeGeneration++
	state.handleWorkspacePaneInput([]byte("\r"))
	identity, _ := ducklord.IdentityFromSession(b)
	if !state.workspacePaneMode || !strings.Contains(state.workspacePaneErr, "changed") || len(state.activity().ProjectLayout.ProjectsFor(identity)) != 1 ||
		state.activity().ProjectLayout.ProjectsFor(identity)[0] != ducklord.DefaultProjectID {
		t.Fatal("stale drag candidate changed Project layout")
	}
}

func TestWorkspaceMouseDragKnownOfflineSessionLocally(t *testing.T) {
	state, projectID, _, b := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	state.sessions[1].Status = "disconnected"
	state.disconnectedHosts = map[string]bool{"host": true}
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	target := ducklord.WorkspaceVisiblePaneRects(&state.activity().ProjectLayout, nav, geometry)[0].Rect
	state.handleWorkspaceMouse(workspaceMouse(0, geometry.Quick.X+1, geometry.Quick.Y+2, false))
	state.handleWorkspaceMouse(workspaceMouse(0, target.X+1, target.Y+1, true))
	if !state.workspacePaneMode {
		t.Fatal("known offline Session could not open local drag placement")
	}
	state.handleWorkspacePaneInput([]byte("\r")) // new tab
	identity, _ := ducklord.IdentityFromSession(b)
	if state.workspacePaneMode || len(state.sessions) != 2 || len(state.activity().ProjectLayout.ProjectsFor(identity)) != 1 ||
		state.activity().ProjectLayout.ProjectsFor(identity)[0] != projectID {
		t.Fatal("offline drag did not place local pane without touching remote inventory")
	}
}

func TestWorkspaceMouseDragRejectsSelfSplitAndCancelKeepsLayout(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	target := ducklord.WorkspaceVisiblePaneRects(&state.activity().ProjectLayout, nav, geometry)[0].Rect
	state.handleWorkspaceMouse(workspaceMouse(0, geometry.Quick.X+1, geometry.Quick.Y+1, false)) // A
	state.handleWorkspaceMouse(workspaceMouse(0, target.X+1, target.Y+1, true))
	state.handleWorkspacePaneInput([]byte("j")) // split A beside itself
	state.handleWorkspacePaneInput([]byte("\r"))
	if !state.workspacePaneMode || !strings.Contains(state.workspacePaneErr, "different pane") {
		t.Fatal("self-split was not rejected")
	}
	state.handleWorkspacePaneInput([]byte("\x03"))
	if state.workspacePaneMode {
		t.Fatal("Ctrl+C did not close drag placement modal")
	}
	aID, _ := ducklord.IdentityFromSession(a)
	bID, _ := ducklord.IdentityFromSession(b)
	project := state.activity().ProjectLayout.Project(projectID)
	if project == nil || len(project.Tabs) != 1 || paneIDForSession(project.Tabs[0].Root, aID) == "" ||
		paneIDForSession(project.Tabs[0].Root, bID) != "" {
		t.Fatal("canceled self-split changed Project layout")
	}
}

func TestWorkspaceQuickMouseRowsUseRenderedIdentityProjection(t *testing.T) {
	state, _, a, b := workspacePaneTestState(t)
	invalid := a
	invalid.SessionID = ""
	state.sessions = []ducklord.RemoteSession{invalid, a, b}
	rows := state.workspaceQuickSessions()
	if len(rows) != 2 || rows[0].SessionID != a.SessionID || rows[1].SessionID != b.SessionID {
		t.Fatalf("quick mouse mapping diverged from rendered rows: %+v", rows)
	}
}

func TestWorkspaceQuickMousePressUsesScrolledRow(t *testing.T) {
	state, _, a, _ := workspacePaneTestState(t)
	width, height := terminalSize()
	for i := 0; i < 40; i++ {
		extra := a
		extra.SessionID = fmt.Sprintf("X%05d", i)
		extra.Name = extra.SessionID
		state.sessions = append(state.sessions, extra)
	}
	state.selected = len(state.sessions) - 1
	state.selectedKey = sessionKey(state.sessions[state.selected])
	actualGeometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	rows := state.workspaceQuickSessions()
	state.workspaceQuickOffset = ducklord.WorkspaceListOffset(0, len(rows)-1, actualGeometry.Quick.Height-1, len(rows))
	expected := rows[state.workspaceQuickOffset]
	state.handleWorkspaceMouse(workspaceMouse(0, actualGeometry.Quick.X+1, actualGeometry.Quick.Y+1, false))
	if state.workspaceDragSession.SessionID != expected.SessionID {
		t.Fatalf("scrolled first row selected %q, want %q", state.workspaceDragSession.SessionID, expected.SessionID)
	}
}
