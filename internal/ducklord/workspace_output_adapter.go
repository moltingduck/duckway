package ducklord

import (
	"context"
	"errors"
	"sync"
	"time"
)

// WorkspaceOutputAdapter presents one PaneOutputManager through the selected-
// terminal event contract while also exposing live background pane views. One
// pool owns every raw stream and snapshot writer in the workspace TUI.
type WorkspaceOutputAdapter struct {
	ctx           context.Context
	cancel        context.CancelFunc
	pane          *PaneOutputManager
	visible       func(TerminalSelection) []TerminalSelection
	events        chan TerminalOutputEvent
	repaint       chan struct{}
	mu            sync.Mutex
	nextID        uint64
	selected      TerminalSelection
	selectedLease uint64
	requestCancel context.CancelFunc
	workers       sync.WaitGroup
	closeOnce     sync.Once
	closeDone     chan struct{}
	closeErr      error
}

func NewWorkspaceOutputAdapter(parent context.Context, capacity int, source TerminalOutputSource, store SnapshotStore,
	visible func(TerminalSelection) []TerminalSelection) (*WorkspaceOutputAdapter, error) {
	pane, err := NewPaneOutputManager(parent, capacity, source, store)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	m := &WorkspaceOutputAdapter{ctx: ctx, cancel: cancel, pane: pane, visible: visible,
		events: make(chan TerminalOutputEvent, 32), repaint: make(chan struct{}, 1), closeDone: make(chan struct{})}
	m.workers.Add(1)
	go m.watchDirty()
	return m, nil
}

func (m *WorkspaceOutputAdapter) Select(selection TerminalSelection) uint64 {
	return m.selectOutput(selection, false)
}

func (m *WorkspaceOutputAdapter) selectOutput(selection TerminalSelection, reconnect bool) uint64 {
	visible := []TerminalSelection(nil)
	if m.visible != nil {
		visible = m.visible(selection)
	}
	m.mu.Lock()
	if m.requestCancel != nil {
		m.requestCancel()
	}
	requestCtx, requestCancel := context.WithCancel(m.ctx)
	m.requestCancel = requestCancel
	m.nextID++
	id := m.nextID
	m.selected = selection
	m.selectedLease = 0
	m.mu.Unlock()
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		defer requestCancel()
		if reconnect {
			if err := m.pane.ReconnectVisible(requestCtx, selection); err != nil {
				if m.currentRequest(id) {
					m.publish(TerminalOutputEvent{RequestID: id, Key: selection.key(), Err: err})
				}
				return
			}
		}
		_, openErr := m.pane.SetVisible(requestCtx, selection, visible)
		m.mu.Lock()
		current := m.nextID == id && requestCtx.Err() == nil
		m.mu.Unlock()
		if !current || m.ctx.Err() != nil {
			return
		}
		event := TerminalOutputEvent{RequestID: id, Key: selection.key(), Revision: OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}}
		if activation, err := m.pane.Activation(event.Key); err == nil && activation.Revision == event.Revision {
			event.Lease = activation.Lease
			event.Warning = openErr
			if final, finalErr := m.pane.FinalViewLease(event.Key, event.Lease); finalErr == nil {
				event.FinalView = &final
			}
			m.mu.Lock()
			if m.nextID == id {
				m.selectedLease = event.Lease
			}
			m.mu.Unlock()
		} else {
			event.Err = errors.Join(openErr, err)
		}
		m.publish(event)
		m.repaintNow()
	}()
	return id
}

func (m *WorkspaceOutputAdapter) currentRequest(id uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextID == id && m.ctx.Err() == nil
}

func (m *WorkspaceOutputAdapter) Reconnect(selection TerminalSelection) uint64 {
	return m.selectOutput(selection, true)
}

func (m *WorkspaceOutputAdapter) Events() <-chan TerminalOutputEvent { return m.events }

func (m *WorkspaceOutputAdapter) RepaintReady() <-chan struct{} { return m.repaint }

func (m *WorkspaceOutputAdapter) View(event TerminalOutputEvent) (PooledTerminalView, error) {
	if event.Err != nil || event.Lease == 0 {
		return PooledTerminalView{}, ErrStaleOutputLease
	}
	return m.pane.ViewLease(event.Key, event.Lease)
}

func (m *WorkspaceOutputAdapter) PaneView(key OutputKey) (PooledTerminalView, error) {
	return m.pane.View(key)
}

func (m *WorkspaceOutputAdapter) ClearVisible() {
	m.pane.ClearVisible()
	m.repaintNow()
}

func (m *WorkspaceOutputAdapter) Resize(event TerminalOutputEvent, rows, cols uint16,
	resize func(uint16, uint16) (uint64, error)) (uint64, error) {
	if event.Lease == 0 {
		return 0, ErrStaleOutputLease
	}
	return m.pane.ResizeFocusedLease(event.Key, event.Lease, event.Revision, rows, cols, resize)
}

func (m *WorkspaceOutputAdapter) SetInputFocus(key OutputKey) error { return m.pane.SetInputFocus(key) }
func (m *WorkspaceOutputAdapter) ClearInputFocus()                  { m.pane.ClearInputFocus() }

func (m *WorkspaceOutputAdapter) ForgetHost(clientKey, instanceID string) {
	_ = m.pane.ForgetHost(clientKey, instanceID)
	m.repaintNow()
}

func (m *WorkspaceOutputAdapter) SyncHost(clientKey, instanceID string, live bool, selections []TerminalSelection) {
	if !live {
		_ = m.pane.DisconnectHost(clientKey, instanceID)
		m.repaintNow()
		return
	}
	go func() {
		m.pane.RestoreHost(m.ctx, clientKey, instanceID, selections)
		m.repaintNow()
	}()
}

// Coalesce visual updates across every pane; raw VT ingestion stays lossless.
const workspaceOutputRefreshInterval = time.Second / 30

func (m *WorkspaceOutputAdapter) watchDirty() {
	defer m.workers.Done()
	ticker := time.NewTicker(workspaceOutputRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.pane.DirtyReady():
			// Leave keys in the bounded dirty set until the next display tick.
			// A trailing update is delivered even when output stops during the wait.
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
			}
			keys := m.pane.DrainDirty()
			m.mu.Lock()
			id, selected, selectedLease := m.nextID, m.selected, m.selectedLease
			m.mu.Unlock()
			for _, key := range keys {
				if key != selected.key() {
					m.repaintNow()
					continue
				}
				activation, err := m.pane.Activation(key)
				if err == nil && activation.Lease == selectedLease && activation.Revision.RuntimeGeneration == selected.RuntimeGeneration {
					event := TerminalOutputEvent{RequestID: id, Key: key, Revision: activation.Revision, Lease: activation.Lease}
					if final, finalErr := m.pane.FinalViewLease(key, activation.Lease); finalErr == nil {
						event.FinalView = &final
					}
					m.publish(event)
					m.repaintNow() // a Project-only priority may not be the quick-list Session
				}
			}
		}
	}
}

func (m *WorkspaceOutputAdapter) publish(event TerminalOutputEvent) {
	select {
	case m.events <- event:
	case <-m.ctx.Done():
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

func (m *WorkspaceOutputAdapter) repaintNow() {
	select {
	case m.repaint <- struct{}{}:
	default:
	}
}

func (m *WorkspaceOutputAdapter) Close() error {
	m.closeOnce.Do(func() {
		m.cancel()
		m.closeErr = m.pane.Close()
		m.workers.Wait()
		close(m.events)
		close(m.repaint)
		close(m.closeDone)
	})
	<-m.closeDone
	return m.closeErr
}
