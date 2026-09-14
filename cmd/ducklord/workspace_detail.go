package main

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// Detailed mode is a local view over the already-known inventory. Neither
// searching nor preview selection contacts a remote Host or grants PTY input.
func (s *tuiState) detailedSessionItems() []ducklord.DetailedSessionItem {
	items := make([]ducklord.DetailedSessionItem, 0, len(s.sessions))
	for _, session := range s.sessions {
		identity, ok := ducklord.IdentityFromSession(session)
		if !ok || session.Status == string(model.StatusStopped) {
			continue
		}
		projects := s.activity().ProjectLayout.ProjectsFor(identity)
		if len(projects) == 0 {
			continue
		}
		projectNames := make([]string, 0, len(projects))
		for _, id := range projects {
			if project := s.activity().ProjectLayout.Project(id); project != nil {
				projectNames = append(projectNames, project.Name)
			}
		}
		writer := session.WriterID
		if writer == "" {
			writer = session.WriterKind
		}
		activity := s.activity().Sessions[identity.Key()]
		needsAction := sessionNeedsAttention(session)
		for _, category := range []model.NotificationCategory{model.NotificationAgentNeedsInput, model.NotificationApprovalRequired, model.NotificationTaskFailed, model.NotificationTaskTimeout} {
			needsAction = needsAction || activity.Unread[category]
		}
		disconnected := s.disconnectedHosts[session.Client] || !s.hostIsLive(session.Client)
		status := displayField(session.Status)
		if disconnected {
			status = "disconnected"
		}
		var lastNotification time.Time
		if activity.LastEventAtMS > 0 {
			lastNotification = time.UnixMilli(activity.LastEventAtMS)
		}
		items = append(items, ducklord.DetailedSessionItem{Identity: identity, Name: displayField(session.Name), Host: displayField(session.Client),
			Projects: projectNames, Type: sessionTypeLabel(session), Writer: displayField(writer), State: status,
			LastNotification: lastNotification, Unread: session.Unread, NeedsAction: needsAction, Disconnected: disconnected})
	}
	return items
}

func (s *tuiState) detailedResults() []ducklord.DetailedSessionItem {
	return ducklord.FilterDetailedSessions(s.detailedSessionItems(), s.detailQuery, s.detailFilter)
}

func (s *tuiState) syncDetailSelection() bool {
	results := s.detailedResults()
	if len(results) == 0 {
		s.detailSelected = ducklord.SessionIdentity{}
		if nav, err := s.workspaceNavigation(); err == nil {
			nav.ClearDetailSelection()
		}
		return false
	}
	for _, item := range results {
		if item.Identity == s.detailSelected {
			return true
		}
	}
	s.detailSelected = results[0].Identity
	if nav, err := s.workspaceNavigation(); err == nil {
		_ = nav.PreviewDetail(s.detailSelected)
	}
	return true
}

func (s *tuiState) enterDetailedMode() bool {
	if !s.workspacePreview || s.hostScoped || s.centralModalOpen() {
		return false
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return false
	}
	nav.EnterDetail()
	s.detailQuery, s.detailFilter, s.detailSearchFocused = "", ducklord.DetailAll, false
	s.detailSelected = nav.DetailSelection()
	s.syncDetailSelection()
	s.workspaceProjectFocus = false
	return true
}

func (s *tuiState) exitDetailedMode() {
	if nav, err := s.workspaceNavigation(); err == nil {
		nav.ExitDetail()
	}
	s.detailQuery, s.detailFilter, s.detailSearchFocused = "", ducklord.DetailAll, false
	s.detailSelected = ducklord.SessionIdentity{}
}

