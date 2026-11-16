package main

import (
	"os"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (l *tuiLoop) onControlOpened(opened controlOpenEvent) {
	state := l.state
	paneVisible := true
	if state.workspacePreview {
		_, paneErr := state.workspacePaneRect()
		paneVisible = paneErr == nil
	}
	openedForCurrentAttach := opened.id == l.controlID && opened.key == state.activeAttachKey
	restoringFocusedPane := state.workspacePaneFocusRestorePending && openedForCurrentAttach
	restoreSession := state.activePTYSession()
	if restoringFocusedPane && state.workspacePaneFocusRestoreKey != "" {
		if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
			restoreSession = captured
		} else if state.workspacePaneFocusRestoreSession.Client != "" {
			restoreSession = state.workspacePaneFocusRestoreSession
		}
	}
	if state.workspacePaneMode || state.workspaceProjectFocus || !paneVisible && !restoringFocusedPane || !openedForCurrentAttach || opened.control != nil && !controlMatchesSession(opened.control, restoreSession, state.ownerName) {
		if opened.control != nil {
			_ = opened.control.Stdin.Close()
		}
		// A late result from an older attach must never clear the identity
		// captured by a newly placed quick-shell pane.  The id alone is not
		// sufficient when a replacement attach reuses the current lifecycle.
		if openedForCurrentAttach {
			if l.controlOpenCancel != nil {
				l.controlOpenCancel()
				l.controlOpenCancel = nil
			}
			l.clearWorkspacePlacementInput()
			state.clearAttachIdentity()
		}
		if !paneVisible && opened.id == l.controlID {
			state.clearAttachIdentity()
			state.focused = false
			state.outputErr = "Session pane became hidden; PTY control was not opened"
			state.render(os.Stdout)
		}
		return
	}
	l.controlOpenCancel = nil
	if opened.err != nil {
		state.outputErr = sanitizeTerminalText(opened.err.Error())
		if openedForCurrentAttach {
			l.clearWorkspacePlacementInput()
		}
		state.clearAttachIdentity()
		state.render(os.Stdout)
		return
	}
	if opened.control == nil {
		state.outputErr = "PTY control unavailable"
		if openedForCurrentAttach {
			l.clearWorkspacePlacementInput()
		}
		state.clearAttachIdentity()
		state.render(os.Stdout)
		return
	}
	l.control = opened.control
	l.controlDone = l.control.Done
	if l.workspaceOutput != nil {
		var restoreSession ducklord.RemoteSession
		if state.workspacePaneFocusRestorePending && state.workspacePaneFocusRestoreKey != "" {
			if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
				restoreSession = captured
			}
			if restoreSession.Client == "" {
				restoreSession = state.workspacePaneFocusRestoreSession
			}
		}
		if restoreSession.Client == "" {
			if !l.acceptWorkspaceControl() {
				// Keep the placement fence until the output lease is accepted.
				// Host Skills can restore pooled output before its replacement
				// writer; clearing here drops the first command after Ctrl-C.
				if l.control != nil {
					l.pendingControlSince = time.Now()
					state.outputErr = "waiting for Session pane output before PTY input"
				}
				state.render(os.Stdout)
				return
			}
		} else if !l.acceptWorkspaceControl(restoreSession) {
			// A restored focused pane may receive its control writer before
			// the output lease is ready. Keep the first post-Esc command
			// fenced until the lease is accepted; otherwise it falls through
			// to the Session list and is lost.
			if state.workspacePlacementInputPending && !restoringFocusedPane {
				l.clearWorkspacePlacementInput()
			}
			if l.control != nil {
				l.pendingControlSince = time.Now()
				state.outputErr = "waiting for Session pane output before PTY input"
			}
			state.render(os.Stdout)
			return
		}
		l.restorePendingFocus()
		drainNotesPendingInput(state, l.control)
		state.render(os.Stdout)
		return
	}
	state.focused = true
	l.replay.releaseWorkspacePlacement(state)
	l.attachCanResize = l.control.ResizeBarrier != nil
	state.outputErr = ""
	if state.sessionFreshlyDisplayed(state.activePTYSession()) {
		state.markActivitySeen(state.activePTYSession())
	}
	if l.resizeCapability() != nil {
		l.attachInitialResizeQueued = true
		l.queueResize()
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onControlClosed(controlErr error) {
	state := l.state
	l.controlDone = nil
	// The previous control stream can finish after Esc has handed the
	// saved Session back from the detach modal. Its completion belongs to
	// that stale stream; do not tear down the newly restored focus lease.
	if consumeWorkspaceRestoreControlDone(state) {
		// The restored writer itself may be the stream that just completed.
		// Reopen it against the captured Session before accepting keyboard
		// input; otherwise the UI advertises focus with no live writer.
		if state.activeAttachKey != "" {
			restoreSession := state.activePTYSession()
			if state.workspacePaneFocusRestoreKey != "" {
				if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
					restoreSession = captured
				}
			}
			state.workspacePaneFocusRestorePending = true
			state.workspacePaneFocusRestoreKey = sessionKey(restoreSession)
			state.workspacePaneFocusRestoreSession = restoreSession
			state.focused = false
			l.control = nil
			state.workspacePlacementInputPending = true
			l.openControlForSession(restoreSession)
		}
		return
	}
	l.control = nil
	l.attachCanResize = false
	if state.focused || l.workspaceOutput != nil && state.activeAttachKey != "" {
		state.focused = false
		state.clearAttachIdentity()
	}
	if controlErr != nil {
		state.outputErr = "PTY control connection closed"
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onResizeSignal() {
	state := l.state
	l.queueResize()
	state.render(os.Stdout)
}

func (l *tuiLoop) onResizeResult(result resizeDoneEvent) {
	state := l.state
	if result.id == l.attachID {
		l.resizeInFlight = false
		state.resizeStatus = ""
		if l.outputManager != nil {
			if result.err != nil {
				state.resizeStatus = "resize failed: " + sanitizeTerminalText(result.err.Error())
			}
			if l.queuedResize != nil {
				l.queuedResize = nil
				l.queueResize()
			}
			state.render(os.Stdout)
			return
		}
		if result.err != nil {
			state.resizeStatus = "resize failed: " + sanitizeTerminalText(result.err.Error())
		} else if state.terminal != nil && result.barrier > state.terminalOffset {
			l.pendingFramebufferResize = &result
			state.resizeStatus = "resize pending output drain"
		} else if state.terminal != nil {
			state.terminal.Resize(int(result.rows), int(result.cols))
		}
		completion := drainBufferedAttach(state, l.bufferedAttach, &l.pendingFramebufferResize)
		l.bufferedAttach = nil
		l.bufferedAttachBytes = 0
		l.attachOutputSource = l.attachOut
		if completion != nil {
			l.finishAttach(*completion)
		} else if l.pendingFramebufferResize == nil && l.queuedResize != nil {
			queued := *l.queuedResize
			l.queuedResize = nil
			if resize := l.resizeCapability(); resize != nil {
				l.resizeRequests <- resizeRequest{id: l.attachID, rows: queued[0], cols: queued[1], resize: resize}
				l.resizeInFlight = true
			}
		}
		state.render(os.Stdout)
	}
}

func (l *tuiLoop) onAttachOutput(chunk attachOutputEvent) {
	state := l.state
	if chunk.id != l.attachID {
		return
	}
	if l.resizeInFlight {
		l.bufferedAttach = append(l.bufferedAttach, chunk)
		l.bufferedAttachBytes += len(chunk.text)
		if l.bufferedAttachBytes >= maxResizeBufferedBytes {
			l.attachOutputSource = nil
		}
		return
	}
	if chunk.done {
		l.finishAttach(chunk)
		// A burst of PTY output can keep this select case ready. Avoid
		// adding another potentially blocking frame write when keyboard
		// input is already queued; the next iteration services input first.
		if len(l.input) == 0 {
			l.renderOutput()
		}
		return
	}
	applyOrderedAttachChunk(state, chunk, &l.pendingFramebufferResize)
	if !l.attachInitialResizeQueued && l.attachCanResize && initialReplayCaughtUp(state.terminalOffset, l.attachReplayEndOffset) {
		// Show the initial replay before waiting on the remote resize
		// barrier. Bytes emitted after this point remain ordered by the
		// existing barrier buffering path.
		l.attachInitialResizeQueued = true
		l.queuedResize = nil
		l.queueResize()
	}
	if l.pendingFramebufferResize == nil && l.queuedResize != nil {
		l.queuedResize = nil
		l.queueResize()
	}
	// Rendering each output chunk can monopolize the event loop while a
	// command such as `yes` is producing output. Let queued controls (in
	// particular Ctrl-C and the unfocus key) run before another frame.
	if len(l.input) == 0 {
		l.renderOutput()
	}
}
