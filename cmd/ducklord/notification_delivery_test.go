package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

type recordingNotificationSink struct{ got []localNotification }

func (r *recordingNotificationSink) Deliver(n localNotification) bool {
	r.got = append(r.got, n)
	return true
}
func (r *recordingNotificationSink) Close() {}

func TestNotificationDeliveryProjectLabelPrecedence(t *testing.T) {
	state, first, session, other := workspacePaneTestState(t)
	layout := &state.activity().ProjectLayout
	second, err := layout.AddProject("Shared")
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := ducklord.IdentityFromSession(session)
	if _, err := layout.Place(second, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	selectProject := func(id string) {
		t.Helper()
		if err := nav.SelectProject(id); err != nil {
			t.Fatal(err)
		}
	}
	sink := &recordingNotificationSink{}
	state.notificationSink = sink
	state.cfg.NotificationLevels = map[ducklord.NotificationClass]ducklord.NotificationLevel{
		ducklord.NotificationCompleted: ducklord.NotificationSystem,
	}
	check := func(want string) {
		t.Helper()
		project, tab, pane, region, focus := nav.CurrentProjectID(), nav.CurrentTabID(), nav.CurrentPaneID(), nav.Region(), nav.NotificationFocusProjectID()
		sink.got = nil
		state.deliverNotification(session, model.NotificationTaskCompleted)
		if len(sink.got) != 1 || sink.got[0].Project != want {
			t.Fatalf("notification Project want %q, got %+v", want, sink.got)
		}
		if nav.CurrentProjectID() != project || nav.CurrentTabID() != tab || nav.CurrentPaneID() != pane || nav.Region() != region || nav.NotificationFocusProjectID() != focus {
			t.Fatal("notification delivery moved the workspace or notification focus")
		}
	}
	selectProject(second)
	nav.ToggleNotificationFocus()
	selectProject(first)
	check("Shared") // Notification focus wins over the current containing Project.
	otherIdentity, _ := ducklord.IdentityFromSession(other)
	otherPane, err := layout.Place(first, otherIdentity, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectPane(first, otherPane); err != nil {
		t.Fatal(err)
	}
	selectProject(second)
	nav.ToggleNotificationFocus()
	if err := nav.SelectQuickSession(identity); err != nil {
		t.Fatal(err)
	}
	selectProject(first)
	// Work restores the other Session's pane, so this Session's last visit
	// remains Shared while the current containing Project is Work.
	check("Work") // Current Project wins over the last-viewed Project.
	selectProject(ducklord.DefaultProjectID)
	nav.ToggleNotificationFocus()
	check("Shared") // Unrelated focus/current Projects fall back to last-viewed.
	state.workspaceNav = nil
	check("Work") // Before navigation exists, use persisted layout order.
}

func TestFocusedSessionDeliversSoundWithoutUnread(t *testing.T) {
	state, _, session, other := workspacePaneTestState(t)
	sink := &recordingNotificationSink{}
	state.notificationSink = sink
	state.cfg.NotificationLevels = map[ducklord.NotificationClass]ducklord.NotificationLevel{ducklord.NotificationCompleted: ducklord.NotificationSound}
	state.cfg.NotificationSounds = map[ducklord.NotificationClass]string{ducklord.NotificationCompleted: "/tmp/done.wav"}
	state.focused, state.activeAttachKey, state.outputForKey = true, sessionKey(session), sessionKey(session)
	state.outputFresh, state.terminalGeneration, state.terminal = true, 1, &ducklord.Terminal{}
	state.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", InstanceID: session.InstanceID, Generation: 1, Revision: 1}}
	session.ActivitySequences = map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}
	if !state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", State: "live", InstanceID: session.InstanceID,
		Generation: 1, Revision: 2, Sessions: []ducklord.RemoteSession{session, other}}) {
		t.Fatal("live update rejected")
	}
	if len(sink.got) != 1 || sink.got[0].Class != ducklord.NotificationCompleted || sink.got[0].Sound != "/tmp/done.wav" {
		t.Fatalf("focused notification not delivered: %+v", sink.got)
	}
	for _, current := range state.sessions {
		if current.SessionID == session.SessionID && current.Unread {
			t.Fatal("focused Session was marked unread")
		}
	}
}

