package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
	"github.com/hackerduck/duckway/internal/projectregistry"
)

type sessionAction struct {
	ID             string
	Key            string
	Label          string
	Enabled        bool
	DisabledReason string
	Danger         bool
	Operation      protocol.SessionLifecycleOperation
	Mode           protocol.SessionLifecycleMode
}

func actionOwnerLabel(session ducklord.RemoteSession) string {
	if session.Kind == string(model.KindShell) {
		return "shared"
	}
	if session.WriterID == "" {
		return "none"
	}
	return session.WriterKind + ":" + session.WriterID
}

func (s *tuiState) sessionActions(session ducklord.RemoteSession) []sessionAction {
	live := session.SessionID != "" && s.hostIsLive(session.Client)
	attachEnabled := live && canAttach(session)
	attachLabel := "Open PTY"
	if session.Kind == string(model.KindAgent) && (session.WriterKind != string(model.OwnerTerminal) || session.WriterID != s.ownerName) {
		attachLabel = "View PTY (read-only)"
	}
	actions := []sessionAction{
		{ID: "attach", Key: "o", Label: attachLabel, Enabled: attachEnabled, DisabledReason: "session is not running or host is reconnecting"},
		{ID: "reconnect", Key: "c", Label: "Reconnect PTY output", Enabled: attachEnabled, DisabledReason: "session is not running or host is reconnecting"},
	}
	if session.Kind == string(model.KindAgent) {
		adapterHealthy := session.AdapterState == string(model.AdapterHealthy)
		nonOwner := session.WriterKind != string(model.OwnerTerminal) || session.WriterID != s.ownerName
		yieldReady := live && adapterHealthy && nonOwner
		actions = append(actions,
			sessionAction{ID: "yield", Key: "y", Label: "Yield control now", Enabled: yieldReady && session.TaskState == string(model.TaskIdle), DisabledReason: actionDisabledReason(live, adapterHealthy, nonOwner, session.TaskState == string(model.TaskIdle))},
			sessionAction{ID: "yield-wait", Key: "Y", Label: "Yield when idle", Enabled: yieldReady, DisabledReason: actionDisabledReason(live, adapterHealthy, nonOwner, true)},
		)
	}
	actions = append(actions, sessionAction{ID: "notifications", Key: "n", Label: "Notification settings (local)", Enabled: session.InstanceID != "" && session.SessionID != "", DisabledReason: "session identity is unavailable"})
	renameAction := sessionAction{ID: "rename-handle", Key: "h", Label: "Rename handle", Enabled: session.InstanceID != "" && session.SessionID != "", DisabledReason: "session identity is unavailable"}
	lifecycleEnabled := live && !s.lifecycleBusy && (session.Kind == string(model.KindShell) || session.WriterKind == string(model.OwnerTerminal) && session.WriterID == s.ownerName)
	lifecycleReason := "host is reconnecting"
	if s.lifecycleBusy {
		lifecycleReason = "another lifecycle operation is pending"
	} else if live && session.Kind == string(model.KindAgent) {
		lifecycleReason = "yield control to this Ducklord first"
	}
	addLifecycle := func(id, key, label string, operation protocol.SessionLifecycleOperation, mode protocol.SessionLifecycleMode, allowed bool, danger bool) {
		reason := lifecycleReason
		if lifecycleEnabled && !allowed {
			reason = "current task or adapter state does not support this mode"
		}
		actions = append(actions, sessionAction{ID: id, Key: key, Label: label, Enabled: lifecycleEnabled && allowed, DisabledReason: reason, Danger: danger, Operation: operation, Mode: mode})
	}
	if session.Kind == string(model.KindShell) {
		addLifecycle("restart", "r", "Restart shell immediately", protocol.SessionLifecycleRestart, protocol.SessionLifecycleImmediate, true, false)
		addLifecycle("end", "e", "End shell immediately", protocol.SessionLifecycleEnd, protocol.SessionLifecycleImmediate, true, true)
		addLifecycle("destroy", "x", "Destroy shell and logs", protocol.SessionLifecycleDestroy, protocol.SessionLifecycleImmediate, true, true)
		actions = append(actions, renameAction)
		return actions
	}
	idle := session.TaskState == string(model.TaskIdle)
	adapterHealthy := session.AdapterState == string(model.AdapterHealthy)
	addLifecycle("restart", "r", "Restart now", protocol.SessionLifecycleRestart, protocol.SessionLifecycleImmediate, idle, false)
	addLifecycle("restart-wait", "", "Restart when idle", protocol.SessionLifecycleRestart, protocol.SessionLifecycleWait, adapterHealthy, false)
	addLifecycle("restart-force", "", "Restart and force-cancel task", protocol.SessionLifecycleRestart, protocol.SessionLifecycleForce, adapterHealthy, true)
	addLifecycle("end", "e", "End now", protocol.SessionLifecycleEnd, protocol.SessionLifecycleImmediate, idle, true)
	addLifecycle("end-wait", "", "End when idle", protocol.SessionLifecycleEnd, protocol.SessionLifecycleWait, adapterHealthy, true)
	addLifecycle("end-force", "", "End and force-cancel task", protocol.SessionLifecycleEnd, protocol.SessionLifecycleForce, adapterHealthy, true)
	addLifecycle("destroy", "x", "Destroy now", protocol.SessionLifecycleDestroy, protocol.SessionLifecycleImmediate, idle, true)
	addLifecycle("destroy-wait", "", "Destroy when idle", protocol.SessionLifecycleDestroy, protocol.SessionLifecycleWait, adapterHealthy, true)
	addLifecycle("destroy-force", "", "Destroy and force-cancel task", protocol.SessionLifecycleDestroy, protocol.SessionLifecycleForce, adapterHealthy, true)
	actions = append(actions, renameAction)
	return actions
}

