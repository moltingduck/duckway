package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func openActivityTestStore(t *testing.T) (*SQLite, model.Session) {
	t.Helper()
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Now().UTC().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "codex", CWD: t.TempDir(), Status: model.StatusRecovering,
		Writer: &owner, OwnershipEpoch: 1, RuntimeGeneration: 2, TaskState: model.TaskIdle, AdapterState: model.AdapterRecovering,
		RecoveryPublicKey: make([]byte, 32), CreatedAtMS: now, UpdatedAtMS: now}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InsertSessionTx(ctx, tx, session); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return database, session
}

func TestRecordActivityIsDurableCoalescedAndRevisioned(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ducklion.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "codex", CWD: t.TempDir(), Status: model.StatusRecovering,
		Writer: &owner, OwnershipEpoch: 1, RuntimeGeneration: 1, TaskState: model.TaskIdle, AdapterState: model.AdapterRecovering,
		RecoveryPublicKey: make([]byte, 32), CreatedAtMS: now, UpdatedAtMS: now}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InsertSessionTx(ctx, tx, session); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	sequence, advanced, err := database.RecordActivityAtOffset(ctx, session.ID, model.NotificationTerminalAttention, time.Second, 1, 4)
	if err != nil || !advanced || sequence != 1 {
		t.Fatalf("first sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}
	sequence, advanced, err = database.RecordActivityAtOffset(ctx, session.ID, model.NotificationTerminalAttention, time.Second, 1, 8)
	if err != nil || advanced || sequence != 1 {
		t.Fatalf("coalesced sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}
	sequence, advanced, err = database.RecordActivityAtOffset(ctx, session.ID, model.NotificationTerminalAttention, 0, 1, 8)
	if err != nil || advanced || sequence != 1 {
		t.Fatalf("idempotent sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}
	sequence, advanced, err = database.RecordActivityAtOffset(ctx, session.ID, model.NotificationTerminalAttention, time.Second, 2, 2)
	if err != nil || !advanced || sequence != 2 {
		t.Fatalf("new generation sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}
	sequence, advanced, err = database.RecordActivityAtOffset(ctx, session.ID, model.NotificationTerminalAttention, 0, 1, 99)
	if err != nil || advanced || sequence != 2 {
		t.Fatalf("late old generation sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}
	sequence, advanced, err = database.RecordActivity(ctx, session.ID, model.NotificationTaskCompleted, 0)
	if err != nil || !advanced || sequence != 1 {
		t.Fatalf("completed sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}
	sequence, advanced, err = database.RecordActivity(ctx, session.ID, model.NotificationTaskCompleted, 0)
	if err != nil || !advanced || sequence != 2 {
		t.Fatalf("second completed sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}

	snapshot, err := database.SessionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 5 || len(snapshot.Sessions) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	activity := snapshot.Sessions[0].ActivitySequences
	if activity[model.NotificationTerminalAttention] != 2 || activity[model.NotificationTaskCompleted] != 2 {
		t.Fatalf("activity=%+v", activity)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	snapshot, err = database.SessionSnapshot(ctx)
	if err != nil || snapshot.Sessions[0].ActivitySequences[model.NotificationTaskCompleted] != 2 {
		t.Fatalf("reopened snapshot=%+v err=%v", snapshot, err)
	}
}

func TestRecordAgentActivityUsesEventIdentityInsteadOfOutputOffset(t *testing.T) {
	database, session := openActivityTestStore(t)
	sequence, advanced, err := database.RecordAgentActivity(context.Background(), session.ID, model.NotificationTaskCompleted, 2, 1, 0)
	if err != nil || !advanced || sequence != 1 {
		t.Fatalf("first zero-output event: sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}
	sequence, advanced, err = database.RecordAgentActivity(context.Background(), session.ID, model.NotificationTaskCompleted, 2, 1, 0)
	if err != nil || advanced || sequence != 1 {
		t.Fatalf("replay: sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}
	sequence, advanced, err = database.RecordAgentActivity(context.Background(), session.ID, model.NotificationTaskCompleted, 2, 2, 0)
	if err != nil || !advanced || sequence != 2 {
		t.Fatalf("distinct same-offset event: sequence=%d advanced=%v err=%v", sequence, advanced, err)
	}
}

func TestRecordActivityRejectsUnknownSessionAndCategory(t *testing.T) {
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, _, err := database.RecordActivity(context.Background(), "ABC123", model.NotificationTerminalAttention, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session err=%v", err)
	}
	if _, _, err := database.RecordActivity(context.Background(), "ABC123", model.NotificationCategory("payload_injection"), 0); err == nil {
		t.Fatal("invalid category accepted")
	}
}
