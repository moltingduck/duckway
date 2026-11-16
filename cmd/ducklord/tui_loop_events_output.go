package main

import (
	"os"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (l *tuiLoop) onWorkspaceRepaint() {
	state := l.state
	if l.control != nil && !state.focused {
		if state.workspacePaneFocusRestorePending {
			restoreSession := state.activePTYSession()
			if state.workspacePaneFocusRestoreKey != "" {
				if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
					restoreSession = captured
				} else if state.workspacePaneFocusRestoreSession.Client != "" {
					restoreSession = state.workspacePaneFocusRestoreSession
				}
			}
			if l.acceptWorkspaceControl(restoreSession) {
				l.restorePendingFocus()
			}
		} else {
			l.acceptWorkspaceControl()
		}
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onSearchActivationTimeout(requestID uint64) {
	state := l.state
	if state.searchMode && state.searchPendingRequestID == requestID {
		state.searchPendingRequestID = 0
		state.searchErr = "PTY activation timed out; try again"
		l.selectPooledOutput()
		state.render(os.Stdout)
	}
}

func (l *tuiLoop) onOutputEvent(event ducklord.TerminalOutputEvent) {
	state := l.state
	expectedSession := state.activePTYSession()
	if l.workspaceOutput != nil {
		expectedSession = l.selectedOutputSession
	}
	if state.searchMode && state.searchActivatedKey != "" {
		if activated, ok := state.sessionForKey(state.searchActivatedKey); ok {
			expectedSession = activated
		}
	}
	searchActivation := state.searchMode && state.searchPendingRequestID != 0 && event.RequestID == state.searchPendingRequestID
	if searchActivation {
		if state.searchPendingRevision != state.searchRevision || !state.selectableSessionMatches(state.searchPendingKey, state.searchPendingGeneration) {
			state.searchPendingRequestID = 0
			state.searchErr = "session changed while switching; try again"
			l.selectPooledOutput()
			state.render(os.Stdout)
			return
		}
		expectedSession, _ = state.sessionForKey(state.searchPendingKey)
	}
	expectedKey, expectedOK := terminalOutputKey(expectedSession)
	if searchActivation && (!expectedOK || event.Key != expectedKey || event.Revision.RuntimeGeneration != expectedSession.RuntimeGeneration) {
		state.searchPendingRequestID = 0
		state.searchErr = "PTY activation returned stale session output; try again"
		l.selectPooledOutput()
		state.render(os.Stdout)
		return
	}
	if event.RequestID != l.outputRequestID || !expectedOK || event.Key != expectedKey || event.Revision.RuntimeGeneration != expectedSession.RuntimeGeneration || event.Err != nil {
		if event.RequestID == l.outputRequestID && event.Err != nil {
			state.outputReconnecting = false
			if event.RequestID == l.reconnectRequestID {
				l.reconnectRequestID = 0
				state.outputStale = true
				state.outputErr = "PTY reconnect failed; previous live view retained"
				state.render(os.Stdout)
				return
			}
			if searchActivation {
				state.searchPendingRequestID = 0
				state.searchErr = "PTY output stream unavailable"
				l.selectPooledOutput()
			} else {
				state.outputFresh = false
				state.outputErr = "PTY output stream unavailable"
			}
			state.render(os.Stdout)
		}
		return
	}
	var view ducklord.PooledTerminalView
	var viewErr error
	if event.FinalView != nil {
		view = *event.FinalView
	} else {
		view, viewErr = l.outputManager.View(event)
		if viewErr != nil {
			if searchActivation {
				state.searchPendingRequestID = 0
				state.searchErr = "PTY framebuffer is unavailable; try again"
				l.selectPooledOutput()
				state.render(os.Stdout)
			}
			return
		}
	}
	if searchActivation && (!view.Ready || view.Disconnected || view.Ended) {
		state.searchPendingRequestID = 0
		state.searchErr = "PTY output is not ready; try again"
		l.selectPooledOutput()
		state.render(os.Stdout)
		return
	}
	terminal, valid := ducklord.NewTerminalFromState(view.Framebuffer, ducklord.DefaultTerminalScrollback)
	if !valid {
		if searchActivation {
			state.searchPendingRequestID = 0
			state.searchErr = "PTY framebuffer is invalid"
			l.selectPooledOutput()
		} else {
			state.outputFresh = false
			state.outputErr = "PTY framebuffer is invalid"
		}
		state.render(os.Stdout)
		return
	}
	if searchActivation && !state.prepareSearchWorkspaceSelection(state.searchPendingKey) {
		l.selectPooledOutput()
		state.render(os.Stdout)
		return
	}
	state.terminal = terminal
	state.ptyScrollOffset = 0
	l.activeOutputEvent = event
	l.activeOutputEvent.FinalView = nil
	state.outputText = terminal.Text()
	state.terminalGeneration = view.RuntimeGeneration
	state.terminalOffset = view.OutputOffset
	state.terminalCursorValid = view.Ready && !view.Disconnected
	state.outputStale = false
	state.outputReconnecting = false
	if event.RequestID == l.reconnectRequestID {
		l.reconnectRequestID = 0
	}
	state.outputFresh = view.Ready && !view.Disconnected
	state.outputErr = view.Error
	if view.Ended {
		state.outputErr = "PTY process ended"
	}
	if state.outputFresh {
		l.restorePendingFocus()
		drainNotesPendingInput(state, l.control)
	}
	if searchActivation {
		state.outputForKey = state.searchPendingKey
		state.activeAttachKey = state.searchPendingKey
		state.activeAttachFresh = state.outputFresh
		state.searchActivatedKey = state.searchPendingKey
		state.searchActivatedRevision = state.searchPendingRevision
		state.searchActivatedGeneration = state.searchPendingGeneration
		state.searchPendingRequestID = 0
		state.searchErr = "Active · Enter again to focus"
	}
	if l.control != nil && !state.focused {
		l.acceptWorkspaceControl()
	}
	if state.sessionFreshlyDisplayed(state.activePTYSession()) {
		state.markActivitySeen(state.activePTYSession())
	}
	if state.focused && l.control != nil && !l.attachInitialResizeQueued && l.resizeCapability() != nil {
		l.attachInitialResizeQueued = true
		l.queueResize()
	}
	l.renderOutput()
}

func (l *tuiLoop) onPreviewDone(result previewOutputEvent) {
	state := l.state
	if l.previewCancel != nil {
		l.previewCancel()
		l.previewCancel = nil
	}
	l.previewInFlight = false
	if state.applyPreviewOutput(result, l.previewID) {
		state.render(os.Stdout)
	}
	l.startPreview()
}
