package ducklord

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTerminalInterpretsCursorEraseAndChunkedUTF8(t *testing.T) {
	terminal := NewTerminal(4, 12, 10)
	terminal.Write([]byte("hello\nworld"))
	terminal.Write([]byte("\x1b[2;1H\x1b[2Kduck"))
	zh := []byte("中文")
	terminal.Write(zh[:2])
	terminal.Write(zh[2:])
	if got := terminal.Text(); got != "hello\nduck中文" {
		t.Fatalf("text=%q", got)
	}
	if terminal.screen().CursorCol != 8 {
		t.Fatalf("wide cursor=%d", terminal.screen().CursorCol)
	}
}

func TestTerminalAlternateScreenRestoresPrimary(t *testing.T) {
	terminal := NewTerminal(3, 20, 10)
	terminal.Write([]byte("primary"))
	terminal.Write([]byte("\x1b[?1049halt\x1b[?1049l"))
	if terminal.useAlternate || terminal.Text() != "primary" {
		t.Fatalf("alternate=%v text=%q", terminal.useAlternate, terminal.Text())
	}
}

func TestTerminalSGRAndBoundedScrollback(t *testing.T) {
	terminal := NewTerminal(2, 8, 2)
	terminal.Write([]byte("\x1b[1;31mred\x1b[0m\n2\n3\n4"))
	if len(terminal.Scrollback) != 2 {
		t.Fatalf("scrollback=%d", len(terminal.Scrollback))
	}
	cell := terminal.Scrollback[0].Cells[0]
	if cell.Rune == 0 {
		t.Fatalf("empty styled scrollback: %+v", terminal.Scrollback)
	}
}

func TestTerminalIgnoresOSCAndDCSWithoutLeakingPayload(t *testing.T) {
	terminal := NewTerminal(2, 40, 0)
	terminal.Write([]byte("before\x1b]52;c;secret\aafter\x1bPprivate\x1b\\done"))
	if got := terminal.Text(); got != "beforeafterdone" {
		t.Fatalf("text=%q", got)
	}
}

func TestTerminalResizeReflowsSoftWrapButKeepsHardBreak(t *testing.T) {
	terminal := NewTerminal(3, 5, 10)
	terminal.Write([]byte("abcdefgh\r\nnext"))
	terminal.Resize(4, 4)
	if got := terminal.Text(); got != "abcd\nefgh\nnext" {
		t.Fatalf("reflowed=%q", got)
	}
}

func TestTerminalResizeKeepsWideGlyphWholeAndMapsCursor(t *testing.T) {
	terminal := NewTerminal(3, 5, 10)
	terminal.Write([]byte("ab中c"))
	terminal.Resize(4, 4)
	if got := terminal.Text(); got != "ab中\nc" {
		t.Fatalf("narrow text=%q", got)
	}
	for _, line := range terminal.primary.Lines {
		if !validTerminalLine(line, 4, true) {
			t.Fatalf("invalid narrow line: %+v", line)
		}
	}
	terminal.Write([]byte("!"))
	if got := terminal.Text(); got != "ab中\nc!" {
		t.Fatalf("cursor did not remain after content: %q", got)
	}
	terminal.Resize(3, 6)
	if got := terminal.Text(); got != "ab中c!" {
		t.Fatalf("wide text after expansion=%q", got)
	}
}

func TestTerminalWideCellEditsPreservePairs(t *testing.T) {
	tests := []struct {
		name string
		seq  string
	}{
		{name: "erase continuation", seq: "\x1b[1;3H\x1b[X"},
		{name: "delete base", seq: "\x1b[1;2H\x1b[P"},
		{name: "insert before base", seq: "\x1b[1;2H\x1b[@"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			terminal := NewTerminal(2, 8, 0)
			terminal.Write([]byte("a中bc" + test.seq))
			for _, line := range terminal.primary.Lines {
				if !validTerminalLine(line, 8, true) {
					t.Fatalf("invalid line after %q: %+v", test.seq, line)
				}
			}
		})
	}
}

func TestTerminalStateRoundTrip(t *testing.T) {
	terminal := NewTerminal(3, 12, 10)
	terminal.Write([]byte("\x1b[32m中文 ok\x1b[0m"))
	restored, ok := NewTerminalFromState(terminal.SnapshotState(), 10)
	if !ok || restored.Text() != terminal.Text() || restored.screen().CursorCol != terminal.screen().CursorCol {
		t.Fatalf("restored ok=%v text=%q cursor=%d", ok, restored.Text(), restored.screen().CursorCol)
	}
}

