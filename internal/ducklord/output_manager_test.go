package ducklord

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/daemon"
)

type unusedTerminalOutputSource struct{}

func (unusedTerminalOutputSource) OpenOutputStream(context.Context, Client, string) (*OutputStream, error) {
	return nil, errors.New("unexpected production opener")
}
func (unusedTerminalOutputSource) OpenOutputStreamFrom(context.Context, Client, string, AttachResume) (*OutputStream, error) {
	return nil, errors.New("unexpected production opener")
}

func TestTerminalOutputManagerLatestSelectionWinsWithoutBlockingCaller(t *testing.T) {
	started := make(chan struct{})
	root := t.TempDir()
	manager, err := newTerminalOutputManager(context.Background(), 2, unusedTerminalOutputSource{}, SnapshotStore{Root: root},
		func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
			if selection.SessionID == "AAA111" {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			reader := newFakePooledOutput()
			metadata := OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID, RuntimeGeneration: revision.RuntimeGeneration}
			return newPooledTerminal(ctx, metadata, reader, PooledTerminalOptions{ExpectedKey: key, ExpectedRevision: revision,
				Rows: selection.Rows, Cols: selection.Cols, Scrollback: 4, Store: SnapshotStore{Root: root}})
		})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	base := TerminalSelection{Client: Client{Name: "host"}, InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", RuntimeGeneration: 1, Rows: 2, Cols: 20}
	first := base
	first.SessionID = "AAA111"
	manager.Select(first)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first activation did not start")
	}
	second := base
	second.SessionID = "BBB222"
	secondID := manager.Select(second)
	select {
	case event := <-manager.Events():
		if event.RequestID != secondID || event.Key.SessionID != "BBB222" || event.Err != nil {
			t.Fatalf("latest event=%+v", event)
		}
		if _, err := manager.View(event); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("latest activation was blocked by canceled selection")
	}
	status := manager.Status()
	if status.Active == nil || status.Active.SessionID != "BBB222" || status.Count != 1 {
		t.Fatalf("pool status=%+v", status)
	}
}

func TestTerminalOutputManagerPublishesFinalViewAfterLeaseDetaches(t *testing.T) {
	root := t.TempDir()
	reader := newFakePooledOutput()
	manager, err := newTerminalOutputManager(context.Background(), 1, unusedTerminalOutputSource{}, SnapshotStore{Root: root},
		func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
			return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID, RuntimeGeneration: revision.RuntimeGeneration}, reader,
				PooledTerminalOptions{ExpectedKey: key, ExpectedRevision: revision, Rows: 2, Cols: 20, Scrollback: 4, Store: SnapshotStore{Root: root}})
		})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	id := manager.Select(TerminalSelection{Client: Client{Name: "host"}, InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971",
		SessionID: "ABC123", RuntimeGeneration: 1, Rows: 2, Cols: 20})
	select {
	case event := <-manager.Events():
		if event.RequestID != id || event.Err != nil {
			t.Fatalf("activation=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("activation event missing")
	}
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("final"), StartOffset: 0, EndOffset: 5}}
	reader.results <- pooledReadResult{err: &daemon.OutputStreamEnded{Reason: "runtime_stopped", NextOffset: 5}}
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-manager.Events():
			if event.FinalView != nil && event.FinalView.Ended {
				if event.FinalView.OutputOffset != 5 {
					t.Fatalf("final offset=%d", event.FinalView.OutputOffset)
				}
				return
			}
		case <-deadline:
			t.Fatal("final detached framebuffer event missing")
		}
	}
}

func TestTerminalOutputManagerReconnectRestoresAllDesiredAndGeneration(t *testing.T) {
	root := t.TempDir()
	manager, err := newTerminalOutputManager(context.Background(), 2, unusedTerminalOutputSource{}, SnapshotStore{Root: root},
		func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
			return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID, RuntimeGeneration: revision.RuntimeGeneration}, newFakePooledOutput(),
				PooledTerminalOptions{ExpectedKey: key, ExpectedRevision: revision, Rows: 2, Cols: 20, Scrollback: 4, Store: SnapshotStore{Root: root}})
		})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	base := TerminalSelection{Client: Client{Name: "host"}, InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", RuntimeGeneration: 1, Rows: 2, Cols: 20}
	a, b := base, base
	a.SessionID, b.SessionID = "AAA111", "BBB222"
	manager.Select(a)
	waitManagerEvent(t, manager)
	manager.Select(b)
	waitManagerEvent(t, manager)
	manager.SyncHost("host", base.InstanceID, false, nil)
	waitOutputStatus(t, manager, func(status OutputPoolStatus) bool { return status.Count == 2 && status.Connected == 0 })
	a.RuntimeGeneration = 2
	manager.SyncHost("host", base.InstanceID, true, []TerminalSelection{a, b})
	waitOutputStatus(t, manager, func(status OutputPoolStatus) bool {
		return status.Count == 2 && status.Connected == 2 && status.Active != nil && status.Active.SessionID == "BBB222"
	})
	id := manager.Select(a)
	var event TerminalOutputEvent
	deadline := time.After(time.Second)
	for event.RequestID != id {
		select {
		case event = <-manager.Events():
		case <-deadline:
			t.Fatal("restored selection event timed out")
		}
	}
	if event.Revision.RuntimeGeneration != 2 || event.Key.SessionID != "AAA111" {
		t.Fatalf("restored generation event=%+v", event)
	}
}

