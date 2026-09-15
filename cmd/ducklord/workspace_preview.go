package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) workspaceNavigation() (*ducklord.WorkspaceState, error) {
	layout := &s.activity().ProjectLayout
	if s.workspaceNav == nil {
		nav, err := ducklord.NewWorkspaceState(layout)
		if err != nil {
			return nil, err
		}
		s.workspaceNav = nav
		// Initial quick-list selection establishes the first visible Project.
		if identity, ok := ducklord.IdentityFromSession(s.currentSession()); ok {
			_ = nav.SelectQuickSession(identity)
		}
	} else if err := s.workspaceNav.RebindLayout(layout); err != nil {
		return nil, err
	}
	return s.workspaceNav, nil
}

// Only an explicit quick-list selection navigates the Terminal area. Async
// inventory refreshes may change the quick cursor's fallback row, but must
// never pull a focused Session pane out from under the user's keyboard.
func (s *tuiState) workspaceFollowQuickSelection() bool {
	if !s.workspacePreview || s.workspaceProjectFocus || s.focused {
		return true
	}
	identity, ok := ducklord.IdentityFromSession(s.currentSession())
	if !ok {
		return true
	}
	if err := s.restoreWorkspaceDefaultPane(identity); err != nil {
		s.outputErr = err.Error()
		return false
	}
	nav, err := s.workspaceNavigation()
	if err == nil {
		err = nav.SelectQuickSession(identity)
	}
	if err != nil {
		s.outputErr = err.Error()
		return false
	}
	return true
}

// handleQuickSelection keeps a failed local reopen from tearing down the
// previous preview or replacing the persistence error with loading status.
func (s *tuiState) handleQuickSelection(control **ducklord.ControlSession, controlDone *<-chan error, requestPreview func(bool)) {
	if !s.workspaceFollowQuickSelection() {
		return
	}
	if *control != nil && !s.focused {
		_ = (*control).Stdin.Close()
		*control, *controlDone = nil, nil
	}
	// List navigation owns the preview pane. Never leave a stale attach
	// identity pointing at the previously selected session.
	s.clearAttachIdentity()
	s.outputForKey = s.currentKey()
	s.outputText = ""
	s.terminal = nil
	s.ptyScrollOffset = 0
	s.outputErr = "loading live preview..."
	requestPreview(true)
}

// Search must not publish activation or synthesize focus until its local pane
// navigation succeeds. Keep failures visible in the still-open search modal.
func (s *tuiState) prepareSearchWorkspaceSelection(key string) bool {
	if !s.selectSessionKey(key) {
		s.searchErr = "selected session is unavailable"
	} else if s.workspaceFollowQuickSelection() {
		return true
	} else {
		s.searchErr = s.outputErr
	}
	s.searchPendingRequestID = 0
	return false
}

// Reopening is an explicit navigation action, never a side effect of rendering
// or inventory discovery. Publish only after persistence succeeds.
func (s *tuiState) restoreWorkspaceDefaultPane(identity ducklord.SessionIdentity) error {
	next := s.activity().Clone()
	changed, err := next.ProjectLayout.RestoreDefaultPane(identity)
	if err != nil || !changed {
		return err
	}
	if err := s.activityStore.Save(next); err != nil {
		return fmt.Errorf("reopen Session pane: %w", err)
	}
	s.activityState = next
	s.workspacePaneChanged = true
	return nil
}

// The Quick column omits sessions without a stable identity. Mouse row lookup
// must use this exact projection, not the unfiltered inventory index.
func (s *tuiState) workspaceQuickSessions() []ducklord.RemoteSession {
	rows := make([]ducklord.RemoteSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		if _, ok := ducklord.IdentityFromSession(session); ok {
			rows = append(rows, session)
		}
	}
	return rows
}

func workspacePaneTitle(session ducklord.RemoteSession) string {
	title := displayField(session.Client) + "/" + displayField(session.Name)
	if session.Kind == string(model.KindShell) {
		switch session.DetectedForeground {
		case "other_agent":
			title += " [other agent?]"
		case "codex", "claude":
			title += " [" + session.DetectedForeground + "?]"
		}
	}
	return title
}

