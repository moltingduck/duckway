package ducklord

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const DefaultTerminalScrollback = 10_000
const MaxTerminalRetainedCells = 250_000

type CellStyle struct {
	Foreground int32 `json:"fg,omitempty"`
	Background int32 `json:"bg,omitempty"`
	Bold       bool  `json:"bold,omitempty"`
	Dim        bool  `json:"dim,omitempty"`
	Italic     bool  `json:"italic,omitempty"`
	Underline  bool  `json:"underline,omitempty"`
	Inverse    bool  `json:"inverse,omitempty"`
}

type TerminalCell struct {
	Rune      rune      `json:"rune,omitempty"`
	Combining string    `json:"combining,omitempty"`
	Width     uint8     `json:"width,omitempty"`
	Style     CellStyle `json:"style,omitempty"`
}

type TerminalLine struct {
	Cells       []TerminalCell `json:"cells"`
	SoftWrapped bool           `json:"soft_wrapped,omitempty"`
}

type terminalScreen struct {
	Lines     []TerminalLine
	CursorRow int
	CursorCol int
	SavedRow  int
	SavedCol  int
}

type TerminalScreenState struct {
	Lines     []TerminalLine `json:"lines"`
	CursorRow int            `json:"cursor_row"`
	CursorCol int            `json:"cursor_col"`
	SavedRow  int            `json:"saved_row,omitempty"`
	SavedCol  int            `json:"saved_col,omitempty"`
}

type TerminalState struct {
	Rows          int                 `json:"rows"`
	Cols          int                 `json:"cols"`
	Scrollback    []TerminalLine      `json:"scrollback,omitempty"`
	Primary       TerminalScreenState `json:"primary"`
	Alternate     TerminalScreenState `json:"alternate"`
	UseAlternate  bool                `json:"use_alternate,omitempty"`
	Style         CellStyle           `json:"style,omitempty"`
	Autowrap      bool                `json:"autowrap"`
	ParserState   terminalParseState  `json:"parser_state,omitempty"`
	CSI           []byte              `json:"csi,omitempty"`
	UTF8Pending   []byte              `json:"utf8_pending,omitempty"`
	WrapPending   bool                `json:"wrap_pending,omitempty"`
	CursorVisible bool                `json:"cursor_visible"`
}

type terminalParseState uint8

const (
	terminalGround terminalParseState = iota
	terminalEscape
	terminalCSI
	terminalOSC
	terminalOSCEscape
	terminalDCS
	terminalDCSEscape
)

// Terminal is Ducklord's bounded, incremental VT framebuffer. It deliberately
// interprets terminal controls instead of retaining replayable raw PTY bytes.
type Terminal struct {
	Rows, Cols    int
	ScrollbackMax int
	Scrollback    []TerminalLine
	primary       terminalScreen
	alternate     terminalScreen
	useAlternate  bool
	style         CellStyle
	state         terminalParseState
	csi           []byte
	utf8Pending   []byte
	autowrap      bool
	wrapPending   bool
	cursorVisible bool
	retainedCells int
}

func NewTerminal(rows, cols, scrollback int) *Terminal {
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	if scrollback < 0 {
		scrollback = 0
	}
	t := &Terminal{Rows: rows, Cols: cols, ScrollbackMax: scrollback, autowrap: true, cursorVisible: true}
	t.primary = newTerminalScreen(rows, cols)
	t.alternate = newTerminalScreen(rows, cols)
	return t
}

func NewTerminalFromState(state TerminalState, scrollback int) (*Terminal, bool) {
	if state.Rows < 1 || state.Rows > 200 || state.Cols < 1 || state.Cols > 500 || state.ParserState > terminalDCSEscape || len(state.CSI) > 256 || len(state.UTF8Pending) > utf8.UTFMax || !validParserBuffer(state) || !validTerminalScreenState(state.Primary, state.Rows, state.Cols) || !validTerminalScreenState(state.Alternate, state.Rows, state.Cols) {
		return nil, false
	}
	if len(state.Scrollback) > DefaultTerminalScrollback {
		return nil, false
	}
	retainedCells := 2 * state.Rows * state.Cols
	for _, line := range state.Scrollback {
		if !validTerminalLine(line, state.Cols, false) {
			return nil, false
		}
		retainedCells += len(line.Cells)
	}
	if retainedCells > MaxTerminalRetainedCells {
		return nil, false
	}
	terminal := NewTerminal(state.Rows, state.Cols, scrollback)
	terminal.Scrollback = cloneTerminalLines(state.Scrollback)
	terminal.retainedCells = terminalCellCount(terminal.Scrollback)
	terminal.primary = screenFromState(state.Primary)
	terminal.alternate = screenFromState(state.Alternate)
	terminal.useAlternate = state.UseAlternate
	terminal.style = state.Style
	terminal.autowrap = state.Autowrap
	terminal.state = state.ParserState
	terminal.csi = append([]byte(nil), state.CSI...)
	terminal.utf8Pending = append([]byte(nil), state.UTF8Pending...)
	terminal.wrapPending = state.WrapPending
	terminal.cursorVisible = state.CursorVisible
	return terminal, true
}

