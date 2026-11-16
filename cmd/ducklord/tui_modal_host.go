package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

var hostActions = []string{"Connections", "Reconnect", "PTY log retention", "Agent notification hooks", "Skills", "Notification defaults", "Add host", "Remove host", "Resources"}

func (s *tuiState) renderHostModal(out io.Writer, cols, rows int) {
	if !s.hostMenuMode {
		return
	}
	s.resetModalMouse()
	if s.hostMenuStep == "host-list" {
		lines := []modalRenderLine{{modalTitle, "  Hosts"}, {modalMuted, "  Choose a host to view its settings"}}
		if len(s.cfg.Clients) == 0 {
			lines = append(lines, modalRenderLine{modalMuted, "  (no hosts configured)"})
		} else {
			for index, client := range s.cfg.Clients {
				s.modalChoice(len(lines), &s.hostMenuIndex, index, "\r")
				style, prefix := modalInput, "  "
				if index == s.hostMenuIndex {
					style, prefix = modalSelected, "› "
				}
				state := "connected"
				if s.disconnectedHosts[client.Name] {
					state = "disconnected"
				}
				lines = append(lines, modalRenderLine{style, prefix + displayField(client.Name) + " · " + state})
			}
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter settings · Esc close"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if strings.HasPrefix(s.hostMenuStep, "skills-") {
		s.renderHostSkillsModal(out, cols, rows)
		return
	}
	if strings.HasPrefix(s.hostMenuStep, "retention-") {
		lines := []modalRenderLine{{modalTitle, "  Host PTY log retention · " + displayField(s.hostMenuTarget)}}
		switch s.hostMenuStep {
		case "retention-loading":
			lines = append(lines, modalRenderLine{modalMuted, "  Reading current Host setting…"})
		case "retention-edit":
			lines = append(lines, modalRenderLine{modalInput, fmt.Sprintf("  Current: %d days", s.hostMenuOldDays)}, modalRenderLine{modalSelected, "  New days (1–3650): " + s.hostMenuDraft + "▏"}, modalRenderLine{modalMuted, "  Enter review · Esc back"})
		case "retention-confirm":
			lines = append(lines, modalRenderLine{modalSelected, fmt.Sprintf("  Change %d → %s days?", s.hostMenuOldDays, s.hostMenuDraft)}, modalRenderLine{modalDanger, "  Shortening retention may delete expired PTY logs now."}, modalRenderLine{modalMuted, "  Enter confirm · Esc edit"})
		case "retention-saving":
			lines = append(lines, modalRenderLine{modalMuted, "  Saving and verifying Host setting…"})
		case "retention-saved":
			lines = append(lines, modalRenderLine{modalStatus, fmt.Sprintf("  Saved: %d days", s.hostMenuOldDays)}, modalRenderLine{modalMuted, "  Enter or Esc close"})
		case "retention-error":
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr}, modalRenderLine{modalMuted, "  Enter retry · Esc back"})
		}
		if s.hostMenuErr != "" && s.hostMenuStep != "retention-error" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if strings.HasPrefix(s.hostMenuStep, "resources-") {
		lines := []modalRenderLine{{modalTitle, "  Host resources · " + displayField(s.hostMenuTarget)}}
		if s.hostMenuStep == "resources-loading" {
			lines = append(lines, modalRenderLine{modalMuted, "  Reading Host resources…"})
		} else if s.hostMenuErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr})
			if s.hostMenuStep == "resources-error" {
				lines = append(lines, modalRenderLine{modalMuted, "  Enter retry · Esc back"})
			}
		} else {
			r := s.hostResourceStatus
			lines = append(lines,
				modalRenderLine{modalStatus, fmt.Sprintf("  OS / arch: %s / %s", r.GOOS, r.GOARCH)},
				modalRenderLine{modalStatus, fmt.Sprintf("  CPU: %d · heap: %d bytes", r.CPUCount, r.HeapAllocBytes)},
				modalRenderLine{modalStatus, fmt.Sprintf("  Uptime: %s", (time.Duration(r.UptimeSeconds) * time.Second).String())},
				modalRenderLine{modalStatus, fmt.Sprintf("  Sessions: %d · PTYs: %d", r.ManagedSessionCount, r.ActivePTYCount)},
				modalRenderLine{modalMuted, "  r refresh · Esc back"})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if strings.HasPrefix(s.hostMenuStep, "hook-") {
		lines := []modalRenderLine{{modalTitle, "  Agent notification hooks · " + displayField(s.hostMenuTarget)}}
		if s.hostHookStatusLoading {
			lines = append(lines, modalRenderLine{modalMuted, "  Reading Host hook status…"})
		} else if s.hostHookStatusErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostHookStatusErr})
		} else if len(s.hostHookStatuses) > 0 {
			for _, agent := range []string{"codex", "claude"} {
				if status, ok := s.hostHookStatuses[agent]; ok {
					lines = append(lines, modalRenderLine{modalStatus, hostHookStatusLine(status)})
					if agent == "codex" && status.Installed && status.Activation != "operational" {
						lines = append(lines, modalRenderLine{modalMuted, "  Review Codex /hooks; awaiting a valid session callback."})
					}
				}
			}
		}
		switch s.hostMenuStep {
		case "hook-select":
			choices := []string{"Install Codex Stop hook", "Remove Codex Stop hook", "Install Claude Stop hooks", "Remove Claude Stop hooks"}
			for index, choice := range choices {
				s.modalChoice(len(lines), &s.hostMenuIndex, index, "\r")
				style, prefix := modalInput, "  "
				if index == s.hostMenuIndex {
					style, prefix = modalSelected, "› "
				}
				lines = append(lines, modalRenderLine{style, prefix + choice})
			}
			lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter review · Esc back"})
		case "hook-confirm":
			path := "~/.claude/settings.json"
			if s.hostHookAgent == "codex" {
				path = "~/.codex/hooks.json"
			}
			lines = append(lines,
				modalRenderLine{modalSelected, "  " + strings.ToUpper(s.hostHookAction[:1]) + s.hostHookAction[1:] + " " + s.hostHookAgent + " hook on this Host?"},
				modalRenderLine{modalInput, "  Edit: " + path},
				modalRenderLine{modalMuted, "  Existing settings are preserved; changed files get a private backup."},
				modalRenderLine{modalMuted, "  Only Ducklion's own hook entry is changed."})
			if s.hostHookAgent == "codex" && s.hostHookAction == "install" {
				lines = append(lines, modalRenderLine{modalDanger, "  Codex activation pending until trusted with /hooks."})
			}
			lines = append(lines, modalRenderLine{modalMuted, "  Enter confirm · Esc choose again"})
		case "hook-saving":
			lines = append(lines, modalRenderLine{modalMuted, "  Updating Host hook configuration…"})
		case "hook-done":
			lines = append(lines, modalRenderLine{modalStatus, "  " + s.hostMenuErr}, modalRenderLine{modalMuted, "  Enter or Esc close"})
		case "hook-error":
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr}, modalRenderLine{modalMuted, "  Enter retry · Esc back"})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if s.hostMenuStep == "connections" {
		lines := []modalRenderLine{{modalTitle, "  Host connections · checked means connected"}}
		for index, client := range s.cfg.Clients {
			s.modalChoice(len(lines), &s.hostMenuIndex, index, " ")
			style, cursor := modalInput, "  "
			changed := s.hostMenuSelected[client.Name] == s.disconnectedHosts[client.Name]
			changeMark := " "
			if changed {
				style, changeMark = modalStatus, "◆"
			}
			if index == s.hostMenuIndex {
				style, cursor = modalSelected, "› "
			}
			check := "[ ]"
			if s.hostMenuSelected[client.Name] {
				check = "[✓]"
			}
			state := "connected"
			if s.disconnectedHosts[client.Name] {
				state = "disconnected"
			}
			intent := ""
			if changed && s.hostMenuSelected[client.Name] {
				intent = " → will connect"
			} else if changed {
				intent = " → will disconnect"
			}
			lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%s%s %-18s %s%s", cursor, changeMark, check, displayField(client.Name), state, intent)})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ◆ changed · Space toggle · Enter apply all changes · Esc back"})
		if s.hostMenuErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	lines := []modalRenderLine{{modalTitle, "  Host actions · " + displayField(s.hostMenuTarget)}}
	for index, label := range hostActions {
		if !s.hostScoped || index < 5 {
			s.modalChoice(len(lines), &s.hostMenuIndex, index, "\r")
		}
		style, prefix := "", "  "
		if s.hostScoped && index >= 5 {
			style, label = modalDisabled, label+" (unavailable in host-scoped mode)"
		}
		if index == s.hostMenuIndex {
			prefix = "› "
			if style == "" {
				style = modalSelected
			}
		}
		lines = append(lines, modalRenderLine{style, prefix + label})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  Host connect state covers every session and notification on that host"}, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter run · Esc host list"})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) beginHostMenu() {
	if len(s.cfg.Clients) == 0 {
		s.outputErr = "no host selected"
		return
	}
	s.hostMenuTarget = s.selectedClientName()
	s.hostMenuIndex = 0
	for i, client := range s.cfg.Clients {
		if client.Name == s.hostMenuTarget {
			s.hostMenuIndex = i
			break
		}
	}
	s.hostMenuMode, s.hostMenuStep = true, "host-list"
	s.hostMenuSelected = make(map[string]bool)
	s.hostMenuErr = ""
}

