package main

import (
	"strconv"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) selectListRowForMouse(seq string) (bool, bool) {
	selected, ok := s.listRowForMouse(seq)
	if !ok {
		return false, false
	}
	if selected.isGroup {
		s.selectedGroupID = selected.groupID
		return false, true
	}
	s.selectedGroupID = ""
	s.selected = selected.sessionIndex
	return true, true
}

func (s *tuiState) listRowForMouse(seq string) (sessionListRow, bool) {
	if !strings.HasPrefix(seq, "\x1b[<") || len(seq) < 7 {
		return sessionListRow{}, false
	}
	parts := strings.FieldsFunc(strings.TrimPrefix(seq, "\x1b[<"), func(r rune) bool { return r == ';' || r == 'M' || r == 'm' })
	if len(parts) != 3 {
		return sessionListRow{}, false
	}
	x, err := strconv.Atoi(parts[1])
	if err != nil {
		return sessionListRow{}, false
	}
	width, _ := terminalSize()
	layout := calculateTUILayout(width, s.focused, s.listPaneWidth, s.autoHideList)
	if !layout.showList || x > layout.menuWidth+2 {
		return sessionListRow{}, false
	}
	y, err := strconv.Atoi(parts[2])
	if err != nil {
		return sessionListRow{}, false
	}
	row := y - 5
	rows := s.sessionListRows()
	if row < 0 || row >= len(rows) {
		return sessionListRow{}, false
	}
	return rows[row], true
}

// sessionIndexForMouse remains the non-mutating compatibility helper used by
// tests and callers that only care about session rows.
func (s *tuiState) sessionIndexForMouse(seq string) (int, bool) {
	oldSelected, oldGroup := s.selected, s.selectedGroupID
	session, ok := s.selectListRowForMouse(seq)
	index := s.selected
	s.selected, s.selectedGroupID = oldSelected, oldGroup
	return index, ok && session
}

func (s *tuiState) contentPaneClicked(seq string) bool {
	if !strings.HasPrefix(seq, "\x1b[<0;") || !strings.HasSuffix(seq, "M") {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b[<0;"), "M"), ";")
	if len(parts) != 2 {
		return false
	}
	x, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	y, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	width, height := terminalSize()
	layout := calculateTUILayout(width, s.focused, s.listPaneWidth, s.autoHideList)
	contentStart := 1
	if layout.showList {
		contentStart = layout.menuWidth + 3
	}
	return x >= contentStart && x <= width && y >= 4 && y <= height
}

func parseSGRMouse(seq string) (button, x, y int, ok bool) {
	if len(seq) > 64 || !strings.HasPrefix(seq, "\x1b[<") || len(seq) < 7 || seq[len(seq)-1] != 'M' && seq[len(seq)-1] != 'm' {
		return 0, 0, 0, false
	}
	parts := strings.Split(seq[3:len(seq)-1], ";")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	values := []*int{&button, &x, &y}
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 10000 {
			return 0, 0, 0, false
		}
		*values[index] = value
	}
	return button, x, y, true
}

func (s *tuiState) contentPanePoint(x, y int) bool {
	width, height := terminalSize()
	if s.workspacePreview {
		pane := ducklord.CalculateWorkspaceGeometry(width, height, 4).Terminal
		return x >= pane.X && x < pane.X+pane.Width && y >= pane.Y+2 && y < pane.Y+pane.Height
	}
	layout := calculateTUILayout(width, s.focused, s.listPaneWidth, s.autoHideList)
	contentStart := 1
	if layout.showList {
		contentStart = layout.menuWidth + 3
	}
	return x >= contentStart && x <= width && y >= 5 && y <= height
}
