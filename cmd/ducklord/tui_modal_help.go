package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) renderHelpModal(out io.Writer, cols, rows int) {
	if !s.helpMode || s.blockingModalOpen() {
		return
	}
	s.resetModalMouse()
	entries := helpCatalog(s.workspacePreview)
	query := strings.ToLower(strings.TrimSpace(s.helpSearchQuery))
	results := []modalRenderLine{}
	helpMouseActions := make(map[string]string)
	category := ""
	categoryEntries := []modalRenderLine{}
	flushCategory := func() {
		if len(categoryEntries) == 0 {
			return
		}
		results = append(results, modalRenderLine{modalStatus, "  " + category})
		results = append(results, categoryEntries...)
		categoryEntries = nil
	}
	for _, entry := range entries {
		if entry.category != "" {
			flushCategory()
			category = entry.category
		}
		shortcut, keyInput := "", ""
		if strings.HasPrefix(entry.action, "prefix+") {
			suffix := strings.TrimPrefix(entry.action, "prefix+")
			displaySuffix := suffix
			switch suffix {
			case "slash":
				displaySuffix = "/"
			case "space":
				displaySuffix = "Space"
			case "up":
				displaySuffix = "↑"
			case "down":
				displaySuffix = "↓"
			case "left":
				displaySuffix = "←"
			case "right":
				displaySuffix = "→"
			}
			quickSequence := suffix == "--" || suffix == "\\\\" || suffix == "tt"
			if quickSequence {
				displaySuffix = suffix
			}
			shortcut = s.cfg.Shortcut("pane_prefix") + " " + displaySuffix
			if suffix == "slash" {
				keyInput = shortcutInput(s.cfg.Shortcut("pane_prefix")) + "/"
			} else if suffix == "space" {
				keyInput = shortcutInput(s.cfg.Shortcut("pane_prefix")) + " "
			} else if quickSequence {
				keyInput = shortcutInput(s.cfg.Shortcut("pane_prefix")) + suffix
			} else {
				keyInput = shortcutInput(s.cfg.Shortcut("pane_prefix")) + shortcutInput(suffix)
			}
		} else if entry.action != "" {
			shortcut = s.cfg.Shortcut(entry.action)
			keyInput = shortcutInput(shortcut)
		}
		searchText := strings.ToLower(category + " " + entry.action + " " + entry.label + " " + entry.detail + " " + shortcut)
		if query == "" || strings.Contains(searchText, query) {
			text := "  " + fmt.Sprintf("%-14s %s", shortcut, entry.label)
			if entry.detail != "" {
				text += " · " + entry.detail
			}
			style := modalMuted
			if entry.action != "" && keyInput != "" {
				style = modalInput
				available := s.helpActionAvailable(entry.action)
				if s.helpOriginFocused && s.helpFocusRestorePending {
					available = s.helpActionAvailableFromTerminalOrigin(entry.action)
				}
				if available {
					style = modalSelected
				}
			}
			wrapped := wrapHelpEntry(text, min(68, max(1, cols-4)), "                  ")
			for _, line := range wrapped {
				categoryEntries = append(categoryEntries, modalRenderLine{style, line})
			}
			quickSequence := entry.action == "prefix+--" || entry.action == "prefix+\\\\" || entry.action == "prefix+tt"
			if keyInput != "" && !quickSequence && s.helpActionAvailable(entry.action) && strings.HasPrefix(entry.action, "prefix+") {
				helpMouseActions[wrapped[0]] = keyInput
			}
		}
	}
	flushCategory()
	mouseHelp := "Click a prefix shortcut to send its sequence · drag reorder · right-click focus/toggle"
	if s.workspacePreview {
		mouseHelp = "Click a prefix shortcut to send its sequence · right-click target: config · drag Session into Terminal: add pane"
	}
	if query == "" || strings.Contains(strings.ToLower("mouse "+mouseHelp), query) {
		results = append(results, modalRenderLine{modalStatus, "  MOUSE"})
		results = append(results, helpWrappedLines("  "+mouseHelp, min(68, max(1, cols-4)), "  ")...)
	}
	if query == "" || strings.Contains("modals choose enter confirm esc back ctrl+c close", query) {
		results = append(results, modalRenderLine{modalStatus, "  MODAL CONTROLS"})
		results = append(results, helpWrappedLines("  ↑/↓ choose · Enter confirm · Esc back · Ctrl+C close; forms may use Tab to move fields", min(68, max(1, cols-4)), "  ")...)
	}
	if query == "" || strings.Contains("host connect disconnect reconnect sessions notifications", query) {
		results = append(results, modalRenderLine{modalMuted, "  Host connect/disconnect/reconnect affects all host sessions and notifications"})
	}
	if len(results) == 0 {
		results = append(results, modalRenderLine{modalMuted, "  No matching shortcuts"})
	}
	helpKey := s.cfg.Shortcut("help")
	searchHint := "  / search · " + helpKey + " close help"
	if s.helpSearchActive {
		searchHint = "  Search: " + s.helpSearchQuery + "_ · ↑/↓ browse · Esc clear"
	} else if s.helpSearchQuery != "" {
		searchHint = "  Filter: " + s.helpSearchQuery + "  (/ edit · search then Esc clears)"
	}
	maxVisible := max(1, rows-5)
	s.helpResultTotal = len(results)
	s.helpOffset = min(max(s.helpOffset, 0), max(0, len(results)-maxVisible))
	lines := []modalRenderLine{{modalTitle, "  Keyboard shortcuts and feature guide"}, {modalInput, searchHint}}
	lines = append(lines, results[s.helpOffset:min(len(results), s.helpOffset+maxVisible)]...)
	footer := "  Pinned · / search · Enter pins search · " + helpKey + " close"
	if s.helpSearchQuery != "" {
		footer = "  Filtered · / edit · search then Esc clears · " + helpKey + " close"
	}
	lines = append(lines, modalRenderLine{modalMuted, footer})
	for i, line := range lines {
		if key := helpMouseActions[line.text]; key != "" {
			s.modalChoice(i, nil, 0, key)
			a := s.modalMouseLines[i]
			a.before = func() { s.helpSearchActive = false }
			s.modalMouseLines[i] = a
		}
	}
	if !s.helpSearchActive {
		s.modalChoice(1, nil, 0, "/")
	}
	s.renderModalBox(out, cols, rows, lines)
	if len(results) > maxVisible && cols >= 8 && rows >= 8 {
		boxWidth := min(72, max(8, cols-2))
		left := max(1, (cols-boxWidth)/2+1)
		top := max(1, (rows-min(len(lines), rows-2)-2)/2+1)
		trackX, trackY := left+boxWidth-2, top+3
		thumbHeight := max(1, maxVisible*maxVisible/len(results))
		maxOffset := len(results) - maxVisible
		thumbTop := trackY + (maxVisible-thumbHeight)*s.helpOffset/maxOffset
		for row := 0; row < maxVisible; row++ {
			glyph, color := "│", "2;90"
			if row >= thumbTop-trackY && row < thumbTop-trackY+thumbHeight {
				glyph, color = "█", "1;97"
			}
			fmt.Fprintf(out, "\x1b[%d;%dH\x1b[%sm%s\x1b[0m", trackY+row, trackX, color, glyph)
		}
	}
	if cols >= 8 && rows >= 3 && len(lines) <= rows-2 {
		width := min(72, max(8, cols-2))
		left := max(1, (cols-width)/2+1) + 1
		row := max(1, (rows-len(lines)-2)/2+1) + len(lines)
		footer := lines[len(lines)-1].text
		for _, button := range []struct{ label, key string }{{"/ search", "/"}, {helpKey + " close", shortcutInput(helpKey)}} {
			start := strings.LastIndex(footer, button.label)
			if start < 0 {
				continue
			}
			column := modalCellWidth(footer[:start])
			if column+modalCellWidth(button.label) <= width-2 {
				s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left + column, left + column + modalCellWidth(button.label) - 1, row, modalMouseAction{key: button.key}})
			}
		}
	}
}

