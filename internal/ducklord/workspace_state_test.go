package ducklord

import (
	"maps"
	"reflect"
	"testing"
)

func TestWorkspaceSuppressedDefaultRequiresExplicitRestore(t *testing.T) {
	layout := NewProjectLayout()
	session := testLayoutIdentity("ABC123")
	if err := layout.Discover(session); err != nil {
		t.Fatal(err)
	}
	if err := layout.Detach(DefaultProjectID, session); err != nil {
		t.Fatal(err)
	}
	w, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	assertClosed := func() {
		t.Helper()
		if len(layout.Project(DefaultProjectID).Tabs) != 0 || !layout.defaultSuppressed(session) {
			t.Fatal("passive navigation reopened closed Default pane")
		}
		if w.CurrentPaneID() != "" || w.CurrentTabID() != "" {
			t.Fatal("closed pane retained navigation target")
		}
	}
	assertClosed()
	w.ReconcileLayout()
	if err := w.SelectProject(DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	if got := w.NotificationProjectID(session); got != DefaultProjectID {
		t.Fatalf("hidden membership label = %q", got)
	}
	assertClosed()
	if err := w.SelectQuickSession(session); err == nil {
		t.Fatal("quick navigation accepted a closed pane without restore")
	}
	w.EnterDetail()
	if err := w.PreviewDetail(session); err != nil {
		t.Fatal(err)
	}
	w.ReconcileLayout()
	if w.DetailSelection() != session {
		t.Fatal("reconcile lost hidden detail selection")
	}
	assertClosed()
	if _, err := w.JumpDetail(); err == nil || !w.InDetailMode() {
		t.Fatal("failed jump changed detail mode")
	}
	w.ExitDetail()
	clone := layout.Clone()
	if err := w.RebindLayout(&clone); err != nil {
		t.Fatal(err)
	}
	if len(clone.Project(DefaultProjectID).Tabs) != 0 || !clone.defaultSuppressed(session) {
		t.Fatal("rebind reopened pane")
	}
	if changed, err := clone.RestoreDefaultPane(session); err != nil || !changed {
		t.Fatalf("restore = %v, %v", changed, err)
	}
	if err := w.SelectQuickSession(session); err != nil {
		t.Fatal(err)
	}
	if w.CurrentPaneID() == "" || w.Region() != RegionQuickList {
		t.Fatal("restored pane not navigable")
	}
	w.EnterDetail()
	if err := w.PreviewDetail(session); err != nil {
		t.Fatal(err)
	}
	if got, err := w.JumpDetail(); err != nil || got != session || w.Region() != RegionTerminal {
		t.Fatalf("restored detail jump = %v, %v", got, err)
	}
}

func TestNotificationProjectPrecedenceDoesNotNavigate(t *testing.T) {
	layout := NewProjectLayout()
	session := testLayoutIdentity("ABC123")
	first, _ := layout.AddProject("First")
	second, _ := layout.AddProject("Second")
	third, _ := layout.AddProject("Third")
	for _, project := range []string{first, second, third} {
		if _, err := layout.Place(project, session, PlaceNewTab, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, focused, current, last, want string
	}{
		{"focused before current", third, second, first, third},
		{"current before last", "", second, third, second},
		{"unrelated focus", DefaultProjectID, second, third, second},
		{"last before order", "", DefaultProjectID, third, third},
		{"order fallback", "", DefaultProjectID, "", first},
		{"stale preferences", "removed", "removed", "removed", first},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, err := NewWorkspaceState(&layout)
			if err != nil {
				t.Fatal(err)
			}
			w.notificationFocusProjectID = tt.focused
			w.location.projectID = tt.current
			w.lastProject[session] = tt.last
			before := *w
			before.lastProject = map[SessionIdentity]string{session: tt.last}
			before.projectLocation = make(map[string]workspaceLocation)
			for id, location := range w.projectLocation {
				before.projectLocation[id] = location
			}
			if got := w.NotificationProjectID(session); got != tt.want {
				t.Fatalf("Project = %q, want %q", got, tt.want)
			}
			if got := w.NotificationProjectID(testLayoutIdentity("DEF456")); got != "" {
				t.Fatalf("unknown Session resolved to Project %q", got)
			}
			if !reflect.DeepEqual(before, *w) {
				t.Fatal("notification label changed workspace navigation")
			}
		})
	}
}

