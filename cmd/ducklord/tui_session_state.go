package main

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) currentKey() string {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		return ""
	}
	return sessionKey(s.sessions[s.selected])
}

func (s *tuiState) selectedClientName() string {
	if s.selectedGroupID != "" && s.organizationMode() == ducklord.OrganizationHost {
		if _, ok := s.cfg.Client(s.selectedGroupID); ok {
			return s.selectedGroupID
		}
	}
	if len(s.sessions) > 0 && s.selected >= 0 && s.selected < len(s.sessions) && s.sessions[s.selected].Client != "" {
		return s.sessions[s.selected].Client
	}
	if s.cfg != nil && len(s.cfg.Clients) > 0 {
		return s.cfg.Clients[0].Name
	}
	return ""
}

func (s *tuiState) completeNewSessionStart(ctx context.Context, clientName, sessionID string, err error) {
	s.newSessionStarting = false
	if err != nil {
		if s.workspaceNewSessionIntent != nil {
			message := sanitizeTerminalText(err.Error())
			s.cancelCreate()
			s.newSessionErr = message
			s.outputErr = message
			return
		}
		s.newSessionErr = err.Error()
		return
	}
	intent := s.workspaceNewSessionIntent
	s.workspaceNewSessionIntent = nil
	s.newSessionMode = false
	s.newSessionLine = ""
	s.newSessionErr = ""
	s.outputErr = ""
	if intent != nil {
		generation, instance := s.hostFingerprint(clientName)
		if !s.hostIsLive(clientName) || generation != s.newSessionStartGeneration || instance != s.newSessionStartInstance ||
			s.hostConnectionEpoch[clientName] != s.newSessionStartEpoch {
			s.outputErr = "shell started, but host changed before pane placement; find it in Default Project"
		} else {
			s.pendingWorkspacePlacements = append(s.pendingWorkspacePlacements, pendingWorkspacePlacement{
				intent: *intent, client: clientName, instanceID: instance, sessionID: sessionID,
				generation: generation, connectionEpoch: s.newSessionStartEpoch, createdAt: time.Now(),
			})
		}
	}
	s.refreshSessions(ctx)
	for index, session := range s.sessions {
		if session.Client == clientName && session.InstanceID == s.newSessionStartInstance && session.SessionID == sessionID {
			s.selected = index
			s.selectedKey = s.currentKey()
			break
		}
	}
	if s.outputErr == "" {
		for _, pending := range s.pendingWorkspacePlacements {
			if pending.client == clientName && pending.instanceID == s.newSessionStartInstance && pending.sessionID == sessionID &&
				pending.generation == s.newSessionStartGeneration && pending.connectionEpoch == s.newSessionStartEpoch {
				s.outputErr = "shell started; waiting for session synchronization before placing its pane"
				break
			}
		}
	}
	if !s.pooledOutput {
		placementMessage := s.outputErr
		s.refreshSelectedOutput(ctx)
		if placementMessage != "" {
			s.outputErr = placementMessage
		}
	}
}

// A successful Start may precede the next authoritative inventory. Preserve
// its exact pane intent until that Session appears, while fencing Host changes.
func (s *tuiState) reconcilePendingWorkspacePlacements() {
	if len(s.pendingWorkspacePlacements) == 0 {
		return
	}
	remaining := s.pendingWorkspacePlacements[:0]
	for _, pending := range s.pendingWorkspacePlacements {
		generation, instance := s.hostFingerprint(pending.client)
		if !s.hostIsLive(pending.client) || generation != pending.generation || instance != pending.instanceID ||
			s.hostConnectionEpoch[pending.client] != pending.connectionEpoch {
			s.outputErr = "shell started, but Host changed before pane placement; find it in Default Project"
			continue
		}
		if time.Since(pending.createdAt) > 30*time.Second {
			s.outputErr = "shell started, but its Session was not synchronized; find it in Default Project"
			continue
		}
		found := false
		for _, session := range s.sessions {
			if session.Client != pending.client || session.InstanceID != pending.instanceID || session.SessionID != pending.sessionID {
				continue
			}
			found = true
			s.outputErr = ""
			if err := s.placeCreatedWorkspacePane(pending.intent, session); err != nil {
				s.outputErr = "shell started, but pane placement failed: " + sanitizeTerminalText(err.Error())
			} else if s.outputErr == "" {
				s.outputErr = "Session pane created"
			}
			break
		}
		if !found {
			remaining = append(remaining, pending)
		}
	}
	s.pendingWorkspacePlacements = remaining
}

