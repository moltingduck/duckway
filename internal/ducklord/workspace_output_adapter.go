package ducklord

import (
	"context"
	"errors"
	"sync"
)

// WorkspaceOutputAdapter presents one PaneOutputManager through the selected-
// terminal event contract while also exposing live background pane views. One
// pool owns every raw stream and snapshot writer in the workspace TUI.
type WorkspaceOutputAdapter struct {
	ctx       context.Context
	cancel    context.CancelFunc
	pane      *PaneOutputManager
	visible   func(TerminalSelection) []TerminalSelection
	events    chan TerminalOutputEvent
	repaint   chan struct{}
	mu        sync.Mutex
	nextID    uint64
	selected  TerminalSelection
	workers   sync.WaitGroup
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
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
	m.nextID++
	id := m.nextID
	m.selected = selection
	m.mu.Unlock()
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		if reconnect {
			if err := m.pane.DisconnectHost(selection.Client.Name, selection.InstanceID); err != nil {
				m.publish(TerminalOutputEvent{RequestID: id, Key: selection.key(), Err: err})
				return
			}
		}
		_, openErr := m.pane.SetVisible(m.ctx, selection, visible)
		m.mu.Lock()
		current := m.nextID == id
		m.mu.Unlock()
		if !current || m.ctx.Err() != nil {
			return
		}
		event := TerminalOutputEvent{RequestID: id, Key: selection.key(), Revision: OutputRevision{RuntimeGeneration: selection.RuntimeGeneration}}
		if activation, err := m.pane.Activation(event.Key); err == nil && activation.Revision == event.Revision {
			event.Lease = activation.Lease
			event.Warning = openErr
		} else {
			event.Err = errors.Join(openErr, err)
		}
		m.publish(event)
		m.repaintNow()
	}()
	return id
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

func (m *WorkspaceOutputAdapter) SyncHost(clientKey, instanceID string, live bool, _ []TerminalSelection) {
	if !live {
		_ = m.pane.DisconnectHost(clientKey, instanceID)
		m.repaintNow()
	}
}

func (m *WorkspaceOutputAdapter) watchDirty() {
	defer m.workers.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.pane.DirtyReady():
			keys := m.pane.DrainDirty()
			m.mu.Lock()
			id, selected := m.nextID, m.selected
			m.mu.Unlock()
			for _, key := range keys {
				if key != selected.key() {
					m.repaintNow()
					continue
				}
				activation, err := m.pane.Activation(key)
				if err == nil && activation.Revision.RuntimeGeneration == selected.RuntimeGeneration {
					m.publish(TerminalOutputEvent{RequestID: id, Key: key, Revision: activation.Revision, Lease: activation.Lease})
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
