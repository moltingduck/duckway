package main

import (
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) renderSearchModal(out io.Writer, cols, rows int) {
	if !s.searchMode {
		return
	}
	s.resetModalMouse()
	results := s.searchResults()
	totalResults := len(results)
	lines := []modalRenderLine{
		{modalTitle, "  Search sessions"},
		{modalInput, "  search › " + s.searchQuery},
	}
	maxResults := max(1, (rows-7)/2)
	start := 0
	if len(results) > maxResults {
		start = min(max(0, s.searchSelected-maxResults/2), len(results)-maxResults)
		results = results[start : start+maxResults]
	}
	if len(results) == 0 {
		lines = append(lines, modalRenderLine{modalStatus, "  No matching sessions"})
	} else {
		currentGroup := "\x00"
		for index, session := range results {
			absolute := start + index
			groupID := s.organizationGroupID(session)
			if groupID != currentGroup {
				groupLabel := "  [" + displayField(s.organizationGroupLabel(session)) + "]"
				if s.groupHasUnread(groupID) {
					groupLabel += " •"
				}
				lines = append(lines, modalRenderLine{modalMuted, groupLabel})
				currentGroup = groupID
			}
			style, prefix := "", "  "
			if absolute == s.searchSelected {
				style, prefix = modalSelected, "› "
			}
			mark := " "
			if sessionKey(session) == s.activeAttachKey {
				mark = "●"
			} else if session.Unread {
				mark = "•"
			}
			s.modalChoice(len(lines), &s.searchSelected, absolute, "\r")
			lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%s %-12s %-18s %-8s %s", prefix, mark,
				displayField(session.Client), displayField(session.Name), displayField(session.Kind), displayField(session.AgentType))})
		}
	}
	status := fmt.Sprintf("  %d of %d sessions match", totalResults, len(s.sessions))
	switch totalResults {
	case 0:
		status = fmt.Sprintf("  0 matches (of %d sessions)", len(s.sessions))
	case 1:
		status = fmt.Sprintf("  1 match (of %d sessions)", len(s.sessions))
	}
	if s.searchErr != "" {
		status = "  " + sanitizeTerminalText(s.searchErr)
	}
	lines = append(lines, modalRenderLine{modalStatus, status}, modalRenderLine{modalMuted, "  Type to filter · ↑/↓ select · Enter activate · Esc close"})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) beginSearch() {
	s.searchMode = true
	s.searchQuery = ""
	s.searchSelected = 0
	s.searchSelectedKey = ""
	s.searchRevision++
	s.searchActivatedKey = ""
	s.searchActivatedRevision = 0
	s.searchActivatedGeneration = 0
	s.searchErr = ""
	s.syncSearchSelection()
}

func (s *tuiState) closeSearch() {
	s.searchMode = false
	s.searchQuery = ""
	s.searchSelected = 0
	s.searchSelectedKey = ""
	s.searchActivatedKey = ""
	s.searchActivatedRevision = 0
	s.searchActivatedGeneration = 0
	s.searchErr = ""
	s.searchPendingRequestID = 0
	s.searchPendingKey = ""
	s.searchPendingGeneration = 0
	s.searchPendingRevision = 0
}

func (s *tuiState) searchResult() (ducklord.RemoteSession, bool) {
	results := s.searchResults()
	if len(results) == 0 || s.searchSelected < 0 || s.searchSelected >= len(results) {
		return ducklord.RemoteSession{}, false
	}
	return results[s.searchSelected], true
}

func (s *tuiState) selectSessionKey(key string) bool {
	for index, session := range s.sessions {
		if sessionKey(session) == key {
			s.selected = index
			s.selectedKey = key
			return true
		}
	}
	return false
}

func (s *tuiState) sessionForKey(key string) (ducklord.RemoteSession, bool) {
	for _, session := range s.sessions {
		if sessionKey(session) == key {
			return session, true
		}
	}
	return ducklord.RemoteSession{}, false
}

func (s *tuiState) selectableSessionMatches(key string, generation uint64) bool {
	session, ok := s.sessionForKey(key)
	return ok && session.RuntimeGeneration == generation && canRead(session) && s.hostIsLive(session.Client)
}

func (s *tuiState) searchResultIsActivated(session ducklord.RemoteSession) bool {
	key := sessionKey(session)
	return s.searchActivatedKey == key && s.searchActivatedRevision == s.searchRevision &&
		s.searchActivatedGeneration == session.RuntimeGeneration && s.outputForKey == key && s.outputFresh
}

func (s *tuiState) searchResults() []ducklord.RemoteSession {
	terms := strings.Fields(strings.ToLower(modalDisplayText(s.searchQuery)))
	if len(terms) == 0 {
		return append([]ducklord.RemoteSession(nil), s.sessions...)
	}
	results := make([]ducklord.RemoteSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		fields := strings.ToLower(strings.Join([]string{
			modalDisplayText(session.Name), modalDisplayText(session.ProjectName), modalDisplayText(session.Client),
			modalDisplayText(s.organizationGroupLabel(session)), modalDisplayText(session.Kind), modalDisplayText(session.AgentType), modalDisplayText(sessionTypeLabel(session)),
		}, "\n"))
		matched := true
		for _, term := range terms {
			if !strings.Contains(fields, term) {
				matched = false
				break
			}
		}
		if matched {
			results = append(results, session)
		}
	}
	return results
}

func (s *tuiState) syncSearchSelection() {
	results := s.searchResults()
	if len(results) == 0 {
		s.searchSelected = 0
		s.searchSelectedKey = ""
		return
	}
	if s.searchSelectedKey != "" {
		for index, session := range results {
			if sessionKey(session) == s.searchSelectedKey {
				s.searchSelected = index
				return
			}
		}
	}
	s.searchSelected = min(max(s.searchSelected, 0), len(results)-1)
	s.searchSelectedKey = sessionKey(results[s.searchSelected])
}

func (s *tuiState) handleSearchInput(input []byte) string {
	results := s.searchResults()
	switch string(input) {
	case "\x1b":
		return "cancel"
	case "\x1b[B":
		if s.searchSelected < len(results)-1 {
			s.searchSelected++
			s.searchSelectedKey = sessionKey(results[s.searchSelected])
		}
	case "\x1b[A":
		if s.searchSelected > 0 {
			s.searchSelected--
			s.searchSelectedKey = sessionKey(results[s.searchSelected])
		}
	case "\r", "\n":
		if len(results) > 0 {
			return "activate"
		}
		return ""
	case "\b", "\x7f":
		if s.searchQuery != "" {
			runes := []rune(s.searchQuery)
			s.searchQuery = string(runes[:len(runes)-1])
			s.searchRevision++
			s.searchSelected = 0
			s.searchSelectedKey = ""
			s.searchActivatedKey = ""
			s.searchActivatedGeneration = 0
		}
	default:
		if utf8.Valid(input) {
			for _, r := range string(input) {
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && len(s.searchQuery)+utf8.RuneLen(r) <= 1024 {
					s.searchQuery += string(r)
				}
			}
			s.searchRevision++
			s.searchSelected = 0
			s.searchSelectedKey = ""
			s.searchActivatedKey = ""
			s.searchActivatedGeneration = 0
		}
	}
	if string(input) == "\x1b[A" || string(input) == "\x1b[B" {
		s.searchRevision++
		s.searchActivatedKey = ""
		s.searchActivatedRevision = 0
		s.searchActivatedGeneration = 0
	}
	s.syncSearchSelection()
	return ""
}
