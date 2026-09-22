package main

import (
	"fmt"
	"hash/fnv"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) commandPaletteGeneration() uint64 {
	h := fnv.New64a()
	if s.activityState != nil {
		_, _ = fmt.Fprintf(h, "%#v", s.activity().ProjectLayout.Projects)
	}
	// Palette session targets are live rows; fence replacement/removal and
	// renames as well as workspace layout changes.
	_, _ = fmt.Fprintf(h, "%#v", s.sessions)
	return h.Sum64()
}

type commandPaletteItem struct {
	label, kind   string
	project, pane string
}

func (s *tuiState) openCommandPalette() {
	if s.commandPaletteMode {
		return
	}
	s.commandPalettePreviousFocused = s.focused
	s.commandPalettePreviousAttachKey = s.activeAttachKey
	s.commandPalettePreviousProjectFocus = s.workspaceProjectFocus
	s.commandPaletteWorkspaceGeneration = s.commandPaletteGeneration()
	if nav, err := s.workspaceNavigation(); err == nil {
		s.commandPalettePreviousProjectID, s.commandPalettePreviousPaneID = nav.CurrentProjectID(), nav.CurrentPaneID()
	}
	s.commandPaletteMode, s.commandPaletteQuery, s.commandPaletteIndex = true, "", 0
	s.commandPaletteInput = nil
	s.focused = false
}

func (s *tuiState) closeCommandPalette() {
	if !s.commandPaletteMode {
		return
	}
	restored := true
	if nav, err := s.workspaceNavigation(); err == nil {
		if s.commandPalettePreviousProjectID != "" {
			if err := nav.SelectProject(s.commandPalettePreviousProjectID); err != nil {
				restored = false
				s.outputErr = "command palette restore: " + err.Error()
			}
		}
		if restored && s.commandPalettePreviousPaneID != "" {
			if err := nav.SelectPane(s.commandPalettePreviousProjectID, s.commandPalettePreviousPaneID); err != nil {
				restored = false
				s.outputErr = "command palette restore: " + err.Error()
			}
		}
	} else if s.commandPalettePreviousProjectID != "" || s.commandPalettePreviousPaneID != "" {
		// A focused shell can exist before the user has saved a Project layout.
		// There is no workspace position to restore in that case; preserving the
		// still-live PTY is the exact origin restoration.
		restored = false
	}
	if restored && s.commandPalettePreviousFocused && s.commandPalettePreviousAttachKey != "" {
		if _, ok := s.sessionForKey(s.commandPalettePreviousAttachKey); !ok {
			restored = false
		}
	}
	if restored {
		s.workspaceProjectFocus = s.commandPalettePreviousProjectFocus
		s.focused, s.activeAttachKey = s.commandPalettePreviousFocused, s.commandPalettePreviousAttachKey
	} else {
		// The origin may have disappeared while the modal was open. Leave input
		// with the surviving workspace navigation rather than a dead PTY lease.
		s.workspaceProjectFocus, s.focused, s.activeAttachKey = true, false, ""
	}
	s.commandPaletteMode, s.commandPaletteQuery = false, ""
	s.commandPaletteInput = nil
}

func (s *tuiState) commandPaletteItems() []commandPaletteItem {
	items := []commandPaletteItem{{"Notes", "notes", "", ""}, {"Help", "help", "", ""}, {"Host list", "host", "", ""}, {"Project files", "project-files", "", ""}}
	if s.activityState != nil {
		for _, p := range s.activity().ProjectLayout.Projects {
			items = append(items, commandPaletteItem{p.Name, "project", p.ID, ""})
		}
		for _, session := range s.sessions {
			project := session.ProjectName
			for _, p := range s.activity().ProjectLayout.Projects {
				if p.Name != project {
					continue
				}
				identity, ok := ducklord.IdentityFromSession(session)
				if !ok {
					continue
				}
				for _, tab := range p.Tabs {
					if pane := paletteFindPane(tab.Root, identity); pane != "" {
						items = append(items, commandPaletteItem{session.Name, "session", p.ID, pane})
						break
					}
				}
				break
			}
		}
	}
	q := strings.ToLower(strings.TrimSpace(s.commandPaletteQuery))
	if q == "" {
		return items
	}
	out := items[:0]
	for _, item := range items {
		if paletteFuzzyMatch(item.label, q) {
			out = append(out, item)
		}
	}
	return out
}