func (s *tuiState) refreshSelectedOutput(ctx context.Context) {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		s.outputText = ""
		s.outputErr = ""
		s.outputForKey = ""
		return
	}
	sess := s.sessions[s.selected]
	key := sessionKey(sess)
	s.selectedKey = key
	if s.outputForKey != key {
		s.outputText = ""
		s.terminal = nil
		s.ptyScrollOffset = 0
		s.outputErr = ""
		s.outputStale = false
		s.outputFresh = false
		if snapshot, err := s.snapshotStore.Load(sess.InstanceID, sess.SessionID); err == nil {
			if state, decodeErr := ducklord.DecodeTerminalRenderState(snapshot.Payload); decodeErr == nil {
				s.terminalGeneration = state.RuntimeGeneration
				s.terminalOffset = state.OutputOffset
				s.terminalCursorValid = state.ResumeCursorValid
				if state.Framebuffer != nil {
					s.terminal, _ = ducklord.NewTerminalFromState(*state.Framebuffer, ducklord.DefaultTerminalScrollback)
					s.ptyScrollOffset = 0
				}
				if s.terminal != nil {
					s.outputText = s.terminal.Text()
				} else {
					s.outputText = state.Text
				}
				s.outputStale = true
			}
		}
	}
	s.outputForKey = key
	if !s.hostIsLive(sess.Client) {
		s.outputFresh = false
		s.outputErr = "host reconnecting; showing last synchronized output"
		return
	}
	if !canRead(sess) {
		s.outputFresh = false
		s.outputErr = sess.Error
		if s.outputErr == "" {
			s.outputErr = sess.Status
		}
		return
	}
	if sess.Status == string(model.StatusRunning) && s.outputStale && s.terminal != nil && s.terminalCursorValid && s.terminalGeneration == sess.RuntimeGeneration {
		s.outputFresh = false
		s.outputErr = "saved terminal state; press enter to resume from its exact output position"
		return
	}
	c, err := mustClient(s.cfg, sess.Client)
	if err != nil {
		s.outputFresh = false
		if !s.outputStale {
			s.outputText = ""
		}
		s.outputErr = err.Error()
		return
	}
	ref := sess.SessionID
	if ref == "" {
		ref = sess.Name
	}
	text, err := s.runner.Read(ctx, c, ref, 80)
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	rows, cols := s.activePTYSize()
	s.terminal = ducklord.NewTerminal(int(rows), int(cols), ducklord.DefaultTerminalScrollback)
	s.ptyScrollOffset = 0
	s.terminal.Write([]byte(text))
	s.outputText = s.terminal.Text()
	s.outputErr = ""
	s.outputStale = false
	s.outputFresh = true
	s.terminalGeneration = sess.RuntimeGeneration
	s.terminalOffset = 0
	s.terminalCursorValid = false
	if s.sessionFreshlyDisplayed(sess) {
		s.markActivitySeen(sess)
	}
}

func (s *tuiState) applyAttachOutput(text string, runtimeGeneration, outputOffset uint64) {
	s.outputStale = false
	if s.terminal == nil {
		rows, cols := s.activePTYSize()
		s.terminal = ducklord.NewTerminal(int(rows), int(cols), ducklord.DefaultTerminalScrollback)
		s.ptyScrollOffset = 0
	}
	s.terminal.Write([]byte(text))
	s.terminalGeneration = runtimeGeneration
	s.terminalOffset = outputOffset
	s.terminalCursorValid = runtimeGeneration != 0
	s.outputFresh = true
	if s.pendingAttachKey != "" {
		s.activeAttachKey = s.pendingAttachKey
		s.pendingAttachKey = ""
	}
	s.activeAttachFresh = true
	if s.sessionFreshlyDisplayed(s.currentSession()) {
		s.markActivitySeen(s.currentSession())
	}
}

