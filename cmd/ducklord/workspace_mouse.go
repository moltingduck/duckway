package main

import (
	"fmt"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// handleWorkspaceMouse keeps drag-and-drop local to Ducklord. It never writes
// mouse escape sequences to a PTY or changes Ducklion writer ownership.
func (s *tuiState) handleWorkspaceMouse(input []byte) (handled, changed bool) {
	// List wheel reports are always local. In terminal focus they are consumed
	// without changing selection, preserving the exact attached Session.
	if s.workspacePreview && !s.hostScoped && !s.workspacePaneMode && !s.centralModalOpen() {
		if button, x, y, ok := parseSGRMouse(string(input)); ok && (button == 64 || button == 65) {
			width, height := terminalSize()
			geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
			if insideWorkspaceRect(geometry.Projects, x, y) || insideWorkspaceRect(geometry.Quick, x, y) {
				if s.focused {
					return true, false
				}
				nav, err := s.workspaceNavigation()
				if err != nil {
					return true, false
				}
				// Detail mode has its own session list and viewport geometry.
				// Leave its mouse behavior to the established detail handler.
				if nav.InDetailMode() {
					return true, false
				}
				delta := -3
				if button == 65 {
					delta = -delta
				}
				if insideWorkspaceRect(geometry.Projects, x, y) {
					projects := s.activity().ProjectLayout.Projects
					selected := 0
					for i := range projects {
						if projects[i].ID == nav.CurrentProjectID() {
							selected = i
							break
						}
					}
					selected = min(max(0, selected+delta), len(projects)-1)
					if len(projects) > 0 {
						s.workspaceProjectFocus = true
						s.workspaceConfigFocus = "project-pane"
						return true, nav.SelectProject(projects[selected].ID) == nil
					}
					return true, false
				}
				quick := s.workspaceQuickSessions()
				if len(quick) == 0 {
					return true, false
				}
				selected := 0
				current := sessionKey(s.currentSession())
				for i := range quick {
					if sessionKey(quick[i]) == current {
						selected = i
						break
					}
				}
				selected = min(max(0, selected+delta), len(quick)-1)
				s.workspaceProjectFocus = false
				s.workspaceConfigFocus = "session-list"
				if s.selectSessionKey(sessionKey(quick[selected])) {
					return true, s.workspaceFollowQuickSelection()
				}
				return true, false
			}
		}
	}
	if s.workspacePreview && !s.focused && !s.hostScoped && !s.workspacePaneMode && !s.centralModalOpen() {
		if button, x, y, ok := parseSGRMouse(string(input)); ok {
			width, height := terminalSize()
			geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
			nav, navErr := s.workspaceNavigation()
			if s.workspaceScrollbarDrag != "" {
				if strings.HasSuffix(string(input), "m") {
					s.workspaceScrollbarDrag = ""
					s.workspaceScrollbarDragGrab = 0
					return true, false
				}
				if navErr == nil && button == 32 && strings.HasSuffix(string(input), "M") {
					return true, s.workspaceScrollTo(geometry, nav, s.workspaceScrollbarDrag, y-s.workspaceScrollbarDragGrab)
				}
			}
			if button == 0 && strings.HasSuffix(string(input), "M") && navErr == nil && !nav.InDetailMode() {
				quick := s.workspaceQuickSessions()
				offsets := s.workspaceColumnOffsets(geometry, nav, quick)
				if bar, yes := ducklord.CalculateWorkspaceScrollbar(geometry.Projects, len(s.activity().ProjectLayout.Projects), offsets.Projects); yes && x == bar.TrackX && y >= bar.TrackY && y < bar.TrackY+bar.TrackHeight {
					if y >= bar.ThumbY && y < bar.ThumbY+bar.ThumbHeight {
						s.workspaceScrollbarDrag = "projects"
						s.workspaceScrollbarDragGrab = y - bar.ThumbY
						return true, false
					}
					page := max(1, geometry.Projects.Height-1)
					if y < bar.ThumbY {
						page = -page
					}
					return true, s.workspaceScrollPage(geometry, nav, "projects", page)
				}
				if bar, yes := ducklord.CalculateWorkspaceScrollbar(geometry.Quick, len(quick), offsets.Quick); yes && x == bar.TrackX && y >= bar.TrackY && y < bar.TrackY+bar.TrackHeight {
					if y >= bar.ThumbY && y < bar.ThumbY+bar.ThumbHeight {
						s.workspaceScrollbarDrag = "sessions"
						s.workspaceScrollbarDragGrab = y - bar.ThumbY
						return true, false
					}
					page := max(1, geometry.Quick.Height-1)
					if y < bar.ThumbY {
						page = -page
					}
					return true, s.workspaceScrollPage(geometry, nav, "sessions", page)
				}
			}
		}
	}
	if !s.workspacePreview || s.focused || s.hostScoped || s.workspacePaneMode || s.centralModalOpen() {
		s.workspaceDragSession = ducklord.RemoteSession{}
		s.workspaceDragKind, s.workspaceDragProjectID, s.workspaceDragTabID = "", "", ""
		s.workspaceDragMoved = false
		return false, false
	}
	key := string(input)
	if !strings.HasPrefix(key, "\x1b[<") {
		s.workspaceDragSession = ducklord.RemoteSession{}
		s.workspaceDragKind, s.workspaceDragProjectID, s.workspaceDragTabID = "", "", ""
		s.workspaceDragMoved = false
		return false, false
	}
	button, x, y, ok := parseSGRMouse(key)
	if !ok {
		return true, false
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.workspaceDragSession = ducklord.RemoteSession{}
		s.workspaceDragKind, s.workspaceDragProjectID, s.workspaceDragTabID = "", "", ""
		s.workspaceDragMoved = false
		return false, false
	}
	width, height := terminalSize()
	if button == 2 && strings.HasSuffix(key, "M") {
		s.workspaceDragSession = ducklord.RemoteSession{}
		s.workspaceDragMoved = false
		s.workspaceMouseFocus = false
		return true, s.openWorkspaceContextConfig(x, y, width, height)
	}
	if nav.InDetailMode() {
		if button != 0 || !strings.HasSuffix(key, "M") {
			return true, false
		}
		geometry := ducklord.CalculateDetailGeometry(width, height, 4)
		s.workspaceConfigFocus = ""
		if insideWorkspaceRect(geometry.Pane, x, y) && (y == geometry.Pane.Y || s.detailSelected.Key() == "") {
			s.workspaceConfigFocus = "terminal"
			s.workspaceMouseFocus = false
			return true, false
		}
		if insideWorkspaceRect(geometry.List, x, y) {
			s.workspaceConfigFocus = "session-list"
			s.workspaceProjectFocus = false
			s.workspaceMouseFocus = false
		}
		if insideWorkspaceRect(geometry.Pane, x, y) && s.detailSelected.Key() != "" {
			s.detailSearchFocused = false
			s.workspaceMouseFocus = true
			return true, true
		}
		if insideWorkspaceRect(geometry.List, x, y) {
			s.detailSearchFocused = y == geometry.List.Y+1
			results := s.detailedResults()
			selected := -1
			for i := range results {
				if results[i].Identity == s.detailSelected {
					selected = i
					break
				}
			}
			if index := ducklord.DetailSessionIndexAt(geometry.List, selected, len(results), x, y); index >= 0 {
				s.workspaceConfigFocus = ""
				s.detailSelected = results[index].Identity
				return true, nav.PreviewDetail(s.detailSelected) == nil
			}
		}
		return true, false
	}
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	quickSessions := s.workspaceQuickSessions()
	quickOffset := s.workspaceColumnOffsets(geometry, nav, quickSessions).Quick
	if button == 0 && strings.HasSuffix(key, "M") {
		s.workspaceDragSession = ducklord.RemoteSession{}
		s.workspaceDragKind, s.workspaceDragProjectID, s.workspaceDragTabID = "", "", ""
		s.workspaceDragMoved = false
		s.workspaceDragX, s.workspaceDragY = x, y
		s.workspaceConfigFocus = "terminal"
		if insideWorkspaceRect(geometry.Quick, x, y) {
			s.workspaceConfigFocus = "session-list"
			s.workspaceProjectFocus = false
			s.workspaceMouseFocus = false
		}
		if insideWorkspaceRect(geometry.Projects, x, y) {
			s.workspaceConfigFocus = "project-pane"
		}
		if index, inside := workspaceQuickRowAt(geometry.Quick, x, y); inside && index+quickOffset < len(quickSessions) {
			s.workspaceConfigFocus = ""
			s.workspaceDragSession = quickSessions[index+quickOffset]
			s.workspaceProjectFocus = false
		}
		if insideWorkspaceRect(geometry.Projects, x, y) {
			s.workspaceProjectFocus = true
			index, inside := workspaceQuickRowAt(geometry.Projects, x, y)
			index += s.workspaceColumnOffsets(geometry, nav, quickSessions).Projects
			projects := s.activity().ProjectLayout.Projects
			if inside && index < len(projects) {
				s.workspaceConfigFocus = ""
				s.workspaceDragKind = "project"
				s.workspaceDragProjectID = projects[index].ID
				return true, nav.SelectProject(projects[index].ID) == nil
			}
		}
		if y == geometry.Terminal.Y && insideWorkspaceRect(geometry.Terminal, x, y) {
			project := s.activity().ProjectLayout.Project(nav.CurrentProjectID())
			if project != nil {
				left := geometry.Terminal.X + modalCellWidth(" "+project.Name+"  ")
				for i, tab := range project.Tabs {
					cells := modalCellWidth(ducklord.WorkspaceTabLabel(tab, i, tab.ID == nav.CurrentTabID()))
					if x >= left && x < left+cells {
						s.workspaceDragKind = "tab"
						s.workspaceDragProjectID = project.ID
						s.workspaceDragTabID = tab.ID
						if id := firstWorkspacePaneID(tab.Root); id != "" {
							s.workspaceConfigFocus = "tab"
							s.workspaceProjectFocus = true
							return true, nav.SelectPane(project.ID, id) == nil
						}
					}
					left += cells
				}
				if x >= left && x < left+modalCellWidth(" [+] ") {
					s.beginWorkspaceNewTab()
					return true, false
				}
			}
		}
		for _, pane := range ducklord.WorkspaceVisibleLeafRects(&s.activity().ProjectLayout, nav, geometry) {
			if insideWorkspaceRect(pane.Rect, x, y) {
				target := pane.PaneID
				if target == "" {
					target = s.projectPaneID(nav.CurrentProjectID(), pane.Identity)
				}
				if err := nav.SelectPane(nav.CurrentProjectID(), target); err != nil {
					s.outputErr = err.Error()
					return true, false
				}
				s.workspaceProjectFocus = false
				s.workspaceMouseFocus = true
				return true, true
			}
		}
		return true, false
	}
	if button == 32 && strings.HasSuffix(key, "M") {
		if s.workspaceDragSession.Client != "" || s.workspaceDragKind != "" {
			s.workspaceDragMoved = true
		}
		return true, false
	}
	if button != 0 || !strings.HasSuffix(key, "m") {
		return true, false
	}
	source, moved := s.workspaceDragSession, s.workspaceDragMoved || x != s.workspaceDragX || y != s.workspaceDragY
	s.workspaceDragSession = ducklord.RemoteSession{}
	s.workspaceDragMoved = false
	dragKind, dragProject, dragTab := s.workspaceDragKind, s.workspaceDragProjectID, s.workspaceDragTabID
	s.workspaceDragKind, s.workspaceDragProjectID, s.workspaceDragTabID = "", "", ""
	if dragKind != "" {
		if !moved {
			return true, false
		}
		next := s.activity().Clone()
		before := false
		var err error
		dropped := false
		if dragKind == "project" {
			if index, inside := workspaceQuickRowAt(geometry.Projects, x, y); inside {
				index += s.workspaceColumnOffsets(geometry, nav, quickSessions).Projects
				projects := next.ProjectLayout.Projects
				if index >= 0 && index < len(projects) {
					dropped = true
					from := -1
					for i := range projects {
						if projects[i].ID == dragProject {
							from = i
							break
						}
					}
					before = index <= from
					err = next.ProjectLayout.MoveProject(dragProject, projects[index].ID, before)
				}
			}
		} else if dragKind == "tab" {
			project := next.ProjectLayout.Project(dragProject)
			if project != nil && insideWorkspaceRect(geometry.Terminal, x, y) && y == geometry.Terminal.Y {
				left := geometry.Terminal.X + modalCellWidth(" "+project.Name+"  ")
				for i := range project.Tabs {
					cells := modalCellWidth(ducklord.WorkspaceTabLabel(project.Tabs[i], i, project.Tabs[i].ID == nav.CurrentTabID()))
					if x >= left && x < left+cells {
						dropped = true
						before = x < left+cells/2
						err = next.ProjectLayout.MoveTab(dragProject, dragTab, project.Tabs[i].ID, before)
						break
					}
					left += cells
				}
			}
		}
		if err != nil {
			s.outputErr = sanitizeTerminalText(err.Error())
			return true, false
		}
		if !dropped {
			return true, false
		}
		if err == nil {
			if saveErr := s.activityStore.Save(next); saveErr != nil {
				s.outputErr = "save layout: " + sanitizeTerminalText(saveErr.Error())
				return true, false
			}
			s.activityState, s.workspacePaneChanged = next, true
			return true, true
		}
		return true, false
	}
	if source.Client == "" {
		return true, false
	}
	identity, valid := ducklord.IdentityFromSession(source)
	if !valid {
		return true, false
	}
	if !moved {
		if index, inside := workspaceQuickRowAt(geometry.Quick, x, y); inside && index+quickOffset < len(quickSessions) {
			candidate := quickSessions[index+quickOffset]
			if current, ok := ducklord.IdentityFromSession(candidate); ok && current == identity && candidate.RuntimeGeneration == source.RuntimeGeneration {
				s.selectSessionKey(sessionKey(candidate))
				return true, s.workspaceFollowQuickSelection()
			}
		}
		return true, false
	}
	for _, pane := range ducklord.WorkspaceVisiblePaneRects(&s.activity().ProjectLayout, nav, geometry) {
		if x < pane.Rect.X || x >= pane.Rect.X+pane.Rect.Width || y < pane.Rect.Y || y >= pane.Rect.Y+pane.Rect.Height {
			continue
		}
		targetID := s.projectPaneID(nav.CurrentProjectID(), pane.Identity)
		if targetID == "" {
			return true, false
		}
		s.beginWorkspaceDrop(source, identity, nav.CurrentProjectID(), targetID)
		return true, false
	}
	if x >= geometry.Terminal.X && x < geometry.Terminal.X+geometry.Terminal.Width &&
		y >= geometry.Terminal.Y && y < geometry.Terminal.Y+geometry.Terminal.Height {
		// Empty areas (including a Project with no panes) can only create a
		// new tab; there is no Session pane there to split.
		s.beginWorkspaceDrop(source, identity, nav.CurrentProjectID(), "")
	}
	return true, false
}

func (s *tuiState) workspaceScrollPage(g ducklord.WorkspaceGeometry, nav *ducklord.WorkspaceState, kind string, delta int) bool {
	quick := s.workspaceQuickSessions()
	if kind == "projects" {
		s.workspaceProjectFocus = true
		s.workspaceConfigFocus = "project-pane"
		projects := s.activity().ProjectLayout.Projects
		index := 0
		for i := range projects {
			if projects[i].ID == nav.CurrentProjectID() {
				index = i
				break
			}
		}
		index = min(len(projects)-1, max(0, index+delta))
		if len(projects) == 0 {
			return false
		}
		return nav.SelectProject(projects[index].ID) == nil
	}
	s.workspaceProjectFocus = false
	s.workspaceConfigFocus = "session-list"
	index := 0
	current := sessionKey(s.currentSession())
	for i := range quick {
		if sessionKey(quick[i]) == current {
			index = i
			break
		}
	}
	index = min(len(quick)-1, max(0, index+delta))
	if len(quick) == 0 {
		return false
	}
	if s.selectSessionKey(sessionKey(quick[index])) {
		return s.workspaceFollowQuickSelection()
	}
	return false
}

func (s *tuiState) workspaceScrollTo(g ducklord.WorkspaceGeometry, nav *ducklord.WorkspaceState, kind string, y int) bool {
	quick := s.workspaceQuickSessions()
	offsets := s.workspaceColumnOffsets(g, nav, quick)
	if kind == "projects" {
		projects := s.activity().ProjectLayout.Projects
		bar, ok := ducklord.CalculateWorkspaceScrollbar(g.Projects, len(projects), offsets.Projects)
		if !ok || len(projects) == 0 {
			return false
		}
		thumbTop := min(bar.TrackY+bar.TrackHeight-bar.ThumbHeight, max(bar.TrackY, y))
		offset := ducklord.WorkspaceScrollbarOffset(bar, len(projects), thumbTop)
		index := min(len(projects)-1, offset)
		s.workspaceProjectOffset = offset
		s.workspaceProjectFocus = true
		s.workspaceConfigFocus = "project-pane"
		return nav.SelectProject(projects[index].ID) == nil
	}
	bar, ok := ducklord.CalculateWorkspaceScrollbar(g.Quick, len(quick), offsets.Quick)
	if !ok || len(quick) == 0 {
		return false
	}
	thumbTop := min(bar.TrackY+bar.TrackHeight-bar.ThumbHeight, max(bar.TrackY, y))
	offset := ducklord.WorkspaceScrollbarOffset(bar, len(quick), thumbTop)
	index := min(len(quick)-1, offset)
	s.workspaceQuickOffset = offset
	s.workspaceProjectFocus = false
	s.workspaceConfigFocus = "session-list"
	if s.selectSessionKey(sessionKey(quick[index])) {
		return s.workspaceFollowQuickSelection()
	}
	return false
}

func insideWorkspaceRect(rect ducklord.WorkspaceRect, x, y int) bool {
	return rect.Width > 0 && rect.Height > 0 && x >= rect.X && x < rect.X+rect.Width && y >= rect.Y && y < rect.Y+rect.Height
}

func firstWorkspacePaneID(pane *ducklord.SessionPane) string {
	if pane == nil {
		return ""
	}
	if pane.Session != nil {
		return pane.ID
	}
	if id := firstWorkspacePaneID(pane.First); id != "" {
		return id
	}
	return firstWorkspacePaneID(pane.Second)
}

func (s *tuiState) beginWorkspaceDrop(source ducklord.RemoteSession, identity ducklord.SessionIdentity, projectID, targetID string) {
	if !s.workspacePaneCandidateCurrent(source) {
		s.outputErr = "dragged Session changed or is unavailable; try again"
		return
	}
	s.workspacePaneIntent = workspacePaneIntent{projectID: projectID, targetID: targetID}
	s.workspacePaneCandidate = source
	s.workspacePaneIdentity = identity
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "drop-placement", 0
	s.workspacePaneErr = ""
}

func workspaceQuickRowAt(rect ducklord.WorkspaceRect, x, y int) (int, bool) {
	index := y - rect.Y - 1 // column title occupies the first row
	return index, rect.Width > 0 && x >= rect.X && x < rect.X+rect.Width && index >= 0 && index < rect.Height-1
}

func (s *tuiState) projectPaneID(projectID string, identity ducklord.SessionIdentity) string {
	project := s.activity().ProjectLayout.Project(projectID)
	if project == nil {
		return ""
	}
	for _, tab := range project.Tabs {
		if id := paneIDForSession(tab.Root, identity); id != "" {
			return id
		}
	}
	return ""
}

// Pane placement is local-only. A disconnected Host must not prevent an
// already-known Session view from being reorganized, but a changed identity
// or runtime generation invalidates the selected candidate.
func (s *tuiState) workspacePaneCandidateCurrent(source ducklord.RemoteSession) bool {
	for _, candidate := range s.sessions {
		if candidate.Client == source.Client && candidate.InstanceID == source.InstanceID && candidate.SessionID == source.SessionID &&
			candidate.RuntimeGeneration == source.RuntimeGeneration && workspacePaneKnownSession(candidate) {
			return true
		}
	}
	return false
}

func (s *tuiState) placeDroppedWorkspacePane() error {
	candidate := s.workspacePaneCandidate
	identity, ok := ducklord.IdentityFromSession(candidate)
	if !ok || identity != s.workspacePaneIdentity || !s.workspacePaneCandidateCurrent(candidate) {
		return fmt.Errorf("dragged Session changed or is unavailable; try again")
	}
	if sourceID := s.projectPaneID(s.workspacePaneIntent.projectID, identity); sourceID != "" {
		if s.workspacePaneIntent.placement != ducklord.PlaceNewTab && sourceID == s.workspacePaneIntent.targetID {
			return fmt.Errorf("select a different pane to split")
		}
		s.workspacePaneSourceID = sourceID
		s.workspacePaneStep, s.workspacePaneIndex = "existing-move-confirm", 1
		return nil
	}
	return s.placeWorkspacePane(s.workspacePaneIntent, candidate)
}