func actionDisabledReason(live, adapterHealthy, nonOwner, idle bool) string {
	switch {
	case !live:
		return "host is reconnecting"
	case !adapterHealthy:
		return "agent adapter is not healthy"
	case !nonOwner:
		return "this Ducklord already controls the session"
	case !idle:
		return "agent task is running; use Yield when idle"
	default:
		return "unavailable"
	}
}

func (s *tuiState) renderCreateModal(out io.Writer, cols, rows int) {
	if !s.newSessionMode || cols < 8 || rows < 3 {
		return
	}
	s.resetModalMouse()
	if cols < 20 || rows < 7 {
		choices := s.createModalChoices()
		selected := s.createModalSelectedIndex(len(choices))
		if s.newSessionStep == "path" {
			selected = -1
			if len(choices) > 0 {
				selected = min(max(s.newSessionPathSelected, 0), len(choices)-1)
			}
		}
		choice := ""
		if selected >= 0 {
			choice = "› " + choices[selected]
		}
		boxWidth := max(8, cols-2)
		innerWidth := boxWidth - 2
		boxHeight := min(4, rows)
		top := max(1, (rows-boxHeight)/2+1)
		left := max(1, (cols-boxWidth)/2+1)
		writeRow := func(row int, style, text string) {
			text = modalCellPad(modalCellTruncate(modalDisplayText(text), innerWidth), innerWidth)
			fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", row, left, modalSurface+modalBorder, modalSurface+style, text, modalReset+modalSurface+modalBorder, modalReset)
		}
		writePrompt := func(row int) {
			s.renderCreatePromptRow(out, row, left, innerWidth, "")
		}
		fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
		if boxHeight == 3 {
			s.renderCreatePromptRow(out, top+1, left, innerWidth, choice+"  ")
			s.createMouseRegion(left+1, top+1, min(innerWidth, modalCellWidth(choice)), selected)
		} else {
			writeRow(top+1, modalSelected, choice)
			s.createMouseRegion(left+1, top+1, innerWidth, selected)
			writePrompt(top + 2)
		}
		fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+boxHeight-1, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
		return
	}
	boxWidth := min(72, cols-4)
	innerWidth := boxWidth - 2
	allChoices := s.createModalChoices()
	selected := s.createModalSelectedIndex(len(allChoices))
	if s.newSessionStep == "path" {
		selected = -1
		if len(allChoices) > 0 {
			selected = min(max(s.newSessionPathSelected, 0), len(allChoices)-1)
		}
	}
	choices := allChoices
	maxChoices := max(1, rows-7)
	hiddenBefore, hiddenAfter := 0, 0
	if len(choices) > maxChoices {
		start := max(0, selected-maxChoices/2)
		start = min(start, len(choices)-maxChoices)
		hiddenBefore = start
		hiddenAfter = len(choices) - start - maxChoices
		choices = choices[start : start+maxChoices]
		selected -= start
	}
	boxHeight := len(choices) + 6
	top := max(1, (rows-boxHeight)/2+1)
	left := max(1, (cols-boxWidth)/2+1)
	writeRow := func(row int, style, text string) {
		text = modalCellPad(modalCellTruncate(modalDisplayText(text), innerWidth), innerWidth)
		fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", row, left, modalSurface+modalBorder, modalSurface+style, text, modalReset+modalSurface+modalBorder, modalReset)
	}
	fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
	writeRow(top+1, modalTitle, "  "+s.createHeader())
	for i, choice := range choices {
		prefix, style := "  ", ""
		if i == selected && !s.newSessionDiscovering && !s.newSessionStarting {
			prefix, style = "› ", modalSelected
		}
		writeRow(top+2+i, style, prefix+choice)
		s.createMouseRegion(left+1, top+2+i, innerWidth, hiddenBefore+i)
	}
	status := s.newSessionErr
	if hiddenBefore+hiddenAfter > 0 {
		status = fmt.Sprintf("%s  (%d above, %d below)", status, hiddenBefore, hiddenAfter)
	}
	writeRow(top+2+len(choices), modalStatus, "  "+status)
	s.renderCreatePromptRow(out, top+3+len(choices), left, innerWidth, "  ")
	writeRow(top+4+len(choices), modalMuted, "  ↑/↓ select   Enter continue   Esc back   Ctrl+C close")
	s.modalHintRegions("  ↑/↓ select   Enter continue   Esc back   Ctrl+C close", left+1, top+4+len(choices), innerWidth)
	fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+5+len(choices), left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
}