func (s *tuiState) handleHostMenuInput(input []byte) string {
	if s.hostMenuStep == "host-list" {
		switch string(input) {
		case "\x1b", "\x03":
			s.hostMenuMode = false
			return "cancel"
		case "j", "\x1b[B":
			s.hostMenuIndex = min(len(s.cfg.Clients)-1, s.hostMenuIndex+1)
		case "k", "\x1b[A":
			s.hostMenuIndex = max(0, s.hostMenuIndex-1)
		case "\r", "\n":
			if len(s.cfg.Clients) > 0 {
				s.hostMenuTarget = s.cfg.Clients[s.hostMenuIndex].Name
				s.hostMenuStep, s.hostMenuIndex = "actions", 0
			}
		}
		return ""
	}
	if strings.HasPrefix(s.hostMenuStep, "resources-") {
		switch string(input) {
		case "r":
			s.hostMenuStep = "resources-loading"
			return "host-resources-read"
		case "\r", "\n":
			if s.hostMenuStep == "resources-error" {
				s.hostMenuStep = "resources-loading"
				return "host-resources-read"
			}
		case "\x1b":
			s.hostMenuStep, s.hostMenuIndex = "actions", 8
		case "\x03":
			s.hostMenuMode = false
		}
		return ""
	}
	if strings.HasPrefix(s.hostMenuStep, "skills-") {
		return s.handleHostSkillsInput(input)
	}
	if strings.HasPrefix(s.hostMenuStep, "hook-") {
		switch string(input) {
		case "\x03":
			if s.hostMenuStep != "hook-saving" {
				s.hostMenuMode = false
			}
		case "\x1b":
			switch s.hostMenuStep {
			case "hook-confirm":
				s.hostMenuStep = "hook-select"
			case "hook-done":
				s.hostMenuMode = false
			case "hook-saving":
			default:
				s.hostMenuStep, s.hostMenuIndex = "actions", 3
			}
		case "j", "\x1b[B":
			if s.hostMenuStep == "hook-select" {
				s.hostMenuIndex = min(3, s.hostMenuIndex+1)
			}
		case "k", "\x1b[A":
			if s.hostMenuStep == "hook-select" {
				s.hostMenuIndex = max(0, s.hostMenuIndex-1)
			}
		case "\r", "\n":
			switch s.hostMenuStep {
			case "hook-select":
				s.hostHookAgent, s.hostHookAction = []string{"codex", "codex", "claude", "claude"}[s.hostMenuIndex], []string{"install", "remove", "install", "remove"}[s.hostMenuIndex]
				s.hostMenuStep = "hook-confirm"
			case "hook-confirm":
				s.hostMenuStep = "hook-saving"
				return "host-hook-save"
			case "hook-error":
				s.hostMenuStep = "hook-saving"
				return "host-hook-save"
			case "hook-done":
				s.hostMenuMode = false
			}
		}
		return ""
	}
	if strings.HasPrefix(s.hostMenuStep, "retention-") {
		switch string(input) {
		case "\x03":
			if s.hostMenuStep != "retention-saving" {
				s.hostMenuMode = false
			}
		case "\x1b":
			if s.hostMenuStep == "retention-confirm" {
				s.hostMenuStep = "retention-edit"
			} else if s.hostMenuStep == "retention-saved" {
				s.hostMenuMode = false
			} else if s.hostMenuStep != "retention-saving" {
				s.hostMenuStep, s.hostMenuIndex = "actions", 2
			}
		case "\x7f", "\b":
			if s.hostMenuStep == "retention-edit" && len(s.hostMenuDraft) > 0 {
				if s.hostMenuReplaceDraft {
					s.hostMenuDraft = ""
				} else {
					s.hostMenuDraft = s.hostMenuDraft[:len(s.hostMenuDraft)-1]
				}
				s.hostMenuReplaceDraft = false
				s.hostMenuErr = ""
			}
		case "\r", "\n":
			switch s.hostMenuStep {
			case "retention-saved":
				s.hostMenuMode = false
			case "retention-error":
				s.hostMenuStep, s.hostMenuErr = "retention-loading", ""
				return "host-retention-read"
			case "retention-edit":
				days, err := strconv.Atoi(s.hostMenuDraft)
				if err != nil || days < 1 || days > 3650 {
					s.hostMenuErr = "Enter a number from 1 to 3650"
				} else if days == s.hostMenuOldDays {
					s.hostMenuStep, s.hostMenuIndex = "actions", 3
				} else {
					s.hostMenuStep = "retention-confirm"
				}
			case "retention-confirm":
				s.hostMenuStep = "retention-saving"
				return "host-retention-save"
			}
		default:
			if s.hostMenuStep == "retention-edit" && len(input) == 1 && input[0] >= '0' && input[0] <= '9' && len(s.hostMenuDraft) < 4 {
				if s.hostMenuReplaceDraft {
					s.hostMenuDraft = string(input)
				} else {
					s.hostMenuDraft += string(input)
				}
				s.hostMenuReplaceDraft = false
				s.hostMenuErr = ""
			}
		}
		return ""
	}
	if s.hostMenuStep == "connections" {
		if len(s.cfg.Clients) == 0 {
			return ""
		}
		switch string(input) {
		case "\x03":
			s.hostMenuMode = false
			return "cancel"
		case "\x1b":
			s.hostMenuStep, s.hostMenuIndex = "actions", 0
			return ""
		case "j", "\x1b[B":
			s.hostMenuIndex = min(len(s.cfg.Clients)-1, s.hostMenuIndex+1)
		case "k", "\x1b[A":
			s.hostMenuIndex = max(0, s.hostMenuIndex-1)
		case " ":
			name := s.cfg.Clients[s.hostMenuIndex].Name
			s.hostMenuSelected[name] = !s.hostMenuSelected[name]
			s.hostMenuErr = ""
		case "\r", "\n":
			if len(s.changedHostMenuTargets(true))+len(s.changedHostMenuTargets(false)) == 0 {
				s.hostMenuErr = "no connection changes selected"
				return ""
			}
			return "host-apply-connections"
		}
		return ""
	}
	switch string(input) {
	case "\x1b", "\x03":
		if string(input) == "\x03" {
			s.hostMenuMode = false
			return "cancel"
		}
		s.hostMenuStep = "host-list"
		for i, client := range s.cfg.Clients {
			if client.Name == s.hostMenuTarget {
				s.hostMenuIndex = i
				break
			}
		}
		return ""
	case "j", "\x1b[B":
		s.hostMenuIndex = min(len(hostActions)-1, s.hostMenuIndex+1)
	case "k", "\x1b[A":
		s.hostMenuIndex = max(0, s.hostMenuIndex-1)
	case "\r", "\n":
		action := []string{"host-connections", "host-reconnect", "host-retention-read", "host-hooks", "host-skills", "host-notification-settings", "host-add", "host-remove", "host-resources-read"}[s.hostMenuIndex]
		if s.hostScoped && (action == "host-add" || action == "host-remove" || action == "host-notification-settings") {
			s.outputErr = "host configuration changes are unavailable in host-scoped mode"
			return ""
		}
		if action == "host-connections" {
			s.hostMenuStep = "connections"
			s.hostMenuIndex = 0
			s.hostMenuErr = ""
			for _, client := range s.cfg.Clients {
				s.hostMenuSelected[client.Name] = !s.disconnectedHosts[client.Name]
			}
			if s.hostMenuTarget != "" {
				for index, client := range s.cfg.Clients {
					if client.Name == s.hostMenuTarget {
						s.hostMenuIndex = index
						break
					}
				}
			}
			return ""
		}
		if action == "host-resources-read" {
			s.hostMenuStep = "resources-loading"
			s.hostMenuErr = ""
			return action
		}
		if action == "host-retention-read" {
			s.hostMenuStep = "retention-loading"
			s.hostMenuErr = ""
		}
		if action == "host-hooks" {
			s.hostMenuStep, s.hostMenuIndex = "hook-select", 0
			s.hostMenuErr = ""
			return "host-hooks-read"
		}
		if action == "host-skills" {
			s.beginHostSkills()
			return ""
		}
		return action
	}
	return ""
}

