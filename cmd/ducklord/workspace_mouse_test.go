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

func TestWorkspaceProjectWheelDispatchPassesListReportToWorkspaceHandler(t *testing.T) {
	state, projectID, _, _ := workspacePaneTestState(t)
	state.workspacePreview = true
	state.focused = false
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := state.activity().ProjectLayout.AddProject(fmt.Sprintf("Wheel %02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	x, y := geometry.Projects.X+2, geometry.Projects.Y+2
	if !state.workspaceListWheelOwnsInput(x, y) {
		t.Fatalf("Project-list wheel at (%d,%d) was claimed by generic wheel handling", x, y)
	}
	state.focused = true
	if state.workspaceListWheelOwnsInput(x, y) {
		t.Fatal("focused terminal wheel was routed to the workspace list")
	}
	state.focused = false
	if handled, changed := state.handleWorkspaceMouse(workspaceMouse(65, x, y, false)); !handled || !changed {
		t.Fatalf("Project-list wheel was not applied locally: handled=%t changed=%t", handled, changed)
	}
	if nav.CurrentProjectID() != state.activity().ProjectLayout.Projects[4].ID {
		t.Fatalf("Project-list wheel selected %q, want third project after origin", nav.CurrentProjectID())
	}
	if state.focused || !state.workspaceProjectFocus || state.workspaceMouseFocus {
		t.Fatalf("Project-list wheel changed terminal ownership: focused=%t projectFocus=%t mouseFocus=%t", state.focused, state.workspaceProjectFocus, state.workspaceMouseFocus)
	}
}

func TestWorkspaceMousePaneRequestsOwnerGatedFocus(t *testing.T) {
	state, projectID, _, _ := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	target := ducklord.WorkspaceVisiblePaneRects(&state.activity().ProjectLayout, nav, geometry)[0].Rect
	handled, changed := state.handleWorkspaceMouse(workspaceMouse(0, target.X+1, target.Y+1, false))
	if !handled || !changed || !state.workspaceMouseFocus || state.focused {
		t.Fatal("pane click must request focus, not bypass remote control authorization")
	}
	state.workspaceMouseFocus = false
	state.handleWorkspaceMouse(workspaceMouse(0, geometry.Projects.X+1, geometry.Projects.Y+1, false))
	if !state.workspaceProjectFocus || state.workspaceMouseFocus {
		t.Fatal("Project click should focus local navigation only")
	}
}

func TestWorkspaceMouseSelectsNotesLeafBody(t *testing.T) {
	state, projectID, _, _ := workspacePaneTestState(t)
	noteID, err := state.activity().ProjectLayout.PlaceNote(projectID)
	if err != nil {
		t.Fatal(err)
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err = nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	if err = nav.SelectPane(projectID, noteID); err != nil {
		t.Fatal(err)
	}
	geometry := ducklord.CalculateWorkspaceGeometry(120, 40, 4)
	leaf := ducklord.WorkspaceVisibleLeafRects(&state.activity().ProjectLayout, nav, geometry)
	if len(leaf) == 0 || leaf[len(leaf)-1].PaneID != noteID {
		t.Fatalf("notes leaf missing: %+v", leaf)
	}
	r := leaf[len(leaf)-1].Rect
	handled, changed := state.handleWorkspaceMouse(workspaceMouse(0, r.X+1, r.Y+1, false))
	if !handled || !changed || nav.CurrentPaneID() != noteID {
		t.Fatalf("notes click did not select: handled=%v changed=%v pane=%s", handled, changed, nav.CurrentPaneID())
	}
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

func TestWorkspaceMouseDragReordersProjectsAndPersists(t *testing.T) {
	state, firstProject, _, _ := workspacePaneTestState(t)
	secondProject, err := state.activity().ProjectLayout.AddProject("Second")
	if err != nil {
		t.Fatal(err)
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(firstProject); err != nil {
		t.Fatal(err)
	}
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	// Work is row 1 and Second is row 2 after the Default Project title row.
	state.handleWorkspaceMouse(workspaceMouse(0, geometry.Projects.X+1, geometry.Projects.Y+2, false))
	state.handleWorkspaceMouse(workspaceMouse(0, geometry.Projects.X+1, geometry.Projects.Y+3, true))
	projects := state.activity().ProjectLayout.Projects
	if projects[1].ID != secondProject || projects[2].ID != firstProject {
		t.Fatalf("project drag order = %q, %q; want second then first", projects[1].ID, projects[2].ID)
	}
	loaded, err := state.activityStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProjectLayout.Projects[1].ID != secondProject || loaded.ProjectLayout.Projects[2].ID != firstProject {
		t.Fatalf("persisted project drag order = %q, %q", loaded.ProjectLayout.Projects[1].ID, loaded.ProjectLayout.Projects[2].ID)
	}
}

func TestWorkspaceMouseDragReordersTabs(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	identity, ok := ducklord.IdentityFromSession(b)
	if !ok {
		t.Fatal("invalid test session identity")
	}
	if _, err := state.activity().ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	project := state.activity().ProjectLayout.Project(projectID)
	if project == nil || len(project.Tabs) != 2 {
		t.Fatal("expected two tabs")
	}
	left := geometry.Terminal.X + modalCellWidth(" "+project.Name+"  ")
	firstWidth := modalCellWidth(ducklord.WorkspaceTabLabel(project.Tabs[0], 0, true))
	secondX := left + firstWidth + modalCellWidth(ducklord.WorkspaceTabLabel(project.Tabs[1], 1, false))/2
	state.handleWorkspaceMouse(workspaceMouse(0, left+firstWidth/2, geometry.Terminal.Y, false))
	state.handleWorkspaceMouse(workspaceMouse(0, secondX, geometry.Terminal.Y, true))
	project = state.activity().ProjectLayout.Project(projectID)
	if project.Tabs[0].ID == project.Tabs[1].ID || project.Tabs[0].Root == nil || project.Tabs[1].Root == nil {
		t.Fatal("tab drag produced invalid tab order")
	}
	// The first tab contains A and the second contains B before the drag.
	aIdentity, ok := ducklord.IdentityFromSession(a)
	if !ok || paneIDForSession(project.Tabs[0].Root, identity) == "" || paneIDForSession(project.Tabs[1].Root, aIdentity) == "" {
		t.Fatalf("tab drag did not move B after A: %+v", project.Tabs)
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

func TestWorkspaceScrollbarTrackPagesThumbDragAndRelease(t *testing.T) {
	state, firstID, _, _ := workspacePaneTestState(t)
	state.workspacePreview = true
	state.focused = false
	state.workspaceProjectFocus = false
	state.workspaceConfigFocus = "session-list"
	for i := 0; i < 100; i++ {
		if _, err := state.activity().ProjectLayout.AddProject(fmt.Sprintf("Scroll %03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(firstID); err != nil {
		t.Fatal(err)
	}
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	bar, ok := ducklord.CalculateWorkspaceScrollbar(geometry.Projects, len(state.activity().ProjectLayout.Projects), 0)
	if !ok {
		t.Skip("terminal is too short to show workspace list overflow")
	}
	// A track click below the thumb pages by the visible row count.
	handled, changed := state.handleWorkspaceMouse(workspaceMouse(0, bar.TrackX, bar.TrackY+bar.TrackHeight-1, false))
	if !handled || !changed || nav.CurrentProjectID() == firstID {
		t.Fatalf("track click failed to page: handled=%v changed=%v project=%q", handled, changed, nav.CurrentProjectID())
	}
	if !state.workspaceProjectFocus || state.workspaceConfigFocus != "project-pane" || state.focused {
		t.Fatalf("Project track click did not focus local Project navigation: project=%t config=%q terminal=%t", state.workspaceProjectFocus, state.workspaceConfigFocus, state.focused)
	}
	// Thumb press captures without moving, then movement preserves the grab offset.
	offsets := state.workspaceColumnOffsets(geometry, nav, state.workspaceQuickSessions())
	bar, _ = ducklord.CalculateWorkspaceScrollbar(geometry.Projects, len(state.activity().ProjectLayout.Projects), offsets.Projects)
	before := nav.CurrentProjectID()
	handled, changed = state.handleWorkspaceMouse(workspaceMouse(0, bar.TrackX, bar.ThumbY, false))
	if !handled || changed || nav.CurrentProjectID() != before || state.workspaceScrollbarDrag != "projects" {
		t.Fatalf("thumb press jumped or failed capture: handled=%v changed=%v", handled, changed)
	}
	state.handleWorkspaceMouse(workspaceMouse(32, bar.TrackX, bar.TrackY+bar.TrackHeight-1, false))
	if nav.CurrentProjectID() == before {
		t.Fatal("thumb drag did not update project selection")
	}
	offsets = state.workspaceColumnOffsets(geometry, nav, state.workspaceQuickSessions())
	bar, _ = ducklord.CalculateWorkspaceScrollbar(geometry.Projects, len(state.activity().ProjectLayout.Projects), offsets.Projects)
	if bar.ThumbY+bar.ThumbHeight != bar.TrackY+bar.TrackHeight {
		t.Fatalf("dragging to track bottom left thumb at %d, want bottom %d", bar.ThumbY+bar.ThumbHeight, bar.TrackY+bar.TrackHeight)
	}
	handled, _ = state.handleWorkspaceMouse(workspaceMouse(0, 1, 1, true))
	if !handled || state.workspaceScrollbarDrag != "" {
		t.Fatal("release outside scrollbar did not clear capture")
	}
}

func TestWorkspaceSessionTrackClickFocusesListWithoutAttaching(t *testing.T) {
	state, _, _, base := workspacePaneTestState(t)
	state.workspacePreview = true
	state.focused = false
	state.workspaceProjectFocus = true
	for i := 0; i < 80; i++ {
		session := base
		session.SessionID = fmt.Sprintf("TRACK%03d", i)
		session.Name = session.SessionID
		state.sessions = append(state.sessions, session)
	}
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	rows := state.workspaceQuickSessions()
	bar, ok := ducklord.CalculateWorkspaceScrollbar(geometry.Quick, len(rows), 0)
	if !ok {
		t.Skip("terminal is too short to show session list overflow")
	}
	before := state.currentKey()
	handled, changed := state.handleWorkspaceMouse(workspaceMouse(0, bar.TrackX, bar.TrackY+bar.TrackHeight-1, false))
	if !handled || !changed || state.currentKey() == before {
		t.Fatalf("session track click failed to page: handled=%v changed=%v key=%q", handled, changed, state.currentKey())
	}
	if state.workspaceProjectFocus || state.workspaceConfigFocus != "session-list" || state.focused || state.workspaceMouseFocus {
		t.Fatalf("session track click did not focus local list: project=%t config=%q terminal=%t mouse=%t", state.workspaceProjectFocus, state.workspaceConfigFocus, state.focused, state.workspaceMouseFocus)
	}
}

func TestWorkspaceScrollbarTrackDoesNotReactToHiddenWorkspaceInDetailMode(t *testing.T) {
	state, firstID, _, _ := workspacePaneTestState(t)
	state.workspacePreview = true
	state.focused = false
	for i := 0; i < 80; i++ {
		if _, err := state.activity().ProjectLayout.AddProject(fmt.Sprintf("Detail %03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(firstID); err != nil {
		t.Fatal(err)
	}
	nav.EnterDetail()
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	bar, ok := ducklord.CalculateWorkspaceScrollbar(geometry.Projects, len(state.activity().ProjectLayout.Projects), 0)
	if !ok {
		t.Skip("terminal is too short to show workspace list overflow")
	}
	handled, _ := state.handleWorkspaceMouse(workspaceMouse(0, bar.TrackX, bar.TrackY+bar.TrackHeight-1, false))
	if !handled || nav.CurrentProjectID() != firstID || state.workspaceScrollbarDrag != "" {
		t.Fatalf("hidden Project scrollbar reacted in detail mode: handled=%v project=%q drag=%q", handled, nav.CurrentProjectID(), state.workspaceScrollbarDrag)
	}
	before := state.selected
	_, _ = state.handleWorkspaceMouse(workspaceMouse(64, geometry.Projects.X+1, geometry.Projects.Y+1, false))
	if state.selected != before || nav.CurrentProjectID() != firstID {
		t.Fatalf("workspace wheel changed hidden selection in detail mode: selected=%d project=%q", state.selected, nav.CurrentProjectID())
	}
}

func TestWorkspaceScrollbarCaptureClearsWhenRouteLosesOwnership(t *testing.T) {
	t.Run("terminal focus release", func(t *testing.T) {
		state, _, _, _ := workspacePaneTestState(t)
		state.workspacePreview = true
		state.focused = true
		state.workspaceScrollbarDrag = "projects"
		state.workspaceScrollbarDragGrab = 2
		handled, _ := state.handleWorkspaceMouse(workspaceMouse(0, 1, 1, true))
		if handled || state.workspaceScrollbarDrag != "" || state.workspaceScrollbarDragGrab != 0 {
			t.Fatalf("release after terminal focus retained list capture: handled=%t drag=%q grab=%d", handled, state.workspaceScrollbarDrag, state.workspaceScrollbarDragGrab)
		}
	})

	t.Run("detail transition motion", func(t *testing.T) {
		state, firstID, _, _ := workspacePaneTestState(t)
		state.workspacePreview = true
		state.focused = false
		for i := 0; i < 80; i++ {
			if _, err := state.activity().ProjectLayout.AddProject(fmt.Sprintf("Capture %03d", i)); err != nil {
				t.Fatal(err)
			}
		}
		nav, err := state.workspaceNavigation()
		if err != nil {
			t.Fatal(err)
		}
		if err := nav.SelectProject(firstID); err != nil {
			t.Fatal(err)
		}
		width, height := terminalSize()
		geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
		offsets := state.workspaceColumnOffsets(geometry, nav, state.workspaceQuickSessions())
		bar, ok := ducklord.CalculateWorkspaceScrollbar(geometry.Projects, len(state.activity().ProjectLayout.Projects), offsets.Projects)
		if !ok {
			t.Skip("terminal is too short to show workspace list overflow")
		}
		_, _ = state.handleWorkspaceMouse(workspaceMouse(0, bar.TrackX, bar.ThumbY, false))
		if state.workspaceScrollbarDrag != "projects" {
			t.Fatal("Project thumb press did not capture drag")
		}
		nav.EnterDetail()
		before := nav.CurrentProjectID()
		_, _ = state.handleWorkspaceMouse(workspaceMouse(32, bar.TrackX, bar.TrackY+bar.TrackHeight-1, false))
		if state.workspaceScrollbarDrag != "" || state.workspaceScrollbarDragGrab != 0 || nav.CurrentProjectID() != before {
			t.Fatalf("drag crossed into detail route: drag=%q grab=%d project=%q before=%q", state.workspaceScrollbarDrag, state.workspaceScrollbarDragGrab, nav.CurrentProjectID(), before)
		}
	})
}

func TestWorkspaceScrollbarCaptureClearsBeforeBlockingModalConsumesRelease(t *testing.T) {
	state, _, _, _ := workspacePaneTestState(t)
	state.workspacePreview = true
	state.workspaceScrollbarDrag = "projects"
	state.workspaceScrollbarDragGrab = 1
	state.actionMenu = true
	release := workspaceMouse(0, 80, 12, true)

	// The event loop invokes this route-boundary hook before its modal branch;
	// that branch consumes releases without calling handleWorkspaceMouse.
	state.prepareWorkspaceMouseDispatch(release)
	if !state.blockingModalOpen() {
		t.Fatal("fixture must route the release to a blocking modal")
	}
	if state.workspaceScrollbarDrag != "" || state.workspaceScrollbarDragGrab != 0 {
		t.Fatalf("modal-consumed release left stale workspace capture: drag=%q grab=%d", state.workspaceScrollbarDrag, state.workspaceScrollbarDragGrab)
	}
}

func TestWorkspaceSessionPageDownUsesVisibleQuickRows(t *testing.T) {
	state, _, _, base := workspacePaneTestState(t)
	state.workspacePreview = true
	state.workspaceProjectFocus = false
	state.sessions = []ducklord.RemoteSession{state.sessions[0]}
	for i := 1; i < 80; i++ {
		session := base
		session.SessionID = fmt.Sprintf("PAGE%03d", i)
		session.Name = fmt.Sprintf("Page %03d", i)
		state.sessions = append(state.sessions, session)
	}
	state.selected = 0
	_, height := terminalSize()
	page := max(1, ducklord.CalculateWorkspaceGeometry(100, height, 4).Quick.Height-1)
	if len(state.sessions) <= page {
		t.Fatalf("fixture has %d sessions, needs more than page size %d", len(state.sessions), page)
	}
	if got := state.handleInput([]byte("\x1b[6~")); got != "select" {
		t.Fatalf("PageDown action = %q, want select", got)
	}
	if state.selected != page {
		t.Fatalf("PageDown selected row %d, want visible-page step %d", state.selected, page)
	}
}
