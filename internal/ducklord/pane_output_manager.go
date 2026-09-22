package ducklord

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// PaneOutputManager owns one raw-output stream per distinct visible Session,
// regardless of how many Project panes reference it. It keeps presentation
// focus separate from stream identity and never silently presents an evicted
// pane as live.
type PaneOutputManager struct {
	ctx          context.Context
	cancel       context.CancelFunc
	pool         *OutputPool
	source       TerminalOutputSource
	store        SnapshotStore
	opener       terminalOutputOpenFunc
	capacity     int
	opMu         sync.Mutex
	requestMu    sync.Mutex
	requestID    uint64
	requestEnd   context.CancelFunc
	requestHosts map[OutputKey]bool
	mu           sync.RWMutex
	priority     OutputKey
	inputFocus   OutputKey
	focusEpoch   uint64
	visible      map[OutputKey]OutputRevision
	leases       map[OutputKey]OutputActivation
	finals       map[OutputKey]pooledTerminalFinal
	watchers     map[OutputKey]context.CancelFunc
	dirty        map[OutputKey]bool
	dirtyReady   chan struct{}
	workers      sync.WaitGroup
}

type pooledTerminalFinal struct {
	lease uint64
	view  PooledTerminalView
}

func NewPaneOutputManager(parent context.Context, capacity int, source TerminalOutputSource, store SnapshotStore) (*PaneOutputManager, error) {
	if source == nil {
		return nil, fmt.Errorf("terminal output source is required")
	}
	pool, err := NewOutputPool(capacity)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	return &PaneOutputManager{ctx: ctx, cancel: cancel, pool: pool, source: source, store: store,
		capacity: capacity, visible: make(map[OutputKey]OutputRevision), leases: make(map[OutputKey]OutputActivation), finals: make(map[OutputKey]pooledTerminalFinal), watchers: make(map[OutputKey]context.CancelFunc),
		dirty: make(map[OutputKey]bool), dirtyReady: make(chan struct{}, 1)}, nil
}

