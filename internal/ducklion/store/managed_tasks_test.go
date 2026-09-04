package store

import (
	"bytes"
	"context"
	"crypto/sha256"
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
