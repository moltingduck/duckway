package main

import (
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) beginWorkspaceNewTab() {
	if !s.workspacePreview || s.hostScoped {
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	s.workspaceProjectFocus = true
	s.workspacePaneIntent = workspacePaneIntent{projectID: nav.CurrentProjectID(), placement: ducklord.PlaceNewTab}
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "source", 0
	s.workspacePaneErr = ""
}

func (s *tuiState) beginWorkspaceTabRename() {
	if !s.workspacePreview || !s.workspaceProjectFocus || s.hostScoped {
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	project := s.activity().ProjectLayout.Project(nav.CurrentProjectID())
	if project != nil {
		for _, tab := range project.Tabs {
			if tab.ID == nav.CurrentTabID() {
				s.workspacePaneIntent = workspacePaneIntent{projectID: project.ID, tabID: tab.ID}
				s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "tab-rename", 0
				s.workspacePaneName, s.workspacePaneErr = tab.Name, ""
				return
			}
		}
	}
	s.outputErr = "select a Terminal tab first"
}

func (s *tuiState) renderWorkspaceTabRenameModal(out io.Writer, cols, rows int) {
	lines := []modalRenderLine{{modalTitle, "  Rename Terminal tab"}, {modalInput, "  name › " + s.workspacePaneName + "_"}}
	if s.workspacePaneErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  Empty name restores tab number"}, modalRenderLine{modalMuted, "  Enter save · Esc/Ctrl+C close"})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) handleWorkspaceTabRenameInput(input []byte) bool {
	key := string(input)
	switch key {
	case "\x03", "\x1b":
		s.closeWorkspacePane()
	case "\x7f", "\b":
		runes := []rune(s.workspacePaneName)
		if len(runes) > 0 {
			s.workspacePaneName = string(runes[:len(runes)-1])
		}
	case "\r":
		next := s.activity().Clone()
		if err := next.ProjectLayout.RenameTab(s.workspacePaneIntent.projectID, s.workspacePaneIntent.tabID, strings.TrimSpace(s.workspacePaneName)); err != nil {
			s.workspacePaneErr = sanitizeTerminalText(err.Error())
			return false
		}
		if err := s.activityStore.Save(next); err != nil {
			s.workspacePaneErr = "save Terminal tab: " + sanitizeTerminalText(err.Error())
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
