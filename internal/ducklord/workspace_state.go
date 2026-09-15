package ducklord

import (
	"fmt"
	"maps"
)

// PaneNavigationTarget plans a local move without changing focus or selection.
// The UI can keep its existing writer when an edge command is a no-op.
func (w *WorkspaceState) PaneNavigationTarget(geometry WorkspaceGeometry, command string) (string, error) {
	preview := *w
	preview.lastProject = maps.Clone(w.lastProject)
	preview.projectLocation = maps.Clone(w.projectLocation)
	var err error
	switch command {
	case "pageup":
		err = preview.CycleTab(-1)
	case "pagedown":
		err = preview.CycleTab(1)
	default:
		err = preview.MoveVisiblePane(geometry, command)
	}
	return preview.CurrentPaneID(), err
}

type WorkspaceRegion string

const (
	RegionProjects        WorkspaceRegion = "projects"
	RegionQuickList       WorkspaceRegion = "quick_list"
	RegionTerminal        WorkspaceRegion = "terminal"
	RegionTerminalPreview WorkspaceRegion = "terminal_preview"
	RegionDetailList      WorkspaceRegion = "detail_list"
	RegionDetailPane      WorkspaceRegion = "detail_pane"
)

type workspaceLocation struct {
	projectID string
	tabID     string
	paneID    string
	region    WorkspaceRegion
}

// WorkspaceState is transient Ducklord UI navigation. The ProjectLayout owns
// local pane placement; Ducklion owns remote Session identity and control.
// Previewing never grants input, resizes a PTY, or marks a notification seen.
type WorkspaceState struct {
	layout                     *ProjectLayout
	location                   workspaceLocation
	quickSelection             SessionIdentity
	lastProject                map[SessionIdentity]string
	projectLocation            map[string]workspaceLocation
	detail                     bool
	detailSelection            SessionIdentity
	beforeDetail               workspaceLocation
	notificationFocusProjectID string
}

func NewWorkspaceState(layout *ProjectLayout) (*WorkspaceState, error) {
	if layout == nil {
		return nil, fmt.Errorf("Project layout is required")
	}
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	w := &WorkspaceState{layout: layout, lastProject: make(map[SessionIdentity]string),
		projectLocation: make(map[string]workspaceLocation)}
	if err := w.selectProject(DefaultProjectID, RegionProjects); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *WorkspaceState) CurrentProjectID() string           { return w.location.projectID }
func (w *WorkspaceState) CurrentTabID() string               { return w.location.tabID }
func (w *WorkspaceState) CurrentPaneID() string              { return w.location.paneID }
func (w *WorkspaceState) Region() WorkspaceRegion            { return w.location.region }
func (w *WorkspaceState) InDetailMode() bool                 { return w.detail }
func (w *WorkspaceState) DetailSelection() SessionIdentity   { return w.detailSelection }
func (w *WorkspaceState) NotificationFocusProjectID() string { return w.notificationFocusProjectID }

// NotificationProjectID labels a shared Session without navigating or changing
// focus. Notification focus takes precedence over the normal navigation order.
func (w *WorkspaceState) NotificationProjectID(session SessionIdentity) string {
	if w.layout.hasMembership(w.notificationFocusProjectID, session) {
		return w.notificationFocusProjectID
	}
	return w.layout.NavigateProject(session, w.location.projectID, w.lastProject[session])
}

func (w *WorkspaceState) ToggleNotificationFocus() string {
	if w.notificationFocusProjectID == w.location.projectID {
		w.notificationFocusProjectID = ""
	} else if w.layout.Project(w.location.projectID) != nil {
		w.notificationFocusProjectID = w.location.projectID
	}
	return w.notificationFocusProjectID
}

// RebindLayout preserves transient navigation when ActivityState is cloned
// for an atomic local save. The replacement layout remains authoritative.
func (w *WorkspaceState) RebindLayout(layout *ProjectLayout) error {
	if layout == nil {
		return fmt.Errorf("Project layout is required")
	}
	if err := layout.Validate(); err != nil {
		return err
	}
	w.layout = layout
	w.ReconcileLayout()
	return nil
}

func (w *WorkspaceState) selectProject(projectID string, region WorkspaceRegion) error {
	project := w.layout.Project(projectID)
	if project == nil {
		return fmt.Errorf("unknown Project %q", projectID)
	}
	if old := w.location.projectID; old != "" {
		w.projectLocation[old] = w.location
	}
	location := w.projectLocation[projectID]
	location.projectID = projectID
	location.region = region
	if !project.hasPane(location.tabID, location.paneID) {
		location.tabID, location.paneID = project.firstPane()
	}
	w.location = location
	return nil
}

