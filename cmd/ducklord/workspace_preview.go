package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// renderWorkspacePreview is the first live TUI integration of the Project
// renderer. The legacy event loop still owns control and one output stream;
// it is deliberately opt-in until multi-pane output and navigation are wired.
func (s *tuiState) renderWorkspacePreview(out io.Writer) {
	width, height := terminalSize()
	s.renderWorkspacePreviewAt(out, width, height)
}

func (s *tuiState) renderWorkspacePreviewAt(out io.Writer, width, height int) {
	if width < 1 || height < 4 {
		return
	}
	fmt.Fprint(out, "\033[?25l\033[H\033[2J")
	fmt.Fprintln(out, truncate("ducklord workspace  owner:"+displayField(s.ownerName)+s.hostSyncLabel(), width))
	status := "Preview: ↑/↓ select · Enter focus · Ctrl-] leave PTY"
	if s.focused {
		status = "Session focus: keys go to PTY · Ctrl-] return to list"
	}
	if s.outputErr != "" {
		status += "  " + sanitizeTerminalText(s.outputErr)
	}
	fmt.Fprintln(out, truncate(status, width))
	fmt.Fprintln(out, strings.Repeat("─", width))

	layout := &s.activity().ProjectLayout
	nav, err := ducklord.NewWorkspaceState(layout)
	if err != nil {
		fmt.Fprintln(out, truncate("Project layout unavailable: "+sanitizeTerminalText(err.Error()), width))
		return
	}
	selected := s.currentSession()
	if identity, ok := ducklord.IdentityFromSession(selected); ok {
		_ = nav.SelectQuickSession(identity)
		if s.focused {
			_, _ = nav.FocusVisiblePane(ducklord.CalculateWorkspaceGeometry(width, height, 4))
		}
	}
	items := make([]ducklord.WorkspaceListItem, 0, len(s.sessions))
	for _, session := range s.sessions {
		identity, ok := ducklord.IdentityFromSession(session)
		if !ok {
			continue
		}
		items = append(items, ducklord.WorkspaceListItem{Identity: identity, Name: displayField(session.Name), Host: displayField(session.Client),
			Unread: session.Unread, Selected: sessionKey(session) == sessionKey(selected)})
	}
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	ducklord.RenderWorkspaceBody(out, geometry, layout, nav, items, func(projectID string) bool {
		for _, session := range s.sessions {
			identity, ok := ducklord.IdentityFromSession(session)
			if ok && session.Unread {
				for _, member := range layout.ProjectsFor(identity) {
					if member == projectID {
						return true
					}
				}
			}
		}
		return false
	}, func(identity ducklord.SessionIdentity, cols, rows int) ducklord.WorkspacePaneView {
		for _, session := range s.sessions {
			current, ok := ducklord.IdentityFromSession(session)
			if !ok || current != identity {
				continue
			}
			view := ducklord.WorkspacePaneView{Title: displayField(session.Client) + "/" + displayField(session.Name),
				Stale: true, ReadOnly: session.Kind != string(model.KindShell) &&
					(session.WriterKind != string(model.OwnerTerminal) || session.WriterID != s.ownerName)}
			if sessionKey(session) != sessionKey(selected) {
				view.Lines = []string{sanitizeTerminalText(session.LastLine), "PTY preview pending"}
				return view
			}
			view.Focused = s.focused && s.activeAttachKey == sessionKey(session)
			view.Stale = !s.outputFresh || s.outputStale || !s.hostIsLive(session.Client) || s.outputForKey != sessionKey(session)
			if s.terminal != nil && s.outputForKey == sessionKey(session) {
				view.Lines = s.terminal.RenderLinesOffset(rows, cols, s.ptyScrollOffset)
			} else if s.outputText != "" {
				view.Lines = tailLines(strings.Split(strings.TrimRight(s.outputText, "\n"), "\n"), rows)
			}
			return view
		}
		return ducklord.WorkspacePaneView{Title: "Session unavailable", Stale: true}
	})
	s.renderCreateModal(out, width, height)
	s.renderSearchModal(out, width, height)
	s.renderActionModal(out, width, height)
	s.renderAddClientModal(out, width, height)
	s.renderRemoveClientModal(out, width, height)
	s.renderHelpModal(out, width, height)
	s.renderShortcutModal(out, width, height)
	s.renderHostModal(out, width, height)
	s.renderGroupModal(out, width, height)
	s.renderNotificationModal(out, width, height)
	s.renderLifecycleModal(out, width, height)
	if s.focused && s.terminal != nil && geometry.Terminal.Height > 2 && geometry.Terminal.Width > 0 &&
		!s.outputStale && s.ptyScrollOffset == 0 {
		if row, col, visible := s.terminal.CursorPosition(geometry.Terminal.Height-2, geometry.Terminal.Width); visible {
			fmt.Fprintf(out, "\033[%d;%dH\033[?25h", geometry.Terminal.Y+2+row, geometry.Terminal.X+col)
		}
	}
}
