package ducklord

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
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
