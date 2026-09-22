package ducklord

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestWorkspaceGeometryKeepsThreeRegionsAndNarrowFallback(t *testing.T) {
	wide := CalculateWorkspaceGeometry(120, 30, 4)
	if wide.Projects.Width <= 0 || wide.Quick.Width <= 0 || wide.Terminal.Width <= 0 ||
		wide.Terminal.X <= wide.Quick.X+wide.Quick.Width || wide.Quick.X != wide.Projects.X || wide.Quick.Y < wide.Projects.Y+wide.Projects.Height {
		t.Fatalf("three regions overlap: %+v", wide)
	}
	narrow := CalculateWorkspaceGeometry(50, 20, 4)
	if narrow.Projects.Width != 0 || narrow.Quick.Width != 0 || narrow.Terminal.Width != 50 {
		t.Fatalf("narrow fallback: %+v", narrow)
	}
}

func TestWorkspaceStackedGeometryStaysInsideScreen(t *testing.T) {
	for width := 0; width <= 125; width++ {
		for height := 0; height <= 35; height++ {
			g := CalculateWorkspaceGeometry(width, height, 4)
			for _, r := range []WorkspaceRect{g.Projects, g.Quick, g.Terminal} {
				if r.Width < 0 || r.Height < 0 {
					t.Fatalf("negative rect %+v", r)
				}
				if r.Width == 0 || r.Height == 0 {
					continue
				}
				if r.X < 1 || r.Y < 4 || r.X+r.Width-1 > width || r.Y+r.Height-1 > height {
					t.Fatalf("%dx%d: rect outside screen %+v", width, height, r)
				}
			}
			if g.Projects.Width > 0 {
				if g.Projects.X != g.Quick.X || g.Projects.Width != g.Quick.Width || g.Projects.Y+g.Projects.Height != g.Quick.Y || g.Quick.X+g.Quick.Width >= g.Terminal.X {
					t.Fatalf("stacked regions overlap: %+v", g)
				}
			}
		}
	}
}

func TestWorkspaceThemeFocusAndViewport(t *testing.T) {
	layout := NewProjectLayout()
	id := testLayoutIdentity("ABC123")
	if _, err := layout.Place(layout.Projects[0].ID, id, PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	nav, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectQuickSession(id); err != nil {
		t.Fatal(err)
	}
	g := CalculateWorkspaceGeometry(120, 25, 4)
	theme := WorkspaceTheme{FocusBackground: "#123456", FocusForeground: "#abcdef"}
	for _, focus := range []WorkspaceFocus{WorkspaceFocusProjects, WorkspaceFocusSessions, WorkspaceFocusTerminal} {
		var out bytes.Buffer
		RenderWorkspaceBodyWithOptions(&out, g, &layout, nav, nil, nil, func(_ SessionIdentity, w, h int) WorkspacePaneView {
			if w != g.Terminal.Width || h != g.Terminal.Height-2 {
				t.Fatalf("viewport changed: %dx%d", w, h)
			}
			return WorkspacePaneView{Title: "pane", Lines: []string{"\x1b[31mred\x1b[0m"}}
		}, WorkspaceRenderOptions{Focus: focus, Theme: theme})
		r := g.Projects
		title := " PROJECTS "
		if focus == WorkspaceFocusSessions {
			r, title = g.Quick, " SESSIONS "
		}
		if focus == WorkspaceFocusTerminal {
			r, title = g.Terminal, " "+layout.Projects[0].Name
		}
		start := fmt.Sprintf("\x1b[%d;%dH\x1b[0m", r.Y, r.X)
		at := strings.Index(out.String(), start)
		if at < 0 {
			t.Fatalf("missing heading position %q", start)
		}
		line := out.String()[at+len(start):]
		end := strings.Index(line, "\x1b[0m")
		if end < 0 || !strings.Contains(line[:end], "\x1b[48;2;18;52;86m") || !strings.Contains(line[:end], title) || !strings.HasSuffix(line[:end], " ") {
			t.Fatalf("focus heading missing full-width background: %q", line)
		}
		if !strings.Contains(out.String(), "\x1b[31mred") {
			t.Fatal("PTY styling lost")
		}
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

func TestWorkspaceNoteLeafVisibilityAndInvariant(t *testing.T) {
	note := &SessionPane{ID: "note", Note: true}
	if got := workspaceCollectVisiblePanes(note, WorkspaceRect{Width: 20, Height: 5}); len(got) != 0 {
		t.Fatalf("note exposed as PTY pane: %v", got)
	}
	if leaves, hidden := workspaceVisibleLeaves(note, WorkspaceRect{Width: 20, Height: 5}); len(leaves) != 0 || hidden != 0 {
		t.Fatalf("note visible session leaves: %v/%d", leaves, hidden)
	}
	if _, ok := workspaceFindVisiblePane(note, WorkspaceRect{Width: 20, Height: 5}, "note"); !ok {
		t.Fatal("note geometry not discoverable")
	}
	session := SessionIdentity{InstanceID: "i", SessionID: "s"}
	if err := (&SessionPane{ID: "bad", Session: &session, Note: true}).validate(map[string]bool{}, map[SessionIdentity]bool{}); err == nil {
		t.Fatal("mixed session/note leaf accepted")
	}
}

func TestWorkspaceNotesRendererHandlesTinyHeights(t *testing.T) {
	node := &SessionPane{ID: "notes", Note: true}
	notes := []NoteEntry{{Title: "First", Preview: "A short entry"}}
	for _, height := range []int{1, 2, 3} {
		var out bytes.Buffer
		renderWorkspaceNode(&out, node, WorkspaceRect{X: 1, Y: 1, Width: 40, Height: height}, "notes", nil, WorkspaceRenderOptions{
			Notes: notes, NoteIndex: 0, NoteScope: NotesProject,
		})
		if out.Len() == 0 {
			t.Fatalf("height %d rendered no output", height)
		}
		if strings.Contains(out.String(), "1/0") {
			t.Fatalf("height %d rendered invalid page count: %q", height, out.String())
		}
	}
}
