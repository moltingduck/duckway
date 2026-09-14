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