// paletteFuzzyMatch reports whether query's characters occur in order in label.
// Matching is case-insensitive and deliberately keeps the original item order.
func paletteFuzzyMatch(label, query string) bool {
	labelRunes, queryRunes := []rune(strings.ToLower(label)), []rune(strings.ToLower(query))
	labelIndex := 0
	for _, queryRune := range queryRunes {
		for labelIndex < len(labelRunes) && labelRunes[labelIndex] != queryRune {
			labelIndex++
		}
		if labelIndex == len(labelRunes) {
			return false
		}
		labelIndex++
	}
	return true
}

func paletteFindPane(p *ducklord.SessionPane, identity ducklord.SessionIdentity) string {
	if p == nil {
		return ""
	}
	if p.Session != nil && *p.Session == identity {
		return p.ID
	}
	if id := paletteFindPane(p.First, identity); id != "" {
		return id
	}
	return paletteFindPane(p.Second, identity)
}

func (s *tuiState) handleCommandPaletteInput(input []byte) bool {
	if !s.commandPaletteMode {
		return false
	}
	switch string(input) {
	case "\x1b", "\x03":
		s.closeCommandPalette()
		return true
	case "\x1b[A", "k":
		if s.commandPaletteIndex > 0 {
			s.commandPaletteIndex--
		}
		return true
	case "\x1b[B", "j":
		if n := len(s.commandPaletteItems()); n > 0 && s.commandPaletteIndex < n-1 {
			s.commandPaletteIndex++
		}
		return true
	case "\r", "\n":
		items := s.commandPaletteItems()
		if len(items) == 0 {
			return true
		}
		item := items[min(s.commandPaletteIndex, len(items)-1)]
		s.closeCommandPalette()
		switch item.kind {
		case "notes":
			s.openPrefixPane("o")
		case "help":
			s.helpMode = true
		case "host":
			s.beginHostMenu()
		case "project-files":
			s.openProjectFiles()
		case "project":
			if s.commandPaletteGeneration() != s.commandPaletteWorkspaceGeneration {
				s.outputErr = "command palette target is stale"
				return true
			}
			if nav, err := s.workspaceNavigation(); err == nil {
				_ = nav.SelectProject(item.project)
				s.workspaceProjectFocus = true
				s.focused = false
			}
		case "session":
			if s.commandPaletteGeneration() != s.commandPaletteWorkspaceGeneration {
				s.outputErr = "command palette target is stale"
				return true
			}
			if nav, err := s.workspaceNavigation(); err == nil {
				_ = nav.SelectProject(item.project)
				_ = nav.SelectPane(item.project, item.pane)
				s.workspaceProjectFocus = false
				s.focused = false
			}
		}
		return true
	default:
		s.commandPaletteInput = append(s.commandPaletteInput, input...)
		for len(s.commandPaletteInput) > 0 {
			r, n := utf8.DecodeRune(s.commandPaletteInput)
			if r == utf8.RuneError && n == 1 && !utf8.Valid(s.commandPaletteInput[:]) {
				break
			}
			s.commandPaletteInput = s.commandPaletteInput[n:]
			if r >= 0x20 && r != 0x7f {
				s.commandPaletteQuery += string(r)
				s.commandPaletteIndex = 0
			}
		}
		return true
	}
	return true
}

func (s *tuiState) renderCommandPalette(out io.Writer, cols, rows int) {
	if !s.commandPaletteMode {
		return
	}
	lines := []modalRenderLine{{modalTitle, "Command palette"}, {modalMuted, fmt.Sprintf("> %s", s.commandPaletteQuery)}}
	items := s.commandPaletteItems()
	for i, item := range items {
		style := modalMuted
		prefix := "  "
		if i == s.commandPaletteIndex {
			style = modalInput
			prefix = "› "
		}
		lines = append(lines, modalRenderLine{style, prefix + item.label})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter run · Esc close"})
	s.renderModalBox(out, cols, rows, lines)
}
