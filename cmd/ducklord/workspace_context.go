package main

import "github.com/hackerduck/duckway/internal/ducklord"

// Context menus capture the clicked identity, independently of quick selection.
// Opening a menu never requests PTY control or executes a Session action.
func (s *tuiState) openWorkspaceSessionConfig(identity ducklord.SessionIdentity) {
	for _, session := range s.sessions {
		if current, ok := ducklord.IdentityFromSession(session); ok && current == identity {
			s.beginSessionActionMenu(session)
			return
		}
	}
	s.outputErr = "clicked Session is unavailable; refresh and try again"
}

func (s *tuiState) openWorkspaceContextConfig(x, y, width, height int) bool {
	nav, err := s.workspaceNavigation()
	if err != nil {
		return false
	}
	if nav.InDetailMode() {
		geometry := ducklord.CalculateDetailGeometry(width, height, 4)
		if insideWorkspaceRect(geometry.Pane, x, y) {
			if s.detailSelected.Key() != "" {
				s.openWorkspaceSessionConfig(s.detailSelected)
			}
			return false
		}
		results := s.detailedResults()
		selected := -1
		for i := range results {
			if results[i].Identity == s.detailSelected {
				selected = i
				break
			}
		}
		if index := ducklord.DetailSessionIndexAt(geometry.List, selected, len(results), x, y); index >= 0 {
			s.detailSearchFocused = false
			s.detailSelected = results[index].Identity
			err = nav.PreviewDetail(s.detailSelected)
			s.openWorkspaceSessionConfig(s.detailSelected)
			return err == nil
		}
		return false
	}
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	quick := s.workspaceQuickSessions()
	offsets := s.workspaceColumnOffsets(geometry, nav, quick)
	if row, ok := workspaceQuickRowAt(geometry.Quick, x, y); ok && row+offsets.Quick < len(quick) {
		identity, valid := ducklord.IdentityFromSession(quick[row+offsets.Quick])
		if valid {
			s.workspaceProjectFocus = false
			s.openWorkspaceSessionConfig(identity)
		}
		return false
	}
	if row, ok := workspaceQuickRowAt(geometry.Projects, x, y); ok && row+offsets.Projects < len(s.activity().ProjectLayout.Projects) {
		project := s.activity().ProjectLayout.Projects[row+offsets.Projects]
		if nav.SelectProject(project.ID) == nil {
			s.workspaceProjectFocus = true
			s.beginWorkspaceProjectHosts()
			return true
		}
		return false
	}
	project := s.activity().ProjectLayout.Project(nav.CurrentProjectID())
	if project == nil {
		return false
	}
	if y == geometry.Terminal.Y && insideWorkspaceRect(geometry.Terminal, x, y) {
		left := geometry.Terminal.X + modalCellWidth(" "+project.Name+"  ")
		for i, tab := range project.Tabs {
			cells := modalCellWidth(ducklord.WorkspaceTabLabel(tab, i, tab.ID == nav.CurrentTabID()))
			if x >= left && x < left+cells {
				if id := firstWorkspacePaneID(tab.Root); id != "" && nav.SelectPane(project.ID, id) == nil {
					s.workspaceProjectFocus = true
					s.beginWorkspaceTabRename()
					return true
				}
				return false
			}
			left += cells
		}
		return false
	}
	for _, pane := range ducklord.WorkspaceVisiblePaneRects(&s.activity().ProjectLayout, nav, geometry) {
		if insideWorkspaceRect(pane.Rect, x, y) {
			if nav.SelectPane(project.ID, s.projectPaneID(project.ID, pane.Identity)) == nil {
				s.openWorkspaceSessionConfig(pane.Identity)
				return true
			}
			return false
		}
	}
	return false
}
