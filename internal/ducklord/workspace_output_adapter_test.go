package ducklord

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/daemon"
)

func TestWorkspaceOutputAdapterTwoLivePanesOnePool(t *testing.T) {
	store := SnapshotStore{Root: t.TempDir()}
	if err := os.Chmod(store.Root, 0700); err != nil {
		t.Fatal(err)
	}
	a, b := paneSelection("AAA111"), paneSelection("BBB222")
	adapter, err := NewWorkspaceOutputAdapter(context.Background(), 2, unusedTerminalOutputSource{}, store,
		func(TerminalSelection) []TerminalSelection { return []TerminalSelection{a, b, a} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	readers := map[string]*fakePooledOutput{}
	var mu sync.Mutex
	adapter.pane.opener = func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision,
		_ *TerminalRenderState) (*PooledTerminal, error) {
		reader := newFakePooledOutput()
		mu.Lock()
		readers[key.SessionID] = reader
		mu.Unlock()
		return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID,
			RuntimeGeneration: revision.RuntimeGeneration}, reader, PooledTerminalOptions{ExpectedKey: key,
			ExpectedRevision: revision, Rows: selection.Rows, Cols: selection.Cols, Scrollback: 8, Store: store})
	}
	id := adapter.Select(a)
	var selected TerminalOutputEvent
	select {
	case selected = <-adapter.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("selected pane did not activate")
	}
	if selected.RequestID != id || selected.Key != a.key() || selected.Lease == 0 || selected.Err != nil {
		t.Fatalf("selected event = %+v", selected)
	}
	if status := adapter.pane.pool.Status(); status.Count != 2 {
		t.Fatalf("duplicate Session pane opened extra stream: %+v", status)
	}
	mu.Lock()
	ra, rb := readers[a.SessionID], readers[b.SessionID]
	mu.Unlock()
	ra.results <- pooledReadResult{frame: OutputFrame{Data: []byte("A-live"), StartOffset: 0, EndOffset: 6}}
	rb.results <- pooledReadResult{frame: OutputFrame{Data: []byte("B-live"), StartOffset: 0, EndOffset: 6}}
	deadline := time.After(2 * time.Second)
	for {
		va, ea := adapter.View(selected)
		vb, eb := adapter.PaneView(b.key())
		if ea == nil && eb == nil && va.OutputOffset == 6 && vb.OutputOffset == 6 {
			break
		}
		select {
		case <-adapter.RepaintReady():
		case <-adapter.Events():
		case <-deadline:
			t.Fatalf("panes did not update together: A=%+v/%v B=%+v/%v", va, ea, vb, eb)
		}
	}
	if _, err := adapter.Resize(selected, 4, 50, func(uint16, uint16) (uint64, error) {
		t.Fatal("unfocused pane reached PTY resize")
		return 0, nil
	}); err == nil {
		t.Fatal("output activation alone granted resize")
	}
	if err := adapter.SetInputFocus(a.key()); err != nil {
		t.Fatal(err)
	}
	adapter.ClearInputFocus()
	if _, err := adapter.Resize(selected, 4, 50, func(uint16, uint16) (uint64, error) {
		t.Fatal("cleared focus reached PTY resize")
		return 0, nil
	}); err == nil {
		t.Fatal("cleared focus still resized")
	}
}

func TestWorkspaceOutputAdapterReconnectNeverLabelsOldLeaseFresh(t *testing.T) {
	store := SnapshotStore{Root: t.TempDir()}
	if err := os.Chmod(store.Root, 0700); err != nil {
		t.Fatal(err)
	}
	a, b := paneSelection("AAA111"), paneSelection("BBB222")
	adapter, err := NewWorkspaceOutputAdapter(context.Background(), 2, unusedTerminalOutputSource{}, store,
		func(TerminalSelection) []TerminalSelection { return []TerminalSelection{a, b} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	var mu sync.Mutex
	var firstA *fakePooledOutput
	openA := 0
	reconnectStarted, releaseReconnect := make(chan struct{}), make(chan struct{})
	adapter.pane.opener = func(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision,
		_ *TerminalRenderState) (*PooledTerminal, error) {
		if key == a.key() {
			mu.Lock()
			openA++
			count := openA
			mu.Unlock()
			if count == 2 {
				close(reconnectStarted)
				select {
				case <-releaseReconnect:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		reader := newFakePooledOutput()
		if key == a.key() {
			mu.Lock()
			if firstA == nil {
				firstA = reader
			}
			mu.Unlock()
		}
		return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID,
			RuntimeGeneration: revision.RuntimeGeneration}, reader, PooledTerminalOptions{ExpectedKey: key,
			ExpectedRevision: revision, Rows: selection.Rows, Cols: selection.Cols, Scrollback: 8, Store: store})
	}
	firstID := adapter.Select(a)
	var first TerminalOutputEvent
	select {
	case first = <-adapter.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("initial selected stream missing")
	}
	if first.RequestID != firstID || first.Lease == 0 || first.Err != nil {
		t.Fatalf("initial event = %+v", first)
	}
	reconnectID := adapter.Reconnect(a)
	select {
	case <-reconnectStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect did not start")
	}
	mu.Lock()
	oldReader := firstA
	mu.Unlock()
	oldReader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("old"), StartOffset: 0, EndOffset: 3}}
	select {
	case event := <-adapter.Events():
		if event.RequestID == reconnectID && event.Lease == first.Lease && event.Err == nil {
			t.Fatalf("old lease was mislabeled as reconnected: %+v", event)
		}
	case <-time.After(50 * time.Millisecond):
	}
	selectedB := adapter.Select(b)
	close(releaseReconnect)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-adapter.Events():
			if event.RequestID == reconnectID && event.Err == nil {
				t.Fatalf("superseded reconnect published success: %+v", event)
			}
			if event.RequestID == selectedB && event.Key == b.key() && event.Err == nil {
				if _, err := adapter.PaneView(b.key()); err != nil {
					t.Fatalf("new selection lost lease: %v", err)
				}
				return
			}
		case <-deadline:
			t.Fatal("superseding selection did not become live")
		}
	}
}

