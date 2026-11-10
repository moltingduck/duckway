package ducklord

import (
	"bytes"
	"strings"
	"testing"
)

func hiddenListGeometry(height int) DetailGeometry {
	return DetailGeometry{Pane: WorkspaceRect{X: 1, Y: 3, Width: 30, Height: height}}
}

func TestDetailHiddenListShowsHintAndShiftsPane(t *testing.T) {
	id := testLayoutIdentity("ABC123")
	items := []DetailedSessionItem{{Identity: id, Name: "s", Host: "h"}}
	g := hiddenListGeometry(6)
	var out bytes.Buffer
	var gotH int
	RenderDetailedSessionBody(&out, g, items, id, "", DetailFilter("all"), false,
		func(_ SessionIdentity, w, h int) WorkspacePaneView {
			gotH = h
			return WorkspacePaneView{Title: "t", Lines: []string{"first", "second"}}
		})
	s := out.String()
	if gotH != g.Pane.Height-2 {
		t.Fatalf("pane height %d want %d", gotH, g.Pane.Height-2)
	}
	if !strings.Contains(s, "\x1b[4;1H\x1b[0m\x1b[2m Resize to show list") {
		t.Fatalf("hint not on row 1: %q", s)
	}
	if !strings.Contains(s, "\x1b[5;1H\x1b[0mfirst") && !strings.Contains(s, "\x1b[5;1H") {
		t.Fatalf("content not shifted: %q", s)
	}
	if i, j := strings.Index(s, "first"), strings.Index(s, "\x1b[5;1H"); j < 0 || i < j {
		t.Fatalf("first line not at row 2: %q", s)
	}
	if strings.Contains(s, "\x1b[9;") {
		t.Fatalf("wrote below pane: %q", s)
	}
}

func TestDetailVisibleListHasNoHint(t *testing.T) {
	var out bytes.Buffer
	RenderDetailedSessionBody(&out, CalculateDetailGeometry(100, 20, 4), nil, SessionIdentity{}, "", DetailFilter("all"), false, nil)
	if strings.Contains(out.String(), "Resize to show list") {
		t.Fatal("hint shown with a visible list")
	}
}

func TestDetailHiddenListNoSelectionShiftsMessage(t *testing.T) {
	var out bytes.Buffer
	RenderDetailedSessionBody(&out, hiddenListGeometry(5), nil, SessionIdentity{}, "", DetailFilter("all"), false, nil)
	s := out.String()
	hint, msg := strings.Index(s, "Resize to show list"), strings.Index(s, "No matching Sessions")
	if hint < 0 || msg < 0 || hint > msg {
		t.Fatalf("hint/message order wrong: %q", s)
	}
	if !strings.Contains(s, "\x1b[4;1H\x1b[0m\x1b[2m Resize") || !strings.Contains(s, "\x1b[5;1H\x1b[0m\x1b[2mNo matching") {
		t.Fatalf("rows wrong: %q", s)
	}
}

func TestDetailHiddenListTinyHeights(t *testing.T) {
	id := testLayoutIdentity("ABC123")
	items := []DetailedSessionItem{{Identity: id, Name: "s", Host: "h"}}
	for _, selected := range []SessionIdentity{{}, id} {
		// Height 1: no hint, nothing below the pane.
		var out bytes.Buffer
		RenderDetailedSessionBody(&out, hiddenListGeometry(1), items, selected, "", DetailFilter("all"), false, nil)
		if strings.Contains(out.String(), "Resize") || strings.Contains(out.String(), "\x1b[4;") {
			t.Fatalf("height 1 wrote extra rows: %q", out.String())
		}
		// Height 2: hint takes the only content row; no message/content below.
		out.Reset()
		var gotH = -1
		RenderDetailedSessionBody(&out, hiddenListGeometry(2), items, selected, "", DetailFilter("all"), false,
			func(_ SessionIdentity, _, h int) WorkspacePaneView {
				gotH = h
				return WorkspacePaneView{Lines: []string{"zzz"}}
			})
		s := out.String()
		if !strings.Contains(s, "Resize to show list") || strings.Contains(s, "\x1b[5;") || strings.Contains(s, "zzz") || strings.Contains(s, "No matching") || strings.Contains(s, "Select a Session") {
			t.Fatalf("height 2 wrong: %q", s)
		}
		if selected == id && gotH != 0 {
			t.Fatalf("pane height %d want 0", gotH)
		}
	}
}
