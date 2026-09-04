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

func TestManagedTaskCompletionAppliesWaitingYieldAtomically(t *testing.T) {
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
	event := ManagedTaskEvent{TaskID: task.TaskID, Sequence: 1, Kind: "completed", OutputEnd: 25, Digest: sha256.Sum256([]byte("event-1"))}
	hookCalls := 0
	updatedTask, updatedSession, err := database.ApplyManagedTaskEvent(ctx, session.ID, 2, event, func(candidate model.Session) error {
		hookCalls++
		if candidate.Writer == nil || candidate.Writer.ID != "desk" || candidate.OwnershipEpoch != 5 {
			t.Fatalf("beforeCommit session=%+v", candidate)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updatedTask.Status != ManagedTaskCompleted || updatedTask.OutputEnd == nil || *updatedTask.OutputEnd != 25 {
		t.Fatalf("task=%+v", updatedTask)
	}
	if updatedSession.TaskState != model.TaskIdle || updatedSession.Writer == nil || updatedSession.Writer.ID != "desk" || updatedSession.OwnershipEpoch != 5 {
		t.Fatalf("session=%+v", updatedSession)
	}
	if _, replayedSession, err := database.ApplyManagedTaskEvent(ctx, session.ID, 2, event, func(model.Session) error { hookCalls++; return nil }); err != nil || replayedSession.OwnershipEpoch != 5 || hookCalls != 1 {
		t.Fatalf("event replay session=%+v hookCalls=%d err=%v", replayedSession, hookCalls, err)
	}
	if pending, err := database.GetPendingYieldTx(ctx, mustBeginReadTx(t, database), session.ID); err != nil || pending != nil {
		t.Fatalf("pending=%+v err=%v", pending, err)
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
