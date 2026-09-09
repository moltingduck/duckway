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

func TestActivityStateOrganizationRoundTripAndClone(t *testing.T) {
	instanceA := string(model.NewInstanceID())
	instanceB := string(model.NewInstanceID())
	groupID := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	a := SessionIdentity{InstanceID: instanceA, SessionID: "ABC123"}
	b := SessionIdentity{InstanceID: instanceB, SessionID: "ABC123"}
	state := NewActivityState()
	state.Organization = OrganizationState{
		Mode:         OrganizationHost,
		SessionOrder: []SessionIdentity{a, b},
		Groups:       []CustomGroup{{ID: groupID, Name: "正式環境"}},
		Membership:   map[SessionIdentity]string{a: groupID},
		GroupOrders: map[OrganizationMode][]string{
			OrganizationCustom: {groupID, UngroupedGroupID},
			OrganizationHost:   {"prod", "lab"},
			OrganizationType:   {"agent", "shell"},
		},
	}
	path := filepath.Join(t.TempDir(), "private", "state.json")
	store := ActivityStateStore{Path: path}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Organization.Mode != OrganizationHost || len(loaded.Organization.SessionOrder) != 2 || loaded.Organization.Membership[a] != groupID {
		t.Fatalf("loaded organization=%+v", loaded.Organization)
	}
	if loaded.Organization.SessionOrder[0].Key() != instanceA+"/ABC123" || loaded.Organization.SessionOrder[1].Key() != instanceB+"/ABC123" {
		t.Fatalf("canonical cross-instance identities lost: %+v", loaded.Organization.SessionOrder)
	}
	clone := loaded.Clone()
	clone.Organization.Groups[0].Name = "changed"
	clone.Organization.SessionOrder[0] = b
	clone.Organization.Membership[b] = groupID
	clone.Organization.GroupOrders[OrganizationHost][0] = "changed"
	if loaded.Organization.Groups[0].Name != "正式環境" || loaded.Organization.SessionOrder[0] != a || loaded.Organization.Membership[b] != "" || loaded.Organization.GroupOrders[OrganizationHost][0] != "prod" {
		t.Fatal("organization clone aliases original state")
	}
}

func TestActivityStateLegacyJSONDefaultsOrganizationAndPreservesUnknownSections(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"notifications":{},"future":{"kept":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := (ActivityStateStore{Path: path}).Load()
	if err != nil || state.Organization.Mode != OrganizationCustom || len(state.ExtraSections["future"]) == 0 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestActivityStateUnknownOrganizationModeWarnsAndKeepsValidState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	groupID := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	data := []byte(`{"version":1,"notifications":{},"organization":{"mode":"future-mode","groups":[{"id":"` + groupID + `","name":"保留我"}]}}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	state, err := (ActivityStateStore{Path: path}).Load()
	var warning *StateLoadWarning
	if state == nil || !errors.As(err, &warning) {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if state.Organization.Mode != OrganizationCustom || len(state.Organization.Groups) != 1 || state.Organization.Groups[0].Name != "保留我" {
		t.Fatalf("valid organization data was discarded: %+v", state.Organization)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("warning unexpectedly preserved state as corrupt: %v", statErr)
	}
}

func TestActivityStateRejectsInvalidOrganization(t *testing.T) {
	instanceID := string(model.NewInstanceID())
	identity := SessionIdentity{InstanceID: instanceID, SessionID: "ABC123"}
	groupID := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	tests := []struct {
		name string
		edit func(*ActivityState)
	}{
		{"duplicate session order", func(state *ActivityState) { state.Organization.SessionOrder = []SessionIdentity{identity, identity} }},
		{"noncanonical session identity", func(state *ActivityState) {
			state.Organization.SessionOrder = []SessionIdentity{{InstanceID: instanceID, SessionID: "abc123"}}
		}},
		{"duplicate group uuid", func(state *ActivityState) {
			state.Organization.Groups = []CustomGroup{{ID: groupID, Name: "一"}, {ID: groupID, Name: "二"}}
		}},
		{"bidi group name", func(state *ActivityState) {
			state.Organization.Groups = []CustomGroup{{ID: groupID, Name: "prod\u202e"}}
		}},
		{"newline group name", func(state *ActivityState) { state.Organization.Groups = []CustomGroup{{ID: groupID, Name: "a\nb"}} }},
		{"dangling membership", func(state *ActivityState) { state.Organization.Membership[identity] = groupID }},
		{"duplicate group order", func(state *ActivityState) {
			state.Organization.GroupOrders[OrganizationHost] = []string{"prod", "prod"}
		}},
		{"unknown group order mode", func(state *ActivityState) { state.Organization.GroupOrders[OrganizationMode("future")] = []string{"x"} }},
		{"invalid type group", func(state *ActivityState) { state.Organization.GroupOrders[OrganizationType] = []string{"codex"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := NewActivityState()
			test.edit(state)
			if err := (ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")}).Save(state); err == nil {
				t.Fatal("invalid organization state was saved")
			}
		})
	}
}

func TestActivityStateSaveRejectsSymlinkDirectoryBeforeChmod(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "state-dir")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := (ActivityStateStore{Path: filepath.Join(link, "state.json")}).Save(NewActivityState())
	if err == nil {
		t.Fatal("state save accepted symlink directory")
	}
	info, statErr := os.Stat(target)
	if statErr != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("symlink target permissions changed to %v: %v", info, statErr)
	}
}
