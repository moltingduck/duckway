package ducklord

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/daemon"
)

type pooledReadResult struct {
	frame OutputFrame
	err   error
}
type fakePooledOutput struct {
	results    chan pooledReadResult
	closed     chan struct{}
	once       sync.Once
	closeCount atomic.Int32
}

func newFakePooledOutput() *fakePooledOutput {
	return &fakePooledOutput{results: make(chan pooledReadResult, 8), closed: make(chan struct{})}
}
func (f *fakePooledOutput) ReadContext(ctx context.Context) (OutputFrame, error) {
	select {
	case r := <-f.results:
		return r.frame, r.err
	case <-f.closed:
		return OutputFrame{}, io.ErrClosedPipe
	case <-ctx.Done():
		return OutputFrame{}, ctx.Err()
	}
}
func (f *fakePooledOutput) Close() error {
	f.once.Do(func() { f.closeCount.Add(1); close(f.closed) })
	return nil
}

func pooledMetadata(end uint64) OutputStreamMetadata {
	return OutputStreamMetadata{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "ABC123", RuntimeGeneration: 2, ReplayEndOffset: end}
}
func pooledOptions(root string) PooledTerminalOptions {
	_ = os.Chmod(root, 0700)
	return PooledTerminalOptions{ExpectedKey: OutputKey{ClientKey: "host", InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "ABC123"}, ExpectedRevision: OutputRevision{RuntimeGeneration: 2}, Rows: 2, Cols: 20, Scrollback: 4, Store: SnapshotStore{Root: root}}
}

func TestPooledTerminalValidatesIdentityAndDimensionsBeforeReading(t *testing.T) {
	for _, mutate := range []func(*PooledTerminalOptions){
		func(o *PooledTerminalOptions) { o.ExpectedKey.SessionID = "DEF456" },
		func(o *PooledTerminalOptions) { o.ExpectedRevision.RuntimeGeneration = 3 },
		func(o *PooledTerminalOptions) { o.Rows = MaxPooledTerminalRows + 1 },
		func(o *PooledTerminalOptions) { o.Cols = MaxPooledTerminalCols + 1 },
		func(o *PooledTerminalOptions) { o.Scrollback = DefaultTerminalScrollback + 1 },
	} {
		reader := newFakePooledOutput()
		options := pooledOptions(t.TempDir())
		mutate(&options)
		if terminal, err := newPooledTerminal(context.Background(), pooledMetadata(0), reader, options); terminal != nil || err == nil {
			t.Fatalf("terminal=%v err=%v", terminal, err)
		}
		if reader.closeCount.Load() != 1 {
			t.Fatalf("invalid input close count=%d", reader.closeCount.Load())
		}
	}
}

func TestPooledTerminalWaitsForReplayAndPublishesImmutableView(t *testing.T) {
	reader := newFakePooledOutput()
	result := make(chan *PooledTerminal, 1)
	errs := make(chan error, 1)
	go func() {
		p, err := newPooledTerminal(context.Background(), pooledMetadata(6), reader, pooledOptions(t.TempDir()))
		result <- p
		errs <- err
	}()
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("abc"), StartOffset: 0, EndOffset: 3}}
	select {
	case <-result:
		t.Fatal("ready before replay barrier")
	default:
	}
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("def"), StartOffset: 3, EndOffset: 6}}
	p := <-result
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	view := p.View()
	if !view.Ready || view.OutputOffset != 6 || view.Revision < 2 {
		t.Fatalf("view=%+v", view)
	}
	view.Framebuffer.Primary.Lines[0].Cells[0].Rune = 'X'
	if p.View().Framebuffer.Primary.Lines[0].Cells[0].Rune == 'X' {
		t.Fatal("View exposed mutable cells")
	}
}