func (s *tuiState) renderCreatePromptRow(out io.Writer, row, left, innerWidth int, indent string) {
	prefix := modalDisplayText(indent + s.createPromptLabel() + " › " + s.newSessionLine)
	ghost := ""
	if s.newSessionLine == "" {
		switch s.newSessionStep {
		case "handle", "project-name":
			ghost = modalDisplayText(defaultSessionHandle(s.newSessionCWD))
			if s.newSessionStep == "project-name" {
				ghost = modalDisplayText(projectregistry.DefaultProjectName(filepath.Base(s.newSessionCWD)))
			}
		}
	}
	prefix = modalCellTruncate(prefix, innerWidth)
	remaining := max(0, innerWidth-modalCellWidth(prefix))
	ghost = modalCellTruncate(ghost, remaining)
	padding := strings.Repeat(" ", max(0, remaining-modalCellWidth(ghost)))
	fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s%s%s%s│%s", row, left, modalSurface+modalBorder, modalSurface+modalInput, prefix, modalMuted, ghost, padding, modalReset+modalSurface+modalBorder, modalReset)
}

func (s *tuiState) renderActionModal(out io.Writer, cols, rows int) {
	if !s.actionMenu || cols < 8 || rows < 3 {
		return
	}
	s.resetModalMouse()
	actions := s.sessionActions(s.actionTarget)
	if len(actions) == 0 {
		return
	}
	selected := min(max(s.actionIndex, 0), len(actions)-1)
	if cols < 20 || rows < 7 {
		action := actions[selected]
		text := "› " + action.Label
		style := modalSelected
		if !action.Enabled {
			style = modalDisabled
		} else if action.Danger {
			style = modalDanger
		}
		fmt.Fprintf(out, "\033[%d;1H%s%s%s\033[K", max(1, rows/2), modalTitle, modalCellTruncate("[ Session actions ]", cols), modalReset)
		fmt.Fprintf(out, "\033[%d;1H%s%s%s\033[K", min(rows, max(1, rows/2)+1), style, modalCellTruncate(text, cols), modalReset)
		if action.Enabled {
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{1, cols, min(rows, max(1, rows/2)+1), modalMouseAction{selection: &s.actionIndex, index: selected, key: "\r"}})
		}
		return
	}
	boxWidth := min(72, cols-4)
	innerWidth := boxWidth - 2
	maxChoices := max(1, rows-7)
	start := 0
	if len(actions) > maxChoices {
		start = max(0, selected-maxChoices/2)
		start = min(start, len(actions)-maxChoices)
	}
	end := min(len(actions), start+maxChoices)
	visible := actions[start:end]
	visibleSelected := selected - start
	boxHeight := len(visible) + 6
	top := max(1, (rows-boxHeight)/2+1)
	left := max(1, (cols-boxWidth)/2+1)
	writeRow := func(row int, style, value string) {
		value = modalCellPad(modalCellTruncate(modalDisplayText(value), innerWidth), innerWidth)
		fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", row, left, modalSurface+modalBorder, modalSurface+style, value, modalReset+modalSurface+modalBorder, modalReset)
	}
	fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
	title := fmt.Sprintf("  Session actions · %s / %s", s.actionTarget.Client, s.actionTarget.Name)
	writeRow(top+1, modalTitle, title)
	for i, action := range visible {
		prefix, style := "  ", ""
		if !action.Enabled {
			style = modalDisabled
		}
		if i == visibleSelected {
			prefix = "› "
			if action.Enabled && action.Danger {
				style = modalDanger
			} else if action.Enabled {
				style = modalSelected
			}
		}
		label := action.Label
		if action.Key != "" {
			label = "[" + action.Key + "] " + label
		}
		writeRow(top+2+i, style, prefix+label)
		if action.Enabled {
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left + 1, left + innerWidth, top + 2 + i, modalMouseAction{selection: &s.actionIndex, index: start + i, key: "\r"}})
		}
	}
	status := fmt.Sprintf("  %s · %s · owner %s", s.actionTarget.Kind, s.actionTarget.Status, actionOwnerLabel(s.actionTarget))
	if !actions[selected].Enabled {
		status = "  unavailable: " + actions[selected].DisabledReason
	}
	writeRow(top+2+len(visible), modalStatus, status)
	writeRow(top+3+len(visible), modalMuted, "  ↑/↓ or j/k select   Enter choose   Esc close")
	s.modalHintRegions("  ↑/↓ or j/k select   Enter choose   Esc close", left+1, top+3+len(visible), innerWidth)
	writeRow(top+4+len(visible), modalMuted, fmt.Sprintf("  ID %s · generation %d", s.actionTarget.SessionID, s.actionTarget.RuntimeGeneration))
	fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+5+len(visible), left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
}