// SetVisible reconciles the distinct visible Session set. Priority is admitted
// first; any subscription overflow is returned explicitly for stale/snapshot
// rendering. Priority selects output/LRU order; it does not grant input or
// resize focus. The returned keys are never counted as live subscriptions.
func (m *PaneOutputManager) SetVisible(ctx context.Context, priority TerminalSelection, visible []TerminalSelection) ([]OutputKey, error) {
	requestCtx, requestEnd := context.WithCancel(ctx)
	m.requestMu.Lock()
	if m.requestEnd != nil {
		m.requestEnd()
	}
	m.requestID++
	requestID := m.requestID
	m.requestEnd = requestEnd
	m.requestHosts = make(map[OutputKey]bool, len(visible)+1)
	m.requestHosts[priority.key()] = true
	for _, selection := range visible {
		m.requestHosts[selection.key()] = true
	}
	m.requestMu.Unlock()
	defer func() {
		requestEnd()
		m.requestMu.Lock()
		if m.requestID == requestID {
			m.requestEnd = nil
			m.requestHosts = nil
		}
		m.requestMu.Unlock()
	}()
	m.opMu.Lock()
	defer m.opMu.Unlock()
	workCtx, cancel := context.WithCancel(requestCtx)
	stop := context.AfterFunc(m.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := workCtx.Err(); err != nil {
		return nil, err
	}
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	if err := priority.key().validate(); err != nil {
		return nil, err
	}
	if err := (OutputRevision{RuntimeGeneration: priority.RuntimeGeneration}).validate(); err != nil {
		return nil, err
	}
	ordered := []TerminalSelection{priority}
	seen := map[OutputKey]OutputRevision{priority.key(): {RuntimeGeneration: priority.RuntimeGeneration}}
	for _, selection := range visible {
		key := selection.key()
		if previous, exists := seen[key]; exists {
			if previous.RuntimeGeneration != selection.RuntimeGeneration {
				return nil, fmt.Errorf("inconsistent runtime generation for Session %s", key.SessionID)
			}
			continue
		}
		if err := key.validate(); err != nil {
			return nil, err
		}
		if err := (OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}).validate(); err != nil {
			return nil, err
		}
		seen[key] = OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}
		ordered = append(ordered, selection)
	}
	admitted := make([]TerminalSelection, 0, len(ordered))
	perDaemon := make(map[string]int)
	var overflow []OutputKey
	for _, selection := range ordered {
		key := selection.key()
		if len(admitted) >= m.capacity || perDaemon[key.InstanceID] >= maxStableOutputObserversPerDaemon {
			overflow = append(overflow, key)
			continue
		}
		admitted = append(admitted, selection)
		perDaemon[key.InstanceID]++
	}
	var failures []error
	// A failed priority handoff leaves the previous displayed set untouched.
	if err := m.activate(workCtx, priority); err != nil {
		return overflow, err
	}
	for _, selection := range admitted[1:] {
		if err := m.activate(workCtx, selection); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", selection.key().SessionID, err))
		}
	}
	// OutputPool uses one active LRU key. Restore it to the actual input/resize
	// focus after activating background panes.
	if len(admitted) > 1 {
		if err := m.activate(workCtx, priority); err != nil {
			failures = append(failures, fmt.Errorf("focused Session: %w", err))
		}
	}
	m.requestMu.Lock()
	if m.requestID != requestID {
		m.requestMu.Unlock()
		return overflow, context.Canceled
	}
	if err := workCtx.Err(); err != nil {
		m.requestMu.Unlock()
		return overflow, err
	}
	m.mu.Lock()
	m.priority = priority.key()
	m.visible = make(map[OutputKey]OutputRevision, len(admitted))
	for _, selection := range admitted {
		m.visible[selection.key()] = OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}
	}
	if _, stillVisible := m.visible[m.inputFocus]; !stillVisible {
		m.inputFocus = OutputKey{}
		m.focusEpoch++
	}
	m.mu.Unlock()
	m.requestMu.Unlock()
	m.pruneEvicted()
	return overflow, errors.Join(failures...)
}

func (m *PaneOutputManager) activate(ctx context.Context, selection TerminalSelection) error {
	key := selection.key()
	revision := OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}
	activation, err := m.pool.Activate(ctx, key, revision, func(openCtx context.Context, key OutputKey, revision OutputRevision, _ uint64) (OutputResource, error) {
		return m.open(openCtx, selection, key, revision)
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	previous, had := m.leases[key]
	m.leases[key] = activation
	if !had || previous.Lease != activation.Lease {
		delete(m.finals, key)
		if cancel := m.watchers[key]; cancel != nil {
			cancel()
		}
		m.watchers[key] = m.watch(key, activation.Lease)
	}
	m.mu.Unlock()
	m.markDirty(key)
	return nil
}

// ReconnectVisible replaces only one visible Session stream. Other panes on
// the same Ducklion keep their leases, output, and input focus unchanged.
func (m *PaneOutputManager) ReconnectVisible(ctx context.Context, selection TerminalSelection) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	key := selection.key()
	revision := OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}
	m.mu.RLock()
	current, visible := m.visible[key]
	m.mu.RUnlock()
	if !visible || current != revision {
		return ErrStaleOutputLease
	}
	activation, err := m.pool.ReconnectDesired(ctx, key, revision, func(openCtx context.Context, key OutputKey,
		revision OutputRevision, _ uint64) (OutputResource, error) {
		return m.open(openCtx, selection, key, revision)
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	if cancel := m.watchers[key]; cancel != nil {
		cancel()
	}
	m.leases[key] = activation
	delete(m.finals, key)
	m.watchers[key] = m.watch(key, activation.Lease)
	m.mu.Unlock()
	m.markDirty(key)
	return nil
}

// RestoreHost rebuilds desired subscriptions after the observer bridge has
// been replaced. OutputPool deliberately retains desired membership across a
// host disconnect, but the workspace adapter must attach fresh readers for
// those members when the host becomes live again.
func (m *PaneOutputManager) RestoreHost(ctx context.Context, clientKey, instanceID string, selections []TerminalSelection) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	byKey := make(map[OutputKey]TerminalSelection, len(selections))
	for _, selection := range selections {
		if key := selection.key(); key.ClientKey == clientKey && key.InstanceID == instanceID {
			byKey[key] = selection
		}
	}
	for _, key := range m.pool.DesiredForHost(clientKey, instanceID) {
		selection, ok := byKey[key]
		if !ok {
			continue
		}
		revision := OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}
		activation, err := m.pool.RestoreDesired(ctx, key, revision, func(openCtx context.Context, key OutputKey, revision OutputRevision, _ uint64) (OutputResource, error) {
			return m.open(openCtx, selection, key, revision)
		})
		if err != nil {
			continue
		}
		m.mu.Lock()
		previous, had := m.leases[key]
		m.leases[key] = activation
		if !had || previous.Lease != activation.Lease {
			delete(m.finals, key)
			if cancel := m.watchers[key]; cancel != nil {
				cancel()
			}
			m.watchers[key] = m.watch(key, activation.Lease)
		}
		m.mu.Unlock()
		m.markDirty(key)
	}
}

