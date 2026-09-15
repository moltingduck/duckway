package main

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

var frameStylePattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// frameOutput remembers only successfully published screen cells. Each renderer
// still composes a complete frame; terminal updates replace changed rows in place.
type frameOutput struct {
	rows          []string
	width, height int
	cursor        string
}

func (f *frameOutput) write(out io.Writer, frame string, width, height int) {
	if width < 1 || height < 1 {
		return
	}
	screen := ducklord.NewTerminal(height, width, 0)
	// The UI uses line-oriented output; model the host terminal's CR/LF mapping.
	screen.Write([]byte(strings.ReplaceAll(frame, "\n", "\r\n")))
	rows := screen.RenderLines(height, width)
	row, col, visible := screen.CursorPosition(height, width)
	cursor := "\033[?25l"
	if visible {
		cursor = fmt.Sprintf("\033[%d;%dH\033[?25h", row+1, col+1)
	}
	reset := f.width != width || f.height != height || len(f.rows) != len(rows)
	var body strings.Builder
	if reset {
		body.WriteString("\033[H\033[2J")
	}
	for i, line := range rows {
		if !reset && line == f.rows[i] {
			continue
		}
		fmt.Fprintf(&body, "\033[%d;1H\033[0m%s%s\033[0m", i+1, line, strings.Repeat(" ", max(0, width-modalCellWidth(frameStylePattern.ReplaceAllString(line, "")))))
	}
	if body.Len() == 0 && cursor == f.cursor {
		return
	}
	// Disable wrapping while painting the bottom-right cell to avoid scrolling.
	data := "\033[?2026h\033[?25l\033[?7l" + body.String() + "\033[?7h" + cursor + "\033[?2026l"
	if reset {
		data = "\033[?2026h" + frame + "\033[?2026l"
	}
	n, err := io.WriteString(out, data)
	if err != nil || n != len(data) {
		*f = frameOutput{}
		return
	}
	f.rows, f.width, f.height, f.cursor = rows, width, height, cursor
}