func validParserBuffer(state TerminalState) bool {
	if state.ParserState != terminalCSI && len(state.CSI) != 0 || state.ParserState != terminalGround && len(state.UTF8Pending) != 0 {
		return false
	}
	for _, b := range state.CSI {
		if b < 0x20 || b > 0x3f {
			return false
		}
	}
	return utf8.Valid(state.UTF8Pending) || !utf8.FullRune(state.UTF8Pending)
}

func validTerminalScreenState(state TerminalScreenState, rows, cols int) bool {
	if len(state.Lines) != rows || state.CursorRow < 0 || state.CursorRow >= rows || state.CursorCol < 0 || state.CursorCol >= cols {
		return false
	}
	for _, line := range state.Lines {
		if !validTerminalLine(line, cols, true) {
			return false
		}
	}
	return true
}

func validTerminalLine(line TerminalLine, cols int, exact bool) bool {
	if len(line.Cells) > cols || exact && len(line.Cells) != cols {
		return false
	}
	for index, cell := range line.Cells {
		if cell.Width == 255 {
			if cell.Rune != 0 || cell.Combining != "" || index == 0 || line.Cells[index-1].Width != 2 {
				return false
			}
			continue
		}
		if cell.Width > 2 || cell.Rune == utf8.RuneError && cell.Width == 0 || cell.Rune != 0 && (cell.Rune < 0x20 || cell.Rune == 0x7f || cell.Rune >= 0x80 && cell.Rune <= 0x9f || !utf8.ValidRune(cell.Rune)) {
			return false
		}
		if cell.Rune != 0 && int(cell.Width) != runeCellWidth(cell.Rune) {
			return false
		}
		for _, r := range cell.Combining {
			if r == 0 || !utf8.ValidRune(r) || r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f || runeCellWidth(r) != 0 {
				return false
			}
		}
	}
	return true
}

func screenFromState(state TerminalScreenState) terminalScreen {
	return terminalScreen{Lines: cloneTerminalLines(state.Lines), CursorRow: state.CursorRow, CursorCol: state.CursorCol, SavedRow: state.SavedRow, SavedCol: state.SavedCol}
}

func (t *Terminal) SnapshotState() TerminalState {
	state := TerminalState{Rows: t.Rows, Cols: t.Cols, Scrollback: cloneTerminalLines(t.Scrollback), Primary: screenState(t.primary), Alternate: screenState(t.alternate),
		UseAlternate: t.useAlternate, Style: t.style, Autowrap: t.autowrap, ParserState: t.state, CSI: append([]byte(nil), t.csi...),
		UTF8Pending: append([]byte(nil), t.utf8Pending...), WrapPending: t.wrapPending}
	state.CursorVisible = t.cursorVisible
	return state
}

// Resize reflows primary soft-wrapped lines while preserving hard line
// boundaries. Alternate-screen content is cropped/padded because full-screen
// TUIs redraw after SIGWINCH.
func (t *Terminal) Resize(rows, cols int) {
	if rows < 1 || cols < 1 || rows == t.Rows && cols == t.Cols {
		return
	}
	primaryLines := append(cloneTerminalLines(t.Scrollback), cloneTerminalLines(t.primary.Lines)...)
	positions := []terminalPosition{
		{Line: len(t.Scrollback) + t.primary.CursorRow, Col: t.primary.CursorCol + boolInt(t.wrapPending)},
		{Line: len(t.Scrollback) + t.primary.SavedRow, Col: t.primary.SavedCol},
	}
	reflowed, mapped := reflowTerminalLinesAt(primaryLines, cols, positions)
	if len(reflowed) < rows {
		padding := make([]TerminalLine, rows-len(reflowed))
		for i := range padding {
			padding[i].Cells = make([]TerminalCell, cols)
		}
		reflowed = append(reflowed, padding...)
	}
	screenStart := maxInt(0, len(reflowed)-rows)
	newScrollback := cloneTerminalLines(reflowed[:screenStart])
	if len(newScrollback) > t.ScrollbackMax {
		newScrollback = newScrollback[len(newScrollback)-t.ScrollbackMax:]
	}
	t.Scrollback = newScrollback
	t.retainedCells = terminalCellCount(newScrollback)
	for len(t.Scrollback) > 0 && t.retainedCells+2*rows*cols > MaxTerminalRetainedCells {
		t.retainedCells -= len(t.Scrollback[0].Cells)
		t.Scrollback = t.Scrollback[1:]
	}
	t.primary.Lines = cloneTerminalLines(reflowed[screenStart:])
	t.primary.CursorRow = clampInt(mapped[0].Line-screenStart, 0, rows-1)
	t.primary.CursorCol = clampInt(mapped[0].Col, 0, cols-1)
	t.primary.SavedRow = clampInt(mapped[1].Line-screenStart, 0, rows-1)
	t.primary.SavedCol = clampInt(mapped[1].Col, 0, cols-1)
	t.alternate = resizeTerminalScreen(t.alternate, rows, cols)
	t.Rows, t.Cols = rows, cols
	t.wrapPending = mapped[0].Wrap
}

