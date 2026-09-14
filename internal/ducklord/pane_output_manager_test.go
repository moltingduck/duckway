package ducklord

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func newTestPaneOutputManager(t *testing.T, capacity int) (*PaneOutputManager, map[string]*fakePooledOutput, *sync.Mutex) {
	t.Helper()
	store := SnapshotStore{Root: t.TempDir()}
	if err := os.Chmod(store.Root, 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewPaneOutputManager(context.Background(), capacity, unusedTerminalOutputSource{}, store)
	if err != nil {
		t.Fatal(err)
	}
	readers := make(map[string]*fakePooledOutput)
	var mu sync.Mutex
	manager.opener = func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
		reader := newFakePooledOutput()
		mu.Lock()
		readers[key.SessionID] = reader
		mu.Unlock()
		return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID,
			RuntimeGeneration: revision.RuntimeGeneration}, reader, PooledTerminalOptions{ExpectedKey: key,
			ExpectedRevision: revision, Rows: selection.Rows, Cols: selection.Cols, Scrollback: 8, Store: store})
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, readers, &mu
}

func paneSelection(sessionID string) TerminalSelection {
	return TerminalSelection{Client: Client{Name: "host"}, InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971",
		SessionID: sessionID, RuntimeGeneration: 1, Rows: 3, Cols: 40}
}

func TestPaneOutputManagerDeduplicatesViewsAndPreservesBothUpdates(t *testing.T) {
	manager, readers, readersMu := newTestPaneOutputManager(t, 3)
	a, b := paneSelection("AAA111"), paneSelection("BBB222")
	overflow, err := manager.SetVisible(context.Background(), a, []TerminalSelection{a, b, a})
	if err != nil || len(overflow) != 0 {
		t.Fatalf("visible activation: overflow=%v err=%v", overflow, err)
	}
	if got := manager.pool.Status(); got.Count != 2 || got.Active == nil || *got.Active != a.key() {
		t.Fatalf("duplicate pane opened another subscription or lost focus: %+v", got)
	}
	manager.DrainDirty()
	readersMu.Lock()
	ra, rb := readers[a.SessionID], readers[b.SessionID]
	readersMu.Unlock()
	ra.results <- pooledReadResult{frame: OutputFrame{Data: []byte("A"), StartOffset: 0, EndOffset: 1}}
	rb.results <- pooledReadResult{frame: OutputFrame{Data: []byte("B"), StartOffset: 0, EndOffset: 1}}
	deadline := time.After(2 * time.Second)
	seen := make(map[OutputKey]bool)
	for len(seen) < 2 {
		select {
		case <-manager.DirtyReady():
			for _, key := range manager.DrainDirty() {
				seen[key] = true
			}
		case <-deadline:
			t.Fatalf("one pane starved another: %v", seen)
		}
	}
	for _, selection := range []TerminalSelection{a, b} {
		view, err := manager.View(selection.key())
		if err != nil || !view.Ready || view.OutputOffset != 1 {
			t.Fatalf("pane %s view=%+v err=%v", selection.SessionID, view, err)
		}
	}
}

func TestPaneOutputManagerOverflowAndStaleFocusAreExplicit(t *testing.T) {
	manager, _, _ := newTestPaneOutputManager(t, 1)
	a, b := paneSelection("AAA111"), paneSelection("BBB222")
	overflow, err := manager.SetVisible(context.Background(), a, []TerminalSelection{a, b})
	if err != nil || len(overflow) != 1 || overflow[0] != b.key() {
		t.Fatalf("overflow=%v err=%v", overflow, err)
	}
	if _, err := manager.View(b.key()); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("overflow pane appeared live: %v", err)
	}
	if _, err := manager.ResizeFocused(a.key(), 4, 50, func(uint16, uint16) (uint64, error) { return 0, nil }); err == nil {
		t.Fatal("quick-list preview was allowed to resize")
	}
	if _, err := manager.ResizeFocused(b.key(), 4, 50, func(uint16, uint16) (uint64, error) { return 0, nil }); err == nil {
		t.Fatal("unfocused overflow pane drove PTY resize")
	}
	if err := manager.SetInputFocus(a.key()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetVisible(context.Background(), b, []TerminalSelection{a, b}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ResizeFocused(a.key(), 4, 50, func(uint16, uint16) (uint64, error) { return 0, nil }); err == nil {
		t.Fatal("old focus drove PTY resize")
	}
	if _, err := manager.View(a.key()); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("evicted pane appeared live: %v", err)
	}
	if err := manager.SetInputFocus(b.key()); err != nil {
		t.Fatal(err)
	}
	manager.ClearInputFocus()
	if _, err := manager.ResizeFocused(b.key(), 4, 50, func(uint16, uint16) (uint64, error) { return 0, nil }); err == nil {
		t.Fatal("cleared input focus still resized")
	}
}

