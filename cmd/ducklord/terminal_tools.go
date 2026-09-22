package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

const terminalSearchBytes = 64 * 1024

func (s *tuiState) openTerminalTool(command string) bool {
	if !s.focused || s.terminal == nil || s.activeAttachKey == "" {
		return false
	}
	s.terminalBookmarkPreviousFocus = s.activeAttachKey
	s.terminalBookmarkListMode = false
	s.terminalBookmarkLabel = ""
	s.terminalSearchQuery = ""
	s.terminalSearchText = boundedTerminalText(s.terminal.Text())
	s.terminalSearchSelected = 0
	s.terminalBookmarkSelected = 0
	s.terminalSearchMode = command == "/"
	s.terminalBookmarkMode = command == "m"
	s.terminalToolInput = nil
	if command == "M" {
		s.terminalBookmarkListMode = true
	}
	// Keep the existing PTY/control lease alive, but transfer keyboard
	// ownership to this local modal so bytes cannot reach the shell.
	s.focused = false
	return true
}

func boundedTerminalText(text string) string {
	if len(text) <= terminalSearchBytes {
		return text
	}
	return text[len(text)-terminalSearchBytes:]
}

// terminalBookmarkLineInCurrent resolves the bookmark against the same
// retained tail used when it was created, then translates that tail-relative
// line into the live terminal's line coordinates for scrolling.
func terminalBookmarkLineInCurrent(text string, bookmark ducklord.TerminalBookmark) (int, bool) {
	line, ok := ducklord.TerminalBookmarkLine(text, bookmark)
	if !ok {
		return 0, false
	}
	return line, true
}

func (s *tuiState) closeTerminalTool() {
	s.terminalSearchMode = false
	s.terminalBookmarkMode = false
	s.terminalBookmarkListMode = false
	s.terminalSearchQuery = ""
	s.terminalSearchText = ""
	s.terminalSearchSelected = 0
	s.terminalBookmarkLabel = ""
	s.terminalToolInput = nil
	s.terminalBookmarkSelected = 0
	// Keep the terminal focus lease unchanged.  The saved key documents the
	// exact pane that owned the modal and lets tests detect accidental bounce.
	if s.terminalBookmarkPreviousFocus != "" {
		if _, ok := s.sessionForKey(s.terminalBookmarkPreviousFocus); !ok {
			s.workspaceProjectFocus, s.focused, s.activeAttachKey = true, false, ""
			return
		}
		s.activeAttachKey = s.terminalBookmarkPreviousFocus
		s.focused = true
	}
}

func (s *tuiState) handleTerminalToolInput(input []byte) bool {
	if !s.terminalSearchMode && !s.terminalBookmarkMode && !s.terminalBookmarkListMode {
		return false
	}
	if string(input) == "\x1b" || string(input) == "\x03" {
		s.closeTerminalTool()
		return true
	}
	if s.terminalSearchMode && (string(input) == "\x1b[A" || string(input) == "\x1b[B") {
		if string(input) == "\x1b[A" {
			s.selectTerminalSearchMatch(-1)
		} else {
			s.selectTerminalSearchMatch(1)
		}
		return true
	}
	if s.terminalBookmarkListMode {
		bookmarks := s.activity().TerminalBookmarksFor(mustTerminalIdentity(s.activePTYSession()))
		if string(input) == "\x1b[A" || string(input) == "k" {
			if len(bookmarks) > 0 {
				s.terminalBookmarkSelected = (s.terminalBookmarkSelected + len(bookmarks) - 1) % len(bookmarks)
			}
			return true
		}
		if string(input) == "\x1b[B" || string(input) == "j" {
			if len(bookmarks) > 0 {
				s.terminalBookmarkSelected = (s.terminalBookmarkSelected + 1) % len(bookmarks)
			}
			return true
		}
		if string(input) == "\r" || string(input) == "\n" {
			if len(bookmarks) > 0 {
				bookmark := bookmarks[minInt(s.terminalBookmarkSelected, len(bookmarks)-1)]
				current := s.terminal.Text()
				if line, ok := terminalBookmarkLineInCurrent(current, bookmark); ok {
					totalLines := strings.Count(current, "\n") + 1
					s.ptyScrollOffset = maxInt(0, totalLines-line-1)
					s.outputErr = "bookmark revealed at retained output"
				} else {
					// Bookmarks intentionally contain no output. If retention has
					// changed, select the deterministic current-history fallback.
					s.ptyScrollOffset = 0
					s.outputErr = "bookmark unavailable; showing current retained history"
				}
			}
			s.closeTerminalTool()
		}
		return true
	}
	if string(input) == "\r" || string(input) == "\n" {
		if s.terminalBookmarkMode {
			identity, ok := ducklord.IdentityFromSession(s.activePTYSession())
			if !ok {
				s.outputErr = "bookmark: no active session"
				return true
			}
			bookmark, err := ducklord.NewTerminalBookmark(identity, s.terminalBookmarkLabel, boundedTerminalText(s.terminal.Text()))
			if err == nil {
				next := s.activity().Clone()
				if err = next.AddTerminalBookmark(bookmark); err == nil {
					err = s.activityStore.Save(next)
				}
				if err == nil {
					s.activityState = next
				} else {
					s.outputErr = "bookmark: " + err.Error()
				}
			}
			if err != nil {
				s.outputErr = "bookmark: " + err.Error()
				return true
			}
		}
		s.closeTerminalTool()
		return true
	}
	s.terminalToolInput = append(s.terminalToolInput, input...)
	for len(s.terminalToolInput) > 0 {
		r, n := utf8.DecodeRune(s.terminalToolInput)
		if r == utf8.RuneError && n == 1 && !utf8.Valid(s.terminalToolInput) {
			break
		}
		s.terminalToolInput = s.terminalToolInput[n:]
		if r < 0x20 || r == 0x7f {
			continue
		}
		if s.terminalSearchMode {
			s.terminalSearchQuery += string(r)
			s.selectTerminalSearchMatch(0)
		} else {
			s.terminalBookmarkLabel += string(r)
		}
	}
	return true
}

