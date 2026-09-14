package ducklord

import (
	"bytes"
	"strings"
	"testing"
)

func TestWorkspaceGeometryKeepsThreeRegionsAndNarrowFallback(t *testing.T) {
	wide := CalculateWorkspaceGeometry(120, 30, 4)
	if wide.Projects.Width <= 0 || wide.Quick.Width <= 0 || wide.Terminal.Width <= 0 ||
		wide.Terminal.X <= wide.Quick.X || wide.Quick.X <= wide.Projects.X {
		t.Fatalf("three regions overlap: %+v", wide)
	}
	narrow := CalculateWorkspaceGeometry(50, 20, 4)
	if narrow.Projects.Width != 0 || narrow.Quick.Width != 0 || narrow.Terminal.Width != 50 {
		t.Fatalf("narrow fallback: %+v", narrow)
	}
}

func TestWorkspaceListOffsetFollowsSelectionAndClampsAfterResize(t *testing.T) {
	if got := WorkspaceListOffset(0, 12, 4, 20); got != 9 {
		t.Fatalf("selection below viewport offset=%d", got)
	}
	if got := WorkspaceListOffset(9, 10, 4, 20); got != 9 {
		t.Fatalf("visible selection shifted viewport to %d", got)
	}
	if got := WorkspaceListOffset(9, 2, 4, 20); got != 2 {
		t.Fatalf("selection above viewport offset=%d", got)
	}
	if got := WorkspaceListOffset(9, 2, 8, 7); got != 0 {
		t.Fatalf("shrunk list offset=%d", got)
	}
}

func TestWorkspaceRendererUsesColumnOffsets(t *testing.T) {
	layout := NewProjectLayout()
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		if _, err := layout.AddProject(name); err != nil {
			t.Fatal(err)
		}
	}
	nav, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(layout.Projects[5].ID); err != nil {
		t.Fatal(err)
	}
	items := []WorkspaceListItem{{Name: "one", Host: "h"}, {Name: "two", Host: "h"}, {Name: "three", Host: "h"}, {Name: "four", Host: "h"}, {Name: "five", Host: "h", Selected: true}}
	geometry := WorkspaceGeometry{Projects: WorkspaceRect{X: 1, Y: 1, Width: 20, Height: 3}, Quick: WorkspaceRect{X: 22, Y: 1, Width: 20, Height: 3}}
	var out bytes.Buffer
	RenderWorkspaceBody(&out, geometry, &layout, nav, items, nil, nil, WorkspaceColumnOffsets{Projects: 4, Quick: 3})
	text := out.String()
	if !strings.Contains(text, "› five") || !strings.Contains(text, "› five @h") || strings.Contains(text, "one @h") {
		t.Fatalf("offset columns did not show selected rows: %q", text)
	}
}

