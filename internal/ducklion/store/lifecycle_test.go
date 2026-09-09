package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func TestLifecycleBarrierIsFencedReplayableAndBlocksAdmission(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC().UnixMilli()
	owner := model.Owner{Kind: model.OwnerCC, ID: "dwch_task"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(), Status: model.StatusRunning,
		Writer: &owner, OwnershipEpoch: 2, RuntimeGeneration: 3, TaskState: model.TaskRunning, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := database.CreateSessionIdempotent(ctx, "terminal:test", "create", Fingerprint("create", "", nil), session); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO discord_bindings(session_id,channel_handle,management_handle,created_at_ms) VALUES(?,?,?,?)`, session.ID, owner.ID, "dwch_mgmt", now); err != nil {
		t.Fatal(err)
	}
	immediate := PendingLifecycle{SessionID: session.ID, Operation: LifecycleEnd, Mode: LifecycleImmediate, Requester: owner,
		SourceEpoch: 2, SourceGeneration: 3, RequestID: "end-now"}
	if _, _, err := database.ReserveLifecycle(ctx, immediate); !errors.Is(err, model.ErrTaskActive) {
		t.Fatalf("immediate busy reserve error=%v", err)
	}
	if pending, err := database.GetPendingLifecycle(ctx, session.ID); err != nil || pending != nil {
		t.Fatalf("failed immediate request installed barrier: pending=%+v err=%v", pending, err)
	}
	waiting := immediate
	waiting.Mode, waiting.RequestID = LifecycleWait, "end-wait"
	if _, replayed, err := database.ReserveLifecycle(ctx, waiting); err != nil || replayed {
		t.Fatalf("wait reserve replayed=%v err=%v", replayed, err)
	}
	if _, replayed, err := database.ReserveLifecycle(ctx, waiting); err != nil || !replayed {
		t.Fatalf("wait replay replayed=%v err=%v", replayed, err)
	}
	conflict := waiting
	conflict.Operation, conflict.RequestID = LifecycleDestroy, "destroy-wait"
	if _, _, err := database.ReserveLifecycle(ctx, conflict); !errors.Is(err, model.ErrLifecyclePending) {
		t.Fatalf("competing lifecycle error=%v", err)
	}
	// Model task completion while the drain remains installed. New admission
	// must still fail even though task_state is now idle.
	if _, err := database.db.Exec(`UPDATE sessions SET task_state='idle' WHERE session_id=?`, session.ID); err != nil {
		t.Fatal(err)
	}
	task := ManagedTask{SessionID: session.ID, TaskID: "next", PromptDigest: sha256.Sum256([]byte("next")), Owner: owner,
		OwnershipEpoch: 2, RuntimeGeneration: 3}
	if _, _, err := database.PrepareManagedTask(ctx, task); !errors.Is(err, model.ErrLifecyclePending) {
		t.Fatalf("task crossed lifecycle barrier: %v", err)
	}
}

func TestLifecycleBarrierRejectsWrongOwnerAndFence(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(), Status: model.StatusRunning,
		Writer: &owner, OwnershipEpoch: 5, RuntimeGeneration: 7, TaskState: model.TaskIdle, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := database.CreateSessionIdempotent(ctx, "terminal:test", "create", Fingerprint("create", "", nil), session); err != nil {
		t.Fatal(err)
	}
	request := PendingLifecycle{SessionID: session.ID, Operation: LifecycleRestart, Mode: LifecycleWait, Requester: model.Owner{Kind: model.OwnerTerminal, ID: "other"},
		SourceEpoch: 5, SourceGeneration: 7, RequestID: "restart"}
	if _, _, err := database.ReserveLifecycle(ctx, request); !errors.Is(err, model.ErrNotOwner) {
		t.Fatalf("wrong owner error=%v", err)
	}
	request.Requester, request.SourceEpoch = owner, 4
	if _, _, err := database.ReserveLifecycle(ctx, request); !errors.Is(err, model.ErrStaleEpoch) {
		t.Fatalf("stale epoch error=%v", err)
	}
}

func TestLifecycleReplayInvalidatesStaleBarrier(t *testing.T) {
	tests := []struct {
		name, update string
		want         error
	}{
		{name: "epoch", update: `UPDATE sessions SET ownership_epoch=6 WHERE session_id='ABC123'`, want: model.ErrStaleEpoch},
		{name: "generation", update: `UPDATE sessions SET runtime_generation=8 WHERE session_id='ABC123'`, want: model.ErrStaleGeneration},
		{name: "owner", update: `UPDATE sessions SET writer_id='other' WHERE session_id='ABC123'`, want: model.ErrNotOwner},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			now := time.Now().UTC().UnixMilli()
			owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
			session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(), Status: model.StatusRunning,
				Writer: &owner, OwnershipEpoch: 5, RuntimeGeneration: 7, TaskState: model.TaskIdle, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
			if _, _, err := database.CreateSessionIdempotent(ctx, "terminal:test", "create", Fingerprint("create", "", nil), session); err != nil {
				t.Fatal(err)
			}
			request := PendingLifecycle{SessionID: session.ID, Operation: LifecycleRestart, Mode: LifecycleWait, Requester: owner,
				SourceEpoch: 5, SourceGeneration: 7, RequestID: "restart"}
			if _, _, err := database.ReserveLifecycle(ctx, request); err != nil {
				t.Fatal(err)
			}
			if _, err := database.db.Exec(tc.update); err != nil {
				t.Fatal(err)
			}
			if _, _, err := database.ReserveLifecycle(ctx, request); !errors.Is(err, tc.want) {
				t.Fatalf("replay error=%v want=%v", err, tc.want)
			}
			if pending, err := database.GetPendingLifecycle(ctx, session.ID); err != nil || pending != nil {
				t.Fatalf("stale barrier remains: pending=%+v err=%v", pending, err)
			}
		})
	}
}

func TestLifecyclePhaseCASListAndCancel(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
	for _, id := range []model.SessionID{"ABC124", "ABC123"} {
		session := model.Session{ID: id, Handle: string(id), Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(), Status: model.StatusRunning,
			Writer: &owner, OwnershipEpoch: 2, RuntimeGeneration: 3, TaskState: model.TaskIdle, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
		if _, _, err := database.CreateSessionIdempotent(ctx, "terminal:desk", "create-"+string(id), Fingerprint("create", id, nil), session); err != nil {
			t.Fatal(err)
		}
		request := PendingLifecycle{SessionID: id, Operation: LifecycleEnd, Mode: LifecycleWait, Requester: owner,
			SourceEpoch: 2, SourceGeneration: 3, RequestID: "end-" + string(id)}
		if _, _, err := database.ReserveLifecycle(ctx, request); err != nil {
			t.Fatal(err)
		}
	}

	listed, err := database.ListPendingLifecycles(ctx)
	if err != nil || len(listed) != 2 || listed[0].SessionID != "ABC123" || listed[1].SessionID != "ABC124" {
		t.Fatalf("pending=%+v err=%v", listed, err)
	}
	if listed[0].Phase != LifecycleWaiting || listed[0].Attempt != 0 || listed[0].UpdatedAtMS == 0 {
		t.Fatalf("initial recovery metadata=%+v", listed[0])
	}
	advanced, err := database.CompareAndSwapLifecyclePhase(ctx, "ABC123", "end-ABC123", LifecycleWaiting, LifecycleStopping, "retryable stop")
	if err != nil || advanced.Phase != LifecycleStopping || advanced.Attempt != 1 || advanced.LastError != "retryable stop" || advanced.UpdatedAtMS < advanced.CreatedAtMS {
		t.Fatalf("advanced=%+v err=%v", advanced, err)
	}
	if _, err := database.CompareAndSwapLifecyclePhase(ctx, "ABC123", "end-ABC123", LifecycleWaiting, LifecycleStopping, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale phase CAS error=%v", err)
	}
	if cancelled, err := database.CancelWaitingLifecycle(ctx, "ABC123", "end-ABC123", owner, 2, 3); err != nil || cancelled {
		t.Fatalf("executing cancellation cancelled=%v err=%v", cancelled, err)
	}
	if cancelled, err := database.CancelWaitingLifecycle(ctx, "ABC124", "end-ABC124", owner, 2, 3); err != nil || !cancelled {
		t.Fatalf("waiting cancellation cancelled=%v err=%v", cancelled, err)
	}
	listed, err = database.ListPendingLifecycles(ctx)
	if err != nil || len(listed) != 1 || listed[0].SessionID != "ABC123" {
		t.Fatalf("pending after cancellation=%+v err=%v", listed, err)
	}
}

func TestCompleteLifecycleStoresOutcomeAndReleasesBarrier(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(), Status: model.StatusRunning,
		Writer: &owner, OwnershipEpoch: 2, RuntimeGeneration: 3, TaskState: model.TaskIdle, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
	if _, _, err := database.CreateSessionIdempotent(ctx, "terminal:desk", "create", Fingerprint("create", session.ID, nil), session); err != nil {
		t.Fatal(err)
	}
	request := PendingLifecycle{SessionID: session.ID, Operation: LifecycleEnd, Mode: LifecycleWait, Requester: owner,
		SourceEpoch: 2, SourceGeneration: 3, RequestID: "end-one"}
	if _, _, err := database.ReserveLifecycle(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteLifecycle(ctx, request); err != nil {
		t.Fatal(err)
	}
	if pending, err := database.GetPendingLifecycle(ctx, session.ID); err != nil || pending != nil {
		t.Fatalf("completed barrier=%+v err=%v", pending, err)
	}
	outcome, err := database.GetLifecycleOutcome(ctx, owner, request.RequestID)
	if err != nil || outcome == nil || !outcome.Matches(request) {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if err := database.CompleteLifecycle(ctx, request); err != nil {
		t.Fatalf("outcome replay: %v", err)
	}
	conflict := request
	conflict.Mode = LifecycleForce
	if err := database.CompleteLifecycle(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("outcome conflict=%v", err)
	}
}

func TestMigrateV9LifecycleRowsGainRecoveryMetadata(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ducklion.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	migrations := []func(context.Context, *sql.Tx) error{migrateV1, migrateV2, migrateV3, migrateV4, migrateV5, migrateV6, migrateV7, migrateV8, migrateV9}
	for index, migrate := range migrations {
		if err := migrate(ctx, tx); err != nil {
			t.Fatalf("migrate v%d: %v", index+1, err)
		}
	}
	now := time.Now().UTC().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "fixture", CWD: t.TempDir(), Status: model.StatusRunning,
		Writer: &owner, OwnershipEpoch: 2, RuntimeGeneration: 3, TaskState: model.TaskIdle, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions
		(session_id,handle,kind,agent_type,cwd,shell,status,writer_kind,writer_id,ownership_epoch,runtime_generation,task_state,adapter_state,recovery_public_key,created_at_ms,updated_at_ms,exit_success,exit_reason)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, session.ID, session.Handle, session.Kind, session.AgentType, session.CWD, session.Shell,
		session.Status, owner.Kind, owner.ID, session.OwnershipEpoch, session.RuntimeGeneration, session.TaskState, session.AdapterState,
		session.RecoveryPublicKey, session.CreatedAtMS, session.UpdatedAtMS, session.ExitSuccess, session.ExitReason); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO pending_lifecycle_operations
		(session_id,operation,mode,requester_kind,requester_id,source_epoch,source_generation,request_id,created_at_ms)
		VALUES('ABC123','end','wait','terminal','desk',2,3,'end-old',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	pending, err := database.GetPendingLifecycle(ctx, "ABC123")
	if err != nil || pending == nil || pending.Phase != LifecycleWaiting || pending.Attempt != 0 || pending.LastError != "" || pending.UpdatedAtMS != now {
		t.Fatalf("migrated pending=%+v err=%v", pending, err)
	}
}