func wrapHelpEntry(text string, width int, continuationIndent string) []string {
	lines := helpWrappedLines(text, width, continuationIndent)
	out := make([]string, len(lines))
	for i := range lines {
		out[i] = lines[i].text
	}
	return out
}

func helpWrappedLines(text string, width int, continuationIndent string) []modalRenderLine {
	if width < 1 {
		return []modalRenderLine{{modalMuted, text}}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []modalRenderLine{{modalMuted, ""}}
	}
	lines := []modalRenderLine{}
	line := words[0]
	lineWidth := width
	for _, word := range words[1:] {
		if modalCellWidth(line)+1+modalCellWidth(word) > lineWidth {
			lines = append(lines, modalRenderLine{modalMuted, line})
			line = continuationIndent + word
			lineWidth = max(1, width-modalCellWidth(continuationIndent))
			continue
		}
		line += " " + word
	}
	return append(lines, modalRenderLine{modalMuted, line})
}

func (s *tuiState) scrollHelpInput(input []byte) bool {
	step := 0
	page := false
	switch string(input) {
	case "\x1b[A":
		step = -1
	case "\x1b[B":
		step = 1
	case "\x1b[5~":
		step, page = -1, true
	case "\x1b[6~":
		step, page = 1, true
	default:
		return false
	}
	if page {
		_, rows := terminalSize()
		step *= max(1, rows-5)
	}
	s.helpOffset = max(0, s.helpOffset+step)
	return true
}

