package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// inputDispatchAction runs the session-list input handler and dispatches its action.
func (l *tuiLoop) inputDispatchAction(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	ctx := l.ctx
	wasDragging := state.dragSession.Key() != ""
	action := state.handleInput(b)
	isDragging := state.dragSession.Key() != ""
	if wasDragging != isDragging {
		shape := "default"
		if isDragging {
			shape = "grabbing"
		}
		fmt.Fprint(os.Stdout, mouseCursorShape(shape))
	}
	switch action {
	case "quit":
		return inputExit
	case "help":
		state.dispatchHelpAction(action)
	case "project-files":
		state.openProjectFiles()
	case "shortcut-settings":
		state.beginShortcutSettings()
	case "notification-settings":
		state.beginNotificationConfig("global", "")
	case "host-actions":
		state.beginHostMenu()
	case "refresh":
		state.refreshSessions(ctx)
		if state.activeAttachKey == "" {
			l.requestPreview(true)
		}
	case "organize":
		state.cycleOrganizationMode()
	case "quick-sort":
		state.cycleQuickSort()
	case "quick-sort-direction":
		state.reverseQuickSortTime()
	case "groups":
		state.beginGroupMenu()
	case "reorder-up":
		state.moveSelectedSession(-1)
	case "reorder-down":
		state.moveSelectedSession(1)
	case "select":
		state.handleQuickSelection(&l.control, &l.controlDone, l.requestPreview)
	case "new":
		state.workspaceNewSessionIntent = nil
		state.beginCreate()
	case "search":
		state.beginSearch()
	case "notifications":
		state.beginNotificationSettings()
	case "copy-mode":
		state.enterCopyMode(os.Stdout)
		return inputDone
	case "actions":
		state.beginActionMenu()
	case "end", "restart", "destroy":
		if flow := l.actionConfirmLifecycle(f, action); flow != inputNext {
			return flow
		}
	case "add-client":
		state.beginAddClient()
	case "remove-client":
		state.beginRemoveClient()
	case "yield", "yield-wait":
		if flow := l.actionYield(f, action); flow != inputNext {
			return flow
		}
	case "attach":
		if flow := l.actionAttach(f); flow != inputNext {
			return flow
		}
	}
	state.render(os.Stdout)
	return inputNext
}