// handleDetailedInput returns "changed" to reselect only the right-hand raw
// output, or "focus"/"jump" to enter the existing owner-gated attach path.
func (s *tuiState) handleDetailedInput(input []byte) string {
	key := string(input)
	nav, err := s.workspaceNavigation()
	if err != nil {
		return ""
	}
	if !nav.InDetailMode() {
		if s.shortcut("detail_list", key) && s.enterDetailedMode() {
			return "changed"
		}
		return ""
	}
	if s.shortcut("help", key) {
		return ""
	}
	if s.detailSearchFocused {
		switch key {
		case "\x1b":
			if s.detailQuery != "" {
				s.detailQuery = ""
				s.syncDetailSelection()
				return "changed"
			}
			s.detailSearchFocused = false
			return "changed"
		case "\r":
			s.detailSearchFocused = false
			return "changed"
		case "\x7f", "\b":
			runes := []rune(s.detailQuery)
			if len(runes) != 0 {
				s.detailQuery = string(runes[:len(runes)-1])
			}
		default:
			if utf8.Valid(input) {
				for _, r := range key {
					if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && utf8.RuneCountInString(s.detailQuery) < 128 {
						s.detailQuery += string(r)
					}
				}
			}
		}
		s.syncDetailSelection()
		return "changed"
	}
	if s.shortcut("detail_list", key) || key == "\x1b" {
		s.exitDetailedMode()
		return "changed"
	}
	if s.shortcut("detail_search", key) {
		s.detailSearchFocused = true
		return "changed"
	}
	if s.shortcut("detail_filter", key) {
		filters := []ducklord.DetailFilter{ducklord.DetailAll, ducklord.DetailUnread, ducklord.DetailNeedsAction, ducklord.DetailDisconnected}
		for i, filter := range filters {
			if filter == s.detailFilter {
				s.detailFilter = filters[(i+1)%len(filters)]
				break
			}
		}
		s.syncDetailSelection()
		return "changed"
	}
	if key == "j" || key == "\x1b[B" || key == "k" || key == "\x1b[A" {
		results := s.detailedResults()
		if len(results) == 0 {
			return "changed"
		}
		index := 0
		for i, item := range results {
			if item.Identity == s.detailSelected {
				index = i
				break
			}
		}
		if key == "j" || key == "\x1b[B" {
			index = min(len(results)-1, index+1)
		} else {
			index = max(0, index-1)
		}
		s.detailSelected = results[index].Identity
		_ = nav.PreviewDetail(s.detailSelected)
		return "changed"
	}
	if s.detailSelected.Key() == "" {
		return "changed"
	}
	if s.shortcut("detail_jump", key) {
		if _, err := nav.JumpDetail(); err != nil {
			s.outputErr = err.Error()
			return "changed"
		}
		s.detailQuery, s.detailSearchFocused = "", false
		s.detailFilter, s.detailSelected = ducklord.DetailAll, ducklord.SessionIdentity{}
		return "jump"
	}
	if key == "\r" {
		return "focus"
	}
	// Unbound keys do not leak into the previewed PTY.
	return "changed"
}

func (s *tuiState) detailSelectionSession() (ducklord.RemoteSession, bool) {
	nav, err := s.workspaceNavigation()
	if err != nil || !nav.InDetailMode() || s.detailSelected.Key() == "" || nav.DetailSelection() != s.detailSelected {
		return ducklord.RemoteSession{}, false
	}
	for _, session := range s.sessions {
		if identity, ok := ducklord.IdentityFromSession(session); ok && identity == nav.DetailSelection() {
			return session, true
		}
	}
	return ducklord.RemoteSession{}, false
}

func (s *tuiState) detailStatusLine() string {
	parts := []string{"Detailed Sessions: ↑/↓ preview · Enter focus · " + s.cfg.Shortcut("detail_jump") + " Project · " + s.cfg.Shortcut("detail_search") + " search · " + s.cfg.Shortcut("detail_filter") + " filter · " + s.cfg.Shortcut("detail_list") + " close"}
	if s.detailSearchFocused {
		parts = append(parts, "searching")
	}
	return strings.Join(parts, " · ")
}