type terminalPosition struct {
	Line int
	Col  int
	Wrap bool
}

// reflowTerminalLinesAt reflows in display-glyph units. A width-two glyph and
// its continuation are never split across rows. Positions are translated from
// old physical cells to the corresponding cell in the new layout.
func reflowTerminalLinesAt(lines []TerminalLine, cols int, positions []terminalPosition) ([]TerminalLine, []terminalPosition) {
	var result []TerminalLine
	mapped := make([]terminalPosition, len(positions))
	for i := range mapped {
		mapped[i] = terminalPosition{Line: -1}
	}
	type glyph struct {
		cell  TerminalCell
		width int
	}
	var logical []glyph
	logicalWidth := 0
	groupPositions := make(map[int]int)
	flush := func() {
		keepWidth := 0
		for _, offset := range groupPositions {
			keepWidth = maxInt(keepWidth, offset)
		}
		for len(logical) > 0 && logical[len(logical)-1].cell.Rune == 0 && logical[len(logical)-1].cell.Combining == "" && logicalWidth-logical[len(logical)-1].width >= keepWidth {
			logicalWidth -= logical[len(logical)-1].width
			logical = logical[:len(logical)-1]
		}
		if len(logical) == 0 {
			row := len(result)
			result = append(result, TerminalLine{Cells: make([]TerminalCell, cols)})
			for index, offset := range groupPositions {
				mapped[index] = terminalPosition{Line: row, Col: clampInt(offset, 0, cols-1)}
			}
			return
		}
		line := TerminalLine{Cells: make([]TerminalCell, cols)}
		column, consumed := 0, 0
		for _, item := range logical {
			if item.width == 2 && column+2 > cols || column >= cols {
				line.SoftWrapped = true
				result = append(result, line)
				line = TerminalLine{Cells: make([]TerminalCell, cols)}
				column = 0
			}
			for index, offset := range groupPositions {
				if mapped[index].Line < 0 && offset >= consumed && offset < consumed+item.width {
					mapped[index] = terminalPosition{Line: len(result), Col: column}
				}
			}
			line.Cells[column] = item.cell
			line.Cells[column].Width = uint8(item.width)
			if item.width == 2 {
				line.Cells[column+1] = TerminalCell{Width: 255, Style: item.cell.Style}
			}
			column += item.width
			consumed += item.width
		}
		result = append(result, line)
		for index, offset := range groupPositions {
			if mapped[index].Line < 0 {
				wrap := offset == consumed && column == cols
				mapped[index] = terminalPosition{Line: len(result) - 1, Col: clampInt(offset-consumed+column, 0, cols-1), Wrap: wrap}
			}
		}
	}
	for lineIndex, line := range lines {
		for index, position := range positions {
			if position.Line == lineIndex {
				groupPositions[index] = logicalWidth + normalizedTerminalColumn(line, position.Col)
			}
		}
		for cellIndex := 0; cellIndex < len(line.Cells); cellIndex++ {
			cell := line.Cells[cellIndex]
			if cell.Width == 255 {
				continue
			}
			width := int(cell.Width)
			if width == 0 {
				width = 1
			}
			logical = append(logical, glyph{cell: cell, width: width})
			logicalWidth += width
		}
		if !line.SoftWrapped {
			flush()
			logical = logical[:0]
			logicalWidth = 0
			groupPositions = make(map[int]int)
		}
	}
	if len(logical) > 0 {
		flush()
	}
	return result, mapped
}