func (s *tuiState) selectTerminalSearchMatch(delta int) {
	query := strings.ToLower(s.terminalSearchQuery)
	if query == "" {
		s.terminalSearchSelected = 0
		return
	}
	matches := make([]int, 0)
	text := strings.ToLower(s.terminalSearchText)
	for start := 0; start <= len(text)-len(query); {
		index := strings.Index(text[start:], query)
		if index < 0 {
			break
		}
		index += start
		matches = append(matches, index)
		start = index + maxInt(1, len(query))
	}
	if len(matches) == 0 {
		s.terminalSearchSelected = 0
		return
	}
	selected := (s.terminalSearchSelected + delta) % len(matches)
	if selected < 0 {
		selected += len(matches)
	}
	s.terminalSearchSelected = selected
	// Locate the selected line in the live retained framebuffer and put it at
	// the bottom of the viewport. The captured snapshot remains authoritative
	// for matching while PTY output continues to refresh.
	if s.terminal != nil {
		lineText := s.terminal.Text()
		match := matches[selected]
		if match < len(s.terminalSearchText) {
			lineStart := strings.LastIndexByte(s.terminalSearchText[:match], '\n') + 1
			lineEnd := strings.IndexByte(s.terminalSearchText[match:], '\n')
			if lineEnd < 0 {
				lineEnd = len(s.terminalSearchText) - match
			}
			needle := s.terminalSearchText[lineStart : match+lineEnd]
			if line := strings.Index(strings.ToLower(lineText), strings.ToLower(needle)); line >= 0 {
				lineNumber := strings.Count(lineText[:line], "\n")
				totalLines := strings.Count(lineText, "\n") + 1
				s.ptyScrollOffset = maxInt(0, totalLines-lineNumber-1)
			}
		}
	}
}

func mustTerminalIdentity(session ducklord.RemoteSession) ducklord.SessionIdentity {
	identity, _ := ducklord.IdentityFromSession(session)
	return identity
}

func (s *tuiState) renderTerminalTool(out io.Writer, cols, rows int) {
	if !s.terminalSearchMode && !s.terminalBookmarkMode && !s.terminalBookmarkListMode {
		return
	}
	lines := []modalRenderLine{{style: modalTitle, text: "terminal tools"}}
	if s.terminalSearchMode {
		lines = append(lines, modalRenderLine{text: "search: " + s.terminalSearchQuery})
		if s.terminalSearchQuery != "" {
			matches := terminalSearchMatchOffsets(s.terminalSearchText, s.terminalSearchQuery)
			if len(matches) > 0 {
				selected := minInt(s.terminalSearchSelected, len(matches)-1)
				lines = append(lines, modalRenderLine{style: modalInput, text: fmt.Sprintf("match %d/%d at byte %d", selected+1, len(matches), matches[selected])})
				lines = append(lines, modalRenderLine{style: modalMuted, text: "Up/Down select match"})
			} else {
				lines = append(lines, modalRenderLine{style: modalDanger, text: "no match"})
			}
		}
	} else if s.terminalBookmarkMode {
		lines = append(lines, modalRenderLine{text: "bookmark label: " + s.terminalBookmarkLabel})
	} else {
		identity, _ := ducklord.IdentityFromSession(s.activePTYSession())
		current := s.terminal.Text()
		for index, bookmark := range s.activity().TerminalBookmarksFor(identity) {
			state := "unavailable; Enter shows retained history"
			if _, ok := terminalBookmarkLineInCurrent(current, bookmark); ok {
				state = "active; Enter reveals retained output"
			}
			prefix := "  "
			if index == s.terminalBookmarkSelected {
				prefix = "> "
			}
			lines = append(lines, modalRenderLine{text: prefix + bookmark.Label + " [" + state + "]"})
		}
		if len(lines) == 1 {
			lines = append(lines, modalRenderLine{text: "no bookmarks"})
		}
	}
	lines = append(lines, modalRenderLine{style: modalMuted, text: "Enter/Esc close"})
	renderModalBox(out, cols, rows, lines)
}

func terminalSearchMatchOffsets(text, query string) []int {
	query = strings.ToLower(query)
	text = strings.ToLower(text)
	if query == "" {
		return nil
	}
	matches := make([]int, 0)
	for start := 0; start <= len(text)-len(query); {
		index := strings.Index(text[start:], query)
		if index < 0 {
			break
		}
		index += start
		matches = append(matches, index)
		start = index + maxInt(1, len(query))
	}
	return matches
}

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
