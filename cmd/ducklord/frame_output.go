package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

var frameStylePattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// frameSGRPattern matches an SGR sequence, including colon sub-parameters.
var frameSGRPattern = regexp.MustCompile(`\x1b\[([0-9;:]*)m`)

type frameColorMode int

const (
	frameColorFull frameColorMode = iota
	frameColorStrip
	frameColorDrop
)

// terminalColorMode reports how SGR styling must be rewritten. TERM=dumb has no
// SGR support, so every sequence goes. A present, non-empty NO_COLOR (see
// no-color.org) removes colour only; bold, reverse video, and similar
// attributes stay so selection and highlight remain visible.
func terminalColorMode() frameColorMode {
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return frameColorStrip
	}
	if os.Getenv("NO_COLOR") != "" {
		return frameColorDrop
	}
	return frameColorFull
}

// dropSGRColor rewrites one SGR sequence, removing colour parameters (30-39,
// 40-49, 90-97, 100-107, 58/59 and extended 38/48/58 with their sub-parameters).
// Empty parameters mean 0. A truncated extended colour drops its tail. When no
// parameter remains the sequence is dropped, except a bare ESC[m reset.
func dropSGRColor(sequence string) string {
	match := frameSGRPattern.FindStringSubmatch(sequence)
	if match == nil {
		return sequence
	}
	if match[1] == "" {
		return sequence
	}
	params := strings.Split(match[1], ";")
	kept := make([]string, 0, len(params))
	for i := 0; i < len(params); i++ {
		param := params[i]
		first := param
		if cut := strings.IndexByte(param, ':'); cut >= 0 {
			first = param[:cut]
			if first == "38" || first == "48" || first == "58" {
				continue
			}
		}
		n := 0
		if first != "" {
			var err error
			if n, err = strconv.Atoi(first); err != nil {
				kept = append(kept, param)
				continue
			}
		}
		switch {
		case n == 38 || n == 48 || n == 58:
			if i+1 < len(params) {
				switch params[i+1] {
				case "5":
					i += 2
				case "2":
					i += 4
				}
			}
			if i >= len(params) {
				i = len(params)
			}
		case n == 59, n >= 30 && n <= 39, n >= 40 && n <= 49, n >= 90 && n <= 97, n >= 100 && n <= 107:
		default:
			kept = append(kept, param)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(kept, ";") + "m"
}

func applyFrameColorMode(data string, mode frameColorMode) string {
	switch mode {
	case frameColorStrip:
		return frameStylePattern.ReplaceAllString(data, "")
	case frameColorDrop:
		return frameSGRPattern.ReplaceAllStringFunc(data, dropSGRColor)
	}
	return data
}

// frameOutput remembers only successfully published screen cells. Each renderer
// still composes a complete frame; terminal updates replace changed rows in place.
type frameOutput struct {
	rows          []string
	width, height int
	cursor        string
	frame         string
}

func (f *frameOutput) write(out io.Writer, frame string, width, height int) {
	if width < 1 || height < 1 || f.width == width && f.height == height && f.frame == frame {
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
		f.frame = frame
		return
	}
	// Disable wrapping while painting the bottom-right cell to avoid scrolling.
	data := "\033[?2026h\033[?25l\033[?7l" + body.String() + "\033[?7h" + cursor + "\033[?2026l"
	if reset {
		data = "\033[?2026h" + frame + "\033[?2026l"
	}
	// Only SGR styling is rewritten; cursor movement, screen clearing, and
	// synchronized-update controls remain available to the TUI.
	data = applyFrameColorMode(data, terminalColorMode())
	n, err := io.WriteString(out, data)
	if err != nil || n != len(data) {
		*f = frameOutput{}
		return
	}
	f.rows, f.width, f.height, f.cursor, f.frame = rows, width, height, cursor, frame
}