// Read-only is a presentation hint, not an authorization decision. Keep it
// aligned with the attach gate so an offline Shell is not shown as writable.
func (s *tuiState) workspaceSessionReadOnly(session ducklord.RemoteSession) bool {
	if s.disconnectedHosts[session.Client] || !s.hostIsLive(session.Client) || !canAttach(session) {
		return true
	}
	return session.Kind != string(model.KindShell) &&
		(session.WriterKind != string(model.OwnerTerminal) || session.WriterID != s.ownerName)
}

func (s *tuiState) workspaceColumnOffsets(geometry ducklord.WorkspaceGeometry, nav *ducklord.WorkspaceState, quick []ducklord.RemoteSession) ducklord.WorkspaceColumnOffsets {
	layout := &s.activity().ProjectLayout
	projectIndex := -1
	for i := range layout.Projects {
		if layout.Projects[i].ID == nav.CurrentProjectID() {
			projectIndex = i
			break
		}
	}
	quickIndex := -1
	selectedKey := sessionKey(s.currentSession())
	for i := range quick {
		if sessionKey(quick[i]) == selectedKey {
			quickIndex = i
			break
		}
	}
	s.workspaceProjectOffset = ducklord.WorkspaceListOffset(s.workspaceProjectOffset, projectIndex, geometry.Projects.Height-1, len(layout.Projects))
	s.workspaceQuickOffset = ducklord.WorkspaceListOffset(s.workspaceQuickOffset, quickIndex, geometry.Quick.Height-1, len(quick))
	return ducklord.WorkspaceColumnOffsets{Projects: s.workspaceProjectOffset, Quick: s.workspaceQuickOffset}
}

func (s *tuiState) workspaceSelectedPaneSession() (ducklord.RemoteSession, error) {
	nav, err := s.workspaceNavigation()
	if err != nil {
		return ducklord.RemoteSession{}, err
	}
	if nav.InDetailMode() {
		if session, ok := s.detailSelectionSession(); ok && session.Status == string(model.StatusRunning) && s.hostIsLive(session.Client) && canRead(session) {
			return session, nil
		}
		return ducklord.RemoteSession{}, fmt.Errorf("selected detailed Session has no live host connection")
	}
	identity, ok := s.activity().ProjectLayout.PaneSession(nav.CurrentProjectID(), nav.CurrentPaneID())
	if !ok {
		return ducklord.RemoteSession{}, fmt.Errorf("selected Project has no Session pane")
	}
	width, height := terminalSize()
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	if _, visible := ducklord.WorkspaceVisiblePaneRect(&s.activity().ProjectLayout, nav, geometry); !visible {
		return ducklord.RemoteSession{}, fmt.Errorf("selected Session pane is hidden by terminal size")
	}
	for _, selection := range s.workspaceVisibleSelections(ducklord.TerminalSelection{}) {
		if selection.InstanceID == identity.InstanceID && selection.SessionID == identity.SessionID {
			key := ducklord.OutputKey{ClientKey: selection.Client.Name, InstanceID: identity.InstanceID, SessionID: identity.SessionID}
			for _, session := range s.sessions {
				if candidate, ok := terminalOutputKey(session); ok && candidate == key {
					return session, nil
				}
			}
		}
	}
	return ducklord.RemoteSession{}, fmt.Errorf("selected Session pane has no live host connection")
}

// workspacePreferredSelection keeps output priority on the selected Project
// pane without changing the independent quick-list selection.
func (s *tuiState) workspacePreferredSelection(visible []ducklord.TerminalSelection, quick ducklord.RemoteSession) ducklord.TerminalSelection {
	if len(visible) == 0 {
		return ducklord.TerminalSelection{}
	}
	if nav, err := s.workspaceNavigation(); err == nil {
		if identity, ok := s.activity().ProjectLayout.PaneSession(nav.CurrentProjectID(), nav.CurrentPaneID()); ok {
			for _, candidate := range visible {
				if candidate.InstanceID == identity.InstanceID && candidate.SessionID == identity.SessionID {
					return candidate
				}
			}
		}
	}
	for _, candidate := range visible {
		if candidate.Client.Name == quick.Client && candidate.InstanceID == quick.InstanceID && candidate.SessionID == quick.SessionID {
			return candidate
		}
	}
	return visible[0]
}

