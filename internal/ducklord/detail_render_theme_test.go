package ducklord

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestDetailedSessionThemeFocusPreservesViewport(t *testing.T) {
	id := testLayoutIdentity("ABC123")
	items := []DetailedSessionItem{{Identity: id, Name: "session", Host: "host"}}
	g := CalculateDetailGeometry(100, 20, 4)
	for _, focus := range []WorkspaceFocus{WorkspaceFocusSessions, WorkspaceFocusTerminal} {
		var out bytes.Buffer
		RenderDetailedSessionBodyWithOptions(&out, g, items, id, "", DetailFilter("all"), false,
			func(_ SessionIdentity, width, height int) WorkspacePaneView {
				if width != g.Pane.Width || height != g.Pane.Height-1 {
					t.Fatalf("viewport changed %dx%d", width, height)
				}
				return WorkspacePaneView{Title: "session", Lines: []string{"\x1b[31mred\x1b[0m"}}
			}, WorkspaceRenderOptions{Focus: focus, Theme: WorkspaceTheme{FocusBackground: "#123456", Separator: "#aabbcc"}})
		r := g.List
		if focus == WorkspaceFocusTerminal {
			r = g.Pane
		}
		start := fmt.Sprintf("\x1b[%d;%dH\x1b[0m", r.Y, r.X)
		at := strings.Index(out.String(), start)
		if at < 0 {
			t.Fatal("missing focused heading")
		}
		line := out.String()[at+len(start):]
		end := strings.Index(line, "\x1b[0m")
		if end < 0 || !strings.Contains(line[:end], "\x1b[48;2;18;52;86m") {
			t.Fatal("missing focus color")
		}
		if !strings.Contains(out.String(), "\x1b[38;2;170;187;204m│") || !strings.Contains(out.String(), "\x1b[31mred") {
			t.Fatal("separator or PTY styling lost")
		}
	}
}

func TestDetailedSessionOneRowEmptyPreviewStaysBounded(t *testing.T) {
	var out bytes.Buffer
	RenderDetailedSessionBody(&out, CalculateDetailGeometry(20, 4, 4), nil, SessionIdentity{}, "", DetailFilter("all"), false, nil)
	if strings.Contains(out.String(), "\x1b[5;") {
		t.Fatal("empty preview wrote below its last row")
	}
}

func TestDetailSessionIndexAtMatchesClippedCards(t *testing.T) {
	r := WorkspaceRect{X: 2, Y: 4, Width: 25, Height: 7}
	for _, tc := range []struct{ x, y, want int }{{2, 4, -1}, {2, 5, -1}, {2, 6, 4}, {26, 9, 4}, {2, 10, 5}, {2, 11, -1}, {1, 6, -1}, {27, 6, -1}} {
		if got := DetailSessionIndexAt(r, 4, 10, tc.x, tc.y); got != tc.want {
			t.Fatalf("(%d,%d) got %d want %d", tc.x, tc.y, got, tc.want)
		}
	}
	if got := DetailSessionIndexAt(r, 9, 10, 2, 10); got != -1 {
		t.Fatalf("empty last row index %d", got)
	}
	r.Height = 3
	if got := DetailSessionIndexAt(r, 7, 10, 2, 6); got != 7 {
		t.Fatalf("partial card index %d", got)
	}
	r.Width, r.Height = 45, 8
	if got := DetailSessionIndexAt(r, 4, 10, 2, 9); got != 4 {
		t.Fatalf("wide card index %d", got)
	}
}