func (m *PaneOutputManager) pruneEvicted() {
	status := m.pool.Status()
	desired := make(map[OutputKey]bool, len(status.Desired))
	for _, key := range status.Desired {
		desired[key] = true
	}
	m.mu.Lock()
	for key := range m.leases {
		if desired[key] {
			continue // background LRU subscriptions remain useful
		}
		if cancel := m.watchers[key]; cancel != nil {
			cancel()
		}
		delete(m.watchers, key)
		delete(m.leases, key)
		delete(m.finals, key)
		delete(m.dirty, key)
	}
	m.mu.Unlock()
}

func (m *PaneOutputManager) open(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision) (*PooledTerminal, error) {
	var initial *TerminalRenderState
	if snapshot, err := m.store.Load(key.InstanceID, key.SessionID); err == nil {
		if decoded, err := DecodeTerminalRenderState(snapshot.Payload); err == nil {
			initial = &decoded
		}
	}
	if m.opener != nil {
		return m.opener(ctx, selection, key, revision, initial)
	}
	var stream *OutputStream
	var err error
	if initial != nil && initial.ResumeCursorValid && initial.RuntimeGeneration == revision.RuntimeGeneration {
		stream, err = m.source.OpenOutputStreamFrom(ctx, selection.Client, key.SessionID,
			AttachResume{RuntimeGeneration: initial.RuntimeGeneration, OutputOffset: initial.OutputOffset})
	} else {
		stream, err = m.source.OpenOutputStream(ctx, selection.Client, key.SessionID)
	}
	if err != nil {
		return nil, err
	}
	return NewPooledTerminal(ctx, stream, PooledTerminalOptions{ExpectedKey: key, ExpectedRevision: revision,
		Rows: selection.Rows, Cols: selection.Cols, Scrollback: DefaultTerminalScrollback, Store: m.store, Initial: initial})
}

func (m *PaneOutputManager) watch(key OutputKey, lease uint64) context.CancelFunc {
	updates, final, done, err := m.pool.TerminalSignals(key, lease)
	if err != nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		storeFinal := func(view PooledTerminalView) {
			m.mu.Lock()
			if activation, ok := m.leases[key]; ok && activation.Lease == lease && activation.Revision.RuntimeGeneration == view.RuntimeGeneration {
				m.finals[key] = pooledTerminalFinal{lease: lease, view: view}
				m.dirty[key] = true
			}
			m.mu.Unlock()
			select {
			case m.dirtyReady <- struct{}{}:
			default:
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case view := <-final:
				storeFinal(view)
				return
			case <-done:
				// finishRead queues the final view before closing done. Drain it
				// even when select chose done first, after the pool detached it.
				select {
				case view := <-final:
					storeFinal(view)
				default:
					m.markDirty(key)
				}
				return
			case <-updates:
				m.markDirty(key)
			}
		}
	}()
	return cancel
}