func TestPooledTerminalDirtySignalCoalescesWithoutBlockingReader(t *testing.T) {
	reader := newFakePooledOutput()
	p, err := newPooledTerminal(context.Background(), pooledMetadata(0), reader, pooledOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for offset := uint64(0); offset < 8; offset++ {
		reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("x"), StartOffset: offset, EndOffset: offset + 1}}
	}
	waitPooledOffset(t, p, 8)
	if len(p.Updates()) != 1 {
		t.Fatalf("dirty signals=%d, want one coalesced notification", len(p.Updates()))
	}
	<-p.Updates()
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("y"), StartOffset: 8, EndOffset: 9}}
	waitPooledOffset(t, p, 9)
	select {
	case <-p.Updates():
	case <-time.After(time.Second):
		t.Fatal("new frame did not publish a dirty signal")
	}
}

func TestOutputPoolTerminalViewIsLeaseFenced(t *testing.T) {
	pool, _ := NewOutputPool(1)
	defer pool.Close()
	reader := newFakePooledOutput()
	options := pooledOptions(t.TempDir())
	activation, err := pool.Activate(context.Background(), options.ExpectedKey, options.ExpectedRevision, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return newPooledTerminal(context.Background(), pooledMetadata(0), reader, options)
	})
	if err != nil {
		t.Fatal(err)
	}
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("safe"), StartOffset: 0, EndOffset: 4}}
	waitPooledOffset(t, mustPooledResource(t, pool, options.ExpectedKey, activation.Lease), 4)
	view, err := pool.TerminalView(options.ExpectedKey, activation.Lease)
	if err != nil || view.OutputOffset != 4 {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if _, err := pool.TerminalView(options.ExpectedKey, activation.Lease+1); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("stale view error=%v", err)
	}
}

func TestOutputPoolResizeAppliesAtExactOutputBarrier(t *testing.T) {
	pool, _ := NewOutputPool(1)
	defer pool.Close()
	reader := newFakePooledOutput()
	options := pooledOptions(t.TempDir())
	activation, err := pool.Activate(context.Background(), options.ExpectedKey, options.ExpectedRevision, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return newPooledTerminal(context.Background(), pooledMetadata(0), reader, options)
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal := mustPooledResource(t, pool, options.ExpectedKey, activation.Lease)
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("abc"), StartOffset: 0, EndOffset: 3}}
	waitPooledOffset(t, terminal, 3)
	barrier, err := pool.ResizeTerminalAt(options.ExpectedKey, activation.Lease, 3, 10, func(rows, cols uint16) (uint64, error) {
		if rows != 3 || cols != 10 {
			t.Fatalf("remote resize=%dx%d", rows, cols)
		}
		return 5, nil
	})
	if err != nil || barrier != 5 {
		t.Fatalf("barrier=%d err=%v", barrier, err)
	}
	if _, err := pool.ResizeTerminalAt(options.ExpectedKey, activation.Lease, 4, 12, func(uint16, uint16) (uint64, error) { return 6, nil }); err != nil {
		t.Fatal(err)
	}
	terminal.mu.Lock()
	pendingCount := len(terminal.pendingResizes)
	terminal.mu.Unlock()
	if pendingCount != 2 {
		t.Fatalf("pending resize barriers=%d, want 2", pendingCount)
	}
	if err := terminal.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := options.Store.Load(options.ExpectedKey.InstanceID, options.ExpectedKey.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := DecodeTerminalRenderState(snapshot.Payload)
	if err != nil || saved.ResumeCursorValid {
		t.Fatalf("snapshot with pending resize claimed exact resume: state=%+v err=%v", saved, err)
	}
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("defg"), StartOffset: 3, EndOffset: 7}}
	waitPooledOffset(t, terminal, 7)
	view, err := pool.TerminalView(options.ExpectedKey, activation.Lease)
	if err != nil || view.Framebuffer.Rows != 4 || view.Framebuffer.Cols != 12 {
		t.Fatalf("resized view=%+v err=%v", view, err)
	}
}

