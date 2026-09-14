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
		inProject := false
		for _, projectID := range s.activity().ProjectLayout.ProjectsFor(identity) {
			inProject = inProject || projectID == s.workspacePaneIntent.projectID
		}
		if inProject {
			continue
		}
		seen[identity] = true
		candidates = append(candidates, session)
	}
	return candidates
}

func (s *tuiState) workspacePaneChoices() []string {
	switch s.workspacePaneStep {
	case "placement":
		return []string{"New Terminal tab", "Split vertically", "Split horizontally"}
	case "source":
		return []string{"New shell session", "Add existing session"}
	case "existing":
		candidates := s.workspacePaneCandidates()
		choices := make([]string, 0, len(candidates))
		for _, session := range candidates {
			choices = append(choices, fmt.Sprintf("%s @%s  [%s]", displayField(session.Name), displayField(session.Client), session.SessionID))
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
	lines := []modalRenderLine{{modalTitle, "  Add Session pane"}}
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
		lines = append(lines, modalRenderLine{modalMuted, "  No eligible sessions"})
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
				s.workspacePaneIndex = min(len(choices)-1, s.workspacePaneIndex+1)
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
		index := min(s.workspacePaneIndex, len(choices)-1)
		switch s.workspacePaneStep {
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
			if err := s.placeWorkspacePane(s.workspacePaneIntent, s.workspacePaneCandidates()[index]); err != nil {
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
