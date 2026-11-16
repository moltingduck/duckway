package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) handleInput(b []byte) string {
	text := string(b)
	if !strings.HasPrefix(text, "\x1b[<") {
		s.dragSession, s.dragTargetGroup, s.dragTargetSession = ducklord.SessionIdentity{}, "", ducklord.SessionIdentity{}
	}
	switch {
	case s.shortcut("quit", text) || text == "\x03":
		return "quit"
	case s.shortcut("help", text):
		return "help"
	case s.shortcut("shortcut_settings", text) && !s.hostScoped:
		return "shortcut-settings"
	case s.shortcut("notification_settings", text) && !s.hostScoped:
		return "notification-settings"
	case s.shortcut("host_actions", text):
		return "host-actions"
	case s.shortcut("approvals", text) && !s.hostScoped:
		return "approvals"
	case s.selectedGroupID != "" && (s.sessionShortcut(text) || s.shortcut("list_reorder_up", text) || s.shortcut("list_reorder_down", text)):
		s.outputErr = "select a session row for this action"
		return "group-select"
	case !s.workspacePreview && text == "\x1b[D":
		if s.selectedGroupID == "" {
			s.selectedGroupID = s.organizationGroupID(s.currentSession())
			return "group-select"
		}
		s.setSelectedGroupCollapsed(true)
		return "group-toggle"
	case !s.workspacePreview && text == "\x1b[C":
		if s.selectedGroupID != "" {
			s.setSelectedGroupCollapsed(false)
			return "group-toggle"
		}
		return "group-select"
	case s.shortcut("refresh", text):
		return "refresh"
	case text == "f" && !s.focused:
		return "project-files"
	case s.shortcut("list_search", text):
		return "search"
	case s.workspacePreview && s.shortcut("list_sort", text):
		return "quick-sort"
	case s.workspacePreview && s.shortcut("list_sort_direction", text):
		return "quick-sort-direction"
	case !s.workspacePreview && s.shortcut("list_organize", text):
		return "organize"
	case !s.workspacePreview && s.shortcut("list_groups", text):
		return "groups"
	case !s.workspacePreview && s.shortcut("list_reorder_up", text):
		return "reorder-up"
	case !s.workspacePreview && s.shortcut("list_reorder_down", text):
		return "reorder-down"
	case s.shortcut("session_yield", text):
		return "yield"
	case s.shortcut("session_yield_wait", text):
		return "yield-wait"
	case s.shortcut("session_end", text):
		return "end"
	case s.shortcut("session_restart", text):
		return "restart"
	case s.shortcut("session_destroy", text):
		return "destroy"
	case s.shortcut("session_create", text) && !s.hostScoped:
		return "new"
	case s.shortcut("session_notifications", text):
		return "notifications"
	case s.shortcut("pty_copy", text):
		return "copy-mode"
	case s.shortcut("session_actions", text):
		return "actions"
	case s.shortcut("host_add", text) && !s.hostScoped:
		return "add-client"
	case s.shortcut("host_remove", text) && !s.hostScoped:
		return "remove-client"
	case text == "\r":
		if s.selectedGroupID != "" {
			s.toggleSelectedGroup()
			return "group-toggle"
		}
		return "attach"
	case s.workspacePreview && (text == "\x1b[5~" || text == "\x1b[6~"):
		width, height := terminalSize()
		page := max(1, ducklord.CalculateWorkspaceGeometry(width, height, 4).Quick.Height-1)
		direction := 1
		if text == "\x1b[5~" {
			direction = -1
		}
		changed := false
		for range page {
			if !s.moveListSelection(direction) {
				break
			}
			changed = true
		}
		if changed {
			if s.selectedGroupID != "" {
				return "group-select"
			}
			return "select"
		}
	case text == "j" || text == "\x1b[B":
		if s.moveListSelection(1) {
			if s.selectedGroupID != "" {
				return "group-select"
			}
			return "select"
		}
	case text == "k" || text == "\x1b[A":
		if s.moveListSelection(-1) {
			if s.selectedGroupID != "" {
				return "group-select"
			}
			return "select"
		}
	case strings.HasPrefix(text, "\x1b[<32;") && strings.HasSuffix(text, "M"):
		if row, ok := s.listRowForMouse(text); ok && s.dragSession.Key() != "" {
			s.dragTargetGroup, s.dragTargetSession = row.groupID, ducklord.SessionIdentity{}
			if row.isGroup {
				s.selectedGroupID = row.groupID
				return "group-select"
			}
			s.dragTargetSession, _ = ducklord.IdentityFromSession(s.sessions[row.sessionIndex])
			s.selectedGroupID, s.selected = "", row.sessionIndex
			return "select"
		}
	case strings.HasPrefix(text, "\x1b[<0;") && strings.HasSuffix(text, "m"):
		if row, ok := s.listRowForMouse(text); ok && row.groupID == s.dragTargetGroup && s.dragSession.Key() != "" {
			if !row.isGroup {
				released, valid := ducklord.IdentityFromSession(s.sessions[row.sessionIndex])
				if !valid || released != s.dragTargetSession {
					s.dragSession, s.dragTargetGroup, s.dragTargetSession = ducklord.SessionIdentity{}, "", ducklord.SessionIdentity{}
					s.outputErr = "session list changed during drag; try again"
					return "group-drop"
				}
			}
			if row.isGroup && s.organizationMode() != ducklord.OrganizationCustom {
				s.dragSession, s.dragTargetGroup, s.dragTargetSession = ducklord.SessionIdentity{}, "", ducklord.SessionIdentity{}
				s.outputErr = "drop onto a session to reorder inside this group"
				return "group-drop"
			}
			before := ducklord.SessionIdentity{}
			if !row.isGroup {
				before, _ = ducklord.IdentityFromSession(s.sessions[row.sessionIndex])
			}
			s.moveDraggedSessionBefore(row.groupID, before)
			return "group-drop"
		}
		s.dragSession, s.dragTargetGroup, s.dragTargetSession = ducklord.SessionIdentity{}, "", ducklord.SessionIdentity{}
	case strings.HasPrefix(text, "\x1b[<0;") && strings.HasSuffix(text, "M"):
		if session, ok := s.selectListRowForMouse(text); ok {
			if session {
				s.selectedKey = s.currentKey()
				s.dragSession, _ = ducklord.IdentityFromSession(s.currentSession())
				return "select"
			}
			return "group-select"
		}
		if s.contentPaneClicked(text) {
			return "attach"
		}
	case strings.HasPrefix(text, "\x1b[<2;") && strings.HasSuffix(text, "M"):
		if session, ok := s.selectListRowForMouse(text); ok {
			if !session {
				s.toggleSelectedGroup()
				return "group-toggle"
			}
			s.selectedKey = s.currentKey()
		}
		return "attach"
	}
	return ""
}