func normalizedTerminalColumn(line TerminalLine, column int) int {
	column = clampInt(column, 0, len(line.Cells))
	if column < len(line.Cells) && line.Cells[column].Width == 255 && column > 0 {
		return column - 1
	}
	return column
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func resizeTerminalScreen(screen terminalScreen, rows, cols int) terminalScreen {
	resized := newTerminalScreen(rows, cols)
	copyRows := minInt(rows, len(screen.Lines))
	for row := 0; row < copyRows; row++ {
		copy(resized.Lines[row].Cells, screen.Lines[row].Cells[:minInt(cols, len(screen.Lines[row].Cells))])
		repairWideLine(&resized.Lines[row])
		resized.Lines[row].SoftWrapped = screen.Lines[row].SoftWrapped
	}
	resized.CursorRow = clampInt(screen.CursorRow, 0, rows-1)
	resized.CursorCol = clampInt(screen.CursorCol, 0, cols-1)
	resized.SavedRow = clampInt(screen.SavedRow, 0, rows-1)
	resized.SavedCol = clampInt(screen.SavedCol, 0, cols-1)
	return resized
}

func screenState(screen terminalScreen) TerminalScreenState {
	return TerminalScreenState{Lines: cloneTerminalLines(screen.Lines), CursorRow: screen.CursorRow, CursorCol: screen.CursorCol, SavedRow: screen.SavedRow, SavedCol: screen.SavedCol}
}

func cloneTerminalLines(lines []TerminalLine) []TerminalLine {
	cloned := make([]TerminalLine, len(lines))
	for i, line := range lines {
		cloned[i] = cloneTerminalLine(line)
	}
	return cloned
}

func newTerminalScreen(rows, cols int) terminalScreen {
	lines := make([]TerminalLine, rows)
	for row := range lines {
		lines[row].Cells = make([]TerminalCell, cols)
	}
	return terminalScreen{Lines: lines}
}

func (t *Terminal) screen() *terminalScreen {
	if t.useAlternate {
		return &t.alternate
	}
	return &t.primary
}

func (t *Terminal) Write(data []byte) {
	for _, b := range data {
		t.writeByte(b)
	}
}

func (t *Terminal) writeByte(b byte) {
	switch t.state {
	case terminalGround:
		if b == 0x1b {
			t.flushInvalidUTF8()
			t.state = terminalEscape
			return
		}
		if b < 0x20 || b == 0x7f {
			t.flushInvalidUTF8()
			t.control(b)
			return
		}
		t.utf8Pending = append(t.utf8Pending, b)
		for len(t.utf8Pending) > 0 {
			if !utf8.FullRune(t.utf8Pending) {
				return
			}
			r, size := utf8.DecodeRune(t.utf8Pending)
			if r == utf8.RuneError && size == 1 {
				t.putRune(utf8.RuneError)
				t.utf8Pending = t.utf8Pending[1:]
				continue
			}
			t.putRune(r)
			t.utf8Pending = t.utf8Pending[size:]
		}
	case terminalEscape:
		switch b {
		case '[':
			t.csi = t.csi[:0]
			t.state = terminalCSI
		case ']':
			t.state = terminalOSC
		case 'P', '^', '_':
			t.state = terminalDCS
		case '7':
			t.saveCursor()
			t.state = terminalGround
		case '8':
			t.restoreCursor()
			t.state = terminalGround
		case 'D':
			t.lineFeed(false)
			t.state = terminalGround
		case 'E':
			t.lineFeed(false)
			t.screen().CursorCol = 0
			t.state = terminalGround
		case 'M':
			t.reverseIndex()
			t.state = terminalGround
		case 'c':
			t.reset()
		default:
			t.state = terminalGround
		}
	case terminalCSI:
		if b >= 0x40 && b <= 0x7e {
			t.executeCSI(b, string(t.csi))
			t.csi = t.csi[:0]
			t.state = terminalGround
		} else if len(t.csi) < 256 {
			t.csi = append(t.csi, b)
		} else {
			t.state = terminalGround
		}
	case terminalOSC:
		switch b {
		case '\a':
			t.state = terminalGround
		case 0x1b:
			t.state = terminalOSCEscape
		}
	case terminalOSCEscape:
		if b == '\\' || b == '\a' {
			t.state = terminalGround
		} else {
			t.state = terminalOSC
		}
	case terminalDCS:
		if b == 0x1b {
			t.state = terminalDCSEscape
		}
	case terminalDCSEscape:
		if b == '\\' {
			t.state = terminalGround
		} else {
			t.state = terminalDCS
		}
	}
}

func (t *Terminal) flushInvalidUTF8() {
	if len(t.utf8Pending) > 0 {
		t.putRune(utf8.RuneError)
		t.utf8Pending = t.utf8Pending[:0]
	}
}

func (t *Terminal) control(b byte) {
	switch b {
	case '\b':
		if t.screen().CursorCol > 0 {
			t.screen().CursorCol--
		}
		t.wrapPending = false
	case '\t':
		next := (t.screen().CursorCol/8 + 1) * 8
		if next >= t.Cols {
			next = t.Cols - 1
		}
		t.screen().CursorCol = next
	case '\n', '\v', '\f':
		t.lineFeed(false)
	case '\r':
		t.screen().CursorCol = 0
		t.wrapPending = false
	}
}

func (t *Terminal) putRune(r rune) {
	width := runeCellWidth(r)
	if width == 0 {
		t.appendCombining(r)
		return
	}
	if t.wrapPending && t.autowrap {
		t.lineFeed(true)
		t.screen().CursorCol = 0
	}
	s := t.screen()
	if width == 2 && s.CursorCol == t.Cols-1 {
		if t.autowrap {
			t.lineFeed(true)
			s.CursorCol = 0
		} else {
			width = 1
		}
	}
	if s.CursorRow < 0 || s.CursorRow >= len(s.Lines) || s.CursorCol < 0 || s.CursorCol >= t.Cols {
		return
	}
	line := &s.Lines[s.CursorRow]
	line.Cells[s.CursorCol] = TerminalCell{Rune: r, Width: uint8(width), Style: t.style}
	if width == 2 && s.CursorCol+1 < t.Cols {
		line.Cells[s.CursorCol+1] = TerminalCell{Width: 255, Style: t.style}
	}
	s.CursorCol += width
	if s.CursorCol >= t.Cols {
		s.CursorCol = t.Cols - 1
		t.wrapPending = true
	}
}

func (t *Terminal) appendCombining(r rune) {
	s := t.screen()
	col := s.CursorCol - 1
	if t.wrapPending {
		col = s.CursorCol
	}
	for col >= 0 && s.Lines[s.CursorRow].Cells[col].Width == 255 {
		col--
	}
	if col >= 0 {
		cell := &s.Lines[s.CursorRow].Cells[col]
		if len(cell.Combining) < 64 {
			cell.Combining += string(r)
		}
	}
}

func (t *Terminal) lineFeed(soft bool) {
	s := t.screen()
	if s.CursorRow < t.Rows-1 {
		s.Lines[s.CursorRow].SoftWrapped = soft
		s.CursorRow++
		t.wrapPending = false
		return
	}
	s.Lines[s.CursorRow].SoftWrapped = soft
	if !t.useAlternate && t.ScrollbackMax > 0 {
		line := cloneTerminalLine(s.Lines[0])
		for len(line.Cells) > 0 {
			cell := line.Cells[len(line.Cells)-1]
			if cell.Rune != 0 || cell.Width == 255 || cell.Combining != "" {
				break
			}
			line.Cells = line.Cells[:len(line.Cells)-1]
		}
		t.Scrollback = append(t.Scrollback, line)
		t.retainedCells += len(line.Cells)
		for len(t.Scrollback) > t.ScrollbackMax || t.retainedCells+2*t.Rows*t.Cols > MaxTerminalRetainedCells {
			t.retainedCells -= len(t.Scrollback[0].Cells)
			t.Scrollback = t.Scrollback[1:]
		}
	}
	copy(s.Lines, s.Lines[1:])
	s.Lines[t.Rows-1] = TerminalLine{Cells: make([]TerminalCell, t.Cols)}
	t.wrapPending = false
}

func terminalCellCount(lines []TerminalLine) int {
	total := 0
	for _, line := range lines {
		total += len(line.Cells)
	}
	return total
}

func cloneTerminalLine(line TerminalLine) TerminalLine {
	return TerminalLine{Cells: append([]TerminalCell(nil), line.Cells...), SoftWrapped: line.SoftWrapped}
}

func (t *Terminal) reverseIndex() {
	s := t.screen()
	if s.CursorRow > 0 {
		s.CursorRow--
		return
	}
	copy(s.Lines[1:], s.Lines[:len(s.Lines)-1])
	s.Lines[0] = TerminalLine{Cells: make([]TerminalCell, t.Cols)}
}

func (t *Terminal) executeCSI(final byte, raw string) {
	private := strings.HasPrefix(raw, "?")
	if private {
		raw = strings.TrimPrefix(raw, "?")
	}
	params := parseCSIParams(raw)
	first := csiParam(params, 0, 1)
	s := t.screen()
	switch final {
	case 'A':
		s.CursorRow = maxInt(0, s.CursorRow-first)
	case 'B', 'e':
		s.CursorRow = minInt(t.Rows-1, s.CursorRow+first)
	case 'C', 'a':
		s.CursorCol = minInt(t.Cols-1, s.CursorCol+first)
	case 'D':
		s.CursorCol = maxInt(0, s.CursorCol-first)
	case 'E':
		s.CursorRow = minInt(t.Rows-1, s.CursorRow+first)
		s.CursorCol = 0
	case 'F':
		s.CursorRow = maxInt(0, s.CursorRow-first)
		s.CursorCol = 0
	case 'G', '`':
		s.CursorCol = clampInt(first-1, 0, t.Cols-1)
	case 'H', 'f':
		s.CursorRow = clampInt(csiParam(params, 0, 1)-1, 0, t.Rows-1)
		s.CursorCol = clampInt(csiParam(params, 1, 1)-1, 0, t.Cols-1)
	case 'J':
		t.eraseDisplay(csiParam(params, 0, 0))
	case 'K':
		t.eraseLine(csiParam(params, 0, 0))
	case 'm':
		t.applySGR(params)
	case 's':
		t.saveCursor()
	case 'u':
		t.restoreCursor()
	case 'h', 'l':
		enabled := final == 'h'
		if private {
			for _, param := range params {
				switch param {
				case 7:
					t.autowrap = enabled
				case 25:
					t.cursorVisible = enabled
				case 47, 1047, 1049:
					t.setAlternate(enabled, param == 1049)
				}
			}
		}
	case '@':
		t.insertChars(first)
	case 'P':
		t.deleteChars(first)
	case 'X':
		t.eraseChars(first)
	case 'L':
		t.insertLines(first)
	case 'M':
		t.deleteLines(first)
	}
	t.wrapPending = false
}

func parseCSIParams(raw string) []int {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ";")
	params := make([]int, len(parts))
	for i, part := range parts {
		if part == "" {
			params[i] = 0
			continue
		}
		value, err := strconv.Atoi(part)
		if err == nil && value >= 0 && value <= 1_000_000 {
			params[i] = value
		}
	}
	return params
}

