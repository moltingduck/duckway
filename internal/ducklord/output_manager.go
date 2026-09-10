package ducklord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type TerminalOutputSource interface {
	OpenOutputStream(context.Context, Client, string) (*OutputStream, error)
	OpenOutputStreamFrom(context.Context, Client, string, AttachResume) (*OutputStream, error)
}

type TerminalSelection struct {
	Client            Client
	InstanceID        string
	SessionID         string
	RuntimeGeneration uint64
	Rows              int
	Cols              int
}

func (s TerminalSelection) key() OutputKey {
	return OutputKey{ClientKey: s.Client.Name, InstanceID: s.InstanceID, SessionID: s.SessionID}
}

type TerminalOutputEvent struct {
	RequestID uint64
	Key       OutputKey
	Revision  OutputRevision
	Lease     uint64
	Err       error
	Warning   error
	FinalView *PooledTerminalView
}

type terminalOutputRequest struct {
	id        uint64
	selection TerminalSelection
	reconnect bool
}

type terminalOutputResult struct {
	request  terminalOutputRequest
	terminal *PooledTerminal
	result   OutputActivation
	err      error
}

type terminalHostSync struct {
	clientKey  string
	instanceID string
	live       bool
	sessions   map[OutputKey]TerminalSelection
}

// TerminalOutputManager is the TUI-facing owner of the raw-output pool. It
// serializes activation, cancels superseded selection work, and publishes only
// lease-fenced immutable views.
type TerminalOutputManager struct {
	ctx         context.Context
	cancel      context.CancelFunc
	source      TerminalOutputSource
	store       SnapshotStore
	pool        *OutputPool
	requests    chan terminalOutputRequest
	hostSync    chan terminalHostSync
	events      chan TerminalOutputEvent
	done        chan struct{}
	mu          sync.Mutex
	nextID      uint64
	current     terminalOutputRequest
	watchMu     sync.Mutex
	watchCancel context.CancelFunc
	opener      terminalOutputOpenFunc
	workers     sync.WaitGroup
	hostMu      sync.Mutex
	hostCancels map[outputHost]context.CancelFunc
	hostEpoch   map[outputHost]uint64
	hostRetired map[outputHost]bool
	closeOnce   sync.Once
	closeDone   chan struct{}
	closeErr    error
}

type terminalOutputOpenFunc func(context.Context, TerminalSelection, OutputKey, OutputRevision, *TerminalRenderState) (*PooledTerminal, error)

func NewTerminalOutputManager(parent context.Context, capacity int, source TerminalOutputSource, store SnapshotStore) (*TerminalOutputManager, error) {
	return newTerminalOutputManager(parent, capacity, source, store, nil)
}

func newTerminalOutputManager(parent context.Context, capacity int, source TerminalOutputSource, store SnapshotStore, opener terminalOutputOpenFunc) (*TerminalOutputManager, error) {
	if source == nil {
		return nil, fmt.Errorf("terminal output source is required")
	}
	pool, err := NewOutputPool(capacity)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	m := &TerminalOutputManager{ctx: ctx, cancel: cancel, source: source, store: store, pool: pool,
		requests: make(chan terminalOutputRequest, 1), hostSync: make(chan terminalHostSync, 32), events: make(chan TerminalOutputEvent, 1), done: make(chan struct{}),
		opener: opener, hostCancels: make(map[outputHost]context.CancelFunc), hostEpoch: make(map[outputHost]uint64), hostRetired: make(map[outputHost]bool), closeDone: make(chan struct{})}
	go m.run()
	return m, nil
}

func (m *TerminalOutputManager) Select(selection TerminalSelection) uint64 {
	return m.request(selection, false)
}

// Reconnect discards the selected session's current raw-output connection and
// framebuffer, then rebuilds both from Ducklion's authoritative replay.
func (m *TerminalOutputManager) Reconnect(selection TerminalSelection) uint64 {
	return m.request(selection, true)
}