// SelectProject immediately switches the Terminal area, but does not mutate
// quick-list selection or notification focus policy.
func (w *WorkspaceState) SelectProject(projectID string) error {
	if w.detail {
		return fmt.Errorf("cannot select a Project in detailed-list mode")
	}
	if err := w.selectProject(projectID, RegionProjects); err != nil {
		return err
	}
	if session, ok := w.layout.PaneSession(projectID, w.location.paneID); ok {
		w.lastProject[session] = projectID
	}
	return nil
}

func (p *LocalProject) hasPane(tabID, paneID string) bool {
	for _, tab := range p.Tabs {
		if tab.ID == tabID && tab.Root.findPane(paneID) != nil {
			return true
		}
	}
	return false
}

func (p *LocalProject) firstPane() (string, string) {
	for _, tab := range p.Tabs {
		if leaf := tab.Root.firstLeaf(); leaf != nil {
			return tab.ID, leaf.ID
		}
	}
	return "", ""
}

func (p *SessionPane) firstLeaf() *SessionPane {
	if p == nil {
		return nil
	}
	if p.Session != nil {
		return p
	}
	if leaf := p.First.firstLeaf(); leaf != nil {
		return leaf
	}
	return p.Second.firstLeaf()
}

func (p *LocalProject) findSessionLocation(session SessionIdentity) (string, string) {
	for _, tab := range p.Tabs {
		if pane := tab.Root.findSession(session); pane != nil {
			return tab.ID, pane.ID
		}
	}
	return "", ""
}

// SelectQuickSession is one-way navigation from the quick list to Project and
// Terminal area. It retains quick-list keyboard focus; Enter/focus is separate.
func (w *WorkspaceState) SelectQuickSession(session SessionIdentity) error {
	if w.detail {
		return fmt.Errorf("quick list unavailable in detailed-list mode")
	}
	projectID := w.layout.NavigateProject(session, w.location.projectID, w.lastProject[session])
	if projectID == "" {
		return fmt.Errorf("session has no Project pane")
	}
	tabID, paneID := w.layout.Project(projectID).findSessionLocation(session)
	if paneID == "" {
		return fmt.Errorf("session Project pane is closed")
	}
	if err := w.selectProject(projectID, RegionQuickList); err != nil {
		return err
	}
	w.location.tabID, w.location.paneID = tabID, paneID
	w.quickSelection = session
	w.lastProject[session] = projectID
	return nil
}

// SelectPane changes the viewed tab/pane but deliberately keeps keyboard
// focus in the existing region. FocusPane is the sole focus transition.
func (w *WorkspaceState) SelectPane(projectID, paneID string) error {
	if w.detail {
		return fmt.Errorf("normal Terminal area unavailable in detailed-list mode")
	}
	project := w.layout.Project(projectID)
	if project == nil {
		return fmt.Errorf("unknown Project %q", projectID)
	}
	for _, tab := range project.Tabs {
		pane := tab.Root.findPane(paneID)
		if pane == nil || pane.Session == nil {
			continue
		}
		region := w.location.region
		if region == RegionTerminal {
			region = RegionTerminalPreview
		}
		if err := w.selectProject(projectID, region); err != nil {
			return err
		}
		w.location.tabID, w.location.paneID = tab.ID, paneID
		w.lastProject[*pane.Session] = projectID
		return nil
	}
	return fmt.Errorf("session pane no longer exists")
}

// CycleTab moves among the current Project's tabs without changing keyboard
// focus. Empty tabs are skipped; a tab always selects one of its Session panes.
func (w *WorkspaceState) CycleTab(delta int) error {
	if w.detail {
		return fmt.Errorf("normal Terminal area unavailable in detailed-list mode")
	}
	project := w.layout.Project(w.location.projectID)
	if project == nil || len(project.Tabs) == 0 {
		return fmt.Errorf("current Project has no Terminal tabs")
	}
	index := 0
	for i := range project.Tabs {
		if project.Tabs[i].ID == w.location.tabID {
			index = i
			break
		}
	}
	for offset := 1; offset <= len(project.Tabs); offset++ {
		candidate := (index + delta*offset%len(project.Tabs) + len(project.Tabs)) % len(project.Tabs)
		if leaf := project.Tabs[candidate].Root.firstLeaf(); leaf != nil {
			return w.SelectPane(project.ID, leaf.ID)
		}
	}
	return fmt.Errorf("current Project has no Session panes")
}