// actionAttach attaches to the selected Session through a PTY control writer.
func (l *tuiLoop) actionAttach(f *inputFrame) inputFlow {
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	runner := l.runner
	if l.control != nil && !state.focused {
		_ = l.control.Stdin.Close()
		l.control, l.controlDone = nil, nil
		state.clearAttachIdentity()
	}
	if len(state.sessions) == 0 {
		return inputDone
	}
	s := state.currentSession()
	if state.workspaceAttachFromProject || state.workspacePaneFocusRestorePending && state.workspacePaneFocusRestoreKey != "" {
		state.workspaceAttachFromProject = false
		if state.workspacePaneFocusRestorePending && state.workspacePaneFocusRestoreKey != "" {
			var ok bool
			s, ok = state.sessionForKey(state.workspacePaneFocusRestoreKey)
			if !ok {
				if state.workspacePaneFocusRestoreSession.Client == "" {
					state.outputErr = "focused Session disappeared before PTY restore"
					return inputNext
				}
				s = state.workspacePaneFocusRestoreSession
			}
		} else {
			var targetErr error
			s, targetErr = state.workspaceSelectedPaneSession()
			if targetErr != nil {
				state.outputErr = targetErr.Error()
				return inputNext
			}
		}
		state.activeAttachKey = sessionKey(s)
	}
	if state.workspacePreview {
		if _, visibleErr := state.workspacePaneRect(); visibleErr != nil {
			state.outputErr = "cannot focus hidden Session pane: " + visibleErr.Error()
			state.clearAttachIdentity()
			return inputNext
		}
	}
	if !canAttach(s) || !state.hostIsLive(s.Client) {
		if !state.hostIsLive(s.Client) {
			state.outputErr = "host is reconnecting; PTY controls are disabled"
		}
		state.clearAttachIdentity()
		return inputDone
	}
	c, err := mustClient(cfg, s.Client)
	if err != nil {
		f.err = err
		return inputExit
	}
	l.fencePreview()
	if l.outputManager != nil {
		l.openControlForSession(s)
		return inputNext
	}
	attachCtx, cancel := context.WithCancel(ctx)
	sessionRef := s.SessionID
	if sessionRef == "" {
		sessionRef = s.Name
	}
	var session *ducklord.AttachSession
	if resumer, ok := runner.(interface {
		AttachStreamFrom(context.Context, ducklord.Client, string, ducklord.AttachResume) (*ducklord.AttachSession, error)
	}); ok && state.terminal != nil && state.outputForKey == sessionKey(s) && state.terminalCursorValid {
		session, err = resumer.AttachStreamFrom(attachCtx, c, sessionRef, ducklord.AttachResume{RuntimeGeneration: state.terminalGeneration, OutputOffset: state.terminalOffset})
	} else {
		session, err = runner.AttachStream(attachCtx, c, sessionRef)
	}
	if err != nil {
		cancel()
		state.outputErr = err.Error()
		return inputNext
	}
	l.attach = session
	l.attachCancel = cancel
	state.resizeStatus = ""
	l.attachID++
	l.resizeInFlight = false
	l.pendingFramebufferResize = nil
	l.queuedResize = nil
	l.bufferedAttach = nil
	l.bufferedAttachBytes = 0
	l.attachOutputSource = l.attachOut
	id := l.attachID
	state.activeAttachKey = ""
	state.activeAttachFresh = false
	state.pendingAttachKey = sessionKey(s)
	l.attachCanResize = state.canResizeCurrentSession() && session.ResizeBarrier != nil
	l.attachInitialResizeQueued = false
	l.attachReplayEndOffset = session.ReplayEndOffset
	state.outputForKey = state.pendingAttachKey
	if !session.ExactResume {
		state.outputText = ""
		state.terminal = nil
		state.terminalGeneration = session.RuntimeGeneration
		state.terminalOffset = session.StartOffset
		state.terminalCursorValid = false
	} else {
		state.outputStale = false
	}
	state.outputErr = ""
	state.outputStale = false
	state.outputFresh = false
	// A detach modal can reopen the saved PTY while its focus lease is
	// still pending. Keep the UI unfocused until the control writer is
	// accepted; otherwise workspaceControlMayAccept rejects that writer
	// and the next input falls through to the Session list.
	state.focused = !state.workspacePaneFocusRestorePending
	go superviseAttach(attachCtx, id, session, l.attachOut)
	if initialReplayCaughtUp(session.StartOffset, session.ReplayEndOffset) && l.attachCanResize {
		l.attachInitialResizeQueued = true
		l.queueResize()
	}
	return inputNext
}

// actionYield yields PTY control back to the Session list.
func (l *tuiLoop) actionYield(f *inputFrame, action string) inputFlow {
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	runner := l.runner
	if len(state.sessions) == 0 {
		return inputNext
	}
	sess := state.sessions[state.selected]
	if !state.hostIsLive(sess.Client) {
		state.outputErr = "host is reconnecting; yield is disabled until session state is synchronized"
		return inputNext
	}
	client, err := mustClient(cfg, sess.Client)
	if err != nil {
		state.outputErr = err.Error()
		return inputNext
	}
	var result protocol.SessionYieldResult
	if f.selectedActionTarget != nil {
		result, err = runner.YieldSelected(ctx, client, *f.selectedActionTarget, action == "yield-wait")
	} else {
		ref := sess.SessionID
		if ref == "" {
			ref = sess.Name
		}
		result, err = runner.Yield(ctx, client, ref, action == "yield-wait")
	}
	if err != nil {
		state.outputErr = err.Error()
		return inputNext
	}
	state.refreshSessions(ctx)
	state.outputErr = fmt.Sprintf("yield %s (epoch %d)", result.Decision, result.OwnershipEpoch)
	return inputNext
}

// actionConfirmLifecycle opens the end/restart/destroy confirmation for the selected Session.
func (l *tuiLoop) actionConfirmLifecycle(f *inputFrame, action string) inputFlow {
	state := l.state
	if state.lifecycleBusy {
		state.outputErr = "another lifecycle operation is still pending; wait for its result"
		return inputNext
	}
	sess := state.currentSession()
	if sess.SessionID == "" || !state.hostIsLive(sess.Client) {
		state.outputErr = "session lifecycle is unavailable while the host is reconnecting"
		return inputNext
	}
	if sess.Kind == string(model.KindAgent) && (sess.WriterKind != string(model.OwnerTerminal) || sess.WriterID != state.ownerName) {
		state.outputErr = "read-only session; yield control to this Ducklord before changing its lifecycle"
		return inputNext
	}
	state.lifecycleConfirm = protocol.SessionLifecycleOperation(action)
	state.lifecycleReturnToAction = false
	state.lifecycleTarget = sess
	state.lifecycleMode = ""
	state.outputErr = ""
	return inputNext
}