func mustPooledResource(t *testing.T, pool *OutputPool, key OutputKey, lease uint64) *PooledTerminal {
	t.Helper()
	var terminal *PooledTerminal
	if err := pool.WithLease(key, lease, func(resource OutputResource) { terminal, _ = resource.(*PooledTerminal) }); err != nil || terminal == nil {
		t.Fatalf("pooled resource unavailable: %v", err)
	}
	return terminal
}

func TestPooledTerminalReplayEOFFailsAndCloses(t *testing.T) {
	reader := newFakePooledOutput()
	reader.results <- pooledReadResult{err: io.EOF}
	p, err := newPooledTerminal(context.Background(), pooledMetadata(1), reader, pooledOptions(t.TempDir()))
	if p != nil || err == nil || !errors.Is(err, io.EOF) || reader.closeCount.Load() != 1 {
		t.Fatalf("terminal=%v err=%v closes=%d", p, err, reader.closeCount.Load())
	}
}

func TestPooledTerminalPoolLeaseRejectsPostSnapshotFrame(t *testing.T) {
	root := t.TempDir()
	pool, _ := NewOutputPool(1)
	readerA := newFakePooledOutput()
	optionsA := pooledOptions(root)
	var terminalA *PooledTerminal
	activation, err := pool.Activate(context.Background(), optionsA.ExpectedKey, optionsA.ExpectedRevision, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		var openErr error
		terminalA, openErr = newPooledTerminal(context.Background(), pooledMetadata(0), readerA, optionsA)
		return terminalA, openErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if activation.Lease == 0 {
		t.Fatal("missing lease")
	}
	readerA.results <- pooledReadResult{frame: OutputFrame{Data: []byte("before"), StartOffset: 0, EndOffset: 6}}
	waitPooledOffset(t, terminalA, 6)
	entered, release := make(chan struct{}), make(chan struct{})
	terminalA.snapshotHook = func() { close(entered); <-release }
	done := make(chan error, 1)
	optionsB := pooledOptions(t.TempDir())
	optionsB.ExpectedKey.SessionID = "DEF456"
	metadataB := pooledMetadata(0)
	metadataB.SessionID = "DEF456"
	go func() {
		_, activateErr := pool.Activate(context.Background(), optionsB.ExpectedKey, optionsB.ExpectedRevision, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
			return newPooledTerminal(context.Background(), metadataB, newFakePooledOutput(), optionsB)
		})
		done <- activateErr
	}()
	<-entered
	readerA.results <- pooledReadResult{frame: OutputFrame{Data: []byte("lost"), StartOffset: 6, EndOffset: 10}}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := terminalA.View().OutputOffset; got != 6 {
		t.Fatalf("post-snapshot frame was accepted at offset %d", got)
	}
	saved, err := optionsA.Store.Load(optionsA.ExpectedKey.InstanceID, optionsA.ExpectedKey.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := DecodeTerminalRenderState(saved.Payload)
	if err != nil || state.OutputOffset != 6 {
		t.Fatalf("saved offset=%d err=%v", state.OutputOffset, err)
	}
}

func waitPooledOffset(t *testing.T, p *PooledTerminal, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if p.View().OutputOffset == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("offset=%d want=%d", p.View().OutputOffset, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPooledTerminalRestoresExactFramebuffer(t *testing.T) {
	base := NewTerminal(2, 20, 4)
	base.Write([]byte("previous"))
	fb := base.SnapshotState()
	initial := TerminalRenderState{Framebuffer: &fb, RuntimeGeneration: 2, OutputOffset: 8, ResumeCursorValid: true}
	metadata := pooledMetadata(8)
	metadata.StartOffset = 8
	metadata.ExactResume = true
	options := pooledOptions(t.TempDir())
	options.Initial = &initial
	reader := newFakePooledOutput()
	p, err := newPooledTerminal(context.Background(), metadata, reader, options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	restored, ok := NewTerminalFromState(p.View().Framebuffer, 4)
	if !ok || !strings.Contains(restored.Text(), "previous") {
		t.Fatalf("restored=%v ok=%v", restored, ok)
	}
}

func TestPooledTerminalSerializesSnapshotsWithoutOffsetRegression(t *testing.T) {
	root := t.TempDir()
	pool, _ := NewOutputPool(1)
	reader := newFakePooledOutput()
	options := pooledOptions(root)
	var terminal *PooledTerminal
	if _, err := pool.Activate(context.Background(), options.ExpectedKey, options.ExpectedRevision, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		var openErr error
		terminal, openErr = newPooledTerminal(context.Background(), pooledMetadata(0), reader, options)
		return terminal, openErr
	}); err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("first"), StartOffset: 0, EndOffset: 5}}
	waitPooledOffset(t, terminal, 5)
	entered, release := make(chan struct{}), make(chan struct{})
	var hookCalls atomic.Int32
	terminal.snapshotHook = func() {
		if hookCalls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- terminal.Snapshot(context.Background()) }()
	<-entered
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("second"), StartOffset: 5, EndOffset: 11}}
	waitPooledOffset(t, terminal, 11)
	secondDone := make(chan error, 1)
	go func() { secondDone <- terminal.Snapshot(context.Background()) }()
	select {
	case err := <-secondDone:
		t.Fatalf("newer snapshot bypassed serialization: %v", err)
	default:
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	saved, err := options.Store.Load(options.ExpectedKey.InstanceID, options.ExpectedKey.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := DecodeTerminalRenderState(saved.Payload)
	if err != nil || state.OutputOffset != 11 {
		t.Fatalf("snapshot regressed to offset %d err=%v", state.OutputOffset, err)
	}
}

func TestPooledTerminalCleanEndDetachesLeaseWithoutError(t *testing.T) {
	pool, _ := NewOutputPool(1)
	reader := newFakePooledOutput()
	options := pooledOptions(t.TempDir())
	var terminal *PooledTerminal
	if _, err := pool.Activate(context.Background(), options.ExpectedKey, options.ExpectedRevision, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		var openErr error
		terminal, openErr = newPooledTerminal(context.Background(), pooledMetadata(0), reader, options)
		return terminal, openErr
	}); err != nil {
		t.Fatal(err)
	}
	reader.results <- pooledReadResult{err: &daemon.OutputStreamEnded{Reason: "runtime_stopped", NextOffset: 0}}
	deadline := time.Now().Add(time.Second)
	for {
		view, status := terminal.View(), pool.Status()
		if view.Ended && !view.Disconnected && view.Error == "" && status.Count == 1 && status.Connected == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("view=%+v status=%+v", view, status)
		}
		time.Sleep(time.Millisecond)
	}
	if reader.closeCount.Load() != 1 {
		t.Fatalf("clean end close count=%d", reader.closeCount.Load())
	}
}

func TestPooledTerminalBoundGapDisconnectsWithoutLeakingErrorText(t *testing.T) {
	pool, _ := NewOutputPool(1)
	reader := newFakePooledOutput()
	options := pooledOptions(t.TempDir())
	var terminal *PooledTerminal
	if _, err := pool.Activate(context.Background(), options.ExpectedKey, options.ExpectedRevision, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		var openErr error
		terminal, openErr = newPooledTerminal(context.Background(), pooledMetadata(0), reader, options)
		return terminal, openErr
	}); err != nil {
		t.Fatal(err)
	}
	reader.results <- pooledReadResult{frame: OutputFrame{Data: []byte("secret-canary"), StartOffset: 9, EndOffset: 22}}
	deadline := time.Now().Add(time.Second)
	for {
		view, status := terminal.View(), pool.Status()
		if view.Disconnected && status.Connected == 0 {
			if strings.Contains(view.Error, "secret-canary") || view.OutputOffset != 0 {
				t.Fatalf("gap leaked/applied data: %+v", view)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("view=%+v status=%+v", view, status)
		}
		time.Sleep(time.Millisecond)
	}
}
