package main

import (
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) beginWorkspaceProjectRename() {
	if !s.workspacePreview || s.hostScoped {
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.workspacePaneErr = err.Error()
		return
	}
	project := s.activity().ProjectLayout.Project(nav.CurrentProjectID())
	if project == nil || project.ID == ducklord.DefaultProjectID {
		s.workspacePaneErr = "select a custom Project first"
		return
	}
	s.workspacePaneIntent = workspacePaneIntent{projectID: project.ID}
	s.workspacePaneName = project.Name
	s.workspacePaneErr = ""
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "project-rename", 0
}

func (s *tuiState) renderWorkspaceProjectRenameModal(out io.Writer, cols, rows int) {
	lines := []modalRenderLine{{modalTitle, "  Rename Project"}, {modalInput, "  name › " + s.workspacePaneName + "_"}}
	if s.workspacePaneErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  Enter save · Esc/Ctrl+C close"})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) handleWorkspaceProjectRenameInput(input []byte) bool {
	key := string(input)
	switch key {
	case "\x03", "\x1b":
		s.closeWorkspacePane()
	case "\x7f", "\b":
		r := []rune(s.workspacePaneName)
		if len(r) > 0 {
			s.workspacePaneName = string(r[:len(r)-1])
		}
	case "\r", "\n":
		next := s.activity().Clone()
		if err := next.ProjectLayout.RenameProject(s.workspacePaneIntent.projectID, strings.TrimSpace(s.workspacePaneName)); err != nil {
			s.workspacePaneErr = sanitizeTerminalText(err.Error())
			return false
		}
		if err := s.activityStore.Save(next); err != nil {
			s.workspacePaneErr = "save Project: " + sanitizeTerminalText(err.Error())
			return false
		}
		s.activityState, s.workspacePaneChanged = next, true
		s.closeWorkspacePane()
	default:
		if utf8.Valid(input) && !strings.ContainsRune(key, '\x1b') {
			for _, r := range key {
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && utf8.RuneCountInString(s.workspacePaneName) < 128 {
					s.workspacePaneName += string(r)
				}
			}
		}
	}
	return false
}