func TestPaneOutputManagerFocusChangeWaitsForRemoteResize(t *testing.T) {
	manager, _, _ := newTestPaneOutputManager(t, 2)
	a, b := paneSelection("AAA111"), paneSelection("BBB222")
	if _, err := manager.SetVisible(context.Background(), a, []TerminalSelection{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetInputFocus(a.key()); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	resized := make(chan error, 1)
	go func() {
		_, err := manager.ResizeFocused(a.key(), 4, 50, func(uint16, uint16) (uint64, error) {
			close(started)
			<-release
			return 0, nil
		})
		resized <- err
	}()
	<-started
	focused := make(chan error, 1)
	go func() { focused <- manager.SetInputFocus(b.key()) }()
	select {
	case err := <-focused:
		t.Fatalf("focus switched before resize completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-resized; err != nil {
		t.Fatal(err)
	}
	if err := <-focused; err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ResizeFocused(a.key(), 4, 50, func(uint16, uint16) (uint64, error) {
		t.Fatal("old pane reached remote resize")
		return 0, nil
	}); err == nil {
		t.Fatal("old pane retained resize focus")
	}
}

func TestPaneOutputManagerNewVisibleRequestCancelsSlowPreviousOpen(t *testing.T) {
	manager, _, _ := newTestPaneOutputManager(t, 2)
	a, b := paneSelection("AAA111"), paneSelection("BBB222")
	started := make(chan struct{})
	manager.opener = func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
		if selection.SessionID == a.SessionID {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID,
			RuntimeGeneration: revision.RuntimeGeneration}, newFakePooledOutput(), PooledTerminalOptions{ExpectedKey: key,
			ExpectedRevision: revision, Rows: selection.Rows, Cols: selection.Cols, Scrollback: 8, Store: manager.store})
	}
	first := make(chan error, 1)
	go func() {
		_, err := manager.SetVisible(context.Background(), a, nil)
		first <- err
	}()
	<-started
	second := make(chan error, 1)
	go func() {
		_, err := manager.SetVisible(context.Background(), b, nil)
		second <- err
	}()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("superseded open error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("superseded open was not canceled")
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("new visible pane waited for stale host")
	}
	if view, err := manager.View(b.key()); err != nil || !view.Ready {
		t.Fatalf("new pane view=%+v err=%v", view, err)
	}
}

func TestPaneOutputManagerOtherHostDisconnectDoesNotCancelVisibleOpen(t *testing.T) {
	manager, _, _ := newTestPaneOutputManager(t, 2)
	b := paneSelection("BBB222")
	b.Client.Name = "host-b"
	started, release := make(chan struct{}), make(chan struct{})
	manager.opener = func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID,
			RuntimeGeneration: revision.RuntimeGeneration}, newFakePooledOutput(), PooledTerminalOptions{ExpectedKey: key,
			ExpectedRevision: revision, Rows: selection.Rows, Cols: selection.Cols, Scrollback: 8, Store: manager.store})
	}
	opened := make(chan error, 1)
	go func() { _, err := manager.SetVisible(context.Background(), b, nil); opened <- err }()
	<-started
	disconnected := make(chan error, 1)
	go func() { disconnected <- manager.DisconnectHost("host-a", b.InstanceID) }()
	close(release)
	if err := <-opened; err != nil {
		t.Fatalf("unrelated host canceled visible open: %v", err)
	}
	if err := <-disconnected; err != nil {
		t.Fatal(err)
	}
	if view, err := manager.View(b.key()); err != nil || !view.Ready {
		t.Fatalf("other host lost live pane: view=%+v err=%v", view, err)
	}
}

