package main

import (
	"path/filepath"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestQuickSortEventTimeAndDirectionPreserveSelection(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	sessions := []ducklord.RemoteSession{
		{Client: "host-b", InstanceID: instance, SessionID: "AAA111", Name: "old", Kind: string(model.KindShell)},
		{Client: "host-a", InstanceID: instance, SessionID: "BBB222", Name: "new", Kind: string(model.KindShell)},
		{Client: "host-c", InstanceID: instance, SessionID: "CCC333", Name: "none", Kind: string(model.KindShell)},
	}
	activity := ducklord.NewActivityState()
	activity.Sessions[ducklord.SessionIdentity{InstanceID: instance, SessionID: "AAA111"}.Key()] = ducklord.SessionNotificationState{LastEventAtMS: 10}
	activity.Sessions[ducklord.SessionIdentity{InstanceID: instance, SessionID: "BBB222"}.Key()] = ducklord.SessionNotificationState{LastEventAtMS: 20}
	state := tuiState{cfg: &ducklord.Config{}, cfgPath: filepath.Join(t.TempDir(), "config.yaml"), sessions: sessions, selected: 0, activityState: activity, workspacePreview: true}
	state.sortQuickSessions()
	if state.sessions[0].Name != "new" || state.sessions[1].Name != "old" || state.sessions[2].Name != "none" {
		t.Fatalf("newest sort: %+v", state.sessions)
	}
	state.selected = 1
	selected := state.currentKey()
	state.reverseQuickSortTime()
	if state.sessions[0].Name != "old" || state.sessions[1].Name != "new" || state.sessions[2].Name != "none" || state.currentKey() != selected {
		t.Fatalf("oldest sort lost order or selection: %+v selected=%q", state.sessions, state.currentKey())
	}
	loaded, err := ducklord.LoadConfig(state.cfgPath)
	if err != nil || !loaded.QuickOldestFirst {
		t.Fatalf("direction was not persisted: cfg=%+v err=%v", loaded, err)
	}
}

func TestQuickSortImportanceAndHostIgnoreLegacyGroups(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	a := ducklord.RemoteSession{Client: "z", InstanceID: instance, SessionID: "AAA111", Name: "complete", Group: "first"}
	b := ducklord.RemoteSession{Client: "a", InstanceID: instance, SessionID: "BBB222", Name: "needs-input", Group: "second"}
	activity := ducklord.NewActivityState()
	activity.Sessions[ducklord.SessionIdentity{InstanceID: instance, SessionID: a.SessionID}.Key()] = ducklord.SessionNotificationState{LastEventAtMS: 20, LastEventCategory: model.NotificationTaskCompleted}
	activity.Sessions[ducklord.SessionIdentity{InstanceID: instance, SessionID: b.SessionID}.Key()] = ducklord.SessionNotificationState{LastEventAtMS: 10, LastEventCategory: model.NotificationAgentNeedsInput}
	state := tuiState{cfg: &ducklord.Config{QuickSort: "event_importance"}, activityState: activity, workspacePreview: true, sessions: []ducklord.RemoteSession{a, b}}
	state.sortQuickSessions()
	if state.sessions[0].SessionID != b.SessionID {
		t.Fatalf("importance ordering ignored action-needed event: %+v", state.sessions)
	}
	state.cfg.QuickSort = "host"
	state.sortQuickSessions()
	if state.sessions[0].Client != "a" {
		t.Fatalf("host ordering still grouped by legacy organization: %+v", state.sessions)
	}
}

func TestQuickSortPromotesOnlyEligibleUnreadEvents(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	suppressed := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "AAA111", Name: "suppressed", Unread: true}
	eligible := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "BBB222", Name: "eligible", Unread: true}
	state := tuiState{cfg: &ducklord.Config{QuickSort: "event_time"}, workspacePreview: true,
		sessions: []ducklord.RemoteSession{suppressed, eligible}, promoted: map[string]bool{sessionKey(eligible): true}}
	firstIdentity, _ := ducklord.IdentityFromSession(suppressed)
	secondIdentity, _ := ducklord.IdentityFromSession(eligible)
	state.activity().Organization.SessionOrder = []ducklord.SessionIdentity{firstIdentity, secondIdentity}
	state.sortQuickSessions()
	if state.sessions[0].SessionID != eligible.SessionID {
		t.Fatalf("suppressed unread event was promoted: %+v", state.sessions)
	}
	state.promoted[sessionKey(eligible)] = false
	state.sortQuickSessions()
	if state.sessions[0].SessionID != suppressed.SessionID {
		t.Fatal("stable base order was not restored when promotion expired")
	}
}
