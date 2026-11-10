package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestFrameOutputColorModes(t *testing.T) {
	frame := "\033[?25l\033[H\033[2J\033[1;31mred\033[0m\nplain\033[38;2;1;2;3mx\033[m"
	cases := []struct {
		name, noColor, term string
		set                 bool
		wantColor           bool // colour params present
		wantAttrs           bool // bold SGR present
	}{
		{"unset", "", "xterm", false, true, true},
		{"no_color", "1", "xterm", true, false, true},
		{"no_color_empty", "", "xterm", true, true, true},
		{"dumb", "", "dumb", false, false, false},
		{"dumb_upper", "", "DUMB", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			if tc.set {
				t.Setenv("NO_COLOR", tc.noColor)
			} else {
				t.Setenv("NO_COLOR", "x")
				unsetEnv(t, "NO_COLOR")
			}
			var output frameOutput
			var out strings.Builder
			output.write(&out, frame, 20, 4)
			got := out.String()
			if hasColor := strings.Contains(got, "31") || strings.Contains(got, "38;2"); hasColor != tc.wantColor {
				t.Fatalf("colour present=%v want %v: %q", hasColor, tc.wantColor, got)
			}
			if hasBold := strings.Contains(got, "\033[1m") || strings.Contains(got, "\033[1;"); hasBold != tc.wantAttrs {
				t.Fatalf("bold present=%v want %v: %q", hasBold, tc.wantAttrs, got)
			}
			for _, keep := range []string{"\033[?2026h", "\033[?2026l", "\033[H", "\033[2J", "\033[?25l", "red", "plain"} {
				if !strings.Contains(got, keep) {
					t.Fatalf("missing %q in %q", keep, got)
				}
			}
		})
	}
}

func TestFrameOutputNoColorKeepsIncrementalCursorMoves(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var output frameOutput
	var out strings.Builder
	output.write(&out, "\033[H\033[2J\033[31ma\033[0m\nb", 10, 3)
	out.Reset()
	output.write(&out, "\033[H\033[2J\033[32mz\033[0m\nb", 10, 3)
	got := out.String()
	if strings.Contains(got, "\033[31m") || strings.Contains(got, "\033[32m") || !strings.Contains(got, "\033[1;1H") || !strings.Contains(got, "z") {
		t.Fatalf("incremental update wrong: %q", got)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestEmptySessionGuidance(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}}
	var out bytes.Buffer
	s.render(&out)
	got := out.String()
	want := "No hosts configured. Press " + s.cfg.Shortcut("host_add") + " to add a host"
	if !strings.Contains(got, want) || !strings.Contains(got, s.cfg.Shortcut("help")+" for help") {
		t.Fatalf("no-host guidance missing: %q", got)
	}
	s = &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "h"}}}}
	out.Reset()
	s.render(&out)
	got = out.String()
	for _, w := range []string{"No sessions yet. Press " + s.cfg.Shortcut("session_create") + " to create a session", s.cfg.Shortcut("host_add") + " to add a host", s.cfg.Shortcut("help") + " for help"} {
		if !strings.Contains(got, w) {
			t.Fatalf("session guidance missing %q: %q", w, got)
		}
	}
	if strings.Contains(got, "No hosts configured") {
		t.Fatal("host variant shown with a configured host")
	}
}

func TestSearchModalShowsMatchCountsAndTypeHint(t *testing.T) {
	sessions := []ducklord.RemoteSession{{Client: "h", Name: "alpha"}, {Client: "h", Name: "beta"}, {Client: "h", Name: "alps"}}
	for _, tc := range []struct{ query, want string }{
		{"", "3 of 3 sessions match"},
		{"al", "2 of 3 sessions match"},
		{"beta", "1 match (of 3 sessions)"},
		{"zzz", "0 matches (of 3 sessions)"},
	} {
		s := &tuiState{searchMode: true, searchQuery: tc.query, sessions: sessions}
		var out bytes.Buffer
		s.renderSearchModal(&out, 100, 30)
		got := out.String()
		if !strings.Contains(got, tc.want) {
			t.Fatalf("query %q missing %q: %q", tc.query, tc.want, got)
		}
		if !strings.Contains(got, "Type to filter · ↑/↓ select · Enter activate · Esc close") {
			t.Fatalf("footer missing hint: %q", got)
		}
	}
}