func TestWorkspaceQuickNavigationIsOneWayAndDoesNotFocus(t *testing.T) {
	layout := NewProjectLayout()
	a := testLayoutIdentity("ABC123")
	b := testLayoutIdentity("DEF456")
	first, _ := layout.AddProject("One")
	second, _ := layout.AddProject("Two")
	_, _ = layout.Place(first, a, PlaceNewTab, "")
	_, _ = layout.Place(second, b, PlaceNewTab, "")
	w, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SelectQuickSession(a); err != nil {
		t.Fatal(err)
	}
	if w.CurrentProjectID() != first || w.Region() != RegionQuickList {
		t.Fatalf("quick list did not navigate without focusing: %+v", w.location)
	}
	if err := w.SelectProject(second); err != nil {
		t.Fatal(err)
	}
	if w.CurrentProjectID() != second || w.quickSelection != a {
		t.Fatal("Project navigation altered quick-list selection")
	}
	if err := w.SelectQuickSession(b); err != nil {
		t.Fatal(err)
	}
	identity, err := w.FocusPane()
	if err != nil || identity != b || w.Region() != RegionTerminal {
		t.Fatalf("focus did not select intended Session: %v %+v", err, w.location)
	}
}

func TestWorkspaceProjectVisitUpdatesSessionNavigationHistory(t *testing.T) {
	for _, focus := range []bool{false, true} {
		for _, navigation := range []string{"quick", "detail"} {
			name := navigation + "/preview"
			if focus {
				name = navigation + "/focused"
			}
			t.Run(name, func(t *testing.T) {
				layout := NewProjectLayout()
				session := testLayoutIdentity("ABC123")
				first, _ := layout.AddProject("First")
				lastViewed, _ := layout.AddProject("Last viewed")
				unrelated, _ := layout.AddProject("Unrelated")
				for _, project := range []string{first, lastViewed} {
					if _, err := layout.Place(project, session, PlaceNewTab, ""); err != nil {
						t.Fatal(err)
					}
				}
				w, err := NewWorkspaceState(&layout)
				if err != nil {
					t.Fatal(err)
				}
				if err := w.SelectQuickSession(session); err != nil {
					t.Fatal(err)
				}
				if w.CurrentProjectID() != first {
					t.Fatal("initial navigation did not establish first Project")
				}
				if err := w.SelectProject(lastViewed); err != nil {
					t.Fatal(err)
				}
				if focus {
					if _, err := w.FocusPane(); err != nil {
						t.Fatal(err)
					}
				}
				if err := w.SelectProject(unrelated); err != nil {
					t.Fatal(err)
				}
				// Merely previewing the Session in detailed mode must not
				// replace its last normal Project with the unrelated one.
				w.EnterDetail()
				if err := w.PreviewDetail(session); err != nil {
					t.Fatal(err)
				}
				if navigation == "detail" {
					_, err = w.JumpDetail()
				} else {
					w.ExitDetail()
					err = w.SelectQuickSession(session)
				}
				if err != nil {
					t.Fatal(err)
				}
				if w.CurrentProjectID() != lastViewed {
					t.Fatalf("navigation selected %q, want last-viewed Project %q", w.CurrentProjectID(), lastViewed)
				}
			})
		}
	}
}

func TestNotificationFocusIsExplicitAndClearsWhenProjectDeleted(t *testing.T) {
	layout := NewProjectLayout()
	a, _ := layout.AddProject("First")
	b, _ := layout.AddProject("Second")
	w, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	if w.NotificationFocusProjectID() != "" {
		t.Fatal("notification focus started enabled")
	}
	if err := w.SelectProject(a); err != nil {
		t.Fatal(err)
	}
	w.ToggleNotificationFocus()
	if err := w.SelectProject(b); err != nil || w.NotificationFocusProjectID() != a {
		t.Fatal("ordinary Project navigation retargeted focus")
	}
	w.ToggleNotificationFocus()
	if w.NotificationFocusProjectID() != b {
		t.Fatal("explicit focus toggle did not switch target")
	}
	if err := layout.RemoveProject(b); err != nil {
		t.Fatal(err)
	}
	w.ReconcileLayout()
	if w.NotificationFocusProjectID() != "" {
		t.Fatal("deleted focused Project retained notification focus")
	}
}

