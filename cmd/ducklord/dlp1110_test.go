package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestFrameOutputStripsSGRWhenColorDisabled(t *testing.T) {
	frame := "\033[?25l\033[H\033[2J\033[1;31mred\033[0m\nplain\033[38;2;1;2;3mx\033[m"
	cases := []struct {
		name, noColor, term string
		set                 bool
		strip               bool
	}{
		{"unset", "", "xterm", false, false},
		{"no_color", "1", "xterm", true, true},
		{"no_color_empty", "", "xterm", true, true},
		{"dumb", "", "dumb", false, true},
		{"dumb_upper", "", "DUMB", false, true},
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
			hasSGR := frameStylePattern.MatchString(got)
			if hasSGR == tc.strip {
				t.Fatalf("strip=%v but SGR present=%v: %q", tc.strip, hasSGR, got)
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
	if frameStylePattern.MatchString(got) || !strings.Contains(got, "\033[1;1H") || !strings.Contains(got, "z") {
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