func (s *tuiState) createModalChoices() []string {
	switch s.newSessionStep {
	case "kind":
		return []string{"1  Agent session", "2  Shell session"}
	case "host":
		hosts := s.workspaceCreateHosts()
		choices := make([]string, 0, len(hosts))
		for i, client := range hosts {
			choices = append(choices, fmt.Sprintf("%d  %s  %s", i+1, displayField(client.Name), displayField(client.Target())))
		}
		return choices
	case "project":
		choices := make([]string, 0, len(s.newSessionProjects)+1)
		for i, project := range s.newSessionProjects {
			choices = append(choices, fmt.Sprintf("%d  %s  %s", i+1, displayField(project.Name), displayField(project.Path)))
		}
		choices = append(choices, fmt.Sprintf("%d  Browse remote path…", len(choices)+1))
		return choices
	case "path":
		choices := make([]string, 0, len(s.newSessionPathSuggestions))
		for _, path := range s.newSessionPathSuggestions {
			choices = append(choices, displayField(path))
		}
		return choices
	case "path-confirm":
		return []string{"1  Create directory recursively", "2  Back without creating"}
	case "project-policy":
		return []string{"1  Add path to bookmarks", "2  Use path once"}
	case "project-name":
		return nil
	case "agent":
		choices := make([]string, 0, len(s.newSessionAgents))
		for i, agent := range s.newSessionAgents {
			choices = append(choices, fmt.Sprintf("%d  %s", i+1, displayField(agent.Type)))
		}
		return choices
	case "handle":
		return nil
	default:
		return nil
	}
}