func TestTerminalOutputManagerSelectionDoesNotCancelHostRestore(t *testing.T) {
	root := t.TempDir()
	restoreA := make(chan struct{})
	releaseA := make(chan struct{})
	var reconnecting atomic.Bool
	var blockOnce sync.Once
	manager, err := newTerminalOutputManager(context.Background(), 2, unusedTerminalOutputSource{}, SnapshotStore{Root: root},
		func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
			if reconnecting.Load() && key.SessionID == "AAA111" {
				blockOnce.Do(func() { close(restoreA) })
				select {
				case <-releaseA:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID, RuntimeGeneration: revision.RuntimeGeneration}, newFakePooledOutput(),
				PooledTerminalOptions{ExpectedKey: key, ExpectedRevision: revision, Rows: selection.Rows, Cols: selection.Cols, Scrollback: 4, Store: SnapshotStore{Root: root}})
		})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	base := TerminalSelection{Client: Client{Name: "host"}, InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", RuntimeGeneration: 1, Rows: 2, Cols: 20}
	a, b := base, base
	a.SessionID, b.SessionID = "AAA111", "BBB222"
	manager.Select(a)
	waitManagerEvent(t, manager)
	manager.Select(b)
	waitManagerEvent(t, manager)
	manager.SyncHost("host", base.InstanceID, false, nil)
	waitOutputStatus(t, manager, func(status OutputPoolStatus) bool { return status.Count == 2 && status.Connected == 0 })
	reconnecting.Store(true)
	manager.SyncHost("host", base.InstanceID, true, []TerminalSelection{a, b})
	select {
	case <-restoreA:
	case <-time.After(time.Second):
		t.Fatal("host restore did not begin")
	}
	requestID := manager.Select(b)
	close(releaseA)
	waitOutputStatus(t, manager, func(status OutputPoolStatus) bool {
		return status.Count == 2 && status.Connected == 2 && status.Active != nil && status.Active.SessionID == b.SessionID
	})
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-manager.Events():
			if event.RequestID == requestID && event.Err == nil && event.Key.SessionID == b.SessionID {
				return
			}
		case <-deadline:
			t.Fatal("selection did not complete after concurrent host restore")
		}
	}
}

func TestTerminalOutputManagerRestoreContinuesAfterOneOpenFailure(t *testing.T) {
	root := t.TempDir()
	var reconnecting atomic.Bool
	manager, err := newTerminalOutputManager(context.Background(), 2, unusedTerminalOutputSource{}, SnapshotStore{Root: root},
		func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
			if reconnecting.Load() && key.SessionID == "AAA111" {
				return nil, errors.New("injected restore failure")
			}
			return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID, RuntimeGeneration: revision.RuntimeGeneration}, newFakePooledOutput(),
				PooledTerminalOptions{ExpectedKey: key, ExpectedRevision: revision, Rows: selection.Rows, Cols: selection.Cols, Scrollback: 4, Store: SnapshotStore{Root: root}})
		})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	base := TerminalSelection{Client: Client{Name: "host"}, InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", RuntimeGeneration: 1, Rows: 2, Cols: 20}
	a, b := base, base
	a.SessionID, b.SessionID = "AAA111", "BBB222"
	manager.Select(a)
	waitManagerEvent(t, manager)
	manager.Select(b)
	waitManagerEvent(t, manager)
	manager.SyncHost("host", base.InstanceID, false, nil)
	waitOutputStatus(t, manager, func(status OutputPoolStatus) bool { return status.Connected == 0 })
	reconnecting.Store(true)
	manager.SyncHost("host", base.InstanceID, true, []TerminalSelection{a, b})
	waitOutputStatus(t, manager, func(status OutputPoolStatus) bool {
		return status.Count == 2 && status.Connected == 1 && status.Active != nil && status.Active.SessionID == b.SessionID
	})
	status := manager.Status()
	if len(status.Desired) != 2 || status.Desired[0].SessionID != a.SessionID || status.Desired[1].SessionID != b.SessionID {
		t.Fatalf("restore failure changed desired order: %+v", status)
	}
}

func TestTerminalOutputManagerCloseIsConcurrentAndClosesEvents(t *testing.T) {
	manager, err := newTerminalOutputManager(context.Background(), 1, unusedTerminalOutputSource{}, SnapshotStore{Root: t.TempDir()},
		func(context.Context, TerminalSelection, OutputKey, OutputRevision, *TerminalRenderState) (*PooledTerminal, error) {
			return nil, errors.New("unused")
		})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- manager.Close() }()
	go func() { done <- manager.Close() }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := <-manager.Events(); ok {
		t.Fatal("events remained open after Close")
	}
}

func waitManagerEvent(t *testing.T, manager *TerminalOutputManager) TerminalOutputEvent {
	t.Helper()
	select {
	case event := <-manager.Events():
		if event.Err != nil {
			t.Fatalf("manager event error: %v", event.Err)
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("manager event timed out")
		return TerminalOutputEvent{}
	}
}

func waitOutputStatus(t *testing.T, manager *TerminalOutputManager, ready func(OutputPoolStatus) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ready(manager.Status()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("manager status timed out: %+v", manager.Status())
}