func (m *PaneOutputManager) markDirty(key OutputKey) {
	m.mu.Lock()
	m.dirty[key] = true
	m.mu.Unlock()
	select {
	case m.dirtyReady <- struct{}{}:
	default:
	}
}

func (m *PaneOutputManager) DirtyReady() <-chan struct{} { return m.dirtyReady }

func (m *PaneOutputManager) DrainDirty() []OutputKey {
	m.mu.Lock()
	keys := make([]OutputKey, 0, len(m.dirty))
	for key := range m.dirty {
		keys = append(keys, key)
	}
	clear(m.dirty)
	m.mu.Unlock()
	return keys
}

func (m *PaneOutputManager) View(key OutputKey) (PooledTerminalView, error) {
	m.mu.RLock()
	activation, ok := m.leases[key]
	revision, visible := m.visible[key]
	m.mu.RUnlock()
	if !ok || !visible || activation.Revision != revision {
		return PooledTerminalView{}, ErrStaleOutputLease
	}
	return m.pool.TerminalView(key, activation.Lease)
}

// Activation returns the exact visible lease for an event-loop fence. A
// caller must still use ViewLease when it consumes a delayed event.
func (m *PaneOutputManager) Activation(key OutputKey) (OutputActivation, error) {
	m.mu.RLock()
	activation, ok := m.leases[key]
	revision, visible := m.visible[key]
	m.mu.RUnlock()
	if !ok || !visible || activation.Revision != revision {
		return OutputActivation{}, ErrStaleOutputLease
	}
	return activation, nil
}

func (m *PaneOutputManager) ViewLease(key OutputKey, lease uint64) (PooledTerminalView, error) {
	activation, err := m.Activation(key)
	if err != nil || activation.Lease != lease {
		return PooledTerminalView{}, ErrStaleOutputLease
	}
	return m.pool.TerminalView(key, lease)
}

// FinalViewLease is retained only for the current visible activation. A late
// final from an evicted/reconnected observer can never label a new lease ended.
func (m *PaneOutputManager) FinalViewLease(key OutputKey, lease uint64) (PooledTerminalView, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	activation, exists := m.leases[key]
	revision, visible := m.visible[key]
	final, found := m.finals[key]
	if !exists || !visible || !found || activation.Lease != lease || final.lease != lease || activation.Revision != revision ||
		final.view.RuntimeGeneration != revision.RuntimeGeneration {
		return PooledTerminalView{}, ErrStaleOutputLease
	}
	return final.view, nil
}

// SetInputFocus is called only after the UI has given a Session pane keyboard
// focus and verified the local writer. Preview/list navigation must clear it.
func (m *PaneOutputManager) SetInputFocus(key OutputKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	activation, exists := m.leases[key]
	revision, visible := m.visible[key]
	if !exists || !visible || activation.Revision != revision {
		return ErrStaleOutputLease
	}
	m.inputFocus = key
	m.focusEpoch++
	return nil
}

func (m *PaneOutputManager) ClearInputFocus() {
	m.mu.Lock()
	m.inputFocus = OutputKey{}
	m.focusEpoch++
	m.mu.Unlock()
}

// ClearVisible stops presenting panes when the current Project has no usable
// PTY cells. Desired LRU readers may remain subscribed for a later revisit.
func (m *PaneOutputManager) ClearVisible() {
	m.cancelVisibleRequest()
	m.opMu.Lock()
	m.mu.Lock()
	m.visible = make(map[OutputKey]OutputRevision)
	m.inputFocus = OutputKey{}
	m.focusEpoch++
	m.mu.Unlock()
	m.opMu.Unlock()
}

