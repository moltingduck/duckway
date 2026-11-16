package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) render(out io.Writer) {
	s.modalMouseRegions = nil
	for s.pendingBells > 0 {
		_, _ = out.Write([]byte{'\a'})
		s.pendingBells--
	}
	if s.copyMode {
		return
	}
	var frame strings.Builder
	destination := out
	out = &frame
	defer func() {
		width, height := terminalSize()
		if s.copyRendering {
			status := modalCellTruncate(" COPY MODE  Drag to select · j/k browse · Esc/q/v/Ctrl+C exits", max(1, width))
			fmt.Fprintf(&frame, "\033[2;1H\033[2K%s%s%s", modalSelected, status, modalReset)
		}
		s.frameOutput.write(destination, frame.String(), width, height)
	}()
	if s.workspacePreview {
		s.renderWorkspacePreview(out)
		return
	}
	width, height := terminalSize()
	modalHeight := height
	layout := calculateTUILayout(width, s.focused, s.listPaneWidth, s.autoHideList)
	renderWidth := layout.width
	if height < 12 {
		height = 12
	}
	menuWidth := layout.menuWidth
	contentX := layout.contentX
	contentWidth := layout.contentWidth
	fmt.Fprint(out, "\033[?25l\033[H\033[2J")
	status := ""
	if s.resizeStatus != "" {
		status = "  " + s.resizeStatus
	}
	header := fmt.Sprintf("ducklord remote agents  owner:%s%s%s%s", displayField(s.ownerName), s.hostSyncLabel(), s.approvalsHeaderLabel(), truncate(status, renderWidth/2))
	fmt.Fprintf(out, "%s%s%s\n", modalTitle, truncate(header, renderWidth), modalReset)
	if s.localWarning != "" {
		fmt.Fprintf(out, "%s%s%s\n", modalDanger, truncate("warning: "+sanitizeTerminalText(s.localWarning), renderWidth), modalReset)
	} else if s.focused {
		fmt.Fprintf(out, "%s%s%s\n", modalSelected, truncate("  ●  SESSION FOCUS   keys go to session   ctrl-] menu", renderWidth), modalReset)
	} else if s.addClientMode {
		fmt.Fprintf(out, "%s%s%s\n", modalMuted, truncate("add ducklion host: ssh config number/name or user@host  enter add  esc cancel", renderWidth), modalReset)
	} else if s.newSessionMode {
		fmt.Fprintf(out, "%s%s%s\n", modalMuted, truncate(s.createHeader()+"  enter next  esc cancel", renderWidth), modalReset)
	} else if s.searchMode {
		fmt.Fprintf(out, "%s%s%s\n", modalMuted, truncate("search sessions  type to filter  ↑/↓ move  enter activate  esc close", renderWidth), modalReset)
	} else if s.actionMenu {
		fmt.Fprintf(out, "%s%s%s\n", modalMuted, truncate("session actions  ↑/↓ or j/k move  enter choose  esc close", renderWidth), modalReset)
	} else if s.groupMenu {
		fmt.Fprintf(out, "%s%s%s\n", modalMuted, truncate("custom groups  ↑/↓ choose  enter confirm  esc close", renderWidth), modalReset)
	} else if s.lifecycleConfirm != "" {
		fmt.Fprintf(out, "%s%s%s\n", modalStatus, truncate("lifecycle confirmation open; use the modal controls", renderWidth), modalReset)
	} else {
		fmt.Fprintf(out, "%s%s%s\n", modalMuted, truncate(s.cfg.Shortcut("help")+" help · Enter focus/group · "+s.cfg.Shortcut("pty_unfocus")+" leave PTY", renderWidth), modalReset)
	}
	fmt.Fprintf(out, "%s%s%s\n", modalBorder, strings.Repeat("─", renderWidth), modalReset)
	if len(s.sessions) == 0 {
		guidance := "No sessions yet. Press " + s.cfg.Shortcut("session_create") + " to create a session · " + s.cfg.Shortcut("host_add") + " to add a host · " + s.cfg.Shortcut("help") + " for help"
		if s.cfg != nil && len(s.cfg.Clients) == 0 {
			guidance = "No hosts configured. Press " + s.cfg.Shortcut("host_add") + " to add a host · " + s.cfg.Shortcut("help") + " for help"
		}
		fmt.Fprintln(out, truncate(guidance, renderWidth))
		s.renderCreateModal(out, width, modalHeight)
		s.renderSearchModal(out, width, modalHeight)
		s.renderActionModal(out, width, modalHeight)
		s.renderSessionHandleRenameModal(out, width, modalHeight)
		s.renderAddClientModal(out, width, modalHeight)
		s.renderRemoveClientModal(out, width, modalHeight)
		s.renderHelpModal(out, width, modalHeight)
		s.renderShortcutModal(out, width, modalHeight)
		s.renderNotificationConfigModal(out, width, modalHeight)
		s.renderHostModal(out, width, modalHeight)
		s.renderGroupModal(out, width, modalHeight)
		s.renderNotificationModal(out, width, modalHeight)
		s.renderLifecycleModal(out, width, modalHeight)
		s.renderCommandPalette(out, width, modalHeight)
		s.renderProjectFilesModal(out, width, modalHeight)
		s.renderApprovalsModal(out, width, modalHeight)
		return
	}
	if layout.overlay {
		s.renderContent(out, contentX, contentWidth, height)
	}
	if layout.showList {
		separator := " | content"
		if layout.overlay {
			separator = ""
		}
		clear := "\033[K"
		if layout.overlay {
			clear = ""
		}
		heading := truncate("SESSIONS  ·  "+strings.ToUpper(string(s.organizationMode())), menuWidth)
		fmt.Fprintf(out, "\033[4;1H%s%-*s%s%s%s", modalTitle, menuWidth, heading, modalReset, separator, clear)
	} else {
		fmt.Fprintf(out, "\033[4;1H%s%s%s\033[K", modalTitle, "CONTENT", modalReset)
	}
	row := 5
	for _, listRow := range s.sessionListRows() {
		if !layout.showList {
			break
		}
		if row > height {
			break
		}
		if listRow.isGroup {
			prefix := " "
			if s.selectedGroupID == listRow.groupID {
				prefix = ">"
			}
			disclosure := "▾"
			if s.groupCollapsed(listRow.groupID) {
				disclosure = "▸"
			}
			groupLabel := prefix + disclosure + " [" + displayField(listRow.groupLabel) + "]"
			if s.groupHasUnread(listRow.groupID) {
				groupLabel += " •"
			}
			separator := " |"
			if layout.overlay {
				separator = ""
			}
			clear := "\033[K"
			if layout.overlay {
				clear = ""
			}
			plain := modalCellPad(modalCellTruncate(groupLabel, menuWidth), menuWidth)
			if s.selectedGroupID == listRow.groupID {
				plain = modalSelected + plain + modalReset
			} else if s.organizationMode() == ducklord.OrganizationHost {
				plain = s.hostRowStyle(listRow.groupID) + plain + modalReset
			}
			fmt.Fprintf(out, "\033[%d;1H%s%s%s", row, plain, separator, clear)
			row++
			continue
		}
		i, sess := listRow.sessionIndex, s.sessions[listRow.sessionIndex]
		prefix := " "
		if s.selectedGroupID == "" && i == s.selected {
			prefix = ">"
		}
		mark := " "
		if sessionNeedsAttention(sess) {
			mark = "💀"
		} else if sessionKey(sess) == s.activeAttachKey {
			mark = "●"
		} else if sess.Unread {
			mark = "•"
		} else if sess.Updated {
			mark = "*"
		}
		markColumn := mark + " "
		if modalCellWidth(mark) >= 2 {
			markColumn = mark
		}
		line := fmt.Sprintf("%s%s%-12s %-18s %-9s %-14s %s", prefix, markColumn, displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), displayField(sessionTypeLabel(sess)), sess.LastLine)
		if s.organizationMode() == ducklord.OrganizationHost {
			line = fmt.Sprintf("%s%s%-18s %-9s %-14s %s", prefix, markColumn, displayField(sess.Name), displayField(sess.Status), displayField(sessionTypeLabel(sess)), sess.LastLine)
		}
		if sess.Error != "" {
			errorMark := "!"
			if sessionNeedsAttention(sess) {
				errorMark = "💀"
			}
			errorColumn := errorMark + " "
			if modalCellWidth(errorMark) >= 2 {
				errorColumn = errorMark
			}
			line = fmt.Sprintf("%s%s%-12s %-18s %-9s %s", prefix, errorColumn, displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), sess.Error)
			if s.organizationMode() == ducklord.OrganizationHost {
				line = fmt.Sprintf("%s%s%-18s %-9s %s", prefix, errorColumn, displayField(sess.Name), displayField(sess.Status), sess.Error)
			}
		}
		separator := " |"
		if layout.overlay {
			separator = ""
		}
		clear := "\033[K"
		if layout.overlay {
			clear = ""
		}
		plain := modalCellPad(modalCellTruncate(line, menuWidth), menuWidth)
		style := s.hostRowStyle(sess.Client)
		if s.selectedGroupID == "" && i == s.selected {
			style = modalSelected
		}
		fmt.Fprintf(out, "\033[%d;1H%s%s%s%s%s", row, style, plain, modalReset, separator, clear)
		row++
	}
	if !layout.overlay {
		s.renderContent(out, contentX, contentWidth, height)
	}
	s.renderCreateModal(out, width, modalHeight)
	s.renderSearchModal(out, width, modalHeight)
	s.renderActionModal(out, width, modalHeight)
	s.renderSessionHandleRenameModal(out, width, modalHeight)
	s.renderAddClientModal(out, width, modalHeight)
	s.renderRemoveClientModal(out, width, modalHeight)
	s.renderHelpModal(out, width, modalHeight)
	s.renderShortcutModal(out, width, modalHeight)
	s.renderNotificationConfigModal(out, width, modalHeight)
	s.renderHostModal(out, width, modalHeight)
	s.renderGroupModal(out, width, modalHeight)
	s.renderNotificationModal(out, width, modalHeight)
	s.renderLifecycleModal(out, width, modalHeight)
	s.renderCommandPalette(out, width, modalHeight)
	s.renderProjectFilesModal(out, width, modalHeight)
	s.renderApprovalsModal(out, width, modalHeight)
	if s.focused && s.terminal != nil && !s.outputStale && s.ptyScrollOffset == 0 {
		if cursorRow, cursorCol, visible := s.terminal.CursorPosition(height-5, contentWidth); visible {
			fmt.Fprintf(out, "\033[%d;%dH\033[?25h", 6+cursorRow, contentX+cursorCol)
		}
	}
}