func (m *TerminalOutputManager) request(selection TerminalSelection, reconnect bool) uint64 {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.mu.Unlock()
	request := terminalOutputRequest{id: id, selection: selection, reconnect: reconnect}
	m.mu.Lock()
	m.current = request
	m.mu.Unlock()
	select {
	case m.requests <- request:
	default:
		select {
		case <-m.requests:
		default:
		}
		select {
		case m.requests <- request:
		case <-m.ctx.Done():
		}
	}
	return id
}

func (m *TerminalOutputManager) Events() <-chan TerminalOutputEvent { return m.events }

func (m *TerminalOutputManager) View(event TerminalOutputEvent) (PooledTerminalView, error) {
	if event.Err != nil || event.Lease == 0 {
		return PooledTerminalView{}, ErrStaleOutputLease
	}
	return m.pool.TerminalView(event.Key, event.Lease)
}

func (m *TerminalOutputManager) Status() OutputPoolStatus { return m.pool.Status() }

func (m *TerminalOutputManager) Resize(event TerminalOutputEvent, rows, cols uint16, resize func(uint16, uint16) (uint64, error)) (uint64, error) {
	return m.pool.ResizeTerminalAt(event.Key, event.Lease, rows, cols, resize)
}

func (m *TerminalOutputManager) Close() error {
	m.closeOnce.Do(func() {
		m.cancel()
		<-m.done
		m.closeErr = m.pool.Close()
		m.workers.Wait()
		close(m.events)
		close(m.closeDone)
	})
	<-m.closeDone
	return m.closeErr
}

func (m *TerminalOutputManager) run() {
	defer close(m.done)
	results := make(chan terminalOutputResult, 1)
	var activeCancel context.CancelFunc
	var pending *terminalOutputRequest
	var inFlight bool
	latest := uint64(0)
	start := func(request terminalOutputRequest) {
		workCtx, cancel := context.WithCancel(m.ctx)
		activeCancel, inFlight = cancel, true
		m.workers.Add(1)
		go func() {
			defer m.workers.Done()
			result := m.activate(workCtx, request)
			select {
			case results <- result:
			case <-m.ctx.Done():
			}
		}()
	}
	for {
		select {
		case <-m.ctx.Done():
			if activeCancel != nil {
				activeCancel()
			}
			return
		case request := <-m.requests:
			latest = request.id
			if inFlight {
				activeCancel()
				copy := request
				pending = &copy
			} else {
				start(request)
			}
		case syncRequest := <-m.hostSync:
			m.scheduleHostSync(syncRequest)
		case result := <-results:
			inFlight, activeCancel = false, nil
			if result.request.id == latest {
				event := TerminalOutputEvent{RequestID: result.request.id, Key: result.result.Key, Revision: result.result.Revision,
					Lease: result.result.Lease, Err: result.err, Warning: result.result.EvictionCloseError}
				if result.err != nil {
					event.Key = result.request.selection.key()
					event.Revision = OutputRevision{RuntimeGeneration: result.request.selection.RuntimeGeneration}
				}
				m.publish(event)
				if result.err == nil {
					m.watch(event)
				}
			}
			if pending != nil {
				next := *pending
				pending = nil
				start(next)
			}
		}
	}
}

func (m *TerminalOutputManager) activate(ctx context.Context, request terminalOutputRequest) terminalOutputResult {
	selection := request.selection
	key := selection.key()
	revision := OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}
	result := terminalOutputResult{request: request}
	if err := key.validate(); err != nil {
		result.err = err
		return result
	}
	if err := revision.validate(); err != nil {
		result.err = err
		return result
	}
	var activation OutputActivation
	var err error
	for {
		open := func(openCtx context.Context, key OutputKey, revision OutputRevision, _ uint64) (OutputResource, error) {
			terminal, openErr := m.openResource(openCtx, selection, key, revision, !request.reconnect)
			result.terminal = terminal
			return terminal, openErr
		}
		if request.reconnect {
			activation, err = m.pool.ReconnectDesired(ctx, key, revision, open)
		} else {
			activation, err = m.pool.Activate(ctx, key, revision, open)
		}
		if !outputTransitionBusy(err) || ctx.Err() != nil {
			break
		}
		if !waitOutputTransition(ctx) {
			err = ctx.Err()
			break
		}
	}
	result.result, result.err = activation, err
	return result
}

