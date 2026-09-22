package ducklord

import (
	"strings"
	"testing"
)

func TestTerminalFloodRetainsCompactIndependentHistory(t *testing.T) {
	term := NewTerminal(24, 120, 128)
	term.Write([]byte(strings.Repeat("y\r\n", 100_000)))
	if len(term.Scrollback) != 128 {
		t.Fatalf("history rows = %d", len(term.Scrollback))
	}
	for _, line := range term.Scrollback {
		if len(line.Cells) != 1 || cap(line.Cells) > 2 || line.Cells[0].Rune != 'y' {
			t.Fatalf("short history retained full row or was overwritten: len=%d cap=%d", len(line.Cells), cap(line.Cells))
		}
	}
	term.Write([]byte("\033[2J\033[HRECOVERED"))
	if !strings.Contains(strings.Join(term.RenderLines(24, 120), "\n"), "RECOVERED") {
		t.Fatal("parser did not recover after flood")
	}
	for _, line := range term.Scrollback {
		if line.Cells[0].Rune != 'y' {
			t.Fatal("live cells alias history")
		}
	}
}

func BenchmarkTerminalYesFlood(b *testing.B) {
	term := NewTerminal(24, 120, DefaultTerminalScrollback)
	data := []byte(strings.Repeat("y\r\n", 4096))
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		term.Write(data)
	}
}
