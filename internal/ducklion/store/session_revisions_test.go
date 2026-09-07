package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func TestSessionRevisionJournalTracksCommittedProjectionChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ducklion.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "codex", CWD: t.TempDir(), Status: model.StatusRecovering,
		Writer: &owner, OwnershipEpoch: 1, RuntimeGeneration: 1, TaskState: model.TaskIdle, AdapterState: model.AdapterRecovering,
		RecoveryPublicKey: make([]byte, 32), CreatedAtMS: now, UpdatedAtMS: now}
	fingerprint := sha256.Sum256([]byte("create"))
	if _, replayed, err := database.CreateSessionIdempotent(ctx, "terminal:desk", "create-1", fingerprint, session); err != nil || replayed {
		t.Fatalf("create replayed=%v err=%v", replayed, err)
	}
	snapshot, err := database.SessionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 1 || snapshot.EarliestRevision != 1 || len(snapshot.Sessions) != 1 || snapshot.Sessions[0].Session.Handle != "agent" {
		t.Fatalf("initial snapshot=%+v", snapshot)
	}
	if _, replayed, err := database.CreateSessionIdempotent(ctx, "terminal:desk", "create-1", fingerprint, session); err != nil || !replayed {
		t.Fatalf("replay replayed=%v err=%v", replayed, err)
	}
	if afterReplay, _ := database.SessionSnapshot(ctx); afterReplay.Revision != 1 {
		t.Fatalf("idempotent replay advanced revision to %d", afterReplay.Revision)
	}
	if err := database.MarkRuntimeConnected(ctx, session.ID, 1); err != nil {
		t.Fatal(err)
	}
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InsertBindingTx(ctx, tx, DiscordBinding{SessionID: session.ID, ChannelHandle: "task", ManagementHandle: "manage", CreatedAtMS: now}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snapshot, err = database.SessionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 3 || snapshot.Sessions[0].Session.Status != model.StatusRunning || snapshot.Sessions[0].ChannelHandle != "task" {
		t.Fatalf("updated snapshot=%+v", snapshot)
	}
	revisions, earliest, latest, err := database.SessionRevisionsAfter(ctx, 1, 10)
	if err != nil || earliest != 1 || latest != 3 || len(revisions) != 2 || revisions[0].Revision != 2 || revisions[1].Revision != 3 {
		t.Fatalf("revisions=%+v earliest=%d latest=%d err=%v", revisions, earliest, latest, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if reopened, err := database.SessionSnapshot(ctx); err != nil || reopened.Revision < 3 {
		t.Fatalf("reopened snapshot=%+v err=%v", reopened, err)
	}
}

func TestSessionRevisionJournalIgnoresRolledBackMutation(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	owner := model.Owner{Kind: model.OwnerTerminal, ID: "desk"}
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "codex", CWD: t.TempDir(), Status: model.StatusRecovering,
		Writer: &owner, OwnershipEpoch: 1, RuntimeGeneration: 1, TaskState: model.TaskIdle, AdapterState: model.AdapterRecovering,
		RecoveryPublicKey: make([]byte, 32), CreatedAtMS: now, UpdatedAtMS: now}
	if err := database.InsertSessionTx(ctx, tx, session); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.SessionSnapshot(ctx)
	if err != nil || snapshot.Revision != 0 || len(snapshot.Sessions) != 0 {
		t.Fatalf("rolled back snapshot=%+v err=%v", snapshot, err)
	}
}

func TestSessionRevisionJournalRetainsBoundedSuffix(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UnixMilli()
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
	for i := 0; i < 4100; i++ {
		if _, err := database.db.ExecContext(ctx, `UPDATE sessions SET exit_reason=? WHERE session_id=?`, fmt.Sprintf("event-%d", i), session.ID); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := database.SessionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 4101 || snapshot.EarliestRevision != 6 {
		t.Fatalf("revision=%d earliest=%d", snapshot.Revision, snapshot.EarliestRevision)
	}
	revisions, earliest, latest, err := database.SessionRevisionsAfter(ctx, 1, 512)
	if err != nil || earliest != 6 || latest != 4101 || len(revisions) != 512 || revisions[0].Revision != 6 {
		t.Fatalf("revisions=%d first=%d earliest=%d latest=%d err=%v", len(revisions), revisions[0].Revision, earliest, latest, err)
	}
}