func (s *tuiState) handleHelpSearchInput(input []byte) {
	switch string(input) {
	case "\x1b", "\x03":
		s.helpSearchActive, s.helpSearchQuery, s.helpOffset = false, "", 0
	case "\r", "\n":
		s.helpSearchActive = false
	case "\x1b[A":
		s.helpOffset = max(0, s.helpOffset-1)
	case "\x1b[B":
		s.helpOffset++
	case "\b", "\x7f":
		s.helpSearchQuery = trimLastRune(s.helpSearchQuery)
		s.helpOffset = 0
	default:
		if utf8.Valid(input) && !strings.ContainsRune(string(input), '\x1b') {
			for _, r := range string(input) {
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && len(s.helpSearchQuery)+utf8.RuneLen(r) <= 256 {
					s.helpSearchQuery += string(r)
				}
			}
			s.helpOffset = 0
		}
	}
}

func shortcutActions() []string {
	actions := make([]string, 0, len(ducklord.DefaultShortcuts))
	for action := range ducklord.DefaultShortcuts {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}

func trimLastRune(text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return text
	}
	return string(runes[:len(runes)-1])
}

func (s *tuiState) beginShortcutSettings() {
	if s.cfgPath == "" {
		s.outputErr = "shortcut settings are unavailable without a config path"
		return
	}
	s.shortcutMode, s.shortcutStep, s.shortcutIndex = true, "list", 0
	s.shortcutLine, s.shortcutErr = "", ""
	s.shortcutDraft = s.cfg.Clone()
}

func (s *tuiState) renderShortcutModal(out io.Writer, cols, rows int) {
	if !s.shortcutMode {
		return
	}
	s.resetModalMouse()
	actions := shortcutActions()
	lines := []modalRenderLine{{modalTitle, "  Configure keyboard shortcuts"}}
	switch s.shortcutStep {
	case "edit":
		action := actions[s.shortcutIndex]
		lines = append(lines, modalRenderLine{modalStatus, "  " + action}, modalRenderLine{modalInput, "  binding › " + s.shortcutLine}, modalRenderLine{modalMuted, "  Examples: x, ?, ctrl-k, ctrl-] · Enter save · Esc back"})
	case "restart":
		lines = append(lines, modalRenderLine{modalStatus, "  Shortcut saved. Restart Ducklord TUI now?"}, modalRenderLine{modalInput, "  y / Enter  restart now"}, modalRenderLine{modalMuted, "  n / Esc     keep running with current bindings"})
		s.modalChoice(2, nil, 0, "\r")
		s.modalChoice(3, nil, 0, "\x1b")
	default:
		visible := max(3, rows-8)
		start := max(0, min(s.shortcutIndex-visible/2, len(actions)-visible))
		for index := start; index < min(len(actions), start+visible); index++ {
			s.modalChoice(len(lines), &s.shortcutIndex, index, "\r")
			style, prefix := modalInput, "  "
			if index == s.shortcutIndex {
				style, prefix = modalSelected, "› "
			}
			lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%-24s %s", prefix, actions[index], s.shortcutDraft.Shortcut(actions[index]))})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter edit · Esc/Ctrl+C close"})
	}
	if s.shortcutErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + sanitizeTerminalText(s.shortcutErr)})
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) handleShortcutInput(input []byte) string {
	actions := shortcutActions()
	text := string(input)
	switch s.shortcutStep {
	case "restart":
		if text == "y" || text == "Y" || text == "\r" || text == "\n" {
			return "restart-tui"
		}
		if text == "n" || text == "N" || text == "\x1b" || text == "\x03" {
			s.shortcutMode = false
			s.outputErr = "shortcuts saved; restart Ducklord to load them"
		}
		return ""
	case "edit":
		if text == "\x1b" || text == "\x03" {
			s.shortcutStep, s.shortcutLine, s.shortcutErr = "list", "", ""
			return ""
		}
		if text == "\x7f" || text == "\b" {
			s.shortcutLine = trimLastRune(s.shortcutLine)
			return ""
		}
		if text == "\r" || text == "\n" {
			action := actions[s.shortcutIndex]
			candidate := s.shortcutDraft.Clone()
			candidate.Shortcuts[action] = strings.TrimSpace(s.shortcutLine)
			if err := ducklord.SaveConfig(s.cfgPath, candidate); err != nil {
				s.shortcutErr = err.Error()
				return ""
			}
			s.shortcutDraft, s.shortcutStep, s.shortcutErr = candidate, "restart", ""
			return ""
		}
		for _, r := range text {
			if !unicode.IsControl(r) && len(s.shortcutLine) < 16 {
				s.shortcutLine += string(r)
			}
		}
	default:
		switch text {
		case "\x1b", "\x03":
			s.shortcutMode = false
		case "j", "\x1b[B":
			s.shortcutIndex = min(len(actions)-1, s.shortcutIndex+1)
		case "k", "\x1b[A":
			s.shortcutIndex = max(0, s.shortcutIndex-1)
		case "\r", "\n":
			s.shortcutStep = "edit"
			s.shortcutLine = s.shortcutDraft.Shortcut(actions[s.shortcutIndex])
			s.shortcutErr = ""
		}
	}
	return ""
}