func TestWorkspaceDetailPreviewRestoresNormalWorkspace(t *testing.T) {
	layout := NewProjectLayout()
	a := testLayoutIdentity("ABC123")
	b := testLayoutIdentity("DEF456")
	projectID, _ := layout.AddProject("Work")
	_, _ = layout.Place(projectID, a, PlaceNewTab, "")
	_, _ = layout.Place(projectID, b, PlaceNewTab, "")
	w, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SelectQuickSession(a); err != nil {
		t.Fatal(err)
	}
	if _, err := w.FocusPane(); err != nil {
		t.Fatal(err)
	}
	before := w.location
	w.EnterDetail()
	if err := w.PreviewDetail(b); err != nil {
		t.Fatal(err)
	}
	if w.Region() != RegionDetailList || w.CurrentTabID() != before.tabID || w.CurrentPaneID() != before.paneID {
		t.Fatal("detail preview changed normal Terminal area")
	}
	w.ExitDetail()
	if w.location != before {
		t.Fatalf("detail exit did not restore workspace: before=%+v after=%+v", before, w.location)
	}
	w.EnterDetail()
	if err := w.PreviewDetail(b); err != nil {
		t.Fatal(err)
	}
	got, err := w.JumpDetail()
	if err != nil || got != b || w.InDetailMode() || w.Region() != RegionTerminal {
		t.Fatalf("detail jump failed: identity=%+v err=%v state=%+v", got, err, w.location)
	}
	if w.quickSelection != a {
		t.Fatal("detail jump changed one-way quick-list selection")
	}
}

func TestWorkspacePanePreviewCannotImplicitlyTransferFocus(t *testing.T) {
	layout := NewProjectLayout()
	projectID, _ := layout.AddProject("Work")
	a, b := testLayoutIdentity("ABC123"), testLayoutIdentity("DEF456")
	firstPane, _ := layout.Place(projectID, a, PlaceNewTab, "")
	secondPane, _ := layout.Place(projectID, b, PlaceVertical, firstPane)
	w, _ := NewWorkspaceState(&layout)
	if err := w.SelectQuickSession(a); err != nil {
		t.Fatal(err)
	}
	if _, err := w.FocusPane(); err != nil {
		t.Fatal(err)
	}
	if err := w.SelectPane(projectID, secondPane); err != nil {
		t.Fatal(err)
	}
	if w.Region() != RegionTerminalPreview {
		t.Fatal("pane selection silently transferred keyboard focus")
	}
	got, err := w.FocusPane()
	if err != nil || got != b || w.Region() != RegionTerminal {
		t.Fatalf("explicit pane focus failed: %v %v", got, err)
	}
}

