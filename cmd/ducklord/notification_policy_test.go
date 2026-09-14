package main

import (
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestTUIEffectiveNotificationPolicyRespectsGlobalHostSession(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	session := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "ABC123",
		ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}}
	state := tuiState{cfg: &ducklord.Config{
		NotificationLevels: map[ducklord.NotificationClass]ducklord.NotificationLevel{ducklord.NotificationCompleted: ducklord.NotificationSound},
		Clients: []ducklord.Client{{Name: "host", Host: "host", NotificationLevels: map[ducklord.NotificationClass]ducklord.NotificationLevel{
			ducklord.NotificationCompleted: ducklord.NotificationOff}}},
	}}
	if got := state.effectiveNotificationLevel(session, model.NotificationTaskCompleted); got != ducklord.NotificationOff {
		t.Fatalf("Host off did not override global sound: %s", got)
	}
	if unread, _ := state.reconcileNotificationState(session, false); unread {
		t.Fatal("Host off produced unread")
	}
	identity, _ := ducklord.IdentityFromSession(session)
	entry := state.activity().Sessions[identity.Key()]
	entry.NotificationLevels = map[ducklord.NotificationClass]ducklord.NotificationLevel{ducklord.NotificationCompleted: ducklord.NotificationSystem}
	state.activity().Sessions[identity.Key()] = entry
	session.ActivitySequences[model.NotificationTaskCompleted] = 2
	if got := state.effectiveNotificationLevel(session, model.NotificationTaskCompleted); got != ducklord.NotificationSystem {
		t.Fatalf("Session override failed: %s", got)
	}
	if unread, _ := state.reconcileNotificationState(session, false); !unread {
		t.Fatal("Session override did not create unread")
	}
}

func TestProjectFocusSuppressesDeliveryButKeepsUnread(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	first := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "ABC123",
		ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}}
	second := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "DEF456"}
	activity := ducklord.NewActivityState()
	firstProject, _ := activity.ProjectLayout.AddProject("First")
	secondProject, _ := activity.ProjectLayout.AddProject("Second")
	firstIdentity, _ := ducklord.IdentityFromSession(first)
	secondIdentity, _ := ducklord.IdentityFromSession(second)
	_, _ = activity.ProjectLayout.Place(firstProject, firstIdentity, ducklord.PlaceNewTab, "")
	_, _ = activity.ProjectLayout.Place(secondProject, secondIdentity, ducklord.PlaceNewTab, "")
	state := tuiState{cfg: &ducklord.Config{NotificationLevels: map[ducklord.NotificationClass]ducklord.NotificationLevel{
		ducklord.NotificationCompleted: ducklord.NotificationIndicator}}, activityState: activity, workspacePreview: true}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(secondProject); err != nil {
		t.Fatal(err)
	}
	if got := nav.ToggleNotificationFocus(); got != secondProject {
		t.Fatalf("focus=%q", got)
	}
	if state.shouldDeliverNotification(first, model.NotificationTaskCompleted) {
		t.Fatal("other-Project indicator escaped system threshold")
	}
	if unread, _ := state.reconcileNotificationState(first, false); !unread {
		t.Fatal("Project focus suppression lost unread mark")
	}
	if got := nav.ToggleNotificationFocus(); got != "" {
		t.Fatalf("focus did not turn off: %q", got)
	}
	if !state.shouldDeliverNotification(first, model.NotificationTaskCompleted) {
		t.Fatal("normal delivery stayed suppressed after turning focus off")
	}
}

func TestProjectFocusAllowsSessionSharedWithFocusedProject(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	session := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "ABC123"}
	activity := ducklord.NewActivityState()
	first, _ := activity.ProjectLayout.AddProject("First")
	second, _ := activity.ProjectLayout.AddProject("Second")
	identity, _ := ducklord.IdentityFromSession(session)
	_, _ = activity.ProjectLayout.Place(first, identity, ducklord.PlaceNewTab, "")
	_, _ = activity.ProjectLayout.Place(second, identity, ducklord.PlaceNewTab, "")
	state := tuiState{cfg: &ducklord.Config{NotificationLevels: map[ducklord.NotificationClass]ducklord.NotificationLevel{
		ducklord.NotificationCompleted: ducklord.NotificationSound}}, activityState: activity, workspacePreview: true}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(second); err != nil {
		t.Fatal(err)
	}
	nav.ToggleNotificationFocus()
	if !state.shouldDeliverNotification(session, model.NotificationTaskCompleted) {
		t.Fatal("shared Session was suppressed despite membership in the focused Project")
	}
}