func (s *tuiState) createModalSelectedIndex(choiceCount int) int {
	if choiceCount == 0 {
		return -1
	}
	return min(max(s.newSessionSelected, 0), choiceCount-1)
}

func (s *tuiState) beginActionMenu() {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		s.outputErr = "no session selected"
		return
	}
	s.beginSessionActionMenu(s.sessions[s.selected])
}

func (s *tuiState) beginSessionActionMenu(target ducklord.RemoteSession) {
	if target.InstanceID == "" || target.SessionID == "" {
		s.outputErr = "session identity is unavailable while the host reconnects"
		return
	}
	s.actionMenu = true
	s.actionTarget = target
	s.actionIndex = 0
	s.actionOperation, s.actionMode = "", ""
	s.outputErr = ""
}

func (s *tuiState) closeActionMenu() {
	s.actionMenu = false
	s.actionIndex = 0
	s.actionTarget = ducklord.RemoteSession{}
	s.actionOperation, s.actionMode = "", ""
}

func (s *tuiState) handleActionMenuInput(input []byte) string {
	actions := s.sessionActions(s.actionTarget)
	if len(actions) == 0 {
		s.closeActionMenu()
		return "cancel"
	}
	s.actionIndex = min(max(s.actionIndex, 0), len(actions)-1)
	text := string(input)
	for index, action := range actions {
		if action.Key != "" && text == action.Key && action.Enabled {
			s.actionIndex = index
			return s.chooseActionMenuItem(action)
		}
	}
	switch text {
	case "\x1b", "q":
		s.closeActionMenu()
		return "cancel"
	case "j", "\x1b[B":
		for next := s.actionIndex + 1; next < len(actions); next++ {
			if actions[next].Enabled {
				s.actionIndex = next
				break
			}
		}
	case "k", "\x1b[A":
		for next := s.actionIndex - 1; next >= 0; next-- {
			if actions[next].Enabled {
				s.actionIndex = next
				break
			}
		}
	case "\r", "\n":
		action := actions[s.actionIndex]
		if !action.Enabled {
			return ""
		}
		return s.chooseActionMenuItem(action)
	}
	return ""
}

func (s *tuiState) chooseActionMenuItem(action sessionAction) string {
	switch action.ID {
	case "attach", "reconnect", "yield", "yield-wait":
		s.actionMenu = false
		s.actionOperation, s.actionMode = action.Operation, action.Mode
		return action.ID
	case "notifications":
		s.actionMenu = false
		s.actionOperation, s.actionMode = action.Operation, action.Mode
		return "notifications-action"
	case "rename-handle":
		s.actionMenu = false
		return "rename-handle-action"
	default:
		if action.Operation != "" {
			s.actionMenu = false
			s.actionOperation, s.actionMode = action.Operation, action.Mode
			return "lifecycle-action"
		}
		return ""
	}
}

func sameSessionRevision(left, right ducklord.RemoteSession) bool {
	return left.Client == right.Client && left.InstanceID == right.InstanceID && left.SessionID == right.SessionID &&
		left.Kind == right.Kind && left.WriterKind == right.WriterKind && left.WriterID == right.WriterID &&
		left.OwnershipEpoch == right.OwnershipEpoch && left.RuntimeGeneration == right.RuntimeGeneration &&
		left.Status == right.Status && left.TaskState == right.TaskState && left.AdapterState == right.AdapterState
}

func (s *tuiState) selectActionTarget() bool {
	if !s.hostIsLive(s.actionTarget.Client) {
		return false
	}
	for i, session := range s.sessions {
		if sameSessionRevision(session, s.actionTarget) {
			s.selected = i
			s.selectedKey = sessionKey(session)
			return true
		}
	}
	return false
}