func copyModeExitInput(input []byte) bool {
	text := string(input)
	return text == "q" || text == "v" || text == "\x03" || text == "\x1b"
}

func (s *tuiState) enterCopyMode(out io.Writer) {
	if s.copyMode {
		return
	}
	s.copyMode = true
	if s.terminal != nil {
		s.copyTerminal, _ = ducklord.NewTerminalFromState(s.terminal.SnapshotState(), ducklord.DefaultTerminalScrollback)
	}
	// Native terminal selection requires all mouse reporting to be disabled.
	fmt.Fprint(out, "\033[?1000l\033[?1002l\033[?1003l\033[?1006l")
	s.renderCopyMode(out)
}

func (s *tuiState) renderCopyMode(out io.Writer) {
	if !s.copyMode {
		return
	}
	s.copyMode = false
	s.copyRendering = true
	s.render(out)
	s.copyRendering = false
	s.copyMode = true
}

func (s *tuiState) exitCopyMode(out io.Writer) {
	if !s.copyMode {
		return
	}
	s.copyMode = false
	s.copyTerminal = nil
	// Restore the encoding before event tracking so no mouse report can arrive
	// in an unexpected format during the transition.
	fmt.Fprint(out, "\033[?1000l\033[?1006h\033[?1002h")
}

func (s *tuiState) viewportTerminal() *ducklord.Terminal {
	if s.copyTerminal != nil {
		return s.copyTerminal
	}
	return s.terminal
}

func (s *tuiState) scrollPTY(delta int) {
	terminal := s.viewportTerminal()
	if terminal == nil {
		return
	}
	s.ptyScrollOffset = min(len(terminal.Scrollback), max(0, s.ptyScrollOffset+delta))
}
