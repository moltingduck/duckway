package main

import (
	"io"
	"regexp"
	"strings"
)

type modalMouseAction struct {
	selection *int
	index     int
	key       string
	before    func()
}

type modalMouseRegion struct {
	left, right, row int
	action           modalMouseAction
}

func (s *tuiState) resetModalMouse() {
	s.modalMouseRegions = nil
	s.modalMouseLines = make(map[int]modalMouseAction)
}

func (s *tuiState) modalChoice(line int, selection *int, index int, key string) {
	s.modalMouseLines[line] = modalMouseAction{selection: selection, index: index, key: key}
}

func (s *tuiState) createMouseRegion(left, row, width, index int) {
	if index < 0 || s.newSessionDiscovering || s.newSessionStarting {
		return
	}
	a := modalMouseAction{selection: &s.newSessionSelected, index: index, key: "\r"}
	if s.newSessionStep == "path" {
		a.selection = &s.newSessionPathSelected
		a.before = func() { s.newSessionPathCompletion = "" }
	} else {
		a.before = func() { s.newSessionLine = "" }
	}
	s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left, left + width - 1, row, a})
}

// modalMouseInput translates a visible control into the existing keyboard path.
// In particular, selecting a destructive operation still opens its confirmation.
func (s *tuiState) modalMouseHit(x, y int) bool {
	for _, region := range s.modalMouseRegions {
		if y == region.row && x >= region.left && x <= region.right {
			return true
		}
	}
	return false
}

func (s *tuiState) modalMouseInput(x, y int) []byte {
	for _, region := range s.modalMouseRegions {
		if y != region.row || x < region.left || x > region.right {
			continue
		}
		a := region.action
		if a.selection != nil {
			*a.selection = a.index
		}
		if a.before != nil {
			a.before()
		}
		if s.projectFiles.open {
			if a.selection == &s.projectFiles.left.selected {
				s.projectFiles.active = 0
				s.projectFiles.dragSource = 0
			} else if a.selection == &s.projectFiles.right.selected {
				s.projectFiles.active = 1
				s.projectFiles.dragSource = 1
			} else {
				s.projectFiles.dragSource = s.projectFiles.active
			}
			s.projectFiles.dragSourceIndex = a.index
			s.projectFiles.dragArmed = true
		}
		return []byte(a.key)
	}
	return nil
}

// Only explicit keyboard hints become buttons. Explanatory text and input
// values cannot accidentally become activation targets.
var modalHintKey = regexp.MustCompile(`(^|[ ·])((?:Ctrl\+C|Enter/Space|Enter|Esc|Space|s|w|f|y|n))(?:[ /·]|$)`)

func (s *tuiState) modalHintRegions(text string, left, row, width int) {
	display := modalDisplayText(text)
	for _, match := range modalHintKey.FindAllStringSubmatchIndex(display, -1) {
		start, end := match[4], match[5]
		key := display[start:end]
		column := modalCellWidth(display[:start])
		if column+modalCellWidth(key) > width {
			continue
		}
		switch key {
		case "Enter", "Enter/Space":
			key = "\r"
		case "Esc":
			key = "\x1b"
		case "Ctrl+C":
			key = "\x03"
		case "Space":
			key = " "
		}
		s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left + column, left + column + modalCellWidth(display[start:end]) - 1, row, modalMouseAction{key: key}})
	}
}

func (s *tuiState) renderModalBox(out io.Writer, cols, rows int, lines []modalRenderLine) {
	s.modalMouseRegions = nil
	if cols >= 8 && rows >= 3 && len(lines) > 0 {
		visible := min(len(lines), rows-2)
		width := min(72, max(8, cols-2))
		top, left := max(1, (rows-visible-2)/2+1), max(1, (cols-width)/2+1)
		for i, line := range lines[:visible] {
			if action, ok := s.modalMouseLines[i]; ok && line.style != modalDisabled {
				s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left + 1, left + width - 2, top + i + 1, action})
			} else if (!s.helpMode || s.blockingModalOpen()) && line.style == modalMuted && (strings.Contains(line.text, "Enter") || strings.Contains(line.text, "Esc")) {
				s.modalHintRegions(line.text, left+1, top+i+1, width-2)
			}
		}
	}
	renderModalBox(out, cols, rows, lines)
}
