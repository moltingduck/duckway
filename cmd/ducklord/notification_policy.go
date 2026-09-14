package main

import (
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) effectiveNotificationLevel(session ducklord.RemoteSession, category model.NotificationCategory) ducklord.NotificationLevel {
	class, ok := ducklord.NotificationClassFor(category)
	if !ok || !s.activity().Enabled(session.InstanceID, session.SessionID, category) {
		return ducklord.NotificationOff
	}
	var global, host, local map[ducklord.NotificationClass]ducklord.NotificationLevel
	if s.cfg != nil {
		global = s.cfg.NotificationLevels
		if client, exists := s.cfg.Client(session.Client); exists {
			host = client.NotificationLevels
		}
	}
	if identity, valid := ducklord.IdentityFromSession(session); valid {
		local = s.activity().Sessions[identity.Key()].NotificationLevels
	}
	return ducklord.ResolveNotificationLevel(class, global, host, local)
}

func (s *tuiState) reconcileNotificationState(session ducklord.RemoteSession, activeFresh bool) (bool, bool) {
	return s.activity().ReconcileWithEnabled(session, activeFresh, func(category model.NotificationCategory) bool {
		return s.effectiveNotificationLevel(session, category) != ducklord.NotificationOff
	})
}

func (s *tuiState) shouldDeliverNotification(session ducklord.RemoteSession, category model.NotificationCategory) bool {
	level := s.effectiveNotificationLevel(session, category)
	nav := s.workspaceNav
	if nav == nil || nav.NotificationFocusProjectID() == "" {
		return level != ducklord.NotificationOff
	}
	identity, valid := ducklord.IdentityFromSession(session)
	if !valid {
		return false
	}
	focused := nav.NotificationFocusProjectID()
	inFocusedProject := false
	for _, projectID := range s.activity().ProjectLayout.ProjectsFor(identity) {
		if projectID == focused {
			inFocusedProject = true
			break
		}
	}
	return ducklord.ShouldDeliver(level, true, inFocusedProject, s.cfg.FocusThreshold())
}