// P is a read-only Project-list focus switch. Project movement never changes
// the quick-list selection or grants PTY control.
func (s *tuiState) handleWorkspaceProjectInput(input []byte) (handled, changed bool) {
	if !s.workspacePreview {
		return false, false
	}
	key := string(input)
	if s.shortcut("project_focus", key) {
		s.workspaceProjectFocus = !s.workspaceProjectFocus
		return true, false
	}
	if !s.workspaceProjectFocus {
		return false, false
	}
	if s.shortcut("project_notification_focus", key) {
		if nav, err := s.workspaceNavigation(); err == nil {
			nav.ToggleNotificationFocus()
		}
		return true, false
	}
	if s.shortcut("project_create", key) {
		s.beginWorkspaceProject()
		return true, false
	}
	if s.shortcut("project_delete", key) {
		s.beginWorkspaceProjectDelete()
		return true, false
	}
	if s.shortcut("project_add_pane", key) {
		s.beginWorkspacePane()
		return true, false
	}
	if s.shortcut("project_move_pane", key) {
		s.beginWorkspaceMove()
		return true, false
	}
	if s.shortcut("project_detach_pane", key) {
		s.beginWorkspaceDetach()
		return true, false
	}
	if key == "\r" {
		if _, err := s.workspaceSelectedPaneSession(); err != nil {
			s.outputErr = err.Error()
			return true, false
		}
		s.workspaceProjectFocus = false
		s.workspaceAttachFromProject = true
		s.workspaceFocusFromProject = true
		s.selectedGroupID = ""
		return false, false // the normal attach action consumes this Enter
	}
	if key == "\x1b" || key == "\t" {
		s.workspaceProjectFocus = false
		return true, false
	}
	if s.shortcut("project_prev_tab", key) || s.shortcut("project_next_tab", key) ||
		s.shortcut("project_prev_pane", key) || s.shortcut("project_next_pane", key) {
		nav, err := s.workspaceNavigation()
		if err == nil {
			switch {
			case s.shortcut("project_prev_tab", key):
				err = nav.CycleTab(-1)
			case s.shortcut("project_next_tab", key):
				err = nav.CycleTab(1)
			case s.shortcut("project_prev_pane", key):
				width, height := terminalSize()
				err = nav.CycleVisiblePane(ducklord.CalculateWorkspaceGeometry(width, height, 4), -1)
			case s.shortcut("project_next_pane", key):
				width, height := terminalSize()
				err = nav.CycleVisiblePane(ducklord.CalculateWorkspaceGeometry(width, height, 4), 1)
			}
		}
		if err != nil {
			s.outputErr = err.Error()
			return true, false
		}
		s.outputErr = ""
		return true, true
	}
	if key != "j" && key != "k" && key != "\x1b[A" && key != "\x1b[B" {
		return true, false
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return true, false
	}
	projects := s.activity().ProjectLayout.Projects
	index := 0
	for i := range projects {
		if projects[i].ID == nav.CurrentProjectID() {
			index = i
			break
		}
	}
	if key == "j" || key == "\x1b[B" {
		index = min(len(projects)-1, index+1)
	} else {
		index = max(0, index-1)
	}
	if len(projects) == 0 || projects[index].ID == nav.CurrentProjectID() {
		return true, false
	}
	if err := nav.SelectProject(projects[index].ID); err != nil {
		s.outputErr = err.Error()
		return true, false
	}
	s.outputErr = ""
	return true, true
}

func (s *tuiState) workspacePaneRectAt(width, height int) (ducklord.WorkspaceRect, error) {
	selected := s.activePTYSession()
	identity, ok := ducklord.IdentityFromSession(selected)
	if !ok {
		return ducklord.WorkspaceRect{}, fmt.Errorf("current Session has no stable identity")
	}
	layout := &s.activity().ProjectLayout
	nav, err := s.workspaceNavigation()
	if err != nil {
		return ducklord.WorkspaceRect{}, err
	}
	if nav.InDetailMode() {
		if actual := nav.DetailSelection(); actual != identity || actual != s.detailSelected {
			return ducklord.WorkspaceRect{}, fmt.Errorf("active detailed Session no longer matches PTY Session")
		}
		rect := ducklord.CalculateDetailGeometry(width, height, 4).Pane
		if rect.Width < 1 || rect.Height < 2 {
			return ducklord.WorkspaceRect{}, fmt.Errorf("detailed Session pane is hidden by terminal size")
		}
		return rect, nil
	}
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	actual, ok := layout.PaneSession(nav.CurrentProjectID(), nav.CurrentPaneID())
	if !ok || actual != identity {
		return ducklord.WorkspaceRect{}, fmt.Errorf("active pane no longer matches PTY Session")
	}
	rect, visible := ducklord.WorkspaceVisiblePaneRect(layout, nav, geometry)
	if !visible {
		return ducklord.WorkspaceRect{}, fmt.Errorf("current Session pane is not visible")
	}
	return rect, nil
}