func TestWorkspaceRendererShowsProjectsSplitPanesAndPreservesColor(t *testing.T) {
	layout := NewProjectLayout()
	projectID, _ := layout.AddProject("中文工作")
	a := testLayoutIdentity("ABC123")
	b := testLayoutIdentity("DEF456")
	firstPane, err := layout.Place(projectID, a, PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Place(projectID, b, PlaceVertical, firstPane); err != nil {
		t.Fatal(err)
	}
	nav, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectQuickSession(a); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	RenderWorkspaceBody(&out, CalculateWorkspaceGeometry(120, 15, 4), &layout, nav,
		[]WorkspaceListItem{{Identity: a, Name: "Codex", Host: "host-a", Selected: true},
			{Identity: b, Name: "Claude", Host: "host-b", Unread: true}},
		func(id string) bool { return id == projectID },
		func(session SessionIdentity, _, _ int) WorkspacePaneView {
			if session == a {
				return WorkspacePaneView{Title: "Codex", Lines: []string{"\x1b[31mred\x1b[0m"}}
			}
			return WorkspacePaneView{Title: "Claude", Lines: []string{"blue"}}
		})
	text := out.String()
	for _, want := range []string{"PROJECTS", "SESSIONS", "中文工作", "Codex", "Claude", "\x1b[31mred"} {
		if !strings.Contains(text, want) {
			t.Fatalf("workspace missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "\x1b[K") {
		t.Fatal("column render erased neighboring pane to end of line")
	}
}

func TestWorkspaceMarkedRowPreservesUnreadMarkerForLongNames(t *testing.T) {
	for _, test := range []struct {
		prefix, label, suffix string
		width                 int
	}{
		{"› ", "very-long-project-name", " •", 18},
		{"  ", "very-long-session-name @host-a", " •", 24},
		{"› ", "非常非常長的中文專案名稱", " • ◎", 18},
		{"  ", "非常非常長的工作階段名稱 @主機", " •", 24},
	} {
		row := workspaceMarkedRow(test.prefix, test.label, test.suffix, test.width)
		if !strings.HasPrefix(row, test.prefix) || !strings.HasSuffix(row, test.suffix) || workspaceCellWidth(row) > test.width {
			t.Fatalf("marked row lost its prefix, marker, or bounds: %q", row)
		}
	}
}

func TestWorkspaceVTLineRejectsCursorAndOSCInjection(t *testing.T) {
	var out bytes.Buffer
	workspaceWriteVT(&out, 4, 8, 10, "\x1b[2J\x1b]0;bad\a\x1b[32mOK")
	text := out.String()
	if strings.Contains(text, "\x1b[2J") || strings.Contains(text, "\x1b]") {
		t.Fatalf("unsafe terminal control escaped pane: %q", text)
	}
	if !strings.Contains(text, "\x1b[32mOK") {
		t.Fatalf("safe SGR color lost: %q", text)
	}
}

func TestWorkspaceRendererDistinguishesFocusAndReadonly(t *testing.T) {
	layout := NewProjectLayout()
	a := testLayoutIdentity("ABC123")
	if err := layout.Discover(a); err != nil {
		t.Fatal(err)
	}
	nav, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	geometry := WorkspaceGeometry{Terminal: WorkspaceRect{X: 1, Y: 1, Width: 70, Height: 4}}
	render := func(view WorkspacePaneView) string {
		var out bytes.Buffer
		RenderWorkspaceBody(&out, geometry, &layout, nav, nil, nil, func(SessionIdentity, int, int) WorkspacePaneView { return view })
		return out.String()
	}
	if got := render(WorkspacePaneView{Title: "Codex"}); !strings.Contains(got, "◇ Codex") {
		t.Fatalf("selected preview not distinguished: %q", got)
	}
	if got := render(WorkspacePaneView{Title: "Codex", Focused: true}); !strings.Contains(got, "▣ Codex") {
		t.Fatalf("focused writer not distinguished: %q", got)
	}
	if got := render(WorkspacePaneView{Title: "Codex", Focused: true, ReadOnly: true}); !strings.Contains(got, "◌ Codex") || strings.Contains(got, "▣ Codex") {
		t.Fatalf("readonly pane appeared writable: %q", got)
	}
}

func TestWorkspaceVisibleSessionsMatchesSuppressedSplit(t *testing.T) {
	layout := NewProjectLayout()
	a, b := testLayoutIdentity("ABC123"), testLayoutIdentity("DEF456")
	first, err := layout.Place(DefaultProjectID, a, PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Place(DefaultProjectID, b, PlaceHorizontal, first); err != nil {
		t.Fatal(err)
	}
	nav, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	narrow := WorkspaceGeometry{Terminal: WorkspaceRect{X: 1, Y: 1, Width: 70, Height: 2}}
	if got := WorkspaceVisibleSessions(&layout, nav, narrow); len(got) != 1 || got[0] != a {
		t.Fatalf("suppressed pane remained subscribed: %+v", got)
	}
	var out bytes.Buffer
	RenderWorkspaceBody(&out, narrow, &layout, nav, nil, nil, nil)
	if !strings.Contains(out.String(), "+1 hidden") {
		t.Fatalf("suppressed pane was invisible to user: %q", out.String())
	}
	if err := nav.SelectQuickSession(b); err != nil {
		t.Fatal(err)
	}
	if _, err := nav.FocusVisiblePane(narrow); err == nil {
		t.Fatal("hidden split pane acquired PTY input focus")
	}
	if _, visible := WorkspaceVisiblePaneRect(&layout, nav, narrow); visible {
		t.Fatal("hidden split pane reported a PTY rectangle")
	}
	wide := WorkspaceGeometry{Terminal: WorkspaceRect{X: 1, Y: 1, Width: 70, Height: 6}}
	if got := WorkspaceVisibleSessions(&layout, nav, wide); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("restored split not visible: %+v", got)
	}
	if focused, err := nav.FocusVisiblePane(wide); err != nil || focused != b {
		t.Fatalf("visible pane focus=%+v err=%v", focused, err)
	}
	if rect, visible := WorkspaceVisiblePaneRect(&layout, nav, wide); !visible || rect.Y != 4 || rect.Height != 3 || rect.Width != 70 {
		t.Fatalf("horizontal split PTY rectangle=%+v visible=%v", rect, visible)
	}
}

func TestWorkspaceVisiblePaneRectUsesVerticalLeafWidthAndPosition(t *testing.T) {
	layout := NewProjectLayout()
	a, b := testLayoutIdentity("ABC123"), testLayoutIdentity("DEF456")
	first, err := layout.Place(DefaultProjectID, a, PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Place(DefaultProjectID, b, PlaceVertical, first); err != nil {
		t.Fatal(err)
	}
	nav, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectQuickSession(b); err != nil {
		t.Fatal(err)
	}
	geometry := WorkspaceGeometry{Terminal: WorkspaceRect{X: 55, Y: 4, Width: 66, Height: 16}}
	panes := WorkspaceVisiblePaneRects(&layout, nav, geometry)
	if len(panes) != 2 || panes[0].Identity != a || panes[1].Identity != b || panes[0].Rect.Width != 33 || panes[1].Rect.X != 88 {
		t.Fatalf("visible subscription geometry=%+v", panes)
	}
	if rect, visible := WorkspaceVisiblePaneRect(&layout, nav, geometry); !visible || rect.X != 88 || rect.Y != 5 || rect.Width != 33 || rect.Height != 15 {
		t.Fatalf("vertical split PTY rectangle=%+v visible=%v", rect, visible)
	}
}
