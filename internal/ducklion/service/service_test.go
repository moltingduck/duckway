package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklion/store"
)

func openService(t *testing.T) (*Service, *store.SQLite) {
	t.Helper()
	state, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ducklion.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return New(state), state
}

func newAgent(id model.SessionID, handle string, task model.TaskState) model.Session {
	now := time.Now().UTC().UnixMilli()
	owner := model.Owner{Kind: model.OwnerCC, ID: "channel-1"}
	return model.Session{ID: id, Handle: handle, Kind: model.KindAgent, AgentType: "codex", CWD: "/tmp", Status: model.StatusRunning,
		Writer: &owner, OwnershipEpoch: 1, RuntimeGeneration: 1, TaskState: task, AdapterState: model.AdapterHealthy, CreatedAtMS: now, UpdatedAtMS: now}
}

func TestCreateSessionAllowsDuplicateHandlesAndReplays(t *testing.T) {
	ctx := context.Background()
	service, state := openService(t)
	first := newAgent("ABC123", "相同名稱", model.TaskIdle)
	outcome, replayed, err := service.CreateSession(ctx, "cc:channel-1", "create-1", first)
	if err != nil || replayed || outcome.SessionID != first.ID {
		t.Fatalf("outcome=%+v replayed=%v err=%v", outcome, replayed, err)
	}
	if _, replayed, err := service.CreateSession(ctx, "cc:channel-1", "create-1", first); err != nil || !replayed {
		t.Fatalf("replay=%v err=%v", replayed, err)
	}
	second := newAgent("DEF456", "相同名稱", model.TaskIdle)
	if _, _, err := service.CreateSession(ctx, "cc:channel-1", "create-2", second); err != nil {
		t.Fatal(err)
	}
	if got, err := state.GetSession(ctx, second.ID); err != nil || got.Handle != first.Handle {
		t.Fatalf("second=%+v err=%v", got, err)
	}
}

func TestRequestYieldWaitIsExclusiveAndDurable(t *testing.T) {
	ctx := context.Background()
	service, _ := openService(t)
	session := newAgent("ABC123", "agent", model.TaskRunning)
	if _, _, err := service.CreateSession(ctx, "cc:channel-1", "create", session); err != nil {
		t.Fatal(err)
	}
	requester := model.Owner{Kind: model.OwnerTerminal, ID: "laptop"}
	outcome, replayed, err := service.RequestYield(ctx, "terminal:laptop", "yield-1", session.ID, requester, true, 1, 1)
	if err != nil || replayed || outcome.Decision != model.YieldWaiting {
		t.Fatalf("outcome=%+v replayed=%v err=%v", outcome, replayed, err)
	}
	if _, replayed, err := service.RequestYield(ctx, "terminal:laptop", "yield-1", session.ID, requester, true, 1, 1); err != nil || !replayed {
		t.Fatalf("replay=%v err=%v", replayed, err)
	}
	other := model.Owner{Kind: model.OwnerTerminal, ID: "desktop"}
	outcome, _, err = service.RequestYield(ctx, "terminal:desktop", "yield-2", session.ID, other, true, 1, 1)
	if err != nil || outcome.Error == nil || outcome.Error.Code != protocol.ErrPendingYield {
		t.Fatalf("second outcome=%+v err=%v", outcome, err)
	}
}

func TestImmediateYieldUpdatesEpochAndFencesStaleRetry(t *testing.T) {
	ctx := context.Background()
	service, state := openService(t)
	session := newAgent("ABC123", "agent", model.TaskIdle)
	if _, _, err := service.CreateSession(ctx, "cc:channel-1", "create", session); err != nil {
		t.Fatal(err)
	}
	requester := model.Owner{Kind: model.OwnerTerminal, ID: "laptop"}
	outcome, _, err := service.RequestYield(ctx, "terminal:laptop", "yield-1", session.ID, requester, false, 1, 1)
	if err != nil || outcome.Decision != model.YieldTransferred || outcome.OwnershipEpoch != 2 {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	got, err := state.GetSession(ctx, session.ID)
	if err != nil || got.Writer == nil || *got.Writer != requester || got.OwnershipEpoch != 2 {
		t.Fatalf("session=%+v err=%v", got, err)
	}
	stale, _, err := service.RequestYield(ctx, "terminal:desktop", "yield-2", session.ID, model.Owner{Kind: model.OwnerTerminal, ID: "desktop"}, false, 1, 1)
	if err != nil || stale.Error == nil || stale.Error.Code != protocol.ErrStaleEpoch {
		t.Fatalf("stale=%+v err=%v", stale, err)
	}
}

func TestYieldRejectsSpoofedRequester(t *testing.T) {
	ctx := context.Background()
	service, _ := openService(t)
	session := newAgent("ABC123", "agent", model.TaskIdle)
	if _, _, err := service.CreateSession(ctx, "cc:channel-1", "create", session); err != nil {
		t.Fatal(err)
	}
	_, _, err := service.RequestYield(ctx, "terminal:laptop", "yield", session.ID, model.Owner{Kind: model.OwnerTerminal, ID: "desktop"}, false, 1, 1)
	if err == nil {
		t.Fatal("spoofed requester accepted")
	}
}

func TestWaitingYieldDrainsNewTasksAndTransfersAfterReply(t *testing.T) {
	ctx := context.Background()
	service, state := openService(t)
	session := newAgent("ABC123", "agent", model.TaskRunning)
	if _, _, err := service.CreateSession(ctx, "cc:channel-1", "create", session); err != nil {
		t.Fatal(err)
	}
	terminal := model.Owner{Kind: model.OwnerTerminal, ID: "laptop"}
	if outcome, _, err := service.RequestYield(ctx, "terminal:laptop", "yield", session.ID, terminal, true, 1, 1); err != nil || outcome.Decision != model.YieldWaiting {
		t.Fatalf("yield=%+v err=%v", outcome, err)
	}
	cc := model.Owner{Kind: model.OwnerCC, ID: "channel-1"}
	if outcome, _, err := service.BeginTask(ctx, "cc:channel-1", "second-task", session.ID, cc, 1, 1); err != nil || outcome.Error == nil || outcome.Error.Code != protocol.ErrDraining {
		t.Fatalf("new task=%+v err=%v", outcome, err)
	}
	if outcome, _, err := service.BeginReply(ctx, "supervisor:ABC123", "reply", session.ID, 1); err != nil || outcome.Error != nil {
		t.Fatalf("begin reply=%+v err=%v", outcome, err)
	}
	outcome, _, err := service.CompleteReply(ctx, "supervisor:ABC123", "complete", session.ID, 1)
	if err != nil || outcome.Error != nil || outcome.Writer == nil || *outcome.Writer != terminal || outcome.OwnershipEpoch != 2 {
		t.Fatalf("complete=%+v err=%v", outcome, err)
	}
	got, err := state.GetSession(ctx, session.ID)
	if err != nil || got.TaskState != model.TaskIdle || got.Writer == nil || *got.Writer != terminal {
		t.Fatalf("session=%+v err=%v", got, err)
	}
}