func applyOrderedAttachChunk(state *tuiState, chunk attachOutputEvent, pending **resizeDoneEvent) {
	if chunk.runtimeGeneration != 0 {
		if chunk.outputOffset < chunk.startOffset || chunk.outputOffset-chunk.startOffset != uint64(len(chunk.text)) ||
			state.terminalGeneration != 0 && chunk.runtimeGeneration != state.terminalGeneration || chunk.startOffset != state.terminalOffset {
			state.outputErr = "PTY output stream lost byte continuity; detach and reattach to rebuild the view"
			*pending = nil
			state.resizeStatus = ""
			return
		}
	}
	resize := *pending
	if resize != nil && resize.id == chunk.id && chunk.runtimeGeneration == state.terminalGeneration && resize.barrier > chunk.startOffset && resize.barrier < chunk.outputOffset {
		split := int(resize.barrier - chunk.startOffset)
		state.applyAttachOutput(chunk.text[:split], chunk.runtimeGeneration, resize.barrier)
		state.terminal.Resize(int(resize.rows), int(resize.cols))
		state.applyAttachOutput(chunk.text[split:], chunk.runtimeGeneration, chunk.outputOffset)
		*pending = nil
		state.resizeStatus = ""
		return
	}
	if resize != nil && resize.id == chunk.id && chunk.startOffset >= resize.barrier {
		if state.terminal != nil {
			state.terminal.Resize(int(resize.rows), int(resize.cols))
		}
		*pending = nil
		state.resizeStatus = ""
	}
	state.applyAttachOutput(chunk.text, chunk.runtimeGeneration, chunk.outputOffset)
	if resize != nil && resize.id == chunk.id && chunk.runtimeGeneration == state.terminalGeneration && chunk.outputOffset == resize.barrier {
		state.terminal.Resize(int(resize.rows), int(resize.cols))
		*pending = nil
		state.resizeStatus = ""
	}
}

func drainBufferedAttach(state *tuiState, events []attachOutputEvent, pending **resizeDoneEvent) *attachOutputEvent {
	for index := range events {
		event := events[index]
		if event.done {
			return &event
		}
		applyOrderedAttachChunk(state, event, pending)
	}
	return nil
}

func (s *tuiState) activity() *ducklord.ActivityState {
	if s.activityState == nil {
		s.activityState = ducklord.NewActivityState()
	}
	return s.activityState
}

func (s *tuiState) markActivitySeen(session ducklord.RemoteSession) {
	selectedKey := s.currentKey()
	next := s.activity().Clone()
	activityChanged := next.MarkSeen(session)
	if activityChanged {
		if err := s.activityStore.Save(next); err != nil {
			s.outputErr = "local state: " + err.Error()
			return
		}
		s.activityState = next
	}
	for i := range s.sessions {
		if sessionKey(s.sessions[i]) == sessionKey(session) {
			s.sessions[i].Unread = false
			s.sessions[i].Updated = false
		}
	}
	s.applyOrganizationOrder()
	s.restoreSelection(selectedKey)
}

func (s *tuiState) saveActivityState() error {
	err := s.activityStore.Save(s.activity())
	if err != nil && s.outputErr == "" {
		s.outputErr = "local state: " + err.Error()
	}
	return err
}

func (s *tuiState) currentSession() ducklord.RemoteSession {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		return ducklord.RemoteSession{}
	}
	return s.sessions[s.selected]
}

