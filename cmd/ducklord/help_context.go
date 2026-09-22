package main

import "strings"

// Availability follows input routing: focused PTYs, detailed search, and the
// Project pane consume keys before the general Session-list handler.
func (s *tuiState) helpActionAvailable(action string) bool {
	if s.helpSearchActive {
		return action == "help"
	}
	if s.helpMode && !s.focused && action != "help" && shortcutInput(s.cfg.Shortcut(action)) == "/" {
		return false
	}
	if s.focused {
		return action == "pty_unfocus" ||
			s.workspacePreview && (action == "pane_prefix" || strings.HasPrefix(action, "prefix+"))
	}
	if s.workspacePreview {
		if nav, err := s.workspaceNavigation(); err == nil && nav.InDetailMode() {
			if s.detailSearchFocused {
				return action == "pane_prefix" || strings.HasPrefix(action, "prefix+")
			}
			if action == "help" || action == "detail_list" || action == "detail_search" || action == "detail_filter" {
				return true
			}
			if strings.HasPrefix(action, "detail_") {
				return s.detailSelected.Key() != ""
			}
			return action == "pane_prefix" || strings.HasPrefix(action, "prefix+")
		}
		if action == "help" || action == "detail_list" || action == "project_focus" || action == "pane_prefix" || strings.HasPrefix(action, "prefix+") {
			return true
		}
		if s.workspaceProjectFocus {
			return action == "pty_copy" || strings.HasPrefix(action, "project_")
		}
	}
	if strings.HasPrefix(action, "project_") || strings.HasPrefix(action, "detail_") || strings.HasPrefix(action, "prefix+") || action == "pane_prefix" || action == "pty_unfocus" {
		return false
	}
	if s.hostScoped && (action == "host_add" || action == "host_remove" || action == "session_create" || action == "shortcut_settings" || action == "notification_settings") {
		return false
	}
	if strings.HasPrefix(action, "session_") && action != "session_create" {
		return s.selectedGroupID == "" && s.selected >= 0 && s.selected < len(s.sessions)
	}
	return true
}
