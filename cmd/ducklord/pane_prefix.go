package main

import (
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// Prefix handling stays on the UI event loop and never forwards command bytes.
func (s *tuiState) handlePanePrefix(input []byte) (bool, string) {
	if !s.workspacePreview || s.blockingModalOpen() || s.copyMode || s.helpSearchActive {
		s.panePrefixPending = false
		return false, ""
	}
	// Clickable help entries submit an entire local key sequence.
	prefix := shortcutInput(s.cfg.Shortcut("pane_prefix"))
	if len(input) > len(prefix) && strings.HasPrefix(string(input), prefix) {
		s.panePrefixPending = true
		input = input[len(prefix):]
	}
	if s.panePrefixPending {
		s.panePrefixPending = false
		switch string(input) {
		case "-", "\\", "t", ",":
			return true, string(input)
		default:
			for _, key := range []string{"up", "down", "left", "right", "pageup", "pagedown"} {
				if string(input) == shortcutInput(key) {
					return true, key
				}
			}
			return true, ""
		}
	}
	if s.shortcut("pane_prefix", string(input)) {
		s.panePrefixPending = true
		return true, ""
	}
	return false, ""
}

func paneNavigationCommand(command string) bool {
	switch command {
	case "up", "down", "left", "right", "pageup", "pagedown":
		return true
	}
	return false
}

func (s *tuiState) navigatePrefixPane(command string) bool {
	nav, err := s.workspaceNavigation()
	if err == nil {
		switch command {
		case "pageup":
			err = nav.CycleTab(-1)
		case "pagedown":
			err = nav.CycleTab(1)
		default:
			width, height := terminalSize()
			err = nav.MoveVisiblePane(ducklord.CalculateWorkspaceGeometry(width, height, 4), command)
		}
	}
	if err != nil {
		s.outputErr = err.Error()
		return false
	}
	s.outputErr = ""
	s.workspaceProjectFocus = true
	s.workspaceAttachFromProject = true
	s.workspaceFocusFromProject = true
	s.selectedGroupID = ""
	return true
}

func (s *tuiState) openPrefixPane(command string) {
	s.workspaceProjectFocus = true
	if command == "," {
		s.beginWorkspaceTabRename()
		return
	}
	s.beginWorkspacePane()
	if !s.workspacePaneMode {
		return
	}
	placement := ducklord.PlaceNewTab
	switch command {
	case "-":
		placement = ducklord.PlaceHorizontal
	case "\\":
		placement = ducklord.PlaceVertical
	}
	s.workspacePaneIntent.placement = placement
	if placement != ducklord.PlaceNewTab && s.workspacePaneIntent.targetID == "" {
		s.workspacePaneErr = "choose a Project with a Session pane to split"
		return
	}
	s.workspacePaneStep, s.workspacePaneIndex = "source", 0
}