func (s *tuiState) groupHasUnread(group string) bool {
	for _, session := range s.sessions {
		if s.organizationGroupID(session) == group && session.Unread {
			return true
		}
	}
	return false
}

func (s *tuiState) hostSyncLabel() string {
	if len(s.hostSync) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.hostSync))
	for name := range s.hostSync {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		update := s.hostSync[name]
		state := strings.ToUpper(update.State)
		if state == "" {
			state = "SYNCING"
		}
		if update.Gap {
			state = "RESYNCED-GAP"
		}
		parts = append(parts, fmt.Sprintf("%s:%s r%d", displayField(name), state, update.Revision))
	}
	return "  " + strings.Join(parts, "  ")
}

const (
	modalSurface  = "\033[38;2;211;222;235;48;2;24;34;49m"
	modalReset    = "\033[0m"
	modalBorder   = "\033[38;2;78;116;139m"
	modalTitle    = "\033[1;38;2;129;212;250m"
	modalMuted    = "\033[38;2;148;163;184m"
	modalStatus   = "\033[38;2;250;204;21m"
	modalInput    = "\033[38;2;167;243;208m"
	modalSelected = "\033[1;38;2;255;255;255;48;2;36;83;107m"
	modalDisabled = "\033[38;2;100;116;139m"
	modalDanger   = "\033[1;38;2;255;255;255;48;2;190;24;93m"
)