func (m *TerminalOutputManager) openResource(ctx context.Context, selection TerminalSelection, key OutputKey, revision OutputRevision, restoreSnapshot bool) (*PooledTerminal, error) {
	var initial *TerminalRenderState
	if restoreSnapshot {
		if snapshot, loadErr := m.store.Load(key.InstanceID, key.SessionID); loadErr == nil {
			if decoded, decodeErr := DecodeTerminalRenderState(snapshot.Payload); decodeErr == nil {
				initial = &decoded
			}
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

func (m *TerminalOutputManager) watch(event TerminalOutputEvent) {
	updates, final, done, err := m.pool.TerminalSignals(event.Key, event.Lease)
	if err != nil {
		return
	}
	watchCtx, cancel := context.WithCancel(m.ctx)
	m.watchMu.Lock()
	if m.watchCancel != nil {
		m.watchCancel()
	}
	m.watchCancel = cancel
	m.watchMu.Unlock()
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		defer cancel()
		for {
			select {
			case <-watchCtx.Done():
				return
			case view, ok := <-final:
				if !ok {
					return
				}
				finalEvent := event
				finalEvent.FinalView = &view
				m.publish(finalEvent)
				return
			case <-done:
				select {
				case view, ok := <-final:
					if !ok {
						return
					}
					finalEvent := event
					finalEvent.FinalView = &view
					m.publish(finalEvent)
				default:
				}
				return
			case <-updates:
				m.publish(event)
			}
		}
	}()
}

func (m *TerminalOutputManager) publish(event TerminalOutputEvent) {
	m.mu.Lock()
	current := m.current
	m.mu.Unlock()
	if event.RequestID != current.id || event.Key != current.selection.key() || event.Revision.RuntimeGeneration != current.selection.RuntimeGeneration {
		return
	}
	select {
	case m.events <- event:
	default:
		select {
		case <-m.events:
		default:
		}
		select {
		case m.events <- event:
		case <-m.ctx.Done():
		}
	}
}

func (m *TerminalOutputManager) DisconnectHost(clientKey, instanceID string) error {
	return m.pool.DisconnectHost(clientKey, instanceID)
}

// ForgetHost fences pending restores and removes desired subscriptions for a
// Ducklion process that has been authoritatively replaced.
func (m *TerminalOutputManager) ForgetHost(clientKey, instanceID string) {
	host := outputHost{clientKey, instanceID}
	m.hostMu.Lock()
	if cancel := m.hostCancels[host]; cancel != nil {
		cancel()
		delete(m.hostCancels, host)
	}
	m.hostEpoch[host]++
	m.hostRetired[host] = true
	m.hostMu.Unlock()
	_ = m.pool.ForgetHost(clientKey, instanceID)
}

// RestoreHost explicitly permits subscriptions for a manually reconnected
// Ducklion instance. ForgetHost otherwise leaves a tombstone that fences
// already-queued SyncHost requests.
func (m *TerminalOutputManager) RestoreHost(clientKey, instanceID string) {
	m.hostMu.Lock()
	delete(m.hostRetired, outputHost{clientKey, instanceID})
	m.hostMu.Unlock()
}

func (m *TerminalOutputManager) SyncHost(clientKey, instanceID string, live bool, selections []TerminalSelection) {
	sessions := make(map[OutputKey]TerminalSelection, len(selections))
	for _, selection := range selections {
		if key := selection.key(); key.ClientKey == clientKey && key.InstanceID == instanceID {
			sessions[key] = selection
		}
	}
	request := terminalHostSync{clientKey: clientKey, instanceID: instanceID, live: live, sessions: sessions}
	select {
	case m.hostSync <- request:
	case <-m.ctx.Done():
	}
}

func (m *TerminalOutputManager) scheduleHostSync(request terminalHostSync) {
	host := outputHost{request.clientKey, request.instanceID}
	ctx, cancel := context.WithCancel(m.ctx)
	m.hostMu.Lock()
	if m.hostRetired[host] {
		m.hostMu.Unlock()
		cancel()
		return
	}
	if previous := m.hostCancels[host]; previous != nil {
		previous()
	}
	m.hostEpoch[host]++
	epoch := m.hostEpoch[host]
	m.hostCancels[host] = cancel
	m.hostMu.Unlock()
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		defer cancel()
		m.applyHostSync(ctx, request, host, epoch)
		m.hostMu.Lock()
		if m.hostEpoch[host] == epoch {
			delete(m.hostCancels, host)
		}
		m.hostMu.Unlock()
	}()
}

func (m *TerminalOutputManager) applyHostSync(ctx context.Context, request terminalHostSync, host outputHost, epoch uint64) {
	current := func() bool {
		m.hostMu.Lock()
		defer m.hostMu.Unlock()
		return ctx.Err() == nil && m.hostEpoch[host] == epoch
	}
	if !request.live {
		if !current() {
			return
		}
		_ = m.pool.DisconnectHost(request.clientKey, request.instanceID)
		return
	}
	desired := m.pool.DesiredForHost(request.clientKey, request.instanceID)
nextSession:
	for _, key := range desired {
		selection, exists := request.sessions[key]
		if !exists {
			_ = m.pool.RemoveDesired(key)
			continue
		}
		revision := OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}
		var activation OutputActivation
		for {
			var err error
			activation, err = m.pool.ReplaceDesired(ctx, key, revision, func(openCtx context.Context, key OutputKey, revision OutputRevision, _ uint64) (OutputResource, error) {
				return m.openResource(openCtx, selection, key, revision, true)
			})
			if !outputTransitionBusy(err) || ctx.Err() != nil {
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					if current() {
						m.publishRestoreError(key, revision, err)
					}
					continue nextSession
				}
				break
			}
			if !waitOutputTransition(ctx) {
				return
			}
		}
		if !current() {
			return
		}
		if _, err := m.pool.TerminalView(key, activation.Lease); err != nil {
			continue
		}
		status := m.pool.Status()
		if status.Active != nil && *status.Active == key {
			m.mu.Lock()
			requestID := m.nextID
			m.mu.Unlock()
			event := TerminalOutputEvent{RequestID: requestID, Key: key, Revision: revision, Lease: activation.Lease, Warning: activation.EvictionCloseError}
			m.publish(event)
			m.watch(event)
		}
	}
}

func (m *TerminalOutputManager) publishRestoreError(key OutputKey, revision OutputRevision, err error) {
	m.mu.Lock()
	current := m.current
	m.mu.Unlock()
	if current.selection.key() == key && current.selection.RuntimeGeneration == revision.RuntimeGeneration {
		m.publish(TerminalOutputEvent{RequestID: current.id, Key: key, Revision: revision, Err: err})
	}
}

func outputTransitionBusy(err error) bool {
	return errors.Is(err, ErrHandoffBusy) || errors.Is(err, ErrRestoreBusy)
}

func waitOutputTransition(ctx context.Context) bool {
	timer := time.NewTimer(5 * time.Millisecond)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return false
	case <-timer.C:
		return true
	}
}

func (m *TerminalOutputManager) Remove(selection TerminalSelection) error {
	err := m.pool.RemoveDesired(selection.key())
	if errors.Is(err, ErrOutputNotDesired) {
		return nil
	}
	return err
}
