package ducklord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/hackerduck/duckway/internal/ducklion/daemon"
)

const (
	MaxPooledTerminalRows = 200
	MaxPooledTerminalCols = 500
)

type OutputStreamMetadata struct {
	InstanceID        string
	SessionID         string
	RuntimeGeneration uint64
	StartOffset       uint64
	ReplayEndOffset   uint64
	ExactResume       bool
}

func (s *OutputStream) Metadata() OutputStreamMetadata {
	if s == nil {
		return OutputStreamMetadata{}
	}
	return OutputStreamMetadata{InstanceID: s.InstanceID, SessionID: s.SessionID, RuntimeGeneration: s.RuntimeGeneration,
		StartOffset: s.StartOffset, ReplayEndOffset: s.ReplayEndOffset, ExactResume: s.ExactResume}
}

type PooledTerminalOptions struct {
	ExpectedKey      OutputKey
	ExpectedRevision OutputRevision
	Rows             int
	Cols             int
	Scrollback       int
	Store            SnapshotStore
	Initial          *TerminalRenderState
}

type PooledTerminalView struct {
	Framebuffer       TerminalState
	Revision          uint64
	RuntimeGeneration uint64
	OutputOffset      uint64
	ReplayEndOffset   uint64
	Ready             bool
	Truncated         bool
	Disconnected      bool
	Ended             bool
	Error             string
}

type pooledOutputReader interface {
	ReadContext(context.Context) (OutputFrame, error)
	Close() error
}

// PooledTerminal owns a read-only stream and is bound atomically to its pool
// lease before it becomes visible. After binding, every framebuffer mutation
// passes through OutputPool.WithLease so eviction snapshots are lossless.
type PooledTerminal struct {
	mu           sync.Mutex
	snapshotMu   sync.Mutex
	stream       pooledOutputReader
	terminal     *Terminal
	store        SnapshotStore
	expected     OutputKey
	revision     OutputRevision
	offset       uint64
	replayEnd    uint64
	ready        bool
	truncated    bool
	contiguous   bool
	disconnect   bool
	readErr      error
	cleanEnd     bool
	viewRevision uint64
	pool         *OutputPool
	lease        uint64
	closing      bool
	cancel       context.CancelFunc
	done         chan struct{}
	readyCh      chan struct{}
	updates      chan struct{}
	readyOnce    sync.Once
	closeOnce    sync.Once
	closeErr     error
	snapshotHook func()
}

func NewPooledTerminal(ctx context.Context, stream *OutputStream, options PooledTerminalOptions) (*PooledTerminal, error) {
	if stream == nil || stream.subscription == nil {
		return nil, fmt.Errorf("PTY output stream is required")
	}
	return newPooledTerminal(ctx, stream.Metadata(), stream, options)
}

func newPooledTerminal(ctx context.Context, metadata OutputStreamMetadata, reader pooledOutputReader, options PooledTerminalOptions) (*PooledTerminal, error) {
	if reader == nil {
		return nil, fmt.Errorf("PTY output stream is required")
	}
	fail := func(err error) (*PooledTerminal, error) { return nil, errors.Join(err, reader.Close()) }
	if err := options.ExpectedKey.validate(); err != nil {
		return fail(err)
	}
	if err := options.ExpectedRevision.validate(); err != nil {
		return fail(err)
	}
	if metadata.InstanceID != options.ExpectedKey.InstanceID || metadata.SessionID != options.ExpectedKey.SessionID ||
		metadata.RuntimeGeneration != options.ExpectedRevision.RuntimeGeneration || metadata.ReplayEndOffset < metadata.StartOffset {
		return fail(fmt.Errorf("PTY output stream identity or revision mismatch"))
	}
	rows, cols, scrollback := options.Rows, options.Cols, options.Scrollback
	if rows == 0 {
		rows = 40
	}
	if cols == 0 {
		cols = 120
	}
	if scrollback == 0 {
		scrollback = DefaultTerminalScrollback
	}
	if rows < 1 || rows > MaxPooledTerminalRows || cols < 1 || cols > MaxPooledTerminalCols || scrollback < 0 || scrollback > DefaultTerminalScrollback {
		return fail(fmt.Errorf("PTY framebuffer dimensions are out of range"))
	}
	terminal := NewTerminal(rows, cols, scrollback)
	truncated := metadata.StartOffset > 0
	if initial := options.Initial; metadata.ExactResume && initial != nil && initial.Framebuffer != nil && initial.ResumeCursorValid &&
		initial.RuntimeGeneration == metadata.RuntimeGeneration && initial.OutputOffset == metadata.StartOffset {
		if restored, ok := NewTerminalFromState(*initial.Framebuffer, scrollback); ok {
			terminal, truncated = restored, initial.Truncated
		}
	}
	readCtx, cancel := context.WithCancel(context.Background())
	p := &PooledTerminal{stream: reader, terminal: terminal, store: options.Store, expected: options.ExpectedKey, revision: options.ExpectedRevision,
		offset: metadata.StartOffset, replayEnd: metadata.ReplayEndOffset, truncated: truncated, contiguous: true, viewRevision: 1,
		cancel: cancel, done: make(chan struct{}), readyCh: make(chan struct{})}
	p.updates = make(chan struct{}, 1)
	if p.offset >= p.replayEnd {
		p.markReadyLocked()
	}
	go p.readLoop(readCtx)
	select {
	case <-p.readyCh:
		p.mu.Lock()
		ready, readErr := p.ready, p.readErr
		p.mu.Unlock()
		if !ready {
			_ = p.Close()
			if readErr == nil {
				readErr = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("PTY output replay did not complete: %w", readErr)
		}
		if err := ctx.Err(); err != nil {
			_ = p.Close()
			return nil, err
		}
		return p, nil
	case <-ctx.Done():
		_ = p.Close()
		return nil, ctx.Err()
	}
}

func (p *PooledTerminal) BindOutputLease(pool *OutputPool, key OutputKey, lease uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pool == nil || key != p.expected || lease == 0 || p.pool != nil {
		return fmt.Errorf("PTY output lease binding is invalid")
	}
	p.pool, p.lease = pool, lease
	return nil
}

func (p *PooledTerminal) ValidateOutputCommit() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready || p.readErr != nil || p.closing {
		return fmt.Errorf("PTY output stream ended before pool commit")
	}
	return nil
}

