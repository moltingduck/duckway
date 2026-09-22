package main

import (
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) beginSessionHandleRename(target ducklord.RemoteSession) {
	s.sessionRenameTarget = target
	s.sessionRenameLine = target.Name
	s.sessionRenameErr = ""
	s.sessionRenameMode = true
}

func (s *tuiState) closeSessionHandleRename() {
	s.sessionRenameMode = false
	s.sessionRenameTarget = ducklord.RemoteSession{}
	s.sessionRenameLine, s.sessionRenameErr = "", ""
}

func (s *tuiState) renderSessionHandleRenameModal(out io.Writer, cols, rows int) {
	if !s.sessionRenameMode {
		return
	}
	lines := []modalRenderLine{{modalTitle, "  Rename Session handle"}, {modalInput, "  handle › " + s.sessionRenameLine + "_"}}
	if s.sessionRenameErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + s.sessionRenameErr})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  Enter save · Esc/Ctrl+C close"})
	s.renderModalBox(out, cols, rows, lines)
}

// handleSessionHandleRenameInput returns rename-submit when validation passes.
func (s *tuiState) handleSessionHandleRenameInput(input []byte) string {
	key := string(input)
	switch key {
	case "\x03", "\x1b":
		s.closeSessionHandleRename()
	case "\x7f", "\b":
		r := []rune(s.sessionRenameLine)
		if len(r) > 0 {
			s.sessionRenameLine = string(r[:len(r)-1])
		}
	case "\r", "\n":
		name := strings.TrimSpace(s.sessionRenameLine)
		if _, err := model.ValidateHandle(name); err != nil {
			s.sessionRenameErr = sanitizeTerminalText(err.Error())
			return ""
		}
		return "rename-submit"
	default:
		if utf8.Valid(input) && !strings.ContainsRune(key, '\x1b') {
			for _, r := range key {
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && utf8.RuneCountInString(s.sessionRenameLine) < 128 {
					s.sessionRenameLine += string(r)
				}
			}
		}
	}
	return ""
}