func csiParam(params []int, index, fallback int) int {
	if index >= len(params) || params[index] == 0 {
		return fallback
	}
	return params[index]
}

func (t *Terminal) eraseDisplay(mode int) {
	s := t.screen()
	switch mode {
	case 0:
		t.eraseLine(0)
		for row := s.CursorRow + 1; row < t.Rows; row++ {
			s.Lines[row] = TerminalLine{Cells: make([]TerminalCell, t.Cols)}
		}
	case 1:
		t.eraseLine(1)
		for row := 0; row < s.CursorRow; row++ {
			s.Lines[row] = TerminalLine{Cells: make([]TerminalCell, t.Cols)}
		}
	case 2, 3:
		for row := range s.Lines {
			s.Lines[row] = TerminalLine{Cells: make([]TerminalCell, t.Cols)}
		}
		if mode == 3 {
			t.Scrollback = nil
		}
	}
}

func (t *Terminal) eraseLine(mode int) {
	s := t.screen()
	line := &s.Lines[s.CursorRow]
	start, end := s.CursorCol, t.Cols
	switch mode {
	case 1:
		start, end = 0, s.CursorCol+1
	case 2:
		start, end = 0, t.Cols
	}
	start, end = expandWideRange(line.Cells, start, end)
	for col := start; col < end; col++ {
		line.Cells[col] = TerminalCell{}
	}
}