func (p *PooledTerminal) readLoop(ctx context.Context) {
	defer close(p.done)
	defer p.stream.Close()
	for {
		frame, err := p.stream.ReadContext(ctx)
		if err != nil {
			p.finishRead(err)
			return
		}
		p.mu.Lock()
		if p.pool == nil {
			ok := p.applyFrameLocked(frame)
			p.mu.Unlock()
			if !ok {
				p.finishRead(fmt.Errorf("PTY output stream is not contiguous"))
				return
			}
			continue
		}
		pool, lease, key := p.pool, p.lease, p.expected
		p.mu.Unlock()
		var applyErr error
		leaseErr := pool.WithLease(key, lease, func(resource OutputResource) {
			if resource != p {
				applyErr = ErrStaleOutputLease
				return
			}
			p.mu.Lock()
			if !p.applyFrameLocked(frame) {
				applyErr = fmt.Errorf("PTY output stream is not contiguous")
			}
			p.mu.Unlock()
		})
		err = errors.Join(leaseErr, applyErr)
		if err != nil {
			p.finishRead(err)
			return
		}
	}
}

func (p *PooledTerminal) applyFrameLocked(frame OutputFrame) bool {
	if p.closing || frame.StartOffset != p.offset || frame.EndOffset != frame.StartOffset+uint64(len(frame.Data)) {
		p.contiguous = false
		return false
	}
	p.terminal.Write(frame.Data)
	p.offset = frame.EndOffset
	p.viewRevision++
	p.signalUpdateLocked()
	if p.offset >= p.replayEnd {
		p.markReadyLocked()
	}
	return true
}

func (p *PooledTerminal) finishRead(err error) {
	p.mu.Lock()
	if p.closing || errors.Is(err, context.Canceled) || errors.Is(err, daemon.ErrOutputSubscriptionClosed) || errors.Is(err, ErrStaleOutputLease) {
		p.mu.Unlock()
		return
	}
	var ended *daemon.OutputStreamEnded
	p.cleanEnd = errors.As(err, &ended)
	p.disconnect, p.readErr = !p.cleanEnd, err
	p.viewRevision++
	p.signalUpdateLocked()
	if !p.ready {
		p.signalReadyLocked()
	}
	pool, key, lease := p.pool, p.expected, p.lease
	p.mu.Unlock()
	if pool != nil {
		pool.DisconnectLease(key, lease)
	}
}

func (p *PooledTerminal) markReadyLocked()   { p.ready = true; p.signalReadyLocked() }
func (p *PooledTerminal) signalReadyLocked() { p.readyOnce.Do(func() { close(p.readyCh) }) }

func (p *PooledTerminal) signalUpdateLocked() {
	select {
	case p.updates <- struct{}{}:
	default:
	}
}

// Updates reports that a newer immutable View may be available. Signals are
// deliberately coalesced so a stalled renderer can never block the PTY reader.
func (p *PooledTerminal) Updates() <-chan struct{} {
	if p == nil {
		return nil
	}
	return p.updates
}

func (p *PooledTerminal) View() PooledTerminalView {
	p.mu.Lock()
	defer p.mu.Unlock()
	view := PooledTerminalView{Framebuffer: p.terminal.SnapshotState(), Revision: p.viewRevision, RuntimeGeneration: p.revision.RuntimeGeneration,
		OutputOffset: p.offset, ReplayEndOffset: p.replayEnd, Ready: p.ready, Truncated: p.truncated, Disconnected: p.disconnect, Ended: p.cleanEnd}
	if p.readErr != nil && !p.cleanEnd {
		view.Error = "PTY output stream disconnected"
	}
	return view
}

func (p *PooledTerminal) Snapshot(ctx context.Context) error {
	p.snapshotMu.Lock()
	defer p.snapshotMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	state := p.terminal.SnapshotState()
	generation, offset, truncated := p.revision.RuntimeGeneration, p.offset, p.truncated
	resumeValid := p.ready && p.contiguous
	instanceID, sessionID := p.expected.InstanceID, p.expected.SessionID
	hook := p.snapshotHook
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	payload, err := EncodeTerminalRenderState(TerminalRenderState{Framebuffer: &state, RuntimeGeneration: generation, OutputOffset: offset,
		ResumeCursorValid: resumeValid, Truncated: truncated})
	if err != nil {
		return err
	}
	return p.store.Save(TerminalSnapshot{InstanceID: instanceID, SessionID: sessionID, Payload: payload})
}

func (p *PooledTerminal) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closing = true
		p.mu.Unlock()
		p.cancel()
		p.closeErr = p.stream.Close()
		<-p.done
	})
	return p.closeErr
}
