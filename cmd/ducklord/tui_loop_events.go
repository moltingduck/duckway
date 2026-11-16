package main

import (
	"os"
	"time"
)

func (l *tuiLoop) onPanePrefixTimer() {
	state := l.state
	if state.panePrefixPending && state.panePrefixSuffix != "" && !time.Now().Before(state.panePrefixDeadline) {
		command := state.panePrefixSuffix
		state.panePrefixPending, state.panePrefixSuffix = false, ""
		state.panePrefixDeadline = time.Time{}
		state.dispatchPaneCommand(command)
		if pending := state.takePanePrefixReplay(); len(pending) != 0 {
			l.replay.input <- pending
			l.replay.queued = true
		}
		state.render(os.Stdout)
	}
}

func (l *tuiLoop) shutdown() {
	state := l.state
	state.cleanupHostSkills()
	state.cancelAddClientWork()
	if l.controlOpenCancel != nil {
		l.controlOpenCancel()
	}
	if l.control != nil {
		_ = l.control.Stdin.Close()
	}
	if l.attachCancel != nil {
		l.attachCancel()
	}
	if l.startCancel != nil {
		l.startCancel()
	}
	state.cancelCreateDiscovery()
}

func (l *tuiLoop) onTick() {
	state := l.state
	ctx := l.ctx
	// Start and inventory are separate RPCs. While a new pane is
	// awaiting its Session, poll even in event-driven mode so a missed
	// update cannot strand the placement or its deadline forever.
	hadPendingPlacement := len(state.pendingWorkspacePlacements) > 0
	if hadPendingPlacement {
		state.refreshSessions(ctx)
	}
	if l.workspaceOutput != nil && l.control != nil && !state.focused {
		if !l.pendingControlSince.IsZero() && time.Since(l.pendingControlSince) >= 15*time.Second {
			_ = l.control.Stdin.Close()
			l.control, l.controlDone = nil, nil
			state.clearAttachIdentity()
			state.outputErr = "PTY focus timed out waiting for Session output"
		} else {
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
		return
	}
	if !state.focused && !state.newSessionMode && !state.searchMode {
		if !state.eventDriven && !hadPendingPlacement {
			state.refreshSessions(ctx)
		}
		l.requestPreview(false)
	}
	state.render(os.Stdout)
}

// queueDeferredReplays runs at the top of every loop iteration, before the
// select, and queues local replays that must precede newer keyboard bytes.
func (l *tuiLoop) queueDeferredReplays() {
	state := l.state
	// An earlier local replay can occupy the single replay slot when a
	// writer/output completion accepts focus. Retry after that replay is
	// consumed, before selecting any newer keyboard bytes.
	l.replay.retryWorkspacePlacement(state, l.control)
	// Inventory placement selects a newly created Session asynchronously. Queue
	// the same local attach action used by Enter so the selected pane acquires
	// a PTY control lease before the next user bytes are handled.
	if state.workspacePlacementFocusPending && !state.workspacePaneFocusRestorePending && !l.replay.queued {
		state.workspacePlacementFocusPending = false
		// A quick-shell placement replaces the pane owned by the current
		// control session. Release that old writer before replaying the local
		// attach action; otherwise the replayed Enter is delivered to the old
		// PTY while activeAttachKey already names the newly created Session.
		if l.control != nil {
			l.controlID++
			if l.controlOpenCancel != nil {
				l.controlOpenCancel()
				l.controlOpenCancel = nil
			}
			_ = l.control.Stdin.Close()
			l.control = nil
			l.controlDone = nil
		}
		state.focused = false
		if state.clearOutputFocus != nil {
			state.clearOutputFocus()
		}
		// Placement changes the selected workspace pane after the output
		// manager has already selected the old pane.  Select the created
		// Session before opening its writer lease, otherwise the lease
		// completion cannot transfer raw input focus to the new PTY.
		l.selectPooledOutput()
		l.replay.input <- []byte("\r")
		l.replay.queued = true
	}
	// A prefix command can finish while Help's asynchronous PTY focus lease
	// is still pending.  Keep it queued until the lease restores terminal
	// focus; dispatching it immediately would apply the command to the
	// navigation pane.
	if state.focused && !state.helpFocusRestorePending && !l.replay.queued && len(state.panePrefixReplay) != 0 {
		l.replay.input <- state.takePanePrefixReplay()
		l.replay.queued = true
	}
}