func TestNotificationDeliveryDeduplicatesHostAliases(t *testing.T) {
	state, _, session, _ := workspacePaneTestState(t)
	alias := session
	alias.Client = "alias"
	state.cfg.Clients = append(state.cfg.Clients, ducklord.Client{Name: "alias", Host: "alias"})
	state.cfg.NotificationLevels = map[ducklord.NotificationClass]ducklord.NotificationLevel{ducklord.NotificationCompleted: ducklord.NotificationSound}
	state.sessions = []ducklord.RemoteSession{session, alias}
	state.hostSync = map[string]ducklord.SessionUpdate{
		"host":  {Client: "host", State: "live", InstanceID: session.InstanceID, Generation: 1, Revision: 1},
		"alias": {Client: "alias", State: "live", InstanceID: session.InstanceID, Generation: 1, Revision: 1},
	}
	sink := &recordingNotificationSink{}
	state.notificationSink = sink
	session.ActivitySequences = map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}
	alias.ActivitySequences = session.ActivitySequences
	for _, update := range []ducklord.SessionUpdate{
		{Client: "host", State: "live", InstanceID: session.InstanceID, Generation: 1, Revision: 2, Sessions: []ducklord.RemoteSession{session}},
		{Client: "alias", State: "live", InstanceID: session.InstanceID, Generation: 1, Revision: 2, Sessions: []ducklord.RemoteSession{alias}},
	} {
		if !state.applySessionUpdate(update) {
			t.Fatal("alias update rejected")
		}
	}
	if len(sink.got) != 1 {
		t.Fatalf("same Ducklion event delivered %d times", len(sink.got))
	}
}

func TestColdStartInventoryKeepsUnreadWithoutReplayingDesktopEvent(t *testing.T) {
	state, _, session, _ := workspacePaneTestState(t)
	state.sessions = nil
	state.cfg.NotificationLevels = map[ducklord.NotificationClass]ducklord.NotificationLevel{ducklord.NotificationCompleted: ducklord.NotificationSystem}
	state.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", InstanceID: session.InstanceID, Generation: 1}}
	sink := &recordingNotificationSink{}
	state.notificationSink = sink
	session.ActivitySequences = map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}
	if !state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", State: "live", InstanceID: session.InstanceID,
		Generation: 1, Revision: 1, Sessions: []ducklord.RemoteSession{session}}) {
		t.Fatal("initial inventory rejected")
	}
	if len(sink.got) != 0 || len(state.sessions) != 1 || !state.sessions[0].Unread {
		t.Fatalf("cold-start notification was replayed or unread was lost: delivered=%d unread=%v", len(sink.got), state.sessions)
	}
}

func TestAttentionDeliveryCoalescesOnlyPerSession(t *testing.T) {
	now := time.Now()
	sink := &localNotificationSink{queue: make(chan localNotification, 8), now: func() time.Time { return now },
		lastAttention: make(map[string]time.Time)}
	n := localNotification{Class: ducklord.NotificationAttention, Level: ducklord.NotificationSound, Key: "a"}
	sink.Deliver(n)
	sink.Deliver(n)
	n.Key = "b"
	sink.Deliver(n)
	n.Class = ducklord.NotificationCompleted
	n.Key = "a"
	sink.Deliver(n)
	sink.Deliver(n)
	if got := len(sink.queue); got != 4 {
		t.Fatalf("coalescing lost agent events or duplicated attention: %d", got)
	}
	now = now.Add(2 * time.Second)
	n.Class = ducklord.NotificationAttention
	sink.Deliver(n)
	if got := len(sink.queue); got != 5 {
		t.Fatalf("attention did not resume after window: %d", got)
	}
}