func TestTerminalNoWrapWideRuneAtRightEdgeRoundTrips(t *testing.T) {
	for _, value := range []string{"界", "🙂"} {
		terminal := NewTerminal(2, 4, 10)
		terminal.Write([]byte("\x1b[?7l\x1b[1;4H" + value))
		restored, ok := NewTerminalFromState(terminal.SnapshotState(), 10)
		if !ok {
			t.Fatalf("framebuffer containing clipped %q did not round-trip", value)
		}
		if got := restored.primary.Lines[0].Cells[3]; got.Rune != utf8.RuneError || got.Width != 1 {
			t.Fatalf("clipped cell for %q = %+v", value, got)
		}
	}
}

func TestTerminalGlyphOverwritePreservesWideCellPairs(t *testing.T) {
	tests := []struct {
		name string
		seq  string
		want string
	}{
		{name: "narrow over wide lead", seq: "a中bc\x1b[1;2HA", want: "aA bc"},
		{name: "narrow over wide continuation", seq: "a中bc\x1b[1;3HA", want: "a Abc"},
		{name: "wide over prior continuation", seq: "a中bc\x1b[1;3H界", want: "a 界c"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			terminal := NewTerminal(2, 8, 0)
			terminal.Write([]byte(test.seq))
			restored, ok := NewTerminalFromState(terminal.SnapshotState(), 0)
			if !ok {
				t.Fatalf("framebuffer did not round-trip: %+v", terminal.primary.Lines[0])
			}
			if got := restored.Text(); got != test.want {
				t.Fatalf("text=%q want=%q", got, test.want)
			}
		})
	}
}

func TestTerminalStateRoundTripContinuesPartialUTF8AndCSI(t *testing.T) {
	t.Run("utf8", func(t *testing.T) {
		terminal := NewTerminal(2, 20, 0)
		encoded := []byte("中")
		terminal.Write(encoded[:2])
		restored, ok := NewTerminalFromState(terminal.SnapshotState(), 0)
		if !ok {
			t.Fatal("partial UTF-8 state rejected")
		}
		restored.Write(encoded[2:])
		if got := restored.Text(); got != "中" {
			t.Fatalf("continued utf8=%q", got)
		}
	})
	t.Run("csi", func(t *testing.T) {
		terminal := NewTerminal(2, 20, 0)
		terminal.Write([]byte("\x1b[31"))
		restored, ok := NewTerminalFromState(terminal.SnapshotState(), 0)
		if !ok {
			t.Fatal("partial CSI state rejected")
		}
		restored.Write([]byte("mred"))
		if got := restored.RenderLines(2, 20)[0]; !strings.Contains(got, "\x1b[0;31mred") {
			t.Fatalf("continued csi=%q", got)
		}
	})
}

func TestTerminalCursorPositionTracksViewportAndVisibility(t *testing.T) {
	terminal := NewTerminal(2, 8, 4)
	terminal.Write([]byte("one\r\ntwo\r\nabc"))
	row, col, visible := terminal.CursorPosition(2, 8)
	if !visible || row != 1 || col != 3 {
		t.Fatalf("cursor=(%d,%d,%v)", row, col, visible)
	}
	terminal.Write([]byte("\x1b[?25l"))
	if _, _, visible := terminal.CursorPosition(2, 8); visible {
		t.Fatal("hidden application cursor was exposed")
	}
}

func TestTerminalRenderLinesRecreatesStylesWithoutPayloadEscapes(t *testing.T) {
	terminal := NewTerminal(2, 40, 0)
	terminal.Write([]byte("\x1b[1;38;5;123;48;2;1;2;3mstyled\x1b[0m plain"))
	rendered := strings.Join(terminal.RenderLines(2, 40), "\n")
	for _, want := range []string{"\x1b[0;1;38;5;123;48;2;1;2;3m", "styled", "\x1b[0m", " plain"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered=%q missing=%q", rendered, want)
		}
	}
}

func TestTerminalRenderLinesOffsetInspectsScrollback(t *testing.T) {
	terminal := NewTerminal(2, 20, 20)
	terminal.Write([]byte("one\r\ntwo\r\nthree\r\nfour"))
	live := strings.Join(terminal.RenderLinesOffset(2, 20, 0), "\n")
	older := strings.Join(terminal.RenderLinesOffset(2, 20, 2), "\n")
	if !strings.Contains(live, "four") || strings.Contains(older, "four") || !strings.Contains(older, "one") {
		t.Fatalf("live=%q older=%q", live, older)
	}
}
