package main

import (
	"context"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (l *tuiLoop) openControlForSession(s ducklord.RemoteSession) bool {
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	runner := l.runner
	if l.outputManager == nil || !canAttach(s) || !state.hostIsLive(s.Client) {
		return false
	}
	c, err := mustClient(cfg, s.Client)
	if err != nil {
		state.outputErr = err.Error()
		return false
	}
	l.fencePreview()
	state.focused = false
	state.activeAttachKey = sessionKey(s)
	if l.workspaceOutput != nil {
		l.selectPooledOutput()
	}
	state.activeAttachFresh = state.outputFresh
	state.pendingAttachKey = ""
	l.controlID++
	l.attachID++
	l.resizeInFlight = false
	l.queuedResize = nil
	id := l.controlID
	if !state.canResizeCurrentSession() {
		state.focused = true
		state.outputErr = "read-only PTY view; yield control before sending input"
		return false
	}
	controlRunner, ok := runner.(interface {
		OpenControlSession(context.Context, ducklord.Client, string) (*ducklord.ControlSession, error)
	})
	if !ok {
		state.outputErr = "PTY control is unavailable"
		state.clearAttachIdentity()
		return false
	}
	state.outputErr = "opening PTY control..."
	key := sessionKey(s)
	if l.controlOpenCancel != nil {
		l.controlOpenCancel()
	}
	controlCtx, cancelControlOpen := context.WithCancel(ctx)
	l.controlOpenCancel = cancelControlOpen
	go func() {
		opened, openErr := controlRunner.OpenControlSession(controlCtx, c, s.SessionID)
		select {
		case l.controlOpened <- controlOpenEvent{id: id, key: key, control: opened, err: openErr}:
		case <-ctx.Done():
			if opened != nil {
				_ = opened.Stdin.Close()
			}
		}
	}()
	return true
}

func (l *tuiLoop) resizeCapability() func(uint16, uint16) (uint64, error) {
	state := l.state
	var resize func(uint16, uint16) (uint64, error)
	expectedKey, keyOK := terminalOutputKey(state.activePTYSession())
	if l.outputManager != nil && l.control != nil && l.control.ResizeBarrier != nil && l.activeOutputEvent.Lease != 0 && keyOK && l.activeOutputEvent.Key == expectedKey &&
		l.activeOutputEvent.Revision.RuntimeGeneration == l.control.RuntimeGeneration && l.control.RuntimeGeneration == state.activePTYSession().RuntimeGeneration {
		event := l.activeOutputEvent
		controlSnapshot := l.control
		resize = func(rows, cols uint16) (uint64, error) {
			return l.outputManager.Resize(event, rows, cols, controlSnapshot.ResizeBarrier)
		}
	} else if l.attach != nil {
		resize = l.attach.ResizeBarrier
	}
	return resize
}

func (l *tuiLoop) queueResize() {
	state := l.state
	if !l.attachCanResize {
		return
	}
	resize := l.resizeCapability()
	if resize == nil {
		return
	}
	rows, cols := state.activePTYSize()
	if !l.attachInitialResizeQueued && !initialReplayCaughtUp(state.terminalOffset, l.attachReplayEndOffset) {
		// SIGWINCH may arrive while the initial replay is still being
		// consumed. Remember only the latest dimensions; starting a barrier
		// here would hide the replay until the network RPC returned.
		l.queuedResize = &[2]uint16{rows, cols}
		return
	}
	if l.resizeInFlight || l.pendingFramebufferResize != nil {
		l.queuedResize = &[2]uint16{rows, cols}
		return
	}
	request := resizeRequest{id: l.attachID, rows: rows, cols: cols, resize: resize}
	select {
	case l.resizeRequests <- request:
		l.resizeInFlight = true
	default:
		select {
		case <-l.resizeRequests:
		default:
		}
		select {
		case l.resizeRequests <- request:
			l.resizeInFlight = true
		default:
		}
	}
}

func (l *tuiLoop) resetWorkspacePaneChange() bool {
	state := l.state
	if !state.workspacePaneChanged {
		return false
	}
	state.workspacePaneChanged = false
	l.controlID++
	l.attachID++
	l.resizeInFlight = false
	l.pendingFramebufferResize = nil
	l.queuedResize = nil
	l.bufferedAttach = nil
	l.bufferedAttachBytes = 0
	l.attachOutputSource = l.attachOut
	l.attachCanResize = false
	l.attachInitialResizeQueued = false
	l.attachReplayEndOffset = 0
	if l.controlOpenCancel != nil {
		l.controlOpenCancel()
		l.controlOpenCancel = nil
	}
	if l.control != nil {
		_ = l.control.Stdin.Close()
		l.control, l.controlDone = nil, nil
	}
	if l.attachCancel != nil {
		l.attachCancel()
		l.attachCancel = nil
	}
	if l.attach != nil {
		_ = l.attach.Stdin.Close()
		l.attach = nil
	}
	state.clearAttachIdentity()
	state.focused = false
	if l.workspaceOutput != nil {
		l.selectPooledOutput()
	}
	return true
}

func (l *tuiLoop) resetWorkspacePaneChangeIfNeeded() bool {
	state := l.state
	// Closing a focused detach confirmation with Esc leaves the modal closed
	// while the exact source Session control is being rebound.  Do not let
	// the generic workspace reset invalidate that pending restore; once the
	// lease is accepted, normal changes can still reset as usual.
	if state.workspacePaneFocusRestorePending && !state.workspacePaneChanged {
		return false
	}
	// A focused Default detach confirmation temporarily owns the keyboard,
	// but must keep the originating PTY lease available for Esc restoration.
	// Confirming the detach closes the modal first, so the normal reset still
	// tears down the detached pane's lease.
	if state.workspacePaneMode && state.workspacePaneStep == "detach-confirm" && state.workspacePaneRestoreFocused {
		state.workspacePaneChanged = false
		return false
	}
	return l.resetWorkspacePaneChange()
}

func (l *tuiLoop) acceptWorkspaceControl(restored ...ducklord.RemoteSession) bool {
	state := l.state
	// A workspace modal owns input while it is open.  Async output/control
	// readiness must not reclaim the PTY focus that opening the modal
	// deliberately released; closeNotesModal restores it through the
	// deferred focus lease.
	if l.workspaceOutput == nil || l.control == nil || !state.workspaceControlMayAccept() {
		return false
	}
	session := state.activePTYSession()
	if len(restored) != 0 {
		session = restored[0]
	}
	// A focused Default detach modal is closed before its original PTY
	// control is restored, so the workspace pane rectangle is temporarily
	// unavailable.  The captured source session is authoritative for this
	// handoff; requiring the modal rectangle here strands Esc on the list.
	if (!state.workspacePaneFocusRestorePending && func() bool {
		_, err := state.workspacePaneRect()
		return err != nil
	}()) || !state.hostIsLive(session.Client) || !controlMatchesSession(l.control, session, state.ownerName) {
		_ = l.control.Stdin.Close()
		l.control, l.controlDone = nil, nil
		state.clearAttachIdentity()
		state.outputErr = "Session pane or writer changed before PTY control became ready"
		return false
	}
	if !acceptWorkspaceControlInput(state, l.workspaceOutput, l.control, session, &l.replay) {
		return false // the visible raw-output lease may still be opening
	}
	l.pendingControlSince = time.Time{}
	l.attachCanResize = l.control.ResizeBarrier != nil
	if nav, err := state.workspaceNavigation(); err == nil {
		width, height := terminalSize()
		_, _ = nav.FocusVisiblePane(ducklord.CalculateWorkspaceGeometry(width, height, 4))
	}
	state.outputErr = ""
	if state.sessionFreshlyDisplayed(session) {
		state.markActivitySeen(session)
	}
	if l.resizeCapability() != nil && !l.attachInitialResizeQueued {
		l.attachInitialResizeQueued = true
		l.queueResize()
	}
	return true
}

func (l *tuiLoop) restorePendingFocus() bool {
	state := l.state
	restored := restorePendingNotesFocus(state, l.workspaceOutput, state.outputFresh)
	// The output lease and the PTY writer become ready independently.  Help
	// must not advertise terminal focus until the writer has arrived as well;
	// otherwise the first byte after closing the overlay is silently discarded
	// while control is still nil.
	restored = restorePendingHelpFocusWithAttach(state, l.workspaceOutput, state.outputFresh, l.control, l.attach) || restored
	if restorePendingWorkspacePaneFocus(state, l.workspaceOutput, state.outputFresh, l.control) {
		restored = true
	}
	return restored
}

func (l *tuiLoop) finishAttach(event attachOutputEvent) {
	state := l.state
	// Closing a focused Default detach modal can restore the existing
	// control lease while the old attach stream is still finishing.  Its
	// terminal event belongs to that old stream and must not tear down the
	// focus that Esc just returned to the source Session.
	preserveRestoredFocus := state.workspacePaneFocusRestoreLease || state.workspacePaneFocusRestorePending
	l.attachID++
	l.resizeInFlight = false
	l.pendingFramebufferResize = nil
	l.queuedResize = nil
	l.bufferedAttach = nil
	l.bufferedAttachBytes = 0
	l.attachOutputSource = l.attachOut
	wasFocused := state.focused
	if !preserveRestoredFocus {
		state.focused = false
		state.clearAttachIdentity()
	}
	l.attachCanResize = false
	l.attachInitialResizeQueued = false
	l.attachReplayEndOffset = 0
	l.attach = nil
	if l.attachCancel != nil {
		l.attachCancel()
		l.attachCancel = nil
	}
	l.requestPreview(true)
	if event.err != nil && wasFocused {
		state.outputErr = event.err.Error()
	}
}
