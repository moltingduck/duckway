package main

import (
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

type workspacePaneIntent struct {
	projectID string
	targetID  string
	placement ducklord.PanePlacement
}

func (s *tuiState) beginWorkspacePane() {
	if !s.workspacePreview || !s.workspaceProjectFocus {
		return
	}
	if s.hostScoped {
		s.outputErr = "Project pane placement requires the full Ducklord workspace"
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	s.workspacePaneIntent = workspacePaneIntent{projectID: nav.CurrentProjectID(), targetID: nav.CurrentPaneID()}
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "placement", 0
	s.workspacePaneErr = ""
}

func (s *tuiState) beginWorkspaceMove()   { s.beginWorkspacePaneAction("move-placement") }
func (s *tuiState) beginWorkspaceDetach() { s.beginWorkspacePaneAction("detach-confirm") }

func (s *tuiState) beginWorkspacePaneAction(step string) {
	if !s.workspacePreview || !s.workspaceProjectFocus {
		return
	}
	if s.hostScoped {
		s.outputErr = "Project pane actions require the full Ducklord workspace"
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	projectID, paneID := nav.CurrentProjectID(), nav.CurrentPaneID()
	identity, ok := s.activity().ProjectLayout.PaneSession(projectID, paneID)
	if !ok {
		s.outputErr = "select a Session pane first"
		return
	}
	s.workspacePaneIntent = workspacePaneIntent{projectID: projectID}
	s.workspacePaneSourceID, s.workspacePaneIdentity = paneID, identity
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, step, 0
	if step == "detach-confirm" {
		s.workspacePaneIndex = 1
	}
	s.workspacePaneErr = ""
}

func paneLeafIDs(pane *ducklord.SessionPane, exclude string, ids *[]string) {
	if pane == nil {
		return
	}
	if pane.Session != nil && pane.ID != exclude {
		*ids = append(*ids, pane.ID)
	}
	paneLeafIDs(pane.First, exclude, ids)
	paneLeafIDs(pane.Second, exclude, ids)
}

func (s *tuiState) workspaceMoveTargets() []string {
	project := s.activity().ProjectLayout.Project(s.workspacePaneIntent.projectID)
	if project == nil {
		return nil
	}
	var ids []string
	for _, tab := range project.Tabs {
		paneLeafIDs(tab.Root, s.workspacePaneSourceID, &ids)
	}
	return ids
}

func (s *tuiState) commitWorkspacePaneAction(detach bool) error {
	next := s.activity().Clone()
	identity, ok := next.ProjectLayout.PaneSession(s.workspacePaneIntent.projectID, s.workspacePaneSourceID)
	if !ok || identity != s.workspacePaneIdentity {
		return fmt.Errorf("source Session pane changed; reopen this action")
	}
	var paneID string
	var err error
	if detach {
		_, err = next.ProjectLayout.DetachPane(s.workspacePaneIntent.projectID, s.workspacePaneSourceID)
	} else {
		paneID, err = next.ProjectLayout.MovePane(s.workspacePaneIntent.projectID, s.workspacePaneSourceID, s.workspacePaneIntent.placement, s.workspacePaneIntent.targetID)
	}
	if err != nil {
		return err
	}
	if err = s.activityStore.Save(next); err != nil {
		return fmt.Errorf("save Session pane: %w", err)
	}
	s.activityState = next
	s.workspacePaneChanged = true
	if nav, navErr := s.workspaceNavigation(); navErr == nil {
		if !detach {
			_ = nav.SelectPane(s.workspacePaneIntent.projectID, paneID)
		}
	} else {
		s.outputErr = "Session pane saved; navigation will refresh: " + sanitizeTerminalText(navErr.Error())
	}
	return nil
}

func (s *tuiState) beginWorkspaceProject() {
	if !s.workspacePreview || !s.workspaceProjectFocus {
		return
	}
	if s.hostScoped {
		s.outputErr = "Project creation requires the full Ducklord workspace"
		return
	}
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "project-create", 0
	s.workspacePaneName, s.workspacePaneErr = "", ""
}

func (s *tuiState) workspacePaneCandidates() []ducklord.RemoteSession {
	seen := make(map[ducklord.SessionIdentity]bool)
	var candidates []ducklord.RemoteSession
	for _, session := range s.sessions {
		identity, ok := ducklord.IdentityFromSession(session)
		if !ok || seen[identity] || !s.hostIsLive(session.Client) || !canRead(session) ||
			session.Status != "running" || session.RuntimeGeneration == 0 {
			continue
		}
		if query := strings.ToLower(strings.TrimSpace(s.workspacePaneQuery)); query != "" &&
			!strings.Contains(strings.ToLower(session.Name+" "+session.Client+" "+session.SessionID), query) {
			continue
		}
		seen[identity] = true
		candidates = append(candidates, session)
	}
	return candidates
}

func paneIDForSession(pane *ducklord.SessionPane, identity ducklord.SessionIdentity) string {
	if pane == nil {
		return ""
	}
	if pane.Session != nil && *pane.Session == identity {
		return pane.ID
	}
	if id := paneIDForSession(pane.First, identity); id != "" {
		return id
	}
	return paneIDForSession(pane.Second, identity)
}

func (s *tuiState) projectPaneForSession(identity ducklord.SessionIdentity) string {
	project := s.activity().ProjectLayout.Project(s.workspacePaneIntent.projectID)
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

func (s *tuiState) workspacePaneChoices() []string {
	switch s.workspacePaneStep {
	case "placement":
		return []string{"New Terminal tab", "Split vertically", "Split horizontally"}
	case "source":
		return []string{"New shell session", "Add existing session"}
	case "move-placement":
		return []string{"New Terminal tab", "Split vertically", "Split horizontally"}
	case "move-target":
		ids := s.workspaceMoveTargets()
		choices := make([]string, 0, len(ids))
		for _, id := range ids {
			identity, _ := s.activity().ProjectLayout.PaneSession(s.workspacePaneIntent.projectID, id)
			label := identity.SessionID
			for _, session := range s.sessions {
				if candidate, ok := ducklord.IdentityFromSession(session); ok && candidate == identity {
					label = displayField(session.Name) + " @" + displayField(session.Client)
					break
				}
			}
			choices = append(choices, label)
		}
		return choices
	case "move-confirm":
		return []string{"Move local pane", "Cancel"}
	case "existing-move-confirm":
		return []string{"Move existing pane here", "Cancel"}
	case "detach-confirm":
		return []string{"Detach local pane", "Cancel"}
	case "existing":
		candidates := s.workspacePaneCandidates()
		choices := make([]string, 0, len(candidates))
		for _, session := range candidates {
			label := fmt.Sprintf("%s @%s  [%s]", displayField(session.Name), displayField(session.Client), session.SessionID)
			if identity, ok := ducklord.IdentityFromSession(session); ok && s.projectPaneForSession(identity) != "" {
				label += "  (move in Project)"
			}
			choices = append(choices, label)
		}
		return choices
	}
	return nil
}

func (s *tuiState) renderWorkspacePaneModal(out io.Writer, cols, rows int) {
	if !s.workspacePaneMode {
		return
	}
	if s.workspacePaneStep == "project-create" {
		lines := []modalRenderLine{{modalTitle, "  Create Project"}, {modalInput, "  name › " + s.workspacePaneName + "_"}}
		if s.workspacePaneErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  Enter create · Esc/Ctrl+C close"})
		renderModalBox(out, cols, rows, lines)
		return
	}
	choices := s.workspacePaneChoices()
	title := "  Add Session pane"
	if strings.HasPrefix(s.workspacePaneStep, "move-") {
		title = "  Move Session pane"
	}
	if s.workspacePaneStep == "detach-confirm" {
		title = "  Detach Session pane"
	}
	lines := []modalRenderLine{{modalTitle, title}}
	if s.workspacePaneStep == "detach-confirm" {
		lines = append(lines, modalRenderLine{modalMuted, "  Remote session keeps running."})
	}
	if s.workspacePaneStep == "move-confirm" {
		lines = append(lines, modalRenderLine{modalMuted, "  Local view only; remote session stays running."})
	}
	if s.workspacePaneStep == "existing-move-confirm" {
		lines = append(lines, modalRenderLine{modalMuted, "  Already in this Project; move its pane, not duplicate it."})
		label := s.workspacePaneIdentity.SessionID
		for _, session := range s.sessions {
			if identity, ok := ducklord.IdentityFromSession(session); ok && identity == s.workspacePaneIdentity {
				label = displayField(session.Name) + " @" + displayField(session.Client)
				break
			}
		}
		lines = append(lines, modalRenderLine{modalMuted, "  Session: " + label})
	}
	if s.workspacePaneStep == "existing" {
		lines = append(lines, modalRenderLine{modalInput, "  find › " + s.workspacePaneQuery + "_"})
	}
	start := 0
	maxChoices := max(1, rows-6)
	if s.workspacePaneStep == "existing" {
		maxChoices = max(1, rows-7)
	}
	if len(choices) > maxChoices {
		start = min(max(0, s.workspacePaneIndex-maxChoices/2), len(choices)-maxChoices)
	}
	end := min(len(choices), start+maxChoices)
	for i := start; i < end; i++ {
		choice := choices[i]
		style, prefix := "", "  "
		if i == s.workspacePaneIndex {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, prefix + choice})
	}
	if len(choices) == 0 {
		empty := "  No eligible sessions"
		if s.workspacePaneStep == "move-target" {
			empty = "  No other pane to split; choose a new tab"
		}
		lines = append(lines, modalRenderLine{modalMuted, empty})
	}
	if start > 0 || end < len(choices) {
		lines = append(lines, modalRenderLine{modalMuted, fmt.Sprintf("  %d above · %d below", start, len(choices)-end)})
	}
	if s.workspacePaneErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter continue · Esc back · Ctrl+C close"})
	renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) closeWorkspacePane() {
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneErr = false, "", ""
	s.workspacePaneName = ""
	s.workspacePaneQuery = ""
	s.workspacePaneIntent = workspacePaneIntent{}
	s.workspacePaneSourceID = ""
	s.workspacePaneIdentity = ducklord.SessionIdentity{}
	s.workspacePaneIndex = 0
}

// placeWorkspacePane is one atomic local transaction. The remote Session is
// never started, stopped, or yielded by placing its local view.
func (s *tuiState) placeWorkspacePane(intent workspacePaneIntent, session ducklord.RemoteSession) error {
	identity, ok := ducklord.IdentityFromSession(session)
	if !ok {
		return fmt.Errorf("session has no stable identity")
	}
	current := false
	for _, candidate := range s.sessions {
		if candidate.Client == session.Client && candidate.InstanceID == session.InstanceID && candidate.SessionID == session.SessionID &&
			candidate.RuntimeGeneration == session.RuntimeGeneration && candidate.Status == "running" && canRead(candidate) && s.hostIsLive(candidate.Client) {
			current = true
			break
		}
	}
	if !current {
		return fmt.Errorf("session changed or host disconnected; refresh before placing its pane")
	}
	next := s.activity().Clone()
	paneID, err := next.ProjectLayout.Place(intent.projectID, identity, intent.placement, intent.targetID)
	if err != nil {
		return err
	}
	if err := s.activityStore.Save(next); err != nil {
		return fmt.Errorf("save Session pane: %w", err)
	}
	s.activityState = next
	s.workspacePaneChanged = true
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = "Session pane saved; navigation will refresh: " + sanitizeTerminalText(err.Error())
		return nil
	}
	if err := nav.SelectPane(intent.projectID, paneID); err != nil {
		s.outputErr = "Session pane saved; navigation will refresh: " + sanitizeTerminalText(err.Error())
	}
	return nil
}

// handleWorkspacePaneInput returns whether the existing Create wizard should
// open. Modal input never reaches a PTY.
func (s *tuiState) handleWorkspacePaneInput(input []byte) (openCreate bool) {
	key := string(input)
	if s.workspacePaneStep == "project-create" {
		switch key {
		case "\x03", "\x1b":
			s.closeWorkspacePane()
		case "\x7f", "\b":
			runes := []rune(s.workspacePaneName)
			if len(runes) > 0 {
				s.workspacePaneName = string(runes[:len(runes)-1])
			}
		case "\r":
			next := s.activity().Clone()
			projectID, err := next.ProjectLayout.AddProject(s.workspacePaneName)
			if err == nil {
				err = s.activityStore.Save(next)
			}
			if err != nil {
				s.workspacePaneErr = sanitizeTerminalText(err.Error())
				return false
			}
			s.activityState = next
			s.closeWorkspacePane()
			if nav, err := s.workspaceNavigation(); err == nil {
				_ = nav.SelectProject(projectID)
			}
		default:
			if utf8.Valid(input) {
				for _, r := range key {
					if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && utf8.RuneCountInString(s.workspacePaneName) < 128 {
						s.workspacePaneName += string(r)
					}
				}
			}
		}
		return false
	}
	choices := s.workspacePaneChoices()
	switch key {
	case "\x03":
		s.closeWorkspacePane()
	case "\x1b":
		s.workspacePaneErr = ""
		s.workspacePaneIndex = 0
		switch s.workspacePaneStep {
		case "existing":
			s.workspacePaneStep = "source"
			s.workspacePaneQuery = ""
		case "source":
			s.workspacePaneStep = "placement"
		case "move-target":
			s.workspacePaneStep = "move-placement"
		case "move-confirm":
			if s.workspacePaneIntent.placement == ducklord.PlaceNewTab {
				s.workspacePaneStep = "move-placement"
			} else {
				s.workspacePaneStep = "move-target"
			}
		case "existing-move-confirm":
			s.workspacePaneStep = "existing"
		default:
			s.closeWorkspacePane()
		}
	case "\x1b[B":
		if len(choices) > 0 {
			s.workspacePaneIndex = min(len(choices)-1, s.workspacePaneIndex+1)
		}
	case "\x1b[A":
		s.workspacePaneIndex = max(0, s.workspacePaneIndex-1)
	case "j", "k":
		if s.workspacePaneStep != "existing" {
			if key == "j" {
				if len(choices) > 0 {
					s.workspacePaneIndex = min(len(choices)-1, s.workspacePaneIndex+1)
				}
			} else {
				s.workspacePaneIndex = max(0, s.workspacePaneIndex-1)
			}
			return false
		}
		fallthrough
	case "\x7f", "\b":
		if key == "\x7f" || key == "\b" {
			runes := []rune(s.workspacePaneQuery)
			if len(runes) > 0 {
				s.workspacePaneQuery = string(runes[:len(runes)-1])
			}
		} else {
			s.workspacePaneQuery += key
		}
		s.workspacePaneIndex = 0
	case "\r":
		if len(choices) == 0 {
			return false
		}
		index := max(0, min(s.workspacePaneIndex, len(choices)-1))
		switch s.workspacePaneStep {
		case "move-placement":
			s.workspacePaneIntent.placement = []ducklord.PanePlacement{ducklord.PlaceNewTab, ducklord.PlaceVertical, ducklord.PlaceHorizontal}[index]
			s.workspacePaneIndex, s.workspacePaneErr = 0, ""
			if s.workspacePaneIntent.placement == ducklord.PlaceNewTab {
				s.workspacePaneIntent.targetID = ""
				s.workspacePaneStep = "move-confirm"
				s.workspacePaneIndex = 1
			} else {
				s.workspacePaneStep = "move-target"
			}
		case "move-target":
			s.workspacePaneIntent.targetID = s.workspaceMoveTargets()[index]
			s.workspacePaneStep, s.workspacePaneIndex = "move-confirm", 1
		case "move-confirm", "detach-confirm", "existing-move-confirm":
			if index == 1 {
				s.closeWorkspacePane()
				return false
			}
			if err := s.commitWorkspacePaneAction(s.workspacePaneStep == "detach-confirm"); err != nil {
				s.workspacePaneErr = sanitizeTerminalText(err.Error())
				return false
			}
			s.closeWorkspacePane()
		case "placement":
			s.workspacePaneIntent.placement = []ducklord.PanePlacement{ducklord.PlaceNewTab, ducklord.PlaceVertical, ducklord.PlaceHorizontal}[index]
			if s.workspacePaneIntent.placement != ducklord.PlaceNewTab && s.workspacePaneIntent.targetID == "" {
				s.workspacePaneErr = "choose a Project with a Session pane to split"
				return false
			}
			s.workspacePaneStep, s.workspacePaneIndex, s.workspacePaneErr = "source", 0, ""
		case "source":
			if index == 0 {
				intent := s.workspacePaneIntent
				s.closeWorkspacePane()
				s.workspaceNewSessionIntent = &intent
				return true
			}
			s.workspacePaneStep, s.workspacePaneIndex, s.workspacePaneErr, s.workspacePaneQuery = "existing", 0, "", ""
		case "existing":
			candidate := s.workspacePaneCandidates()[index]
			identity, _ := ducklord.IdentityFromSession(candidate)
			if sourceID := s.projectPaneForSession(identity); sourceID != "" {
				if s.workspacePaneIntent.placement != ducklord.PlaceNewTab && sourceID == s.workspacePaneIntent.targetID {
					s.workspacePaneErr = "select a different pane to split"
					return false
				}
				s.workspacePaneSourceID, s.workspacePaneIdentity = sourceID, identity
				s.workspacePaneStep, s.workspacePaneIndex, s.workspacePaneErr = "existing-move-confirm", 1, ""
				return false
			}
			if err := s.placeWorkspacePane(s.workspacePaneIntent, candidate); err != nil {
				s.workspacePaneErr = sanitizeTerminalText(err.Error())
				return false
			}
			s.closeWorkspacePane()
		}
	default:
		if s.workspacePaneStep == "existing" && utf8.Valid(input) {
			for _, r := range key {
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && utf8.RuneCountInString(s.workspacePaneQuery) < 128 {
					s.workspacePaneQuery += string(r)
				}
			}
			s.workspacePaneIndex = 0
		}
	}
	return false
}