func (t *Terminal) eraseChars(count int) {
	s := t.screen()
	line := &s.Lines[s.CursorRow]
	start, end := expandWideRange(line.Cells, s.CursorCol, minInt(t.Cols, s.CursorCol+count))
	for col := start; col < end; col++ {
		s.Lines[s.CursorRow].Cells[col] = TerminalCell{}
	}
}

func (t *Terminal) insertChars(count int) {
	s := t.screen()
	line := &s.Lines[s.CursorRow]
	count = minInt(count, t.Cols-s.CursorCol)
	copy(line.Cells[s.CursorCol+count:], line.Cells[s.CursorCol:t.Cols-count])
	for col := s.CursorCol; col < s.CursorCol+count; col++ {
		line.Cells[col] = TerminalCell{}
	}
	repairWideLine(line)
}

func (t *Terminal) deleteChars(count int) {
	s := t.screen()
	line := &s.Lines[s.CursorRow]
	count = minInt(count, t.Cols-s.CursorCol)
	copy(line.Cells[s.CursorCol:], line.Cells[s.CursorCol+count:])
	for col := t.Cols - count; col < t.Cols; col++ {
		line.Cells[col] = TerminalCell{}
	}
	repairWideLine(line)
}

func expandWideRange(cells []TerminalCell, start, end int) (int, int) {
	if start > 0 && start < len(cells) && cells[start].Width == 255 {
		start--
	}
	if end > 0 && end <= len(cells) && cells[end-1].Width == 2 {
		end = minInt(len(cells), end+1)
	}
	return start, end
}