func TestDetailPTYRectMatchesRenderedContentRows(t *testing.T) {
	state, _, first, _ := workspacePaneTestState(t)
	id, _ := ducklord.IdentityFromSession(first)
	state.workspacePreview = true
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	nav.EnterDetail()
	state.detailSelected = id
	if err := nav.PreviewDetail(id); err != nil {
		t.Fatal(err)
	}
	for _, size := range []struct {
		width, height int
		hidden        bool
	}{{30, 24, true}, {120, 24, false}} {
		g := ducklord.CalculateDetailGeometry(size.width, size.height, 4)
		if (g.List.Width == 0) != size.hidden {
			t.Fatalf("%dx%d hidden=%v", size.width, size.height, g.List.Width == 0)
		}
		rect, err := state.workspacePaneRectAt(size.width, size.height)
		if err != nil {
			t.Fatal(err)
		}
		wantOffset := 0
		if size.hidden {
			wantOffset = 1
		}
		if rect.Y != g.Pane.Y+wantOffset || rect.Height-1 != g.ContentRows() || g.ContentRows() != g.Pane.Height-1-wantOffset {
			t.Fatalf("%dx%d rect=%+v pane=%+v rows=%d", size.width, size.height, rect, g.Pane, g.ContentRows())
		}
	}
}

func TestDropSGRColorRewritesParameters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[0m", "\x1b[0m"},
		{"\x1b[m", "\x1b[m"},
		{"\x1b[31m", ""},
		{"\x1b[1;31;7m", "\x1b[1;7m"},
		{"\x1b[38;5;196m", ""},
		{"\x1b[48;2;1;2;3m", ""},
		{"\x1b[1;38;5;196;4m", "\x1b[1;4m"},
		{"\x1b[7;48;2;1;2;3;1m", "\x1b[7;1m"},
		{"\x1b[38;5m", ""},
		{"\x1b[1;38;5m", "\x1b[1m"},
		{"\x1b[1;38;2;1;2m", "\x1b[1m"},
		{"\x1b[1;38m", "\x1b[1m"},
		{"\x1b[38;9;7m", "\x1b[9;7m"},
		{"\x1b[0;31m", "\x1b[0m"},
		{"\x1b[;31m", "\x1b[m"},
		{"\x1b[1;;31m", "\x1b[1;m"},
		{"\x1b[90;97;100;107;39;49m", ""},
		{"\x1b[22;27;53m", "\x1b[22;27;53m"},
		{"\x1b[58;5;1;4;59m", "\x1b[4m"},
		{"\x1b[1;38:2::1:2:3;7m", "\x1b[1;7m"},
		{"\x1b[108m", "\x1b[108m"},
	}
	for _, tc := range cases {
		if got := dropSGRColor(tc.in); got != tc.want {
			t.Errorf("dropSGRColor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestApplyFrameColorModeKeepsControls(t *testing.T) {
	frame := "\x1b[?2026h\x1b[H\x1b[2J\x1b[3;4H\x1b[?25l\x1b[7;31mX\x1b[0m\x1b[?2026l"
	if got := applyFrameColorMode(frame, frameColorFull); got != frame {
		t.Fatalf("full mode changed frame: %q", got)
	}
	drop := applyFrameColorMode(frame, frameColorDrop)
	want := "\x1b[?2026h\x1b[H\x1b[2J\x1b[3;4H\x1b[?25l\x1b[7mX\x1b[0m\x1b[?2026l"
	if drop != want {
		t.Fatalf("drop mode = %q, want %q", drop, want)
	}
	strip := applyFrameColorMode(frame, frameColorStrip)
	want = "\x1b[?2026h\x1b[H\x1b[2J\x1b[3;4H\x1b[?25lX\x1b[?2026l"
	if strip != want {
		t.Fatalf("strip mode = %q, want %q", strip, want)
	}
}

func TestTerminalColorModeSelection(t *testing.T) {
	for _, tc := range []struct {
		term, noColor string
		want          frameColorMode
	}{
		{"xterm", "", frameColorFull},
		{"xterm", "1", frameColorDrop},
		{"dumb", "", frameColorStrip},
		{"dumb", "1", frameColorStrip},
	} {
		t.Setenv("TERM", tc.term)
		t.Setenv("NO_COLOR", tc.noColor)
		if got := terminalColorMode(); got != tc.want {
			t.Errorf("TERM=%q NO_COLOR=%q mode=%v want %v", tc.term, tc.noColor, got, tc.want)
		}
	}
}