func (m *PaneOutputManager) cancelVisibleRequest() {
	m.requestMu.Lock()
	m.requestID++
	if m.requestEnd != nil {
		m.requestEnd()
		m.requestEnd = nil
	}
	m.requestMu.Unlock()
}

// DisconnectHost fences presentation and closes every live observer for a
// temporarily unavailable transport. Its desired LRU membership survives so
// a same-instance reconnect can reopen from Ducklion's replay.
func (m *PaneOutputManager) DisconnectHost(clientKey, instanceID string) error {
	return m.fenceHost(clientKey, instanceID, false)
}

// ForgetHost also removes desired membership after an authoritative Ducklion
// instance replacement. The retired instance can never revive an old lease.
func (m *PaneOutputManager) ForgetHost(clientKey, instanceID string) error {
	return m.fenceHost(clientKey, instanceID, true)
}

func (m *PaneOutputManager) fenceHost(clientKey, instanceID string, forget bool) error {
	m.requestMu.Lock()
	for key := range m.requestHosts {
		if key.ClientKey == clientKey && key.InstanceID == instanceID {
			m.requestID++
			if m.requestEnd != nil {
				m.requestEnd()
				m.requestEnd = nil
			}
			m.requestHosts = nil
			break
		}
	}
	m.requestMu.Unlock()
	m.opMu.Lock()
	defer m.opMu.Unlock()
	var err error
	if forget {
		err = m.pool.ForgetHost(clientKey, instanceID)
	} else {
		err = m.pool.DisconnectHost(clientKey, instanceID)
	}
	m.mu.Lock()
	for key := range m.leases {
		if key.ClientKey != clientKey || key.InstanceID != instanceID {
			continue
		}
		if cancel := m.watchers[key]; cancel != nil {
			cancel()
			delete(m.watchers, key)
		}
		delete(m.leases, key)
		delete(m.finals, key)
		delete(m.visible, key)
		delete(m.dirty, key)
		if m.inputFocus == key {
			m.inputFocus = OutputKey{}
			m.focusEpoch++
		}
	}
	m.mu.Unlock()
	return err
}

func (m *PaneOutputManager) ResizeFocused(key OutputKey, rows, cols uint16, resize func(uint16, uint16) (uint64, error)) (uint64, error) {
	return m.ResizeFocusedLease(key, 0, OutputRevision{}, rows, cols, resize)
}

// ResizeFocusedLease verifies the event's exact lease under the same lock that
// fences focus and visibility. A reconnect cannot swap a lease between the
// check and invoking the old control session's resize callback.
func (m *PaneOutputManager) ResizeFocusedLease(key OutputKey, lease uint64, expected OutputRevision, rows, cols uint16,
	resize func(uint16, uint16) (uint64, error)) (uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if key != m.inputFocus {
		return 0, fmt.Errorf("session pane is not focused")
	}
	activation, ok := m.leases[key]
	revision, visible := m.visible[key]
	if !ok || !visible || activation.Revision != revision || lease != 0 && activation.Lease != lease || expected.RuntimeGeneration != 0 && activation.Revision != expected {
		return 0, ErrStaleOutputLease
	}
	// Keep focus stable through the authoritative remote resize. Returning a
	// stale error after the RPC cannot undo an already applied PTY dimension.
	return m.pool.ResizeTerminalAt(key, activation.Lease, rows, cols, resize)
}

func (m *PaneOutputManager) Close() error {
	m.cancel()
	m.requestMu.Lock()
	if m.requestEnd != nil {
		m.requestEnd()
	}
	m.requestMu.Unlock()
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	for _, cancel := range m.watchers {
		cancel()
	}
	m.mu.Unlock()
	err := m.pool.Close()
	m.workers.Wait()
	return err
}