func repairWideLine(line *TerminalLine) {
	for col := 0; col < len(line.Cells); col++ {
		cell := &line.Cells[col]
		switch cell.Width {
		case 2:
			if col+1 >= len(line.Cells) {
				*cell = TerminalCell{}
				continue
			}
			line.Cells[col+1] = TerminalCell{Width: 255, Style: cell.Style}
			col++
		case 255:
			*cell = TerminalCell{}
		}
	}
}

func (t *Terminal) insertLines(count int) {
	s := t.screen()
	count = minInt(count, t.Rows-s.CursorRow)
	copy(s.Lines[s.CursorRow+count:], s.Lines[s.CursorRow:t.Rows-count])
	for row := s.CursorRow; row < s.CursorRow+count; row++ {
		s.Lines[row] = TerminalLine{Cells: make([]TerminalCell, t.Cols)}
	}
}

func (t *Terminal) deleteLines(count int) {
	s := t.screen()
	count = minInt(count, t.Rows-s.CursorRow)
	copy(s.Lines[s.CursorRow:], s.Lines[s.CursorRow+count:])
	for row := t.Rows - count; row < t.Rows; row++ {
		s.Lines[row] = TerminalLine{Cells: make([]TerminalCell, t.Cols)}
	}
}

func (t *Terminal) applySGR(params []int) {
	if len(params) == 0 {
		params = []int{0}
	}
	for i := 0; i < len(params); i++ {
		param := params[i]
		switch {
		case param == 0:
			t.style = CellStyle{}
		case param == 1:
			t.style.Bold = true
		case param == 2:
			t.style.Dim = true
		case param == 3:
			t.style.Italic = true
		case param == 4:
			t.style.Underline = true
		case param == 7:
			t.style.Inverse = true
		case param == 22:
			t.style.Bold, t.style.Dim = false, false
		case param == 23:
			t.style.Italic = false
		case param == 24:
			t.style.Underline = false
		case param == 27:
			t.style.Inverse = false
		case param >= 30 && param <= 37:
			t.style.Foreground = int32(param - 29)
		case param == 39:
			t.style.Foreground = 0
		case param >= 40 && param <= 47:
			t.style.Background = int32(param - 39)
		case param == 49:
			t.style.Background = 0
		case param >= 90 && param <= 97:
			t.style.Foreground = int32(param - 81)
		case param >= 100 && param <= 107:
			t.style.Background = int32(param - 91)
		case (param == 38 || param == 48) && i+2 < len(params) && params[i+1] == 5:
			value := int32(1000 + params[i+2])
			if param == 38 {
				t.style.Foreground = value
			} else {
				t.style.Background = value
			}
			i += 2
		case (param == 38 || param == 48) && i+4 < len(params) && params[i+1] == 2:
			value := int32(0x1000000 + (params[i+2]&255)<<16 + (params[i+3]&255)<<8 + (params[i+4] & 255))
			if param == 38 {
				t.style.Foreground = value
			} else {
				t.style.Background = value
			}
			i += 4
		}
	}
}

func (t *Terminal) saveCursor() {
	s := t.screen()
	s.SavedRow, s.SavedCol = s.CursorRow, s.CursorCol
}

func (t *Terminal) restoreCursor() {
	s := t.screen()
	s.CursorRow = clampInt(s.SavedRow, 0, t.Rows-1)
	s.CursorCol = clampInt(s.SavedCol, 0, t.Cols-1)
}

func (t *Terminal) setAlternate(enabled, saveCursor bool) {
	if enabled == t.useAlternate {
		return
	}
	if enabled {
		if saveCursor {
			t.saveCursor()
		}
		t.alternate = newTerminalScreen(t.Rows, t.Cols)
		t.useAlternate = true
	} else {
		t.useAlternate = false
		if saveCursor {
			t.restoreCursor()
		}
	}
	t.wrapPending = false
}

func (t *Terminal) reset() {
	t.primary = newTerminalScreen(t.Rows, t.Cols)
	t.alternate = newTerminalScreen(t.Rows, t.Cols)
	t.Scrollback = nil
	t.useAlternate = false
	t.style = CellStyle{}
	t.autowrap = true
	t.cursorVisible = true
	t.wrapPending = false
}

