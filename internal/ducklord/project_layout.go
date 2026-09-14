package ducklord

import (
	"fmt"

	"github.com/google/uuid"
)

const DefaultProjectID = "default"

type SplitDirection string

const (
	SplitHorizontal SplitDirection = "horizontal"
	SplitVertical   SplitDirection = "vertical"
)

type PanePlacement string

const (
	PlaceNewTab     PanePlacement = "new_tab"
	PlaceHorizontal PanePlacement = "horizontal"
	PlaceVertical   PanePlacement = "vertical"
)

// SessionPane is either a leaf referring to one remote Session or a binary
// split. IDs belong to local views; SessionIdentity belongs to Ducklion.
type SessionPane struct {
	ID        string           `json:"id"`
	Session   *SessionIdentity `json:"session,omitempty"`
	Direction SplitDirection   `json:"direction,omitempty"`
	First     *SessionPane     `json:"first,omitempty"`
	Second    *SessionPane     `json:"second,omitempty"`
}

type TerminalTab struct {
	ID   string       `json:"id"`
	Root *SessionPane `json:"root"`
}

type LocalProject struct {
	ID   string        `json:"id"`
	Name string        `json:"name"`
	Tabs []TerminalTab `json:"tabs,omitempty"`
}

// ProjectLayout is Ducklord-local. The built-in Default Project holds a
// navigable implicit pane for every live Session without explicit membership.
type ProjectLayout struct {
	Projects []LocalProject `json:"projects"`
}

func (l ProjectLayout) Clone() ProjectLayout {
	clone := ProjectLayout{Projects: make([]LocalProject, len(l.Projects))}
	for i, project := range l.Projects {
		clone.Projects[i] = LocalProject{ID: project.ID, Name: project.Name, Tabs: make([]TerminalTab, len(project.Tabs))}
		for j, tab := range project.Tabs {
			clone.Projects[i].Tabs[j] = TerminalTab{ID: tab.ID, Root: tab.Root.clone()}
		}
	}
	return clone
}

func (p *SessionPane) clone() *SessionPane {
	if p == nil {
		return nil
	}
	clone := &SessionPane{ID: p.ID, Direction: p.Direction, First: p.First.clone(), Second: p.Second.clone()}
	if p.Session != nil {
		identity := *p.Session
		clone.Session = &identity
	}
	return clone
}

func NewProjectLayout() ProjectLayout {
	return ProjectLayout{Projects: []LocalProject{{ID: DefaultProjectID, Name: "Default Project"}}}
}

func (l *ProjectLayout) AddProject(name string) (string, error) {
	if err := validateCustomGroupName(name); err != nil {
		return "", fmt.Errorf("invalid project name: %w", err)
	}
	id := uuid.NewString()
	l.Projects = append(l.Projects, LocalProject{ID: id, Name: name})
	return id, nil
}

func (l *ProjectLayout) Project(id string) *LocalProject {
	for i := range l.Projects {
		if l.Projects[i].ID == id {
			return &l.Projects[i]
		}
	}
	return nil
}

func (l *ProjectLayout) ProjectsFor(session SessionIdentity) []string {
	var ids []string
	for _, project := range l.Projects {
		if project.hasSession(session) {
			ids = append(ids, project.ID)
		}
	}
	return ids
}

// SessionsForInstance returns each locally referenced Session once. It is
// used only with an authoritative full inventory for that same Ducklion
// instance; a disconnected or partial update must never prune these views.
func (l *ProjectLayout) SessionsForInstance(instanceID string) []SessionIdentity {
	seen := make(map[SessionIdentity]bool)
	var sessions []SessionIdentity
	for _, project := range l.Projects {
		for _, tab := range project.Tabs {
			for _, session := range tab.Root.sessions() {
				if session.InstanceID == instanceID && !seen[session] {
					seen[session] = true
					sessions = append(sessions, session)
				}
			}
		}
	}
	return sessions
}

func (l *ProjectLayout) TabSessions(projectID, tabID string) []SessionIdentity {
	project := l.Project(projectID)
	if project == nil {
		return nil
	}
	for _, tab := range project.Tabs {
		if tab.ID == tabID {
			return tab.Root.sessions()
		}
	}
	return nil
}

func (l *ProjectLayout) PaneSession(projectID, paneID string) (SessionIdentity, bool) {
	project := l.Project(projectID)
	if project == nil {
		return SessionIdentity{}, false
	}
	for _, tab := range project.Tabs {
		if pane := tab.Root.findPane(paneID); pane != nil && pane.Session != nil {
			return *pane.Session, true
		}
	}
	return SessionIdentity{}, false
}

// NavigateProject picks the user's current Project when it contains the
// Session, then the last Project used for that Session, then Project-pane
// order. It does not mutate focus or notification state.
func (l *ProjectLayout) NavigateProject(session SessionIdentity, currentID, lastID string) string {
	for _, candidate := range []string{currentID, lastID} {
		if project := l.Project(candidate); project != nil && project.hasSession(session) {
			return candidate
		}
	}
	for _, project := range l.Projects {
		if project.hasSession(session) {
			return project.ID
		}
	}
	return ""
}