func TestWorkspaceCyclesTabsAndOnlyVisiblePanesWithoutFocusing(t *testing.T) {
	layout := NewProjectLayout()
	projectID, _ := layout.AddProject("Work")
	a, b, c := testLayoutIdentity("ABC123"), testLayoutIdentity("DEF456"), testLayoutIdentity("CDE789")
	first, _ := layout.Place(projectID, a, PlaceNewTab, "")
	second, _ := layout.Place(projectID, b, PlaceVertical, first)
	_, _ = layout.Place(projectID, c, PlaceNewTab, "")
	w, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	geometry := CalculateWorkspaceGeometry(120, 24, 4)
	if err := w.CycleVisiblePane(geometry, 1); err != nil {
		t.Fatal(err)
	}
	if w.CurrentPaneID() != second || w.Region() != RegionProjects {
		t.Fatalf("pane cycle changed focus or wrong pane: %+v", w.location)
	}
	if err := w.CycleVisiblePane(geometry, 1); err != nil {
		t.Fatal(err)
	}
	if w.CurrentPaneID() != first {
		t.Fatalf("pane cycle failed to wrap: %+v", w.location)
	}
	if err := w.CycleTab(1); err != nil {
		t.Fatal(err)
	}
	if got, ok := layout.PaneSession(w.CurrentProjectID(), w.CurrentPaneID()); !ok || got != c || w.Region() != RegionProjects {
		t.Fatalf("tab cycle changed focus or wrong tab: %+v", w.location)
	}
	if err := w.CycleTab(-1); err != nil || w.CurrentPaneID() != first {
		t.Fatalf("reverse tab cycle failed: %+v %v", w.location, err)
	}
	// A one-column terminal hides the second split leaf; it cannot be selected.
	narrow := CalculateWorkspaceGeometry(1, 24, 4)
	if err := w.CycleVisiblePane(narrow, 1); err != nil || w.CurrentPaneID() != first {
		t.Fatalf("hidden pane became selected: %+v %v", w.location, err)
	}
	if err := w.SelectPane(projectID, second); err != nil {
		t.Fatal(err)
	}
	if err := w.CycleVisiblePane(narrow, 1); err != nil || w.CurrentPaneID() != first {
		t.Fatalf("cycling from hidden pane skipped first visible pane: %+v %v", w.location, err)
	}
}

func TestWorkspacePaneNavigationTargetDoesNotMutateState(t *testing.T) {
	layout := NewProjectLayout()
	projectID, _ := layout.AddProject("Work")
	a, b := testLayoutIdentity("ABC123"), testLayoutIdentity("DEF456")
	c, d := testLayoutIdentity("CDE789"), testLayoutIdentity("ABC012")
	first, _ := layout.Place(projectID, a, PlaceNewTab, "")
	right, _ := layout.Place(projectID, b, PlaceVertical, first)
	secondTab, _ := layout.Place(projectID, c, PlaceNewTab, "")
	lastTab, _ := layout.Place(projectID, d, PlaceNewTab, "")
	w, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	geometry := CalculateWorkspaceGeometry(120, 24, 4)
	for _, tc := range []struct {
		name, from, command, want string
		wantErr                   bool
	}{
		{"neighbor", first, "right", right, false},
		{"edge", first, "left", first, false},
		{"next tab", first, "pagedown", secondTab, false},
		{"previous tab wraps", first, "pageup", lastTab, false},
		{"next tab wraps", lastTab, "pagedown", first, false},
		{"invalid command", first, "diagonal", first, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := w.SelectPane(projectID, tc.from); err != nil {
				t.Fatal(err)
			}
			if _, err := w.FocusPane(); err != nil {
				t.Fatal(err)
			}
			// Planning a successful move would populate this map and save
			// the focused location if it shared the live state's maps.
			clear(w.lastProject)
			before := *w
			before.lastProject = maps.Clone(w.lastProject)
			before.projectLocation = maps.Clone(w.projectLocation)
			got, err := w.PaneNavigationTarget(geometry, tc.command)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("target=%q err=%v; want=%q wantErr=%v", got, err, tc.want, tc.wantErr)
			}
			if !reflect.DeepEqual(*w, before) {
				t.Fatalf("planning mutated live state: before=%+v after=%+v", before, *w)
			}
		})
	}
}

