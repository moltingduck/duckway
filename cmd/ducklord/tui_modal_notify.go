package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func notificationLabel(category model.NotificationCategory) string {
	switch category {
	case model.NotificationTerminalAttention:
		return "Terminal attention (BEL / OSC)"
	case model.NotificationTaskCompleted:
		return "Task completed"
	case model.NotificationTaskFailed:
		return "Task failed"
	case model.NotificationTaskCancelled:
		return "Task cancelled"
	case model.NotificationTaskTimeout:
		return "Task timeout"
	case model.NotificationApprovalRequired:
		return "Approval required"
	case model.NotificationAgentNeedsInput:
		return "Agent needs input"
	case model.NotificationUnexpectedProcessExit:
		return "Unexpected process exit"
	default:
		return string(category)
	}
}

func (s *tuiState) renderNotificationModal(out io.Writer, cols, rows int) {
	if !s.notificationMode {
		return
	}
	s.resetModalMouse()
	target := s.notificationTarget
	categories := model.NotificationCategories()
	if s.notificationLevelEditing {
		class := notificationConfigClasses[s.notificationIndex-len(categories)]
		lines := []modalRenderLine{{modalTitle, fmt.Sprintf("  Notification delivery · %s / %s", target.Client, target.Name)},
			{modalStatus, "  " + notificationClassLabel(class)}}
		lines = append(lines, modalRenderLine{modalInput, choiceLine(s.notificationLevelChoice == 0, "inherit Host")})
		s.modalChoice(len(lines)-1, &s.notificationLevelChoice, 0, "\r")
		for i, level := range notificationConfigLevels {
			s.modalChoice(len(lines), &s.notificationLevelChoice, i+1, "\r")
			lines = append(lines, modalRenderLine{modalInput, choiceLine(s.notificationLevelChoice == i+1, string(level))})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter stage · Esc back"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	selected := min(max(s.notificationIndex, 0), len(categories)+len(notificationConfigClasses)-1)
	maxChoices := max(1, rows-8)
	start := max(0, selected-maxChoices/2)
	start = min(start, max(0, len(categories)+len(notificationConfigClasses)-maxChoices))
	lines := []modalRenderLine{{modalTitle, fmt.Sprintf("  Notifications · %s / %s", target.Client, target.Name)}}
	for index := start; index < min(len(categories)+len(notificationConfigClasses), start+maxChoices); index++ {
		s.modalChoice(len(lines), &s.notificationIndex, index, "\r")
		label := ""
		if index < len(categories) {
			category := categories[index]
			check := " "
			if s.notificationStaged[category] {
				check = "x"
			}
			label = fmt.Sprintf("[%s] %s", check, notificationLabel(category))
		} else {
			class := notificationConfigClasses[index-len(categories)]
			level := "inherit Host"
			if override := s.notificationLevelsStaged[class]; override != "" {
				level = string(override)
			}
			label = fmt.Sprintf("%s delivery: %s", notificationClassLabel(class), level)
		}
		style, prefix := "", "  "
		if index == selected {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, prefix + label})
	}
	status := fmt.Sprintf("  ID %s · changes are staged", target.SessionID)
	if s.outputErr != "" {
		status = "  Not saved: " + s.outputErr
	}
	lines = append(lines, modalRenderLine{modalStatus, status},
		modalRenderLine{modalMuted, "  a/r all · x none · Space toggle source · Enter edit delivery"},
		modalRenderLine{modalMuted, "  ↑/↓ select · s save · Esc cancel"})
	if rows < 7 {
		lines = lines[:min(len(lines), 2)]
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderLifecycleModal(out io.Writer, cols, rows int) {
	if s.lifecycleConfirm == "" {
		return
	}
	s.resetModalMouse()
	target := s.lifecycleTarget
	verb := strings.ToUpper(string(s.lifecycleConfirm))
	style := modalTitle
	if s.lifecycleConfirm == protocol.SessionLifecycleDestroy || s.lifecycleConfirm == protocol.SessionLifecycleEnd {
		style = modalDanger
	}
	detail := "Restart keeps the session identity and starts a new runtime generation."
	switch s.lifecycleConfirm {
	case protocol.SessionLifecycleDestroy:
		detail = "Destroy permanently removes this PTY and its retained logs."
	case protocol.SessionLifecycleEnd:
		detail = "End keeps the session metadata and retained logs for recovery."
	}
	help := "Enter wait for idle · f force-cancel task · Esc cancel"
	if s.lifecycleConfirm != protocol.SessionLifecycleRestart {
		help = "Enter now · w wait for idle · f force-cancel task · Esc cancel"
	}
	if s.lifecycleMode != "" {
		help = "Enter confirm selected mode · Esc cancel"
	}
	if target.Kind == string(model.KindShell) {
		switch s.lifecycleConfirm {
		case protocol.SessionLifecycleRestart:
			detail = "Shell restarts immediately; this Session pane stays."
			help = "Enter restart process immediately · Esc cancel"
		case protocol.SessionLifecycleEnd:
			detail = "Shell and Session pane disappear; retained PTY logs remain."
			help = "Enter end process immediately · Esc cancel"
		case protocol.SessionLifecycleDestroy:
			detail = "Shell and Session pane disappear; retained PTY logs are removed."
			help = "Enter destroy process immediately · Esc cancel"
		}
	}
	if rows < 7 {
		warning := verb + ": " + detail + "  " + help
		s.renderModalBox(out, cols, rows, []modalRenderLine{{style, warning}})
		return
	}
	lines := []modalRenderLine{
		{style, "  " + verb + " SESSION"},
		{modalInput, fmt.Sprintf("  %s [%s]", target.Name, target.SessionID)},
		{modalMuted, fmt.Sprintf("  Host %s · generation %d", target.Client, target.RuntimeGeneration)},
		{modalStatus, "  " + detail},
		{modalMuted, "  " + help},
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) beginNotificationSettings() {
	session := s.currentSession()
	if session.InstanceID == "" || session.SessionID == "" {
		s.outputErr = "notification settings require a synchronized Ducklion session"
		return
	}
	s.notificationMode = true
	s.notificationIndex = 0
	s.notificationLevelEditing = false
	s.notificationTarget = session
	s.outputErr = ""
	s.notificationStaged = make(map[model.NotificationCategory]bool)
	for _, category := range model.NotificationCategories() {
		s.notificationStaged[category] = s.activity().Enabled(session.InstanceID, session.SessionID, category)
	}
	s.notificationLevelsStaged = make(map[ducklord.NotificationClass]ducklord.NotificationLevel)
	if identity, ok := ducklord.IdentityFromSession(session); ok {
		for class, level := range s.activity().Sessions[identity.Key()].NotificationLevels {
			s.notificationLevelsStaged[class] = level
		}
	}
}

func (s *tuiState) closeNotificationSettings() {
	s.notificationMode = false
	s.notificationStaged = nil
	s.notificationLevelsStaged = nil
	s.notificationLevelEditing = false
	s.notificationTarget = ducklord.RemoteSession{}
}

func (s *tuiState) handleNotificationInput(input []byte) string {
	categories := model.NotificationCategories()
	if s.notificationLevelEditing {
		switch string(input) {
		case "\x1b", "\x03":
			s.notificationLevelEditing = false
		case "j", "\x1b[B":
			s.notificationLevelChoice = min(len(notificationConfigLevels), s.notificationLevelChoice+1)
		case "k", "\x1b[A":
			s.notificationLevelChoice = max(0, s.notificationLevelChoice-1)
		case "\r", "\n":
			class := notificationConfigClasses[s.notificationIndex-len(categories)]
			if s.notificationLevelChoice == 0 {
				delete(s.notificationLevelsStaged, class)
			} else {
				s.notificationLevelsStaged[class] = notificationConfigLevels[s.notificationLevelChoice-1]
			}
			s.notificationLevelEditing = false
		}
		return ""
	}
	switch string(input) {
	case "\x1b", "q":
		return "cancel"
	case "\r", "\n":
		if s.notificationIndex < len(categories) {
			category := categories[s.notificationIndex]
			s.notificationStaged[category] = !s.notificationStaged[category]
			return ""
		}
		class := notificationConfigClasses[s.notificationIndex-len(categories)]
		s.notificationLevelChoice = 0
		for i, level := range notificationConfigLevels {
			if s.notificationLevelsStaged[class] == level {
				s.notificationLevelChoice = i + 1
			}
		}
		s.notificationLevelEditing = true
	case "s":
		return "save"
	case "j", "\x1b[B":
		if s.notificationIndex < len(categories)+len(notificationConfigClasses)-1 {
			s.notificationIndex++
		}
	case "k", "\x1b[A":
		if s.notificationIndex > 0 {
			s.notificationIndex--
		}
	case " ":
		if s.notificationIndex < len(categories) {
			category := categories[s.notificationIndex]
			s.notificationStaged[category] = !s.notificationStaged[category]
		}
	case "a", "r":
		for _, category := range categories {
			s.notificationStaged[category] = true
		}
	case "x":
		for _, category := range categories {
			s.notificationStaged[category] = false
		}
	}
	return ""
}

func (s *tuiState) saveNotificationSettings() {
	session := s.notificationTarget
	found := false
	for _, current := range s.sessions {
		if current.Client == session.Client && current.InstanceID == session.InstanceID && current.SessionID == session.SessionID {
			found = true
			break
		}
	}
	if !found {
		s.outputErr = "notification target changed; reopen settings"
		return
	}
	next := s.activity().Clone()
	for _, category := range model.NotificationCategories() {
		if err := next.SetEnabled(session.InstanceID, session.SessionID, category, s.notificationStaged[category]); err != nil {
			s.outputErr = err.Error()
			return
		}
	}
	if err := ducklord.ValidateNotificationLevels(s.notificationLevelsStaged); err != nil {
		s.outputErr = err.Error()
		return
	}
	identity, ok := ducklord.IdentityFromSession(session)
	if !ok {
		s.outputErr = "notification target identity is unavailable"
		return
	}
	entry := next.Sessions[identity.Key()]
	entry.NotificationLevels = make(map[ducklord.NotificationClass]ducklord.NotificationLevel, len(s.notificationLevelsStaged))
	for class, level := range s.notificationLevelsStaged {
		entry.NotificationLevels[class] = level
	}
	next.Sessions[identity.Key()] = entry
	if err := s.activityStore.Save(next); err != nil {
		s.outputErr = "notification state: " + err.Error()
		return
	}
	s.activityState = next
	s.closeNotificationSettings()
	s.outputErr = "notification settings saved"
}

func wrapDisplayText(text string, width int) []string {
	if width < 1 {
		return nil
	}
	runes := []rune(text)
	lines := make([]string, 0, (len(runes)+width-1)/width)
	for len(runes) > 0 {
		n := width
		if n > len(runes) {
			n = len(runes)
		}
		lines = append(lines, string(runes[:n]))
		runes = runes[n:]
	}
	return lines
}
