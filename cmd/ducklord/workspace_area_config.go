package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) beginWorkspaceAreaConfig(area string) {
	s.closeWorkspacePane()
	s.workspaceConfigFocus = area
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneName = true, "context-config", area
}

func (s *tuiState) openFocusedWorkspaceConfig() {
	switch s.workspaceConfigFocus {
	case "project-pane", "session-list", "terminal":
		s.beginWorkspaceAreaConfig(s.workspaceConfigFocus)
		return
	case "tab":
		s.workspaceProjectFocus = true
		s.beginWorkspaceTabRename()
		return
	case "session":
		s.openWorkspaceSessionConfig(s.workspaceConfigIdentity)
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	if nav.InDetailMode() {
		if s.detailSelected.Key() != "" {
			s.openWorkspaceSessionConfig(s.detailSelected)
		} else {
			s.beginWorkspaceAreaConfig("session-list")
		}
		return
	}
	if s.workspaceProjectFocus {
		if nav.CurrentProjectID() != "" {
			s.beginWorkspaceProjectHosts()
		} else {
			s.beginWorkspaceAreaConfig("project-pane")
		}
		return
	}
	if s.workspaceMouseFocus || s.focused {
		if session, err := s.workspaceSelectedPaneSession(); err == nil {
			identity, _ := ducklord.IdentityFromSession(session)
			s.openWorkspaceSessionConfig(identity)
			return
		}
		s.beginWorkspaceAreaConfig("terminal")
		return
	}
	if len(s.sessions) > 0 {
		identity, ok := ducklord.IdentityFromSession(s.currentSession())
		if ok {
			s.openWorkspaceSessionConfig(identity)
			return
		}
	}
	s.beginWorkspaceAreaConfig("session-list")
}

func (s *tuiState) workspaceAreaChoices() (string, []string) {
	switch s.workspacePaneName {
	case "project-pane":
		return "Project pane config", []string{"Create project", "Workspace appearance · Midnight", "Workspace appearance · Slate", "Keyboard shortcuts"}
	case "session-list":
		return "Session list pane config", []string{"Cycle sort · " + s.quickSortMode(), "Reverse event time order", "Workspace appearance · Midnight", "Workspace appearance · Slate", "Keyboard shortcuts"}
	default:
		return "Terminal area config", []string{"Create tab", "Rename current tab", "Workspace appearance · Midnight", "Workspace appearance · Slate", "Keyboard shortcuts"}
	}
}

func (s *tuiState) renderWorkspaceAreaConfig(out io.Writer, cols, rows int) {
	title, choices := s.workspaceAreaChoices()
	s.resetModalMouse()
	lines := []modalRenderLine{{modalTitle, "  " + title}}
	for i, choice := range choices {
		style := ""
		if i == s.workspacePaneIndex {
			style = modalSelected
		}
		s.modalChoice(len(lines), &s.workspacePaneIndex, i, "\r")
		lines = append(lines, modalRenderLine{style, "  " + choice})
	}
	lines = append(lines, modalRenderLine{modalStatus, "  " + s.workspacePaneErr}, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter apply · Esc close"})
	s.renderModalBox(out, cols, rows, lines)
}

// Reload before changing one setting; never overwrite another process's edits.
func (s *tuiState) saveWorkspaceAreaSetting(change func(*ducklord.Config)) error {
	base, err := os.ReadFile(s.cfgPath)
	if err != nil {
		return err
	}
	cfg, err := ducklord.LoadConfig(s.cfgPath)
	if err != nil {
		return err
	}
	checked, err := os.ReadFile(s.cfgPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(base, checked) {
		return fmt.Errorf("config changed; try again")
	}
	change(cfg)
	if err := ducklord.SaveConfigIfUnchanged(s.cfgPath, cfg, base); err != nil {
		return err
	}
	key := s.currentKey()
	s.cfg = cfg
	s.sortQuickSessions()
	s.restoreSelection(key)
	return nil
}

func (s *tuiState) handleWorkspaceAreaConfig(input []byte) {
	_, choices := s.workspaceAreaChoices()
	switch string(input) {
	case "\x1b", "\x03":
		s.closeWorkspacePane()
	case "\x1b[A", "k":
		s.workspacePaneIndex = max(0, s.workspacePaneIndex-1)
	case "\x1b[B", "j":
		s.workspacePaneIndex = min(len(choices)-1, s.workspacePaneIndex+1)
	case "\r", "\n":
		choice := choices[max(0, min(s.workspacePaneIndex, len(choices)-1))]
		switch choice {
		case "Create project":
			s.closeWorkspacePane()
			s.workspaceProjectFocus = true
			s.beginWorkspaceProject()
		case "Create tab":
			s.closeWorkspacePane()
			s.beginWorkspaceNewTab()
		case "Rename current tab":
			s.closeWorkspacePane()
			s.workspaceProjectFocus = true
			s.beginWorkspaceTabRename()
		case "Keyboard shortcuts":
			s.closeWorkspacePane()
			s.beginShortcutSettings()
		default:
			err := s.saveWorkspaceAreaSetting(func(cfg *ducklord.Config) {
				switch choice {
				case "Workspace appearance · Midnight":
					cfg.WorkspaceTheme = ducklord.DefaultWorkspaceTheme()
				case "Workspace appearance · Slate":
					cfg.WorkspaceTheme = ducklord.WorkspaceTheme{Separator: "#526477", Background: "#242e3c", Foreground: "#d3dce8", FocusBackground: "#3d506b", FocusForeground: "#ffffff"}
				case "Reverse event time order":
					cfg.QuickSort = "event_time"
					cfg.QuickOldestFirst = !cfg.QuickOldestFirst
				default:
					next := quickSortModes[0]
					for i, mode := range quickSortModes {
						if cfg.QuickSort == mode {
							next = quickSortModes[(i+1)%len(quickSortModes)]
							break
						}
					}
					cfg.QuickSort = next
				}
			})
			if err != nil {
				s.workspacePaneErr = err.Error()
			} else {
				s.workspacePaneErr = "Saved"
			}
		}
	}
}