func mouseCursorShape(shape string) string {
	if shape != "grabbing" {
		shape = "default"
	}
	return "\033]22;" + shape + "\033\\"
}

func shortcutInput(binding string) string {
	switch binding {
	case "up":
		return "\x1b[A"
	case "down":
		return "\x1b[B"
	case "right":
		return "\x1b[C"
	case "left":
		return "\x1b[D"
	case "pageup":
		return "\x1b[5~"
	case "pagedown":
		return "\x1b[6~"
	}
	if binding == "enter" {
		return "\r"
	}
	if strings.HasPrefix(binding, "ctrl-") {
		runes := []rune(strings.TrimPrefix(binding, "ctrl-"))
		if len(runes) == 1 {
			return string(byte(unicode.ToUpper(runes[0])) & 0x1f)
		}
	}
	return binding
}

func (s *tuiState) shortcut(action, input string) bool {
	return input == shortcutInput(s.cfg.Shortcut(action))
}

func (s *tuiState) sessionShortcut(input string) bool {
	for _, action := range []string{"session_actions", "session_notifications", "session_yield", "session_yield_wait", "session_end", "session_restart", "session_destroy"} {
		if s.shortcut(action, input) {
			return true
		}
	}
	return false
}

type modalRenderLine struct {
	style string
	text  string
}

