package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func TestPrepareManagedTaskReplaysMetadataWithoutPersistingPrompt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ducklion.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UnixMilli()
	owner := model.Owner{Kind: model.OwnerCC, ID: "dwch_task"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(),
		Status: model.StatusRunning, Writer: &owner, OwnershipEpoch: 2, RuntimeGeneration: 3, TaskState: model.TaskIdle,
		AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := database.CreateSessionIdempotent(ctx, "terminal:test", "create", Fingerprint("create", "", nil), session); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO discord_bindings(session_id,channel_handle,management_handle,created_at_ms) VALUES(?,?,?,?)`, session.ID, owner.ID, "dwch_management", now); err != nil {
		t.Fatal(err)
	}
	prompt := []byte("SECRET_PROMPT_CANARY")
	task := ManagedTask{SessionID: session.ID, TaskID: "cc/42", PromptDigest: sha256.Sum256(prompt), Owner: owner,
		OwnershipEpoch: 2, RuntimeGeneration: 3, OutputStart: 17}
	first, replayed, err := database.PrepareManagedTask(ctx, task)
	if err != nil || replayed || first.Status != ManagedTaskPrepared {
		t.Fatalf("first=%+v replayed=%v err=%v", first, replayed, err)
	}
	second, replayed, err := database.PrepareManagedTask(ctx, task)
	if err != nil || !replayed || second.TaskID != task.TaskID {
		t.Fatalf("second=%+v replayed=%v err=%v", second, replayed, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, prompt) {
		t.Fatal("prompt canary was persisted in Ducklion SQLite")
	}
}

func TestPrepareManagedTaskRejectsChangedDigestOrFence(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UnixMilli()
	owner := model.Owner{Kind: model.OwnerCC, ID: "dwch_task"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(), Status: model.StatusRunning,
		Writer: &owner, OwnershipEpoch: 2, RuntimeGeneration: 3, TaskState: model.TaskIdle, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := database.CreateSessionIdempotent(ctx, "terminal:test", "create", Fingerprint("create", "", nil), session); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO discord_bindings(session_id,channel_handle,management_handle,created_at_ms) VALUES(?,?,?,?)`, session.ID, owner.ID, "dwch_management", now); err != nil {
		t.Fatal(err)
	}
	task := ManagedTask{SessionID: session.ID, TaskID: "cc/42", PromptDigest: sha256.Sum256([]byte("one")), Owner: owner, OwnershipEpoch: 2, RuntimeGeneration: 3}
	if _, _, err := database.PrepareManagedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	task.PromptDigest = sha256.Sum256([]byte("two"))
	if _, replayed, err := database.PrepareManagedTask(ctx, task); !replayed || !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("replayed=%v err=%v", replayed, err)
	}
}