func (s *tuiState) workspacePaneRect() (ducklord.WorkspaceRect, error) {
	width, height := terminalSize()
	return s.workspacePaneRectAt(width, height)
}

func (s *tuiState) workspaceVisibleSelections(selected ducklord.TerminalSelection) []ducklord.TerminalSelection {
	width, height := terminalSize()
	layout := &s.activity().ProjectLayout
	nav, err := s.workspaceNavigation()
	if err != nil {
		return nil
	}
	if nav.InDetailMode() {
		if s.detailSelected.Key() == "" || nav.DetailSelection() != s.detailSelected {
			return nil
		}
		rect := ducklord.CalculateDetailGeometry(width, height, 4).Pane
		for _, session := range s.sessions {
			identity, ok := ducklord.IdentityFromSession(session)
			if !ok || identity != s.detailSelected || !canRead(session) || session.Status != string(model.StatusRunning) ||
				!s.hostIsLive(session.Client) || session.RuntimeGeneration == 0 {
				continue
			}
			client, err := mustClient(s.cfg, session.Client)
			if err == nil {
				return []ducklord.TerminalSelection{{Client: client, InstanceID: session.InstanceID, SessionID: session.SessionID,
					RuntimeGeneration: session.RuntimeGeneration, Rows: max(1, rect.Height-1), Cols: max(1, rect.Width)}}
			}
		}
		return nil
	}
	panes := ducklord.WorkspaceVisiblePaneRects(layout, nav, ducklord.CalculateWorkspaceGeometry(width, height, 4))
	result := make([]ducklord.TerminalSelection, 0, len(panes))
	seen := make(map[ducklord.SessionIdentity]bool, len(panes))
	for _, pane := range panes {
		if seen[pane.Identity] {
			continue
		}
		seen[pane.Identity] = true
		var chosen ducklord.RemoteSession
		for _, session := range s.sessions {
			identity, ok := ducklord.IdentityFromSession(session)
			if !ok || identity != pane.Identity || !canRead(session) || session.Status != string(model.StatusRunning) ||
				!s.hostIsLive(session.Client) || session.RuntimeGeneration == 0 {
				continue
			}
			if chosen.SessionID == "" || session.Client == selected.Client.Name {
				chosen = session
			}
			if session.Client == selected.Client.Name {
				break
			}
		}
		if chosen.SessionID == "" {
			continue
		}
		client, err := mustClient(s.cfg, chosen.Client)
		if err != nil {
			continue
		}
		result = append(result, ducklord.TerminalSelection{Client: client, InstanceID: chosen.InstanceID, SessionID: chosen.SessionID,
			RuntimeGeneration: chosen.RuntimeGeneration, Rows: max(1, pane.Rect.Height-1), Cols: max(1, pane.Rect.Width)})
	}
	return result
}

