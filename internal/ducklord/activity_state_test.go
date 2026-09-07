package ducklord

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func TestActivityStatePreservesCorruptFileAndRecovers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := (ActivityStateStore{Path: path}).Load()
	var recovered *CorruptStateRecoveredError
	if state == nil || !errors.As(err, &recovered) {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("corrupt source remains: %v", statErr)
	}
	data, readErr := os.ReadFile(recovered.PreservedPath)
	if readErr != nil || string(data) != "{broken" {
		t.Fatalf("preserved=%q err=%v", data, readErr)
	}
	if err := (ActivityStateStore{Path: path}).Save(state); err != nil {
		t.Fatal(err)
	}
}

func TestActivityStatePreservesOtherEnvelopeSections(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	original := []byte(`{"version":1,"notifications":{},"manual_order":{"mode":"custom","ids":["ABC123"]}}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	store := ActivityStateStore{Path: path}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	var manual struct {
		Mode string   `json:"mode"`
		IDs  []string `json:"ids"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("saved=%s err=%v", data, err)
	}
	if err := json.Unmarshal(envelope["manual_order"], &manual); err != nil || manual.Mode != "custom" || len(manual.IDs) != 1 || manual.IDs[0] != "ABC123" {
		t.Fatalf("saved=%s err=%v", data, err)
	}
}

func TestActivityStatePersistsUnreadSeenAndDisabledCategories(t *testing.T) {
	instanceID := string(model.NewInstanceID())
	session := RemoteSession{InstanceID: instanceID, SessionID: "ABC123", ActivitySequences: map[model.NotificationCategory]uint64{
		model.NotificationTerminalAttention: 2,
		model.NotificationTaskCompleted:     1,
	}}
	state := NewActivityState()
	if unread, changed := state.Reconcile(session, false); !unread || !changed {
		t.Fatalf("background unread=%v changed=%v", unread, changed)
	}
	if err := state.SetEnabled(instanceID, session.SessionID, model.NotificationTerminalAttention, false); err != nil {
		t.Fatal(err)
	}
	if unread, changed := state.Reconcile(session, false); !unread || !changed {
		t.Fatalf("disabled reconciliation unread=%v changed=%v", unread, changed)
	}
	if !state.MarkSeen(session) {
		t.Fatal("fresh snapshot did not advance remaining cursor")
	}
	if unread, _ := state.Reconcile(session, false); unread {
		t.Fatal("seen session remained unread")
	}
	path := filepath.Join(t.TempDir(), "private", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	store := ActivityStateStore{Path: path}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	key := instanceID + "/ABC123"
	if loaded.Sessions[key].Seen[model.NotificationTerminalAttention] != 2 || !loaded.Sessions[key].Disabled[model.NotificationTerminalAttention] {
		t.Fatalf("loaded=%+v", loaded.Sessions[key])
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions=%v err=%v", info, err)
	}
}

func TestActivityStateActiveAndDisabledEventsAreSeenImmediately(t *testing.T) {
	instanceID := string(model.NewInstanceID())
	state := NewActivityState()
	if err := state.SetEnabled(instanceID, "ABC123", model.NotificationTaskFailed, false); err != nil {
		t.Fatal(err)
	}
	session := RemoteSession{InstanceID: instanceID, SessionID: "ABC123", ActivitySequences: map[model.NotificationCategory]uint64{
		model.NotificationTerminalAttention: 4,
		model.NotificationTaskFailed:        3,
	}}
	if unread, changed := state.Reconcile(session, true); unread || !changed {
		t.Fatalf("active unread=%v changed=%v", unread, changed)
	}
	session.ActivitySequences[model.NotificationTerminalAttention] = 5
	session.ActivitySequences[model.NotificationTaskFailed] = 4
	if unread, changed := state.Reconcile(session, false); !unread || !changed {
		t.Fatalf("background unread=%v changed=%v", unread, changed)
	}
}