func (s *tuiState) activePTYSession() ducklord.RemoteSession {
	if s.workspacePreview && s.activeAttachKey != "" {
		if session, ok := s.sessionForKey(s.activeAttachKey); ok {
			return session
		}
		if sessionKey(s.workspacePaneFocusRestoreSession) == s.activeAttachKey {
			return s.workspacePaneFocusRestoreSession
		}
		return ducklord.RemoteSession{}
	}
	if s.workspacePreview && s.workspaceNav != nil && s.workspaceNav.InDetailMode() {
		identity := s.workspaceNav.DetailSelection()
		for _, session := range s.sessions {
			if candidate, ok := ducklord.IdentityFromSession(session); ok && candidate == identity {
				return session
			}
		}
		return ducklord.RemoteSession{}
	}
	return s.currentSession()
}

func (s *tuiState) hostOwnsActivePTY(host string, control *ducklord.ControlSession) bool {
	if s.activePTYSession().Client == host || control != nil && control.ClientKey == host {
		return true
	}
	if s.pendingAttachKey != "" {
		if pending, ok := s.sessionForKey(s.pendingAttachKey); ok && pending.Client == host {
			return true
		}
	}
	return false
}

func (s *tuiState) acceptHostRetentionEvent(event hostRetentionEvent, watchEpoch uint64) bool {
	return s.hostMenuMode && s.hostMenuTarget == event.host && s.hostMenuRequestID == event.id && !s.disconnectedHosts[event.host] &&
		watchEpoch == event.epoch && s.hostSync[event.host].InstanceID == event.instanceID
}

func (s *tuiState) acceptHostResourceEvent(event hostResourceEvent, watchEpoch uint64) bool {
	return s.hostMenuMode && s.hostMenuTarget == event.host && s.hostMenuRequestID == event.id && strings.HasPrefix(s.hostMenuStep, "resources-") &&
		!s.disconnectedHosts[event.host] && watchEpoch == event.epoch && s.hostSync[event.host].InstanceID == event.instanceID
}

func (s *tuiState) acceptHostHookEvent(event hostHookEvent, watchEpoch uint64) bool {
	return s.hostMenuMode && s.hostMenuStep == "hook-saving" && s.hostMenuTarget == event.host && s.hostMenuRequestID == event.id &&
		s.hostHookAgent == event.agent && s.hostHookAction == event.action && !s.disconnectedHosts[event.host] &&
		watchEpoch == event.epoch && s.hostSync[event.host].InstanceID == event.instanceID
}

func (s *tuiState) beginHostHookOperation(host string, id uint64) bool {
	if s.hostHookInFlight == nil {
		s.hostHookInFlight = make(map[string]uint64)
	}
	if _, exists := s.hostHookInFlight[host]; exists {
		return false
	}
	s.hostHookInFlight[host] = id
	return true
}

func (s *tuiState) finishHostHookOperation(host string, id uint64) {
	if s.hostHookInFlight[host] == id {
		delete(s.hostHookInFlight, host)
	}
}

func (s *tuiState) canResizeCurrentSession() bool {
	session := s.activePTYSession()
	if session.Kind == string(model.KindShell) {
		return true
	}
	return session.WriterKind == string(model.OwnerTerminal) && session.WriterID == s.ownerName
}

func controlMatchesSession(control *ducklord.ControlSession, session ducklord.RemoteSession, owner string) bool {
	if control == nil || session.Client != control.ClientKey || session.InstanceID != control.InstanceID || session.SessionID != control.SessionID ||
		session.OwnershipEpoch != control.OwnershipEpoch || session.RuntimeGeneration != control.RuntimeGeneration {
		return false
	}
	return session.Kind == string(model.KindShell) || session.WriterKind == string(model.OwnerTerminal) && session.WriterID == owner
}

func focusedPTYWriter(control *ducklord.ControlSession, attach *ducklord.AttachSession, session ducklord.RemoteSession, owner string) io.WriteCloser {
	if control != nil && control.Stdin != nil && controlMatchesSession(control, session, owner) {
		return control.Stdin
	}
	if attach != nil && attach.Stdin != nil {
		return attach.Stdin
	}
	return nil
}