// renderWorkspacePreview draws the default Project, quick-list, and Terminal
// area workspace. The legacy renderer remains a temporary test escape hatch.
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
	direction := "newest"
	if s.cfg != nil && s.cfg.QuickOldestFirst {
		direction = "oldest"
	}
	status := "Session list pane: ↑/↓ Session · " + s.cfg.Shortcut("list_sort") + " sort:" + s.quickSortMode() + " (" + direction + ") · " + s.cfg.Shortcut("list_sort_direction") + " time direction · " + s.cfg.Shortcut("project_focus") + " Project pane · Enter focus · Ctrl-] leave PTY"
	if s.workspaceProjectFocus {
		status = fmt.Sprintf("Project pane: ↑/↓ Project · %s focus · %s new · %s delete · %s add · %s move · %s detach · %s/%s tab · %s/%s pane · Enter focus · Esc list",
			s.cfg.Shortcut("project_notification_focus"),
			s.cfg.Shortcut("project_create"), s.cfg.Shortcut("project_delete"), s.cfg.Shortcut("project_add_pane"), s.cfg.Shortcut("project_move_pane"), s.cfg.Shortcut("project_detach_pane"),
			s.cfg.Shortcut("project_prev_tab"), s.cfg.Shortcut("project_next_tab"),
			s.cfg.Shortcut("project_prev_pane"), s.cfg.Shortcut("project_next_pane"))
	}
	if s.focused {
		status = "Session focus: keys go to PTY · Ctrl-] return to list"
		if !s.canResizeCurrentSession() {
			status = "Session focus: read-only · yield control to send input · Ctrl-] return to list"
		}
	}
	if s.workspaceNav != nil && s.workspaceNav.InDetailMode() && !s.focused {
		status = s.detailStatusLine()
	}
	if s.workspaceNav != nil && s.workspaceNav.NotificationFocusProjectID() != "" {
		focused := s.workspaceNav.NotificationFocusProjectID()
		if project := s.activity().ProjectLayout.Project(focused); project != nil {
			focused = displayField(project.Name)
		}
		status = "Focus: " + focused + " (others ≥ " + string(s.cfg.FocusThreshold()) + ") · " + status
	}
	if s.outputErr != "" {
		status += "  " + sanitizeTerminalText(s.outputErr)
	}
	fmt.Fprintln(out, truncate(status, width))
	fmt.Fprintln(out, strings.Repeat("─", width))

	layout := &s.activity().ProjectLayout
	nav, err := s.workspaceNavigation()
	if err != nil {
		fmt.Fprintln(out, truncate("Project layout unavailable: "+sanitizeTerminalText(err.Error()), width))
		return
	}
	if nav.InDetailMode() {
		results := s.detailedResults()
		focus := ducklord.WorkspaceFocusSessions
		if s.focused {
			focus = ducklord.WorkspaceFocusTerminal
		}
		theme := ducklord.WorkspaceTheme{}
		if s.cfg != nil {
			theme = s.cfg.WorkspaceTheme
		}
		ducklord.RenderDetailedSessionBodyWithOptions(out, ducklord.CalculateDetailGeometry(width, height, 4), results, s.detailSelected,
			s.detailQuery, s.detailFilter, s.focused, func(identity ducklord.SessionIdentity, cols, rows int) ducklord.WorkspacePaneView {
				session, ok := s.detailSelectionSession()
				if !ok {
					return ducklord.WorkspacePaneView{Title: "Session unavailable", Stale: true, ReadOnly: true}
				}
				view := ducklord.WorkspacePaneView{Title: workspacePaneTitle(session),
					Stale:    !s.outputFresh || s.outputStale || !s.hostIsLive(session.Client),
					ReadOnly: s.workspaceSessionReadOnly(session),
					Focused:  s.focused && s.activeAttachKey == sessionKey(session)}
				if s.terminal != nil && s.outputForKey == sessionKey(session) && s.terminalGeneration == session.RuntimeGeneration {
					view.Lines = s.terminal.RenderPaneLinesOffset(rows, cols, s.ptyScrollOffset)
				} else {
					view.Lines = []string{sanitizeTerminalText(session.LastLine), "PTY output unavailable"}
				}
				return view
			}, ducklord.WorkspaceRenderOptions{Focus: focus, Theme: theme})
		s.renderHelpModal(out, width, height)
		return
	}
	selected := s.currentSession()
	displayed := s.activePTYSession()
	if s.workspaceOutput != nil && s.outputForKey != "" {
		if outputSession, ok := s.sessionForKey(s.outputForKey); ok {
			displayed = outputSession
		}
	}
	quickSessions := s.workspaceQuickSessions()
	items := make([]ducklord.WorkspaceListItem, 0, len(quickSessions))
	for _, session := range quickSessions {
		identity, _ := ducklord.IdentityFromSession(session)
		items = append(items, ducklord.WorkspaceListItem{Identity: identity, Name: displayField(session.Name), Host: displayField(session.Client),
			Unread: session.Unread, Selected: sessionKey(session) == sessionKey(selected)})
	}
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	offsets := s.workspaceColumnOffsets(geometry, nav, quickSessions)
	var visibleOutput map[ducklord.SessionIdentity]ducklord.TerminalSelection
	if s.workspaceOutput != nil {
		visibleOutput = make(map[ducklord.SessionIdentity]ducklord.TerminalSelection)
		for _, selection := range s.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: selected.Client}}) {
			visibleOutput[ducklord.SessionIdentity{InstanceID: selection.InstanceID, SessionID: selection.SessionID}] = selection
		}
	}
	focus := ducklord.WorkspaceFocusSessions
	if s.workspaceProjectFocus {
		focus = ducklord.WorkspaceFocusProjects
	}
	if s.focused {
		focus = ducklord.WorkspaceFocusTerminal
	}
	theme := ducklord.WorkspaceTheme{}
	if s.cfg != nil {
		theme = s.cfg.WorkspaceTheme
	}
	ducklord.RenderWorkspaceBodyWithOptions(out, geometry, layout, nav, items, func(projectID string) bool {
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
			if selection, live := visibleOutput[identity]; live && session.Client != selection.Client.Name {
				continue // title, ACL and framebuffer must come from the same host alias
			}
			view := ducklord.WorkspacePaneView{Title: workspacePaneTitle(session),
				Stale: true, ReadOnly: s.workspaceSessionReadOnly(session)}
			if sessionKey(session) != sessionKey(displayed) {
				if selection, ok := visibleOutput[identity]; ok && s.workspaceOutput != nil {
					key := ducklord.OutputKey{ClientKey: selection.Client.Name, InstanceID: selection.InstanceID, SessionID: selection.SessionID}
					if live, err := s.workspaceOutput.PaneView(key); err == nil && live.Ready && !live.Disconnected && !live.Ended &&
						live.RuntimeGeneration == selection.RuntimeGeneration {
						if terminal, valid := ducklord.NewTerminalFromState(live.Framebuffer, ducklord.DefaultTerminalScrollback); valid {
							view.Lines = terminal.RenderPaneLinesOffset(rows, cols, 0)
							view.Stale = false
							return view
						}
					}
				}
				view.Lines = []string{sanitizeTerminalText(session.LastLine), "PTY output unavailable"}
				return view
			}
			view.Focused = s.focused && s.activeAttachKey == sessionKey(session)
			view.Stale = !s.outputFresh || s.outputStale || !s.hostIsLive(session.Client) || s.outputForKey != sessionKey(session)
			if s.terminal != nil && s.outputForKey == sessionKey(session) {
				view.Lines = s.terminal.RenderPaneLinesOffset(rows, cols, s.ptyScrollOffset)
			} else if s.outputText != "" {
				view.Lines = tailLines(strings.Split(strings.TrimRight(s.outputText, "\n"), "\n"), rows)
			}
			return view
		}
		return ducklord.WorkspacePaneView{Title: "Session unavailable", Stale: true}
	}, ducklord.WorkspaceRenderOptions{Offsets: offsets, Focus: focus, Theme: theme})
	s.renderCreateModal(out, width, height)
	s.renderWorkspacePaneModal(out, width, height)
	s.renderSearchModal(out, width, height)
	s.renderActionModal(out, width, height)
	s.renderAddClientModal(out, width, height)
	s.renderRemoveClientModal(out, width, height)
	s.renderHelpModal(out, width, height)
	s.renderShortcutModal(out, width, height)
	s.renderNotificationConfigModal(out, width, height)
	s.renderHostModal(out, width, height)
	s.renderGroupModal(out, width, height)
	s.renderNotificationModal(out, width, height)
	s.renderLifecycleModal(out, width, height)
	if pane, err := s.workspacePaneRectAt(width, height); err == nil && s.focused && s.terminal != nil && pane.Height > 1 && pane.Width > 0 &&
		!s.outputStale && s.ptyScrollOffset == 0 {
		if row, col, visible := s.terminal.PaneCursorPosition(pane.Height-1, pane.Width); visible {
			fmt.Fprintf(out, "\033[%d;%dH\033[?25h", pane.Y+1+row, pane.X+col)
		}
	}
}