// RemoveProject removes only local panes. Sessions whose final explicit
// reference was in this Project are rehomed to Default, not destroyed.
func (l *ProjectLayout) RemoveProject(projectID string) error {
	if projectID == DefaultProjectID {
		return fmt.Errorf("default Project cannot be removed")
	}
	index := -1
	for i := range l.Projects {
		if l.Projects[i].ID == projectID {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("unknown project %q", projectID)
	}
	removed := l.Projects[index]
	l.Projects = append(l.Projects[:index], l.Projects[index+1:]...)
	for _, tab := range removed.Tabs {
		for _, session := range tab.Root.sessions() {
			l.ensureDefault(session)
		}
	}
	return nil
}

func (p *LocalProject) hasSession(session SessionIdentity) bool {
	for _, tab := range p.Tabs {
		if tab.Root != nil && tab.Root.findSession(session) != nil {
			return true
		}
	}
	return false
}

func (p *SessionPane) findSession(session SessionIdentity) *SessionPane {
	if p == nil {
		return nil
	}
	if p.Session != nil && *p.Session == session {
		return p
	}
	if child := p.First.findSession(session); child != nil {
		return child
	}
	return p.Second.findSession(session)
}

func (p *SessionPane) findPane(id string) *SessionPane {
	if p == nil {
		return nil
	}
	if p.ID == id {
		return p
	}
	if child := p.First.findPane(id); child != nil {
		return child
	}
	return p.Second.findPane(id)
}

func newSessionPane(session SessionIdentity) *SessionPane {
	copy := session
	return &SessionPane{ID: uuid.NewString(), Session: &copy}
}

func (p *LocalProject) addTab(session SessionIdentity) string {
	leaf := newSessionPane(session)
	p.Tabs = append(p.Tabs, TerminalTab{ID: uuid.NewString(), Root: leaf})
	return leaf.ID
}

// Place adds one view to a Project. Splits require a target leaf in that same
// Project; new tabs do not. A Session may have at most one view per Project.
func (l *ProjectLayout) Place(projectID string, session SessionIdentity, placement PanePlacement, targetPaneID string) (string, error) {
	if err := session.validate(); err != nil {
		return "", err
	}
	project := l.Project(projectID)
	if project == nil {
		return "", fmt.Errorf("unknown project %q", projectID)
	}
	if project.hasSession(session) {
		return "", fmt.Errorf("session already has a pane in project %q", projectID)
	}
	if projectID == DefaultProjectID {
		for _, other := range l.Projects {
			if other.ID != DefaultProjectID && other.hasSession(session) {
				return "", fmt.Errorf("classified session cannot be placed in Default Project")
			}
		}
	}
	var paneID string
	switch placement {
	case PlaceNewTab:
		paneID = project.addTab(session)
	case PlaceHorizontal, PlaceVertical:
		var target *SessionPane
		for i := range project.Tabs {
			target = project.Tabs[i].Root.findPane(targetPaneID)
			if target != nil {
				break
			}
		}
		if target == nil || target.Session == nil {
			return "", fmt.Errorf("split target must be a Session pane in the selected project")
		}
		original := *target
		leaf := newSessionPane(session)
		target.ID = uuid.NewString()
		target.Session = nil
		target.First = &original
		target.Second = leaf
		if placement == PlaceHorizontal {
			target.Direction = SplitHorizontal
		} else {
			target.Direction = SplitVertical
		}
		paneID = leaf.ID
	default:
		return "", fmt.Errorf("invalid pane placement %q", placement)
	}
	if projectID != DefaultProjectID {
		l.removeSessionFromProject(DefaultProjectID, session)
	}
	return paneID, nil
}

func removeSessionNode(node *SessionPane, session SessionIdentity) (*SessionPane, bool) {
	if node == nil {
		return nil, false
	}
	if node.Session != nil {
		if *node.Session == session {
			return nil, true
		}
		return node, false
	}
	var removed bool
	node.First, removed = removeSessionNode(node.First, session)
	if !removed {
		node.Second, removed = removeSessionNode(node.Second, session)
	}
	if !removed {
		return node, false
	}
	if node.First == nil {
		return node.Second, true
	}
	if node.Second == nil {
		return node.First, true
	}
	return node, true
}

func (l *ProjectLayout) removeSessionFromProject(projectID string, session SessionIdentity) bool {
	project := l.Project(projectID)
	if project == nil {
		return false
	}
	for i := range project.Tabs {
		root, removed := removeSessionNode(project.Tabs[i].Root, session)
		if !removed {
			continue
		}
		if root == nil {
			project.Tabs = append(project.Tabs[:i], project.Tabs[i+1:]...)
		} else {
			project.Tabs[i].Root = root
		}
		return true
	}
	return false
}

// Detach removes only the local view. An unclassified live Session always
// returns to the built-in Default Project, including when detached there.
func (l *ProjectLayout) Detach(projectID string, session SessionIdentity) error {
	if !l.removeSessionFromProject(projectID, session) {
		return fmt.Errorf("session has no pane in project %q", projectID)
	}
	l.ensureDefault(session)
	return nil
}

// DetachPane fences a close action by the concrete local pane ID. A stale UI
// action cannot detach a Session that was moved to a different pane meanwhile.
func (l *ProjectLayout) DetachPane(projectID, paneID string) (SessionIdentity, error) {
	project := l.Project(projectID)
	if project == nil {
		return SessionIdentity{}, fmt.Errorf("unknown project %q", projectID)
	}
	for _, tab := range project.Tabs {
		pane := tab.Root.findPane(paneID)
		if pane == nil {
			continue
		}
		if pane.Session == nil {
			return SessionIdentity{}, fmt.Errorf("cannot detach a split container")
		}
		identity := *pane.Session
		return identity, l.Detach(projectID, identity)
	}
	return SessionIdentity{}, fmt.Errorf("session pane no longer exists")
}

func (l *ProjectLayout) ensureDefault(session SessionIdentity) {
	for _, project := range l.Projects {
		if project.ID != DefaultProjectID && project.hasSession(session) {
			return
		}
	}
	defaultProject := l.Project(DefaultProjectID)
	if defaultProject != nil && !defaultProject.hasSession(session) {
		defaultProject.addTab(session)
	}
}

// Discover creates the implicit Default pane for one Session. A partial or
// disconnected Host inventory must never be used to prune local Project
// references; only an authoritative remote destroy/exit calls Destroy.
func (l *ProjectLayout) Discover(session SessionIdentity) error {
	if err := session.validate(); err != nil {
		return err
	}
	l.ensureDefault(session)
	return nil
}

func (p *SessionPane) sessions() []SessionIdentity {
	if p == nil {
		return nil
	}
	if p.Session != nil {
		return []SessionIdentity{*p.Session}
	}
	return append(p.First.sessions(), p.Second.sessions()...)
}

// Destroy removes all local references after the caller has successfully
// destroyed the authoritative remote Session.
func (l *ProjectLayout) Destroy(session SessionIdentity) {
	for _, project := range l.Projects {
		l.removeSessionFromProject(project.ID, session)
	}
}

func (l *ProjectLayout) Validate() error {
	if len(l.Projects) == 0 || l.Projects[0].ID != DefaultProjectID {
		return fmt.Errorf("default project must be first")
	}
	projectIDs := make(map[string]bool, len(l.Projects))
	tabIDs := make(map[string]bool)
	paneIDs := make(map[string]bool)
	customSessions := make(map[SessionIdentity]bool)
	defaultSessions := make(map[SessionIdentity]bool)
	for _, project := range l.Projects {
		if projectIDs[project.ID] || validateCustomGroupName(project.Name) != nil ||
			project.ID != DefaultProjectID && !canonicalLayoutUUID(project.ID) {
			return fmt.Errorf("invalid or duplicate project")
		}
		projectIDs[project.ID] = true
		seen := make(map[SessionIdentity]bool)
		for _, tab := range project.Tabs {
			if !canonicalLayoutUUID(tab.ID) || tabIDs[tab.ID] || tab.Root == nil {
				return fmt.Errorf("invalid or duplicate terminal tab")
			}
			tabIDs[tab.ID] = true
			if err := tab.Root.validate(paneIDs, seen); err != nil {
				return err
			}
		}
		for session := range seen {
			if project.ID == DefaultProjectID {
				defaultSessions[session] = true
			} else {
				customSessions[session] = true
			}
		}
	}
	for session := range defaultSessions {
		if customSessions[session] {
			return fmt.Errorf("classified session also appears in Default Project")
		}
	}
	return nil
}

func (p *SessionPane) validate(paneIDs map[string]bool, seen map[SessionIdentity]bool) error {
	if p == nil || !canonicalLayoutUUID(p.ID) || paneIDs[p.ID] {
		return fmt.Errorf("invalid or duplicate Session pane")
	}
	paneIDs[p.ID] = true
	if p.Session != nil {
		if p.Direction != "" || p.First != nil || p.Second != nil || seen[*p.Session] {
			return fmt.Errorf("invalid or duplicate Session pane leaf")
		}
		if err := p.Session.validate(); err != nil {
			return err
		}
		seen[*p.Session] = true
		return nil
	}
	if (p.Direction != SplitHorizontal && p.Direction != SplitVertical) || p.First == nil || p.Second == nil {
		return fmt.Errorf("invalid Session pane split")
	}
	if err := p.First.validate(paneIDs, seen); err != nil {
		return err
	}
	return p.Second.validate(paneIDs, seen)
}

func canonicalLayoutUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}