func (s *tuiState) activePTYSize() (rows, cols uint16) {
	width, height := terminalSize()
	if s.workspacePreview {
		pane, err := s.workspacePaneRectAt(width, height)
		if err != nil {
			return 1, 1
		}
		return uint16(max(1, min(200, pane.Height-1))), uint16(max(1, min(500, pane.Width)))
	}
	layout := calculateTUILayout(width, true, s.listPaneWidth, s.autoHideList)
	contentWidth := layout.contentWidth
	contentHeight := height - 4
	if contentWidth < 40 {
		contentWidth = 40
	} else if contentWidth > 500 {
		contentWidth = 500
	}
	if contentHeight < 5 {
		contentHeight = 5
	} else if contentHeight > 200 {
		contentHeight = 200
	}
	return uint16(contentHeight), uint16(contentWidth)
}

type tuiLayout struct {
	width, menuWidth, contentX, contentWidth int
	overlay, showList                        bool
}

func calculateTUILayout(width int, ptyFocused bool, configuredListWidth int, autoHide bool) tuiLayout {
	if width < 1 {
		width = 1
	}
	menuWidth := configuredListWidth
	if menuWidth < 20 || menuWidth > 80 {
		menuWidth = ducklord.DefaultSessionListWidth
	}
	if ptyFocused && autoHide {
		return tuiLayout{width: width, menuWidth: menuWidth, contentX: 1, contentWidth: width, showList: false}
	}
	if width < menuWidth+3+40 {
		contentWidth := width
		if menuWidth > width {
			menuWidth = width
		}
		return tuiLayout{width: width, menuWidth: menuWidth, contentX: 1, contentWidth: contentWidth, overlay: true, showList: true}
	}
	return tuiLayout{width: width, menuWidth: menuWidth, contentX: menuWidth + 4, contentWidth: width - menuWidth - 3, showList: true}
}

func (s *tuiState) saveCurrentSnapshot() {
	session := s.currentSession()
	if sessionKey(session) != s.outputForKey {
		return
	}
	s.saveSnapshot(session, s.outputText)
}

func (s *tuiState) saveSnapshot(session ducklord.RemoteSession, text string) {
	if session.InstanceID == "" || session.SessionID == "" || sessionKey(session) != s.outputForKey || text == "" && s.terminal == nil {
		return
	}
	renderState := ducklord.TerminalRenderState{Text: sanitizeTerminalText(text)}
	if s.terminal != nil && sessionKey(session) == s.outputForKey {
		framebuffer := s.terminal.SnapshotState()
		renderState.Framebuffer = &framebuffer
		renderState.Text = s.terminal.Text()
		renderState.RuntimeGeneration = s.terminalGeneration
		renderState.OutputOffset = s.terminalOffset
		renderState.ResumeCursorValid = s.terminalCursorValid
	}
	payload, err := ducklord.EncodeTerminalRenderState(renderState)
	if err == nil {
		err = s.snapshotStore.Save(ducklord.TerminalSnapshot{InstanceID: session.InstanceID, SessionID: session.SessionID, Payload: payload})
	}
	if err != nil {
		s.outputErr = "snapshot: " + err.Error()
	}
}

func sessionKey(sess ducklord.RemoteSession) string {
	if sess.InstanceID != "" && sess.SessionID != "" {
		return sess.Client + "/" + sess.InstanceID + "/" + sess.SessionID
	}
	return sess.Group + "/" + sess.Client + "/" + sess.Name
}

func terminalOutputKey(sess ducklord.RemoteSession) (ducklord.OutputKey, bool) {
	key := ducklord.OutputKey{ClientKey: sess.Client, InstanceID: sess.InstanceID, SessionID: sess.SessionID}
	return key, key.ClientKey != "" && key.InstanceID != "" && key.SessionID != "" && sess.RuntimeGeneration != 0
}

func canAttach(sess ducklord.RemoteSession) bool {
	return canRead(sess) && sess.Status == "running"
}

func canRead(sess ducklord.RemoteSession) bool {
	return sess.Error == "" && sess.Status != "error" && sess.Name != "(offline)"
}
