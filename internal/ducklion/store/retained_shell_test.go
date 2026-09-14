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

func createRetirementTestSession(t *testing.T, database *SQLite, id model.SessionID, kind model.SessionKind) model.Session {
	t.Helper()
	now := time.Now().UTC().UnixMilli()
	session := model.Session{ID: id, Handle: "工作區", Kind: kind, CWD: t.TempDir(), Status: model.StatusRunning,
		OwnershipEpoch: 1, RuntimeGeneration: 3, TaskState: model.TaskIdle, AdapterState: model.AdapterUnavailable,
		RecoveryPublicKey: make([]byte, 32), CreatedAtMS: now, UpdatedAtMS: now}
	if kind == model.KindAgent {
		owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
		session.Writer, session.AgentType, session.AdapterState = &owner, "fixture", model.AdapterHealthy
	}
	if _, _, err := database.CreateSessionIdempotent(context.Background(), "terminal:desk", "create-"+string(id), sha256.Sum256([]byte(id)), session); err != nil {
		t.Fatal(err)
	}
	return session
}

func TestRetireExitedShellAtomicallyDeletesInventoryAndRetainsLogIdentity(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	session := createRetirementTestSession(t, database, "ABC123", model.KindShell)
	before, err := database.SessionSnapshot(ctx)
	if err != nil || len(before.Sessions) != 1 {
		t.Fatalf("before snapshot: %+v %v", before, err)
	}
	retired, err := database.RetireExitedShell(ctx, session.ID, session.RuntimeGeneration, true, "normal exit")
	if err != nil || !retired {
		t.Fatalf("retired=%t err=%v", retired, err)
	}
	if _, err := database.GetSession(ctx, session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired shell remained selectable: %v", err)
	}
	after, err := database.SessionSnapshot(ctx)
	if err != nil || len(after.Sessions) != 0 || after.Revision != before.Revision+1 {
		t.Fatalf("retirement snapshot: %+v %v", after, err)
	}
	revisions, _, _, err := database.SessionRevisionsAfter(ctx, before.Revision, 10)
	if err != nil || len(revisions) != 1 || revisions[0].Change != "delete" || revisions[0].SessionID != session.ID {
		t.Fatalf("retirement revision: %+v %v", revisions, err)
	}
	log, err := database.GetRetainedShellSession(ctx, session.ID, session.RuntimeGeneration)
	if err != nil || log.Handle != session.Handle || !log.ExitSuccess || log.ExitReason != "normal exit" || log.ExitedAtMS <= 0 {
		t.Fatalf("retained log identity: %+v %v", log, err)
	}
	if retired, err := database.RetireExitedShell(ctx, session.ID, session.RuntimeGeneration, true, "normal exit"); err != nil || !retired {
		t.Fatalf("duplicate exit was not acknowledged: retired=%t err=%v", retired, err)
	}
	if replay, err := database.SessionSnapshot(ctx); err != nil || replay.Revision != after.Revision {
		t.Fatalf("duplicate exit advanced journal: %+v %v", replay, err)
	}
	listed, err := database.ListRetainedShellSessions(ctx, 10)
	if err != nil || len(listed) != 1 || listed[0] != log {
		t.Fatalf("diagnostic list: %+v %v", listed, err)
	}
	if all, err := database.ListRetainedShellSessions(ctx, 0); err != nil || len(all) != 1 || all[0] != log {
		t.Fatalf("sweeper list: %+v %v", all, err)
	}
	if err := database.DeleteRetainedShellSession(ctx, session.ID, session.RuntimeGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetRetainedShellSession(ctx, session.ID, session.RuntimeGeneration); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired diagnostic index remained: %v", err)
	}
}

func TestRetireExitedShellPreservesAgentAndPendingLifecycle(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	agent := createRetirementTestSession(t, database, "ABC123", model.KindAgent)
	shell := createRetirementTestSession(t, database, "DEF456", model.KindShell)
	if retired, err := database.RetireExitedShell(ctx, agent.ID, agent.RuntimeGeneration, false, "exit"); err == nil || retired {
		t.Fatalf("agent was retired as a shell: retired=%t err=%v", retired, err)
	}
	if err := database.MarkRuntimeExited(ctx, agent.ID, agent.RuntimeGeneration, true, "completed"); err != nil {
		t.Fatal(err)
	}
	if got, err := database.GetSession(ctx, agent.ID); err != nil || got.Status != model.StatusStopped {
		t.Fatalf("managed Agent lost stopped lifecycle: %+v %v", got, err)
	}
	if _, err := database.db.ExecContext(ctx, `INSERT INTO pending_lifecycle_operations
		(session_id,operation,mode,requester_kind,requester_id,source_epoch,source_generation,request_id,created_at_ms)
		VALUES(?,'restart','immediate','terminal','desk',1,3,'restart-shell',?)`, shell.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	before, err := database.SessionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := database.RetireExitedShell(ctx, shell.ID, shell.RuntimeGeneration, true, "exit"); err != nil || retired {
		t.Fatalf("pending restart should retain shell row: retired=%t err=%v", retired, err)
	}
	after, err := database.SessionSnapshot(ctx)
	if err != nil || after.Revision != before.Revision {
		t.Fatalf("pending lifecycle changed journal: %+v %v", after, err)
	}
	if _, err := database.GetRetainedShellSession(ctx, shell.ID, shell.RuntimeGeneration); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending restart gained tombstone: %v", err)
	}
	if err := database.MarkRuntimeExited(ctx, shell.ID, shell.RuntimeGeneration, true, "exit"); err != nil {
		t.Fatal(err)
	}
	if got, err := database.GetSession(ctx, shell.ID); err != nil || got.Status != model.StatusStopped {
		t.Fatalf("pending restart shell was not stopped durably: %+v %v", got, err)
	}
}

func TestRetireExitedShellRejectsStaleGenerationWithoutTombstone(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	session := createRetirementTestSession(t, database, "ABC123", model.KindShell)
	before, err := database.SessionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := database.RetireExitedShell(ctx, session.ID, session.RuntimeGeneration+1, true, "exit"); err == nil || retired {
		t.Fatalf("stale generation retired shell: retired=%t err=%v", retired, err)
	}
	after, err := database.SessionSnapshot(ctx)
	if err != nil || after.Revision != before.Revision || len(after.Sessions) != 1 {
		t.Fatalf("stale attempt mutated inventory: %+v %v", after, err)
	}
	if _, err := database.GetRetainedShellSession(ctx, session.ID, session.RuntimeGeneration+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale attempt wrote tombstone: %v", err)
	}
}