func TestWorkspaceMoveVisiblePaneUsesScreenDirections(t *testing.T) {
	layout := NewProjectLayout()
	projectID, _ := layout.AddProject("Work")
	topLeft, _ := layout.Place(projectID, testLayoutIdentity("ABC123"), PlaceNewTab, "")
	topRight, _ := layout.Place(projectID, testLayoutIdentity("DEF456"), PlaceVertical, topLeft)
	bottomLeft, _ := layout.Place(projectID, testLayoutIdentity("CDE789"), PlaceHorizontal, topLeft)
	bottomRight, _ := layout.Place(projectID, testLayoutIdentity("ABC012"), PlaceHorizontal, topRight)
	w, _ := NewWorkspaceState(&layout)
	_ = w.SelectProject(projectID)
	geometry := WorkspaceGeometry{Terminal: WorkspaceRect{X: 1, Y: 1, Width: 80, Height: 25}}
	for _, step := range []struct {
		direction string
		want      string
	}{
		{"right", topRight}, // Tree order would select bottomLeft.
		{"right", topRight}, // No wrap at the right edge.
		{"down", bottomRight},
		{"down", bottomRight},
		{"left", bottomLeft},
		{"left", bottomLeft},
		{"up", topLeft},
		{"up", topLeft},
	} {
		if err := w.MoveVisiblePane(geometry, step.direction); err != nil {
			t.Fatal(err)
		}
		if w.CurrentPaneID() != step.want || w.Region() != RegionProjects {
			t.Fatalf("move %s: pane=%s want=%s region=%s", step.direction, w.CurrentPaneID(), step.want, w.Region())
		}
	}
	_, _ = w.FocusPane()
	if err := w.MoveVisiblePane(geometry, "up"); err != nil || w.Region() != RegionTerminal {
		t.Fatalf("edge move changed focus: %s, %v", w.Region(), err)
	}
	if err := w.MoveVisiblePane(geometry, "right"); err != nil || w.Region() != RegionTerminalPreview {
		t.Fatalf("move implicitly granted input: %s, %v", w.Region(), err)
	}
}

func TestWorkspaceMoveVisiblePaneHandlesHiddenAndUnavailablePanes(t *testing.T) {
	layout := NewProjectLayout()
	projectID, _ := layout.AddProject("Work")
	first, _ := layout.Place(projectID, testLayoutIdentity("ABC123"), PlaceNewTab, "")
	second, _ := layout.Place(projectID, testLayoutIdentity("DEF456"), PlaceVertical, first)
	w, _ := NewWorkspaceState(&layout)
	_ = w.SelectProject(projectID)
	narrow := CalculateWorkspaceGeometry(1, 24, 4)
	if err := w.MoveVisiblePane(narrow, "right"); err != nil || w.CurrentPaneID() != first {
		t.Fatalf("selected hidden pane: %s, %v", w.CurrentPaneID(), err)
	}
	_ = w.SelectPane(projectID, second)
	if err := w.MoveVisiblePane(narrow, "left"); err != nil || w.CurrentPaneID() != first {
		t.Fatalf("did not recover hidden selection: %s, %v", w.CurrentPaneID(), err)
	}
	if err := w.MoveVisiblePane(narrow, "diagonal"); err == nil || w.CurrentPaneID() != first {
		t.Fatal("invalid direction should fail without changing selection")
	}
	if err := w.MoveVisiblePane(WorkspaceGeometry{}, "right"); err == nil {
		t.Fatal("empty geometry should fail")
	}
	w.EnterDetail()
	if err := w.MoveVisiblePane(narrow, "right"); err == nil {
		t.Fatal("detail mode should reject normal pane navigation")
	}
}

func TestWorkspaceDetailJumpAndExitRepairRemovedPane(t *testing.T) {
	layout := NewProjectLayout()
	projectID, _ := layout.AddProject("Work")
	a, b := testLayoutIdentity("ABC123"), testLayoutIdentity("DEF456")
	_, _ = layout.Place(projectID, a, PlaceNewTab, "")
	_, _ = layout.Place(projectID, b, PlaceNewTab, "")
	w, _ := NewWorkspaceState(&layout)
	_ = w.SelectQuickSession(a)
	_, _ = w.FocusPane()
	w.EnterDetail()
	_ = w.PreviewDetail(b)
	layout.Destroy(b)
	if _, err := w.JumpDetail(); err == nil || !w.InDetailMode() {
		t.Fatal("stale detail jump exited mode without a destination")
	}
	layout.Destroy(a)
	w.ReconcileLayout()
	w.ExitDetail()
	if w.CurrentProjectID() != projectID || w.CurrentPaneID() != "" || w.Region() == RegionTerminal {
		t.Fatalf("exit restored removed pane: %+v", w.location)
	}
	if err := layout.RemoveProject(projectID); err != nil {
		t.Fatal(err)
	}
	w.ReconcileLayout()
	if w.CurrentProjectID() != DefaultProjectID {
		t.Fatalf("deleted Project remained selected: %+v", w.location)
	}
}