func TestAttentionBellIsCoalescedWithLocalDelivery(t *testing.T) {
	state, _, session, _ := workspacePaneTestState(t)
	now := time.Now()
	state.notificationNow = func() time.Time { return now }
	state.cfg.NotificationLevels = map[ducklord.NotificationClass]ducklord.NotificationLevel{
		ducklord.NotificationAttention: ducklord.NotificationSound,
	}
	sink := &recordingNotificationSink{}
	state.notificationSink = sink
	state.deliverNotification(session, model.NotificationTerminalAttention)
	state.deliverNotification(session, model.NotificationTerminalAttention)
	if state.pendingBells != 1 || len(sink.got) != 1 {
		t.Fatalf("attention burst generated duplicate bell/delivery: bells=%d events=%d", state.pendingBells, len(sink.got))
	}
	now = now.Add(2 * time.Second)
	state.deliverNotification(session, model.NotificationTerminalAttention)
	if state.pendingBells != 2 || len(sink.got) != 2 {
		t.Fatalf("attention did not resume after window: bells=%d events=%d", state.pendingBells, len(sink.got))
	}
}

func TestLocalNotificationQueueReportsOverflow(t *testing.T) {
	sink := &localNotificationSink{queue: make(chan localNotification, 1), now: time.Now,
		lastAttention: make(map[string]time.Time)}
	n := localNotification{Class: ducklord.NotificationCompleted, Level: ducklord.NotificationSystem}
	if !sink.Deliver(n) || sink.Deliver(n) {
		t.Fatal("queue overflow was silent")
	}
}

func TestSoundSetupWarnsWithoutBreakingOtherNotifications(t *testing.T) {
	cfg := &ducklord.Config{NotificationSounds: map[ducklord.NotificationClass]string{
		ducklord.NotificationCompleted: filepath.Join(t.TempDir(), "missing.wav")}}
	if warning := soundSetupWarning(cfg); !strings.Contains(warning, "unavailable") {
		t.Fatalf("missing sound not diagnosed: %q", warning)
	}
	path := filepath.Join(t.TempDir(), "tone.ogg")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.NotificationSounds[ducklord.NotificationCompleted] = path
	if warning := soundSetupWarning(cfg); warning != "" && !strings.Contains(warning, "ffplay") {
		t.Fatalf("unexpected sound warning: %q", warning)
	}
}

func TestLocalNotificationCommandsContainOnlySafeMetadata(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls")
	for _, name := range []string{"ffplay", "notify-send"} {
		program := filepath.Join(dir, name)
		if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s: %s\\n' \"$0\" \"$*\" >> \"$DUCKLORD_NOTIFICATION_TEST_LOG\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("DUCKLORD_NOTIFICATION_TEST_LOG", logPath)
	sound := filepath.Join(dir, "tone.wav")
	if err := os.WriteFile(sound, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := playLocalSound(context.Background(), sound); err != nil {
		t.Fatal(err)
	}
	if err := showDesktopNotification(context.Background(), localNotification{Project: "Alpha", Session: "Agent", Class: ducklord.NotificationCompleted,
		Level: ducklord.NotificationSystem}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "tone.wav") || !strings.Contains(text, "Ducklord · Alpha") ||
		!strings.Contains(text, "Agent · task completed") || strings.Contains(text, "secret prompt") {
		t.Fatalf("unexpected local notification arguments: %q", text)
	}
}

func TestDesktopLabelIsSingleLineBoundedAndMarkupSafe(t *testing.T) {
	label := desktopLabel("<Admin>\n\t" + strings.Repeat("界", 200))
	if !strings.HasPrefix(label, "&lt;Admin&gt;  ") || strings.ContainsAny(label, "\n\r\t<>") ||
		len([]rune(label)) > 100 {
		t.Fatalf("unsafe desktop label: %q", label)
	}
}
