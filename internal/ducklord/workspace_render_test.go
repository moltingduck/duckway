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
	wide := WorkspaceGeometry{Terminal: WorkspaceRect{X: 1, Y: 1, Width: 70, Height: 6}}
	if got := WorkspaceVisibleSessions(&layout, nav, wide); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("restored split not visible: %+v", got)
	}
	if focused, err := nav.FocusVisiblePane(wide); err != nil || focused != b {
		t.Fatalf("visible pane focus=%+v err=%v", focused, err)
	}
}