func TestManagedTaskDeliveryAckAppliesWaitingYieldAtomically(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UnixMilli()
	cc := model.Owner{Kind: model.OwnerCC, ID: "dwch_task"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(), Status: model.StatusRunning,
		Writer: &cc, OwnershipEpoch: 4, RuntimeGeneration: 2, TaskState: model.TaskIdle, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := database.CreateSessionIdempotent(ctx, "terminal:test", "create", Fingerprint("create", "", nil), session); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO discord_bindings(session_id,channel_handle,management_handle,created_at_ms) VALUES(?,?,?,?)`, session.ID, cc.ID, "dwch_management", now); err != nil {
		t.Fatal(err)
	}
	task := ManagedTask{SessionID: session.ID, TaskID: "inbox/7", PromptDigest: sha256.Sum256([]byte("prompt")), Owner: cc, OwnershipEpoch: 4, RuntimeGeneration: 2, OutputStart: 10}
	if _, _, err := database.PrepareManagedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkManagedTaskRunning(ctx, session.ID, task.TaskID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO pending_yields(session_id,requester_kind,requester_id,source_epoch,request_id,created_at_ms) VALUES(?,?,?,?,?,?)`, session.ID, "terminal", "desk", 4, "yield-1", now); err != nil {
		t.Fatal(err)
	}
	event := ManagedTaskEvent{TaskID: task.TaskID, Sequence: 1, Kind: "completed", OutputEnd: 25, Digest: sha256.Sum256([]byte("event-1")),
		NotificationCategory: model.NotificationTaskCompleted}
	hookCalls := 0
	updatedTask, updatedSession, err := database.ApplyManagedTaskEvent(ctx, session.ID, 2, event, func(model.Session) error {
		hookCalls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updatedTask.Status != ManagedTaskCompleted || updatedTask.OutputEnd == nil || *updatedTask.OutputEnd != 25 {
		t.Fatalf("task=%+v", updatedTask)
	}
	if updatedSession.TaskState != model.TaskReplying || updatedSession.Writer == nil || updatedSession.Writer.ID != cc.ID || updatedSession.OwnershipEpoch != 4 {
		t.Fatalf("session=%+v", updatedSession)
	}
	if _, replayedSession, err := database.ApplyManagedTaskEvent(ctx, session.ID, 2, event, func(model.Session) error { hookCalls++; return nil }); err != nil || replayedSession.OwnershipEpoch != 4 || hookCalls != 0 {
		t.Fatalf("event replay session=%+v hookCalls=%d err=%v", replayedSession, hookCalls, err)
	}
	readTx := mustBeginReadTx(t, database)
	if pending, err := database.GetPendingYieldTx(ctx, readTx, session.ID); err != nil || pending == nil {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := readTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	acked, err := database.AckManagedTaskEvent(ctx, session.ID, task.TaskID, event.Sequence, cc)
	if err != nil || acked.AckedEventSeq != event.Sequence {
		t.Fatalf("acked=%+v err=%v", acked, err)
	}
	_, err = database.FinalizeManagedTaskDeliveryWithHook(ctx, session.ID, task.TaskID, cc, func(candidate model.Session) error {
		hookCalls++
		if candidate.Writer == nil || candidate.Writer.ID != "desk" || candidate.OwnershipEpoch != 5 || candidate.TaskState != model.TaskIdle {
			t.Fatalf("ack beforeCommit session=%+v", candidate)
		}
		return nil
	})
	if err != nil || hookCalls != 1 {
		t.Fatalf("finalize hookCalls=%d err=%v", hookCalls, err)
	}
	completed, err := database.GetSession(ctx, session.ID)
	if err != nil || completed.TaskState != model.TaskIdle || completed.Writer == nil || completed.Writer.ID != "desk" || completed.OwnershipEpoch != 5 {
		t.Fatalf("completed session=%+v err=%v", completed, err)
	}
	if _, err := database.AckManagedTaskEvent(ctx, session.ID, task.TaskID, event.Sequence, cc); err != nil {
		t.Fatalf("ack replay err=%v", err)
	}
	if _, err := database.FinalizeManagedTaskDeliveryWithHook(ctx, session.ID, task.TaskID, cc, func(model.Session) error { hookCalls++; return nil }); err != nil || hookCalls != 1 {
		t.Fatalf("finalize replay hookCalls=%d err=%v", hookCalls, err)
	}
	readTx = mustBeginReadTx(t, database)
	if pending, err := database.GetPendingYieldTx(ctx, readTx, session.ID); err != nil || pending != nil {
		t.Fatalf("pending after ack=%+v err=%v", pending, err)
	}
	if err := readTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.SessionSnapshot(ctx)
	if err != nil || snapshot.Sessions[0].ActivitySequences[model.NotificationTaskCompleted] != 1 {
		t.Fatalf("activity snapshot=%+v err=%v", snapshot, err)
	}
}

func TestManagedTaskAckLetsExitedRuntimeApplyWaitingYield(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UnixMilli()
	cc := model.Owner{Kind: model.OwnerCC, ID: "dwch_task"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(), Status: model.StatusRunning,
		Writer: &cc, OwnershipEpoch: 4, RuntimeGeneration: 2, TaskState: model.TaskIdle, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := database.CreateSessionIdempotent(ctx, "terminal:test", "create", Fingerprint("create", "", nil), session); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO discord_bindings(session_id,channel_handle,management_handle,created_at_ms) VALUES(?,?,?,?)`, session.ID, cc.ID, "dwch_management", now); err != nil {
		t.Fatal(err)
	}
	task := ManagedTask{SessionID: session.ID, TaskID: "inbox/exit", PromptDigest: sha256.Sum256([]byte("prompt")), Owner: cc, OwnershipEpoch: 4, RuntimeGeneration: 2}
	if _, _, err := database.PrepareManagedTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkManagedTaskRunning(ctx, session.ID, task.TaskID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO pending_yields(session_id,requester_kind,requester_id,source_epoch,request_id,created_at_ms) VALUES(?,?,?,?,?,?)`, session.ID, "terminal", "desk", 4, "yield-exit", now); err != nil {
		t.Fatal(err)
	}
	event := ManagedTaskEvent{TaskID: task.TaskID, Sequence: 1, Kind: "completed", Digest: sha256.Sum256([]byte("event-exit"))}
	if _, _, err := database.ApplyManagedTaskEvent(ctx, session.ID, 2, event, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AckManagedTaskEvent(ctx, session.ID, task.TaskID, 1, cc); err != nil {
		t.Fatal(err)
	}
	fenceErr := errors.New("runtime already exited")
	if _, err := database.FinalizeManagedTaskDeliveryWithHook(ctx, session.ID, task.TaskID, cc, func(model.Session) error { return fenceErr }); !errors.Is(err, fenceErr) {
		t.Fatalf("finalize err=%v", err)
	}
	acked, err := database.GetManagedTask(ctx, session.ID, task.TaskID)
	if err != nil || acked.AckedEventSeq != 1 {
		t.Fatalf("delivery ACK rolled back with runtime fence: task=%+v err=%v", acked, err)
	}
	if err := database.MarkRuntimeExited(ctx, session.ID, 2, true, "completed"); err != nil {
		t.Fatal(err)
	}
	stopped, err := database.GetSession(ctx, session.ID)
	if err != nil || stopped.Status != model.StatusStopped || stopped.TaskState != model.TaskIdle || stopped.Writer == nil || stopped.Writer.ID != "desk" || stopped.OwnershipEpoch != 5 {
		t.Fatalf("stopped session=%+v err=%v", stopped, err)
	}
	readTx := mustBeginReadTx(t, database)
	if pending, err := database.GetPendingYieldTx(ctx, readTx, session.ID); err != nil || pending != nil {
		t.Fatalf("pending after exit=%+v err=%v", pending, err)
	}
}

func mustBeginReadTx(t *testing.T, database *SQLite) *sql.Tx {
	t.Helper()
	tx, err := database.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}