// CycleVisiblePane navigates only cells currently rendered by the Terminal
// area. Hidden split leaves cannot become a keyboard-control target.
func (w *WorkspaceState) CycleVisiblePane(geometry WorkspaceGeometry, delta int) error {
	if w.detail {
		return fmt.Errorf("normal Terminal area unavailable in detailed-list mode")
	}
	panes := WorkspaceVisiblePaneRects(w.layout, w, geometry)
	if len(panes) == 0 {
		return fmt.Errorf("current Terminal tab has no visible Session panes")
	}
	project := w.layout.Project(w.location.projectID)
	index := 0
	for i, pane := range panes {
		_, paneID := project.findSessionLocation(pane.Identity)
		if paneID == w.location.paneID {
			index = i
			break
		}
	}
	if _, visible := WorkspaceVisiblePaneRect(w.layout, w, geometry); !visible {
		if delta > 0 {
			index = -1
		} else {
			index = 0
		}
	}
	index = (index + delta%len(panes) + len(panes)) % len(panes)
	_, paneID := project.findSessionLocation(panes[index].Identity)
	return w.SelectPane(project.ID, paneID)
}

// MoveVisiblePane selects the closest visible cell in a screen direction.
// At an outer edge it leaves selection and keyboard focus unchanged.
func (w *WorkspaceState) MoveVisiblePane(geometry WorkspaceGeometry, direction string) error {
	if w.detail {
		return fmt.Errorf("normal Terminal area unavailable in detailed-list mode")
	}
	switch direction {
	case "up", "down", "left", "right":
	default:
		return fmt.Errorf("unknown pane direction %q", direction)
	}
	panes := WorkspaceVisiblePaneRects(w.layout, w, geometry)
	if len(panes) == 0 {
		return fmt.Errorf("current Terminal tab has no visible Session panes")
	}
	project := w.layout.Project(w.location.projectID)
	current := -1
	for i, pane := range panes {
		_, paneID := project.findSessionLocation(pane.Identity)
		if paneID == w.location.paneID {
			current = i
			break
		}
	}
	if current < 0 {
		_, paneID := project.findSessionLocation(panes[0].Identity)
		return w.SelectPane(project.ID, paneID)
	}
	origin := panes[current].Rect
	best, bestGap, bestCenter := -1, 0, 0
	for i, pane := range panes {
		if i == current {
			continue
		}
		r := pane.Rect
		eligible := false
		switch direction {
		case "up":
			eligible = r.Y+r.Height <= origin.Y
		case "down":
			eligible = r.Y >= origin.Y+origin.Height
		case "left":
			eligible = r.X+r.Width <= origin.X
		case "right":
			eligible = r.X >= origin.X+origin.Width
		}
		if !eligible {
			continue
		}
		// Measure between occupied cells, so a shared edge is closer than
		// a shared corner. Center distance resolves equally close neighbors.
		dx := max(0, max(origin.X, r.X)-min(origin.X+origin.Width-1, r.X+r.Width-1))
		dy := max(0, max(origin.Y, r.Y)-min(origin.Y+origin.Height-1, r.Y+r.Height-1))
		gap := dx*dx + dy*dy
		cx := (2*r.X + r.Width) - (2*origin.X + origin.Width)
		cy := (2*r.Y + r.Height) - (2*origin.Y + origin.Height)
		center := cx*cx + cy*cy
		if best < 0 || gap < bestGap || (gap == bestGap && center < bestCenter) {
			best, bestGap, bestCenter = i, gap, center
		}
	}
	if best < 0 {
		return nil
	}
	_, paneID := project.findSessionLocation(panes[best].Identity)
	return w.SelectPane(project.ID, paneID)
}

func (w *WorkspaceState) FocusPane() (SessionIdentity, error) {
	if w.detail {
		if w.detailSelection.Key() == "" {
			return SessionIdentity{}, fmt.Errorf("no detailed Session selected")
		}
		w.location.region = RegionDetailPane
		return w.detailSelection, nil
	}
	project := w.layout.Project(w.location.projectID)
	if project == nil {
		return SessionIdentity{}, fmt.Errorf("current Project no longer exists")
	}
	for _, tab := range project.Tabs {
		if tab.ID != w.location.tabID {
			continue
		}
		pane := tab.Root.findPane(w.location.paneID)
		if pane != nil && pane.Session != nil {
			w.location.region = RegionTerminal
			w.lastProject[*pane.Session] = project.ID
			return *pane.Session, nil
		}
	}
	return SessionIdentity{}, fmt.Errorf("current Session pane no longer exists")
}

