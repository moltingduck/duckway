package main

import (
	"fmt"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// handleWorkspaceMouse keeps drag-and-drop local to Ducklord. It never writes
// mouse escape sequences to a PTY or changes Ducklion writer ownership.
func (s *tuiState) handleWorkspaceMouse(input []byte) (handled, changed bool) {
	if !s.workspacePreview || s.focused || s.hostScoped || s.workspacePaneMode || s.centralModalOpen() {
		s.workspaceDragSession = ducklord.RemoteSession{}
		s.workspaceDragMoved = false
		return false, false
	}
	key := string(input)
	if !strings.HasPrefix(key, "\x1b[<") {
		s.workspaceDragSession = ducklord.RemoteSession{}
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
		for _, pane := range ducklord.WorkspaceVisiblePaneRects(&s.activity().ProjectLayout, nav, geometry) {
			if insideWorkspaceRect(pane.Rect, x, y) {
				if err := nav.SelectPane(nav.CurrentProjectID(), s.projectPaneID(nav.CurrentProjectID(), pane.Identity)); err != nil {
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
		if s.workspaceDragSession.Client != "" {
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