func (s *tuiState) changedHostMenuTargets(desired bool) []string {
	targets := make([]string, 0, len(s.cfg.Clients))
	for _, client := range s.cfg.Clients {
		currentDesired := !s.disconnectedHosts[client.Name]
		if s.hostMenuSelected[client.Name] == desired && desired != currentDesired {
			targets = append(targets, client.Name)
		}
	}
	return targets
}

func (s *tuiState) renderGroupModal(out io.Writer, cols, rows int) {
	if !s.groupMenu {
		return
	}
	s.resetModalMouse()
	lines := []modalRenderLine{{modalTitle, "  Custom groups"}}
	if s.groupMenuStep == "name" {
		verb := "Create group"
		if s.groupMenuAction == "rename" {
			verb = "Rename group"
		}
		lines = append(lines,
			modalRenderLine{modalStatus, "  " + verb + " · 1–64 Unicode characters"},
			modalRenderLine{modalInput, "  name › " + s.groupMenuLine})
	} else {
		labels := make([]string, 0)
		if s.groupMenuStep == "action" {
			for _, action := range groupMenuActions {
				labels = append(labels, action.label)
			}
		} else {
			for _, group := range s.groupMenuChoices() {
				label := group.Name
				if group.ID != ducklord.UngroupedGroupID {
					label = s.customGroupDisplayName(group)
				}
				labels = append(labels, label)
			}
		}
		if len(labels) == 0 {
			lines = append(lines, modalRenderLine{modalMuted, "  No custom groups"})
		}
		selected := min(max(s.groupMenuIndex, 0), max(0, len(labels)-1))
		start := 0
		maxChoices := max(1, rows-5)
		if len(labels) > maxChoices {
			start = max(0, selected-maxChoices/2)
			start = min(start, len(labels)-maxChoices)
			labels = labels[start : start+maxChoices]
		}
		for index, label := range labels {
			absoluteIndex := start + index
			s.modalChoice(len(lines), &s.groupMenuIndex, absoluteIndex, "\r")
			style, prefix := "", "  "
			if absoluteIndex == selected {
				style, prefix = modalSelected, "› "
			}
			if s.groupMenuStep == "action" && groupMenuActions[absoluteIndex].id == "delete" {
				style = modalDanger
				if absoluteIndex != selected {
					style = modalStatus
				}
			}
			lines = append(lines, modalRenderLine{style, prefix + label})
		}
	}
	status := s.groupMenuErr
	if status == "" {
		status = "Changes are local to Ducklord"
	}
	footer := "  ↑/↓ or j/k choose · Enter confirm · Esc cancel"
	if s.groupMenuStep == "name" {
		footer = "  Type name · Enter confirm · Esc cancel"
	}
	lines = append(lines, modalRenderLine{modalStatus, "  " + status}, modalRenderLine{modalMuted, footer})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) customGroupDisplayName(group ducklord.CustomGroup) string {
	duplicates := make([]string, 0)
	for _, candidate := range s.activity().Organization.Groups {
		if candidate.Name == group.Name {
			duplicates = append(duplicates, candidate.ID)
		}
	}
	if len(duplicates) < 2 {
		return group.Name
	}
	prefixLength := 4
	for ; prefixLength < len(group.ID); prefixLength++ {
		prefix := group.ID[:prefixLength]
		unique := true
		for _, id := range duplicates {
			if id != group.ID && strings.HasPrefix(id, prefix) {
				unique = false
				break
			}
		}
		if unique {
			break
		}
	}
	return group.Name + " · " + group.ID[:min(prefixLength, len(group.ID))]
}