// FocusVisiblePane is the TUI entry point for granting PTY input and resize.
// A pane hidden by a narrow split can still be selected for navigation, but
// it must not become a writer target until it is actually displayed.
func (w *WorkspaceState) FocusVisiblePane(geometry WorkspaceGeometry) (SessionIdentity, error) {
	if w.detail {
		return w.FocusPane()
	}
	if _, visible := WorkspaceVisiblePaneRect(w.layout, w, geometry); visible {
		return w.FocusPane()
	}
	return SessionIdentity{}, fmt.Errorf("current Session pane is hidden by the terminal size")
}

func (w *WorkspaceState) EnterDetail() {
	if w.detail {
		return
	}
	w.beforeDetail = w.location
	w.detail = true
	w.location.region = RegionDetailList
	w.detailSelection = w.quickSelection
}

func (w *WorkspaceState) PreviewDetail(session SessionIdentity) error {
	if !w.detail {
		return fmt.Errorf("not in detailed-list mode")
	}
	if len(w.layout.ProjectsFor(session)) == 0 {
		return fmt.Errorf("session has no Project pane")
	}
	w.detailSelection = session
	w.location.region = RegionDetailList
	return nil
}

func (w *WorkspaceState) ClearDetailSelection() {
	if !w.detail {
		return
	}
	w.detailSelection = SessionIdentity{}
	w.location.region = RegionDetailList
}

func (w *WorkspaceState) ExitDetail() {
	if !w.detail {
		return
	}
	w.detail = false
	w.location = w.beforeDetail
	w.beforeDetail = workspaceLocation{}
	w.ReconcileLayout()
}

// JumpDetail enters the selected Session's normal Project layout and focuses
// its pane. This is the only detailed-mode navigation that changes the saved
// normal workspace location.
func (w *WorkspaceState) JumpDetail() (SessionIdentity, error) {
	if !w.detail {
		return SessionIdentity{}, fmt.Errorf("not in detailed-list mode")
	}
	session := w.detailSelection
	projectID := w.layout.NavigateProject(session, w.beforeDetail.projectID, w.lastProject[session])
	if projectID == "" {
		return SessionIdentity{}, fmt.Errorf("selected Session no longer has a Project pane")
	}
	tabID, paneID := w.layout.Project(projectID).findSessionLocation(session)
	if paneID == "" {
		return SessionIdentity{}, fmt.Errorf("selected Session Project pane is closed")
	}
	w.ExitDetail()
	if err := w.selectProject(projectID, RegionProjects); err != nil {
		return SessionIdentity{}, err
	}
	w.lastProject[session] = projectID
	w.location.tabID, w.location.paneID = tabID, paneID
	return w.FocusPane()
}

// ReconcileLayout repairs local navigation after an explicit pane detach,
// Project removal, or authoritative remote Session exit. It never recreates
// a remote Session or marks notifications seen.
func (w *WorkspaceState) ReconcileLayout() {
	if w.notificationFocusProjectID != "" && w.layout.Project(w.notificationFocusProjectID) == nil {
		w.notificationFocusProjectID = ""
	}
	fix := func(location *workspaceLocation) {
		project := w.layout.Project(location.projectID)
		if project == nil {
			project = w.layout.Project(DefaultProjectID)
			if project == nil && len(w.layout.Projects) != 0 {
				project = &w.layout.Projects[0]
			}
			if project == nil {
				*location = workspaceLocation{}
				return
			}
			location.projectID = project.ID
		}
		if !project.hasPane(location.tabID, location.paneID) {
			location.tabID, location.paneID = project.firstPane()
			if location.region == RegionTerminal {
				location.region = RegionProjects
			}
		}
	}
	fix(&w.location)
	if w.detail {
		fix(&w.beforeDetail)
		if len(w.layout.ProjectsFor(w.detailSelection)) == 0 {
			w.detailSelection = SessionIdentity{}
			w.location.region = RegionDetailList
		}
	}
	if len(w.layout.ProjectsFor(w.quickSelection)) == 0 {
		w.quickSelection = SessionIdentity{}
	}
}