func TestWorkspaceOutputAdapterPublishesFinalViewAfterDetachedLease(t *testing.T) {
	for _, tc := range []struct {
		name                string
		err                 error
		ended, disconnected bool
	}{
		{name: "clean end", err: &daemon.OutputStreamEnded{Reason: "runtime_stopped", NextOffset: 5}, ended: true},
		{name: "disconnect", err: io.EOF, disconnected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := SnapshotStore{Root: t.TempDir()}
			selection := paneSelection("AAA111")
			adapter, err := NewWorkspaceOutputAdapter(context.Background(), 1, unusedTerminalOutputSource{}, store,
				func(TerminalSelection) []TerminalSelection { return []TerminalSelection{selection} })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = adapter.Close() })
			reader := newFakePooledOutput()
			var opens int
			adapter.pane.opener = func(ctx context.Context, _ TerminalSelection, key OutputKey, revision OutputRevision,
				_ *TerminalRenderState) (*PooledTerminal, error) {
				opens++
				currentReader := reader
				if opens > 1 {
					currentReader = newFakePooledOutput()
				}
				return newPooledTerminal(ctx, OutputStreamMetadata{InstanceID: key.InstanceID, SessionID: key.SessionID,
					RuntimeGeneration: revision.RuntimeGeneration}, currentReader, PooledTerminalOptions{ExpectedKey: key,
					ExpectedRevision: revision, Rows: 3, Cols: 40, Scrollback: 8, Store: store})
			}
			id := adapter.Select(selection)
			select {
			case event := <-adapter.Events():
				if event.RequestID != id || event.Lease == 0 || event.Err != nil {
					t.Fatalf("activation = %+v", event)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("activation event missing")
			}
			reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("final"), StartOffset: 0, EndOffset: 5}}
			reader.results <- pooledReadResult{err: tc.err}
			deadline := time.After(2 * time.Second)
			for {
				select {
				case event := <-adapter.Events():
					if event.RequestID != id || event.FinalView == nil {
						continue
					}
					if event.FinalView.OutputOffset != 5 || event.FinalView.Ended != tc.ended || event.FinalView.Disconnected != tc.disconnected {
						t.Fatalf("final event = %+v", event)
					}
					if _, err := adapter.pane.pool.TerminalView(selection.key(), event.Lease); !errors.Is(err, ErrStaleOutputLease) {
						t.Fatalf("detached lease still live: %v", err)
					}
					reconnectID := adapter.Reconnect(selection)
					for {
						select {
						case next := <-adapter.Events():
							if next.RequestID != reconnectID {
								continue
							}
							if next.Err != nil || next.Lease == event.Lease || next.FinalView != nil {
								t.Fatalf("new lease inherited old final: %+v", next)
							}
							if _, err := adapter.pane.FinalViewLease(selection.key(), event.Lease); !errors.Is(err, ErrStaleOutputLease) {
								t.Fatalf("old final survived replacement: %v", err)
							}
							return
						case <-deadline:
							t.Fatal("reconnected lease missing")
						}
					}
				case <-deadline:
					t.Fatal("detached final framebuffer event missing")
				}
			}
		})
	}
}

// A fast producer must not enqueue a render per chunk. The final dirty key must
// still repaint after the burst ends, including when no pane is selected.
func TestWorkspaceOutputAdapterCoalescesFloodAndTrailingUpdate(t *testing.T) {
	adapter, err := NewWorkspaceOutputAdapter(context.Background(), 1, unusedTerminalOutputSource{}, SnapshotStore{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	key := paneSelection("FLOOD1").key()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				adapter.pane.markDirty(key)
			}
		}
	}()
	deadline := time.NewTimer(250 * time.Millisecond)
	defer deadline.Stop()
	count := 0
loop:
	for {
		select {
		case <-adapter.RepaintReady():
			count++
		case <-deadline.C:
			break loop
		}
	}
	close(stop)
	<-done
	if count == 0 || count > 10 {
		t.Fatalf("repaints during 250ms flood = %d", count)
	}
	adapter.pane.markDirty(key)
	select {
	case <-adapter.RepaintReady():
	case <-time.After(time.Second):
		t.Fatal("last update was lost")
	}
}