func renderModalBox(out io.Writer, cols, rows int, lines []modalRenderLine) {
	renderModalBoxWidth(out, cols, rows, 72, lines)
}

// renderModalBoxWidth keeps the rendered frame, its clipping, and mouse layout
// on the same width for wide modals such as Project files.
func renderModalBoxWidth(out io.Writer, cols, rows, preferredWidth int, lines []modalRenderLine) {
	renderModalBoxWidthInternal(out, cols, rows, preferredWidth, lines, false)
}

// renderModalBoxWidthANSI is reserved for callers that compose trusted SGR
// styling with independently sanitized dynamic text.
func renderModalBoxWidthANSI(out io.Writer, cols, rows, preferredWidth int, lines []modalRenderLine) {
	renderModalBoxWidthInternal(out, cols, rows, preferredWidth, lines, true)
}

func renderModalBoxWidthInternal(out io.Writer, cols, rows, preferredWidth int, lines []modalRenderLine, allowANSI bool) {
	if cols < 8 || rows < 3 || len(lines) == 0 {
		return
	}
	maxLines := rows - 2
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	boxWidth := min(preferredWidth, max(8, cols-2))
	innerWidth := boxWidth - 2
	boxHeight := len(lines) + 2
	top := max(1, (rows-boxHeight)/2+1)
	left := max(1, (cols-boxWidth)/2+1)
	fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
	for index, line := range lines {
		textValue := modalDisplayText(line.text)
		if allowANSI {
			textValue = modalDisplayANSI(line.text)
		}
		text := modalCellPad(modalCellTruncate(textValue, innerWidth), innerWidth)
		fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", top+index+1, left, modalSurface+modalBorder, modalSurface+line.style, text, modalReset+modalSurface+modalBorder, modalReset)
	}
	fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+boxHeight-1, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
}

func (s *tuiState) renderContent(out io.Writer, x, width, height int) {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		return
	}
	sess := s.sessions[s.selected]
	header := fmt.Sprintf("%s / %s  %s  %s", displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), displayField(sessionTypeLabel(sess)))
	if sess.Status == string(model.StatusStopped) && sess.RetainedOutputBytes > 0 {
		header += fmt.Sprintf("  retained:%s until %s", formatByteCount(sess.RetainedOutputBytes), formatRetainedUntil(sess.RetainedOutputUntilMS))
	}
	if s.focused {
		header += "  [focus]"
	}
	if s.outputStale {
		if s.outputReconnecting {
			header += "  [RECONNECTING PTY…]"
		} else {
			header += "  [STALE SNAPSHOT]"
		}
	}
	fmt.Fprintf(out, "\033[5;%dH%s\033[K", x, truncate(header, width))
	startRow := 6
	if s.outputErr != "" && s.outputText == "" && s.terminal == nil {
		for i, line := range wrapDisplayText("error: "+sanitizeTerminalText(s.outputErr), width) {
			if startRow+i > height {
				break
			}
			fmt.Fprintf(out, "\033[%d;%dH%s\033[K", startRow+i, x, line)
		}
		return
	}
	lines := tailLines(strings.Split(strings.TrimRight(s.outputText, "\n"), "\n"), height-startRow+1)
	styled := false
	if terminal := s.viewportTerminal(); terminal != nil {
		lines = terminal.RenderLinesOffset(height-startRow+1, width, s.ptyScrollOffset)
		styled = true
	}
	for i, line := range lines {
		if styled {
			fmt.Fprintf(out, "\033[%d;%dH%s\033[K", startRow+i, x, line)
		} else {
			fmt.Fprintf(out, "\033[%d;%dH%s\033[K", startRow+i, x, truncate(line, width))
		}
	}
}

func (s *tuiState) lifecycleTargetIsCurrent() bool {
	target := s.lifecycleTarget
	if target.Client == "" || target.InstanceID == "" || target.SessionID == "" || !s.hostIsLive(target.Client) {
		return false
	}
	for _, session := range s.sessions {
		if session.Client != target.Client || session.InstanceID != target.InstanceID || session.SessionID != target.SessionID {
			continue
		}
		return sameSessionRevision(session, target)
	}
	return false
}
