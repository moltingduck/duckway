package store

import (
	"context"
	"crypto/sha256"
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
