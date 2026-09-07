package client

import (
	"errors"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func TestCCProvisionStorePersistsPhasesAndRejectsReplayConflict(t *testing.T) {
	dir := t.TempDir()
	store := newCCProvisionStore(dir)
	want := ccProvisionRecord{RequestID: "discord-123", ManagementHandle: "dwch_mgmt", CCID: "cc1", Slug: "測試", Topic: "topic", CWD: "/work"}
	reserved, err := store.Reserve(want)
	if err != nil || reserved.Phase != ccProvisionReserved {
		t.Fatalf("reserve=%+v err=%v", reserved, err)
	}
	reserved.Channel = &CreateCCChannelResult{Handle: "dwch_task", Name: "test", Cwd: "/work", Kind: "task"}
	reserved.Phase = ccProvisionChannelCreated
	if err := store.Save(reserved); err != nil {
		t.Fatal(err)
	}
	reloaded := newCCProvisionStore(dir)
	replayed, err := reloaded.Reserve(want)
	if err != nil || replayed.Channel == nil || replayed.Channel.Handle != "dwch_task" || replayed.Phase != ccProvisionChannelCreated {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	conflict := want
	conflict.CWD = "/different"
	if _, err := reloaded.Reserve(conflict); !errors.Is(err, errCCProvisionConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	replayed.Session = &protocol.SessionSummary{SessionID: "ABC123", AgentType: "fixture"}
	replayed.Phase = ccProvisionReplyDelivered
	if err := reloaded.Save(replayed); err != nil {
		t.Fatal(err)
	}
	if pending := newCCProvisionStore(dir).Pending(); len(pending) != 0 {
		t.Fatalf("completed workflow remained pending: %+v", pending)
	}
}

func TestCCProvisionStoreKeepsActiveUntilReplyDelivered(t *testing.T) {
	store := newCCProvisionStore(t.TempDir())
	record, err := store.Reserve(ccProvisionRecord{RequestID: "discord-456", ManagementHandle: "dwch_mgmt", CCID: "cc1", Slug: "task", CWD: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = ccProvisionActive
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	if pending := store.Pending(); len(pending) != 1 || pending[0].Phase != ccProvisionActive {
		t.Fatalf("pending=%+v", pending)
	}
}