func TestPaneOutputManagerHostDisconnectAndReplacementFenceLeases(t *testing.T) {
	manager, readers, readersMu := newTestPaneOutputManager(t, 2)
	a := paneSelection("AAA111")
	if _, err := manager.SetVisible(context.Background(), a, nil); err != nil {
		t.Fatal(err)
	}
	readersMu.Lock()
	first := readers[a.SessionID]
	readersMu.Unlock()
	if err := manager.SetInputFocus(a.key()); err != nil {
		t.Fatal(err)
	}
	if err := manager.DisconnectHost(a.Client.Name, a.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.View(a.key()); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("disconnected host appeared live: %v", err)
	}
	if _, err := manager.ResizeFocused(a.key(), 4, 50, func(uint16, uint16) (uint64, error) {
		t.Fatal("disconnected host reached remote resize")
		return 0, nil
	}); err == nil {
		t.Fatal("disconnected host retained input focus")
	}
	if _, err := manager.SetVisible(context.Background(), a, nil); err != nil {
		t.Fatalf("same-instance replay reconnect failed: %v", err)
	}
	readersMu.Lock()
	second := readers[a.SessionID]
	readersMu.Unlock()
	if second == first {
		t.Fatal("same-instance reconnect reused old raw-output stream")
	}
	if _, err := manager.View(a.key()); err != nil {
		t.Fatalf("reconnected host has no live view: %v", err)
	}
	if err := manager.ForgetHost(a.Client.Name, a.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetVisible(context.Background(), a, nil); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("retired instance was revived: %v", err)
	}
	b := a
	b.InstanceID = "39d1d165-a1c1-4db8-9354-4b380c4812a0"
	if _, err := manager.SetVisible(context.Background(), b, nil); err != nil {
		t.Fatalf("replacement instance was blocked: %v", err)
	}
	manager.ClearVisible()
	if _, err := manager.View(b.key()); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("cleared pane was still presented: %v", err)
	}
}

func TestPaneOutputManagerFailedHandoffPreservesPreviousView(t *testing.T) {
	manager, _, _ := newTestPaneOutputManager(t, 1)
	a, b := paneSelection("AAA111"), paneSelection("BBB222")
	if _, err := manager.SetVisible(context.Background(), a, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetInputFocus(a.key()); err != nil {
		t.Fatal(err)
	}
	manager.opener = func(_ context.Context, selection TerminalSelection, _ OutputKey, _ OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
		if selection.SessionID == b.SessionID {
			return nil, errors.New("remote open failed")
		}
		return nil, errors.New("unexpected reopen")
	}
	if _, err := manager.SetVisible(context.Background(), b, nil); err == nil {
		t.Fatal("failed handoff reported success")
	}
	if _, err := manager.View(a.key()); err != nil {
		t.Fatalf("failed handoff hid prior live view: %v", err)
	}
	if _, err := manager.View(b.key()); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("failed destination appeared live: %v", err)
	}
	if manager.inputFocus != a.key() {
		t.Fatal("failed handoff stole input focus")
	}
}

func TestPaneOutputManagerCloseCancelsBlockedOpen(t *testing.T) {
	manager, err := NewPaneOutputManager(context.Background(), 1, unusedTerminalOutputSource{}, SnapshotStore{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	manager.opener = func(ctx context.Context, _ TerminalSelection, _ OutputKey, _ OutputRevision, _ *TerminalRenderState) (*PooledTerminal, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		_, _ = manager.SetVisible(context.Background(), paneSelection("AAA111"), nil)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream open never began")
	}
	closed := make(chan struct{})
	go func() { _ = manager.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close hung behind blocked stream open")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SetVisible did not exit after Close")
	}
}
