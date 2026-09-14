package main

import (
	"sort"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

var quickSortModes = []string{"event_time", "event_importance", "host", "type"}

func (s *tuiState) quickSortMode() string {
	if s.cfg == nil || s.cfg.QuickSort == "" {
		return "event_time"
	}
	return s.cfg.QuickSort
}

func quickTypeRank(session ducklord.RemoteSession) int {
	if session.Kind == string(model.KindShell) {
		switch session.DetectedForeground {
		case "codex":
			return 0
		case "claude":
			return 1
		case "other_agent":
			return 2
		default:
			return 3 // uncertainty never claims an agent
		}
	}
	switch strings.ToLower(session.AgentType) {
	case "codex":
		return 0
	case "claude":
		return 1
	case "", "shell":
		return 3
	default:
		return 2
	}
}

func quickEventImportance(category model.NotificationCategory) int {
	switch category {
	case model.NotificationAgentNeedsInput, model.NotificationApprovalRequired:
		return 0
	case model.NotificationTaskFailed, model.NotificationTaskTimeout, model.NotificationUnexpectedProcessExit:
		return 1
	case model.NotificationTaskCompleted:
		return 2
	case model.NotificationTerminalAttention:
		return 3
	default:
		return 4
	}
}

func (s *tuiState) sortQuickSessions() {
	activity := s.activity().Sessions
	baseRank := make(map[string]int, len(s.activity().Organization.SessionOrder))
	for index, identity := range s.activity().Organization.SessionOrder {
		baseRank[identity.Key()] = index
	}
	mode := s.quickSortMode()
	oldest := s.cfg != nil && s.cfg.QuickOldestFirst
	sort.SliceStable(s.sessions, func(i, j int) bool {
		left, right := s.sessions[i], s.sessions[j]
		leftIdentity, leftOK := ducklord.IdentityFromSession(left)
		rightIdentity, rightOK := ducklord.IdentityFromSession(right)
		leftEvent, rightEvent := ducklord.SessionNotificationState{}, ducklord.SessionNotificationState{}
		if leftOK {
			leftEvent = activity[leftIdentity.Key()]
		}
		if rightOK {
			rightEvent = activity[rightIdentity.Key()]
		}
		leftPromoted := left.Unread && s.promoted[sessionKey(left)]
		rightPromoted := right.Unread && s.promoted[sessionKey(right)]
		if s.cfg.PromoteUnread() && leftPromoted != rightPromoted {
			return leftPromoted
		}
		switch mode {
		case "host":
			if left.Client != right.Client {
				return left.Client < right.Client
			}
		case "type":
			if a, b := quickTypeRank(left), quickTypeRank(right); a != b {
				return a < b
			}
		case "event_importance":
			if a, b := quickEventImportance(leftEvent.LastEventCategory), quickEventImportance(rightEvent.LastEventCategory); a != b {
				return a < b
			}
			fallthrough
		default:
			if (leftEvent.LastEventAtMS > 0) != (rightEvent.LastEventAtMS > 0) {
				return leftEvent.LastEventAtMS > 0
			}
			if leftEvent.LastEventAtMS != rightEvent.LastEventAtMS {
				if mode == "event_time" && oldest {
					return leftEvent.LastEventAtMS < rightEvent.LastEventAtMS
				}
				return leftEvent.LastEventAtMS > rightEvent.LastEventAtMS
			}
			if leftOK && rightOK && baseRank[leftIdentity.Key()] != baseRank[rightIdentity.Key()] {
				return baseRank[leftIdentity.Key()] < baseRank[rightIdentity.Key()]
			}
			return false
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return sessionKey(left) < sessionKey(right)
	})
}

func (s *tuiState) cycleQuickSort() {
	if !s.workspacePreview || s.cfg == nil {
		return
	}
	current := s.quickSortMode()
	next := quickSortModes[0]
	for i, mode := range quickSortModes {
		if mode == current {
			next = quickSortModes[(i+1)%len(quickSortModes)]
			break
		}
	}
	saved := s.cfg.Clone()
	saved.QuickSort = next
	if err := ducklord.SaveConfig(s.cfgPath, saved); err != nil {
		s.outputErr = err.Error()
		return
	}
	key := s.currentKey()
	s.cfg = saved
	s.sortQuickSessions()
	s.restoreSelection(key)
}

func (s *tuiState) reverseQuickSortTime() {
	if !s.workspacePreview || s.cfg == nil || s.quickSortMode() != "event_time" {
		return
	}
	saved := s.cfg.Clone()
	saved.QuickOldestFirst = !saved.QuickOldestFirst
	if err := ducklord.SaveConfig(s.cfgPath, saved); err != nil {
		s.outputErr = err.Error()
		return
	}
	key := s.currentKey()
	s.cfg = saved
	s.sortQuickSessions()
	s.restoreSelection(key)
}
