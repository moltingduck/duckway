package main

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// Feed authoritative snapshots through the same ingestion path as the live
// subscription. Each snapshot owns its event maps so changing the next event
// cannot accidentally change the previous inventory's delta baseline.
func applyNotificationReorderInventory(t *testing.T, state *tuiState, revision uint64, sessions []ducklord.RemoteSession) {
	t.Helper()
	if !state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", State: "live", InstanceID: sessions[0].InstanceID,
		Generation: 1, Revision: revision, Sessions: sessions}) {
		t.Fatal("authoritative notification inventory rejected")
	}
}

func TestNotificationFocusOffDoesNotPromoteOldUnreadInventory(t *testing.T) {
	state, project, inside, outside := workspacePaneTestState(t)
	state.cfg.QuickSort = "host" // Event-time base sorting must not masquerade as promotion.
	state.cfg.NotificationLevels = map[ducklord.NotificationClass]ducklord.NotificationLevel{
		ducklord.NotificationCompleted: ducklord.NotificationSound,
	}
	sink := &recordingNotificationSink{}
	state.notificationSink = sink
	state.hostSync = make(map[string]ducklord.SessionUpdate)
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(project); err != nil {
		t.Fatal(err)
	}
	if nav.ToggleNotificationFocus() != project {
		t.Fatal("containing Project was not focused")
	}
	applyNotificationReorderInventory(t, state, 1, []ducklord.RemoteSession{inside, outside})
	outside.ActivitySequences = map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}
	outside.ActivityUpdatedAtMS = map[model.NotificationCategory]int64{model.NotificationTaskCompleted: 100}
	applyNotificationReorderInventory(t, state, 2, []ducklord.RemoteSession{inside, outside})
	assertUnreadWithoutPromotion := func() {
		t.Helper()
		identity, _ := ducklord.IdentityFromSession(outside)
		entry := state.activity().Sessions[identity.Key()]
		if !entry.Unread[model.NotificationTaskCompleted] || entry.Observed[model.NotificationTaskCompleted] != 1 {
			t.Fatalf("suppressed event lost unread/observed state: %+v", entry)
		}
		if len(state.sessions) != 2 || state.sessions[1].SessionID != outside.SessionID || !state.sessions[1].Unread || state.promoted[sessionKey(outside)] {
			t.Fatalf("old suppressed unread event promoted: rows=%+v promoted=%v", state.sessions, state.promoted)
		}
		if len(sink.got) != 0 {
			t.Fatalf("old suppressed event delivered: %+v", sink.got)
		}
	}
	assertUnreadWithoutPromotion()
	if nav.ToggleNotificationFocus() != "" {
		t.Fatal("notification focus did not clear")
	}
	assertUnreadWithoutPromotion()
	// Both an unchanged replay and a newer inventory with unchanged counters
	// must consume no new event, despite the now-permissive focus policy.
	for _, revision := range []uint64{2, 3} {
		applyNotificationReorderInventory(t, state, revision, []ducklord.RemoteSession{inside, outside})
		assertUnreadWithoutPromotion()
	}
	outside.ActivitySequences = map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 2}
	outside.ActivityUpdatedAtMS = map[model.NotificationCategory]int64{model.NotificationTaskCompleted: 200}
	applyNotificationReorderInventory(t, state, 4, []ducklord.RemoteSession{inside, outside})
	if state.sessions[0].SessionID != outside.SessionID || !state.sessions[0].Unread || !state.promoted[sessionKey(outside)] || len(sink.got) != 1 {
		t.Fatalf("new qualifying delta did not promote and deliver once: rows=%+v promoted=%v delivery=%+v", state.sessions, state.promoted, sink.got)
	}
}

func TestNotificationLiveReorderKeepsSelectedIdentityVisible(t *testing.T) {
	state, project, terminalSession, other := workspacePaneTestState(t)
	state.cfg.QuickSort = "host"
	state.cfg.NotificationLevels = map[ducklord.NotificationClass]ducklord.NotificationLevel{
		ducklord.NotificationCompleted: ducklord.NotificationIndicator,
	}
	state.notificationSink = &recordingNotificationSink{}
	state.hostSync = make(map[string]ducklord.SessionUpdate)
	sessions := []ducklord.RemoteSession{terminalSession, other}
	for i := 0; i < 30; i++ {
		extra := terminalSession
		extra.SessionID, extra.Name = fmt.Sprintf("X%05d", i), fmt.Sprintf("row%02d", i)
		sessions = append(sessions, extra)
	}
	applyNotificationReorderInventory(t, state, 1, sessions)
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(project); err != nil {
		t.Fatal(err)
	}
	state.activeAttachKey = sessionKey(terminalSession)
	const width, height = 140, 18
	geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
	visible := geometry.Quick.Height - 1
	if visible < 2 || visible >= len(sessions)-1 {
		t.Fatalf("fixture must overflow the quick viewport: visible=%d sessions=%d", visible, len(sessions))
	}
	// Project navigation can leave the independent quick selection elsewhere.
	// Put that selected row at the viewport bottom, so one promotion above it
	// forces scrolling, without changing the displayed Project/terminal pane.
	state.selected = visible - 1
	state.selectedKey = state.currentKey()
	selectedKey, selectedName := state.currentKey(), state.currentSession().Name
	var before bytes.Buffer
	state.renderWorkspacePreviewAt(&before, width, height)
	if state.workspaceQuickOffset != 0 || !strings.Contains(before.String(), "› "+selectedName) {
		t.Fatal("fixture selected row was not visible at the initial viewport boundary")
	}
	projectID, tabID, paneID, region := nav.CurrentProjectID(), nav.CurrentTabID(), nav.CurrentPaneID(), nav.Region()
	projectOrder := func() []string {
		var ids []string
		for _, p := range state.activity().ProjectLayout.Projects {
			ids = append(ids, p.ID)
		}
		return ids
	}
	order := projectOrder()
	last := len(sessions) - 1
	sessions[last].ActivitySequences = map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}
	sessions[last].ActivityUpdatedAtMS = map[model.NotificationCategory]int64{model.NotificationTaskCompleted: 100}
	applyNotificationReorderInventory(t, state, 2, sessions)
	var after bytes.Buffer
	state.renderWorkspacePreviewAt(&after, width, height)
	if state.sessions[0].SessionID != sessions[last].SessionID || !state.promoted[sessionKey(sessions[last])] {
		t.Fatal("genuine notification delta did not reorder the quick list")
	}
	if state.currentKey() != selectedKey || state.selected != visible || state.workspaceQuickOffset != 1 || !strings.Contains(after.String(), "› "+selectedName) {
		t.Fatalf("live reorder lost visible selected identity: key=%q index=%d offset=%d rendered=%q", state.currentKey(), state.selected, state.workspaceQuickOffset, after.String())
	}
	if !reflect.DeepEqual(order, projectOrder()) || nav.CurrentProjectID() != projectID || nav.CurrentTabID() != tabID || nav.CurrentPaneID() != paneID || nav.Region() != region {
		t.Fatal("notification reorder changed Project order or terminal navigation")
	}
	displayed, err := state.workspaceSelectedPaneSession()
	if err != nil || sessionKey(displayed) != sessionKey(terminalSession) || state.activeAttachKey != sessionKey(terminalSession) {
		t.Fatalf("notification reorder changed displayed/attached terminal identity: %+v err=%v", displayed, err)
	}
}