func (t *Terminal) Text() string {
	lines := make([]string, 0, len(t.Scrollback)+t.Rows)
	if !t.useAlternate {
		for _, line := range t.Scrollback {
			lines = append(lines, terminalLineText(line))
		}
	}
	for _, line := range t.screen().Lines {
		lines = append(lines, terminalLineText(line))
	}
	minimumLines := t.screen().CursorRow + 1
	if !t.useAlternate {
		minimumLines += len(t.Scrollback)
	}
	for len(lines) > minimumLines && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// RenderLines returns safe, self-contained ANSI rows. Control sequences are
// generated only from validated style fields; terminal-provided escapes never
// pass through this method.
func (t *Terminal) RenderLines(maxRows, maxCols int) []string {
	lines := make([]TerminalLine, 0, len(t.Scrollback)+len(t.screen().Lines))
	if !t.useAlternate {
		lines = append(lines, t.Scrollback...)
	}
	lines = append(lines, t.screen().Lines...)
	if maxRows > 0 && len(lines) > maxRows {
		lines = lines[len(lines)-maxRows:]
	}
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		var rendered strings.Builder
		current := CellStyle{}
		visible := 0
		for _, cell := range line.Cells {
			if cell.Width == 255 {
				continue
			}
			width := int(cell.Width)
			if width == 0 {
				width = 1
			}
			if maxCols > 0 && visible+width > maxCols {
				break
			}
			if cell.Style != current {
				rendered.WriteString(styleSequence(cell.Style))
				current = cell.Style
			}
			r := cell.Rune
			if r == 0 {
				r = ' '
			}
			rendered.WriteRune(r)
			rendered.WriteString(cell.Combining)
			visible += width
		}
		if current != (CellStyle{}) {
			rendered.WriteString("\x1b[0m")
		}
		result = append(result, strings.TrimRight(rendered.String(), " "))
	}
	return result
}

// CursorPosition returns the zero-based cursor within the same tail viewport
// used by RenderLines. False means the application hid the cursor or the cursor
// is outside the cropped viewport.
func (t *Terminal) CursorPosition(maxRows, maxCols int) (int, int, bool) {
	if !t.cursorVisible {
		return 0, 0, false
	}
	total := len(t.screen().Lines)
	globalRow := t.screen().CursorRow
	if !t.useAlternate {
		total += len(t.Scrollback)
		globalRow += len(t.Scrollback)
	}
	start := 0
	if maxRows > 0 && total > maxRows {
		start = total - maxRows
	}
	row := globalRow - start
	col := t.screen().CursorCol
	if row < 0 || maxRows > 0 && row >= maxRows || col < 0 || maxCols > 0 && col >= maxCols {
		return 0, 0, false
	}
	return row, col, true
}

func styleSequence(style CellStyle) string {
	params := []string{"0"}
	if style.Bold {
		params = append(params, "1")
	}
	if style.Dim {
		params = append(params, "2")
	}
	if style.Italic {
		params = append(params, "3")
	}
	if style.Underline {
		params = append(params, "4")
	}
	if style.Inverse {
		params = append(params, "7")
	}
	params = appendColorParams(params, style.Foreground, true)
	params = appendColorParams(params, style.Background, false)
	return "\x1b[" + strings.Join(params, ";") + "m"
}

func appendColorParams(params []string, color int32, foreground bool) []string {
	base, bright, extended := 30, 90, 38
	if !foreground {
		base, bright, extended = 40, 100, 48
	}
	switch {
	case color >= 1 && color <= 8:
		return append(params, strconv.Itoa(base+int(color)-1))
	case color >= 9 && color <= 16:
		return append(params, strconv.Itoa(bright+int(color)-9))
	case color >= 1000 && color <= 1255:
		return append(params, strconv.Itoa(extended), "5", strconv.Itoa(int(color)-1000))
	case color >= 0x1000000 && color <= 0x1ffffff:
		rgb := int(color - 0x1000000)
		return append(params, strconv.Itoa(extended), "2", fmt.Sprint((rgb>>16)&255), fmt.Sprint((rgb>>8)&255), fmt.Sprint(rgb&255))
	default:
		return params
	}
}

func terminalLineText(line TerminalLine) string {
	runes := make([]rune, 0, len(line.Cells))
	last := -1
	for _, cell := range line.Cells {
		if cell.Width == 255 {
			continue
		}
		r := cell.Rune
		if r == 0 {
			r = ' '
		}
		runes = append(runes, r)
		runes = append(runes, []rune(cell.Combining)...)
		if r != ' ' {
			last = len(runes) - 1
		}
	}
	if last < 0 {
		return ""
	}
	return string(runes[:last+1])
}

func runeCellWidth(r rune) int {
	if r == 0 || r >= 0x300 && r <= 0x36f || r >= 0xfe00 && r <= 0xfe0f {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a || r >= 0x2e80 && r <= 0xa4cf ||
		r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f ||
		r >= 0xff00 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 || r >= 0x1f300 && r <= 0x1faff ||
		r >= 0x20000 && r <= 0x3fffd) {
		return 2
	}
	return 1
}

func clampInt(value, low, high int) int { return minInt(high, maxInt(low, value)) }
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
