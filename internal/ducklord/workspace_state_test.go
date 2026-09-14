package ducklord

import "testing"

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
