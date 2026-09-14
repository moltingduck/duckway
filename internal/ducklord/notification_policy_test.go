package ducklord

import (
	"path/filepath"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func TestNotificationClassMappingAndPolicyInheritance(t *testing.T) {
	tests := map[model.NotificationCategory]NotificationClass{
		model.NotificationTaskCompleted:     NotificationCompleted,
		model.NotificationTaskFailed:        NotificationFailed,
		model.NotificationTaskTimeout:       NotificationFailed,
		model.NotificationAgentNeedsInput:   NotificationActionNeeded,
		model.NotificationApprovalRequired:  NotificationActionNeeded,
		model.NotificationTerminalAttention: NotificationAttention,
	}
	for category, want := range tests {
		if got, ok := NotificationClassFor(category); !ok || got != want {
			t.Fatalf("category %s mapped to %s ok=%t, want %s", category, got, ok, want)
		}
	}
	global := map[NotificationClass]NotificationLevel{NotificationCompleted: NotificationSound}
	host := map[NotificationClass]NotificationLevel{NotificationCompleted: NotificationOff}
	session := map[NotificationClass]NotificationLevel{NotificationCompleted: NotificationSystem}
	if got := ResolveNotificationLevel(NotificationCompleted, global, host, nil); got != NotificationOff {
		t.Fatalf("explicit Host off was not preserved: %s", got)
	}
	if got := ResolveNotificationLevel(NotificationCompleted, global, host, session); got != NotificationSystem {
		t.Fatalf("Session override failed: %s", got)
	}
	if got := ResolveNotificationLevel(NotificationAttention, global, host, session); got != NotificationIndicator {
		t.Fatalf("default level=%s", got)
	}
}

func TestProjectFocusThresholdDoesNotPromoteLevel(t *testing.T) {
	if ShouldDeliver(NotificationSound, true, false, NotificationSystem) {
		t.Fatal("other-Project sound escaped the system threshold")
	}
	if !ShouldDeliver(NotificationSystem, true, false, NotificationSystem) || !ShouldDeliver(NotificationIndicator, true, true, NotificationSystem) {
		t.Fatal("eligible notification was suppressed")
	}
	if ShouldDeliver(NotificationOff, true, true, NotificationIndicator) {
		t.Fatal("off was promoted by Project focus")
	}
}

func TestNotificationLevelConfigAndStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{NotificationLevels: map[NotificationClass]NotificationLevel{NotificationCompleted: NotificationSound},
		OtherProjectThreshold: NotificationSystem,
		Clients:               []Client{{Name: "host", Host: "host", NotificationLevels: map[NotificationClass]NotificationLevel{NotificationAttention: NotificationOff}}}}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded.NotificationLevels[NotificationCompleted] != NotificationSound || loaded.Clients[0].NotificationLevels[NotificationAttention] != NotificationOff {
		t.Fatalf("policy config round trip: %+v err=%v", loaded, err)
	}
	clone := loaded.Clone()
	clone.Clients[0].NotificationLevels[NotificationAttention] = NotificationSound
	if loaded.Clients[0].NotificationLevels[NotificationAttention] != NotificationOff {
		t.Fatal("Host policy map shared by clone")
	}
	state := NewActivityState()
	identity := testLayoutIdentity("ABC123")
	state.Sessions[identity.Key()] = SessionNotificationState{NotificationLevels: map[NotificationClass]NotificationLevel{NotificationActionNeeded: NotificationSystem}}
	stateCopy := state.Clone()
	stateCopy.Sessions[identity.Key()].NotificationLevels[NotificationActionNeeded] = NotificationOff
	if state.Sessions[identity.Key()].NotificationLevels[NotificationActionNeeded] != NotificationSystem {
		t.Fatal("Session policy map shared by clone")
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := (ActivityStateStore{Path: statePath}).Save(state); err != nil {
		t.Fatal(err)
	}
	reloaded, err := (ActivityStateStore{Path: statePath}).Load()
	if err != nil || reloaded.Sessions[identity.Key()].NotificationLevels[NotificationActionNeeded] != NotificationSystem {
		t.Fatalf("Session policy round trip: %+v err=%v", reloaded, err)
	}
}

func TestReconcileOffPolicyConsumesWithoutUnreadAndReenableDoesNotReplay(t *testing.T) {
	state := NewActivityState()
	identity := testLayoutIdentity("ABC123")
	session := RemoteSession{InstanceID: identity.InstanceID, SessionID: identity.SessionID,
		ActivitySequences: map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 0}}
	state.ReconcileWithEnabled(session, false, func(model.NotificationCategory) bool { return false })
	session.ActivitySequences[model.NotificationTaskCompleted] = 1
	if unread, _ := state.ReconcileWithEnabled(session, false, func(model.NotificationCategory) bool { return false }); unread {
		t.Fatal("off policy created unread")
	}
	if unread, _ := state.ReconcileWithEnabled(session, false, func(model.NotificationCategory) bool { return true }); unread {
		t.Fatal("re-enable replayed old event")
	}
}
