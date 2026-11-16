package main

import (
	"os"
	"strings"
)

// inputPendingFocus restores a pending notes/help/pane focus lease.
func (l *tuiLoop) inputPendingFocus(f *inputFrame) inputFlow {
	state := l.state
	if !f.workspaceNavigationReplay && (state.notesFocusRestorePending || state.helpFocusRestorePending || state.workspacePaneFocusRestorePending) && state.outputFresh {
		l.restorePendingFocus()
		drainNotesPendingInput(state, l.control)
	}
	return inputNext
}

// inputPendingPTY feeds keys to a PTY control that is still opening.
func (l *tuiLoop) inputPendingPTY(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	// Deferred workspace keys are replayed only after the replacement
	// control lease is ready. Keep them on the workspace navigation route;
	// passing them through the pending PTY gate would consume navigation
	// keys such as b before handleWorkspaceProjectInput sees them.
	pendingHandled := false
	if !f.workspaceNavigationReplay && !f.workspaceProjectNavigation {
		pendingHandled = handlePendingPTYInput(state, b, l.workspaceOutput != nil, &l.control, &l.controlOpenCancel, &l.controlID)
	}
	if pendingHandled {
		// The writer may be ready even when the last output frame is stale.
		// Confirm the Help lease before interpreting a completed prefix.
		if !state.focused && state.helpFocusRestorePending {
			restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
		}
		// Remember a prefix even if the writer opens between its two keys.
		if consumed, command := state.handlePanePrefix(b); consumed && command != "" {
			// Command palette is a local modal even when the prefix arrived
			// while a focused PTY control was being handed off.  Open it before
			// the generic focused-command path tears down PTY ownership; the
			// suffix must never be replayed to the shell.
			if command == " " {
				state.openCommandPalette()
				state.render(os.Stdout)
				return inputDone
			}
			// A prefix command completed while PTY focus was still opening
			// must remain a local UI action. In particular, never forward
			// Ctrl-B followed by ? or o to the PTY.
			if paneNavigationCommand(command) {
				// Control readiness can race the two bytes of a focused
				// prefix command.  Preserve the focused handoff contract even
				// when the pending-input gate has just observed the writer as
				// unavailable; dispatchPaneCommand would leave the selection
				// in the list and let the next PTY sentinel be consumed there.
				if state.beginFocusedPaneNavigation(command) && !l.replay.queued {
					l.replay.input <- []byte("\r")
					l.replay.queued = true
				}
			} else if !state.focused && state.helpFocusRestorePending {
				state.panePrefixReplay = append([]byte(shortcutInput(state.cfg.Shortcut("pane_prefix"))), []byte(command)...)
			} else if state.focused && state.dispatchFocusedPaneCommand(command) {
				// A direct focused detach bypasses the workspace modal branch.
				// Run the same lease teardown, then replay Enter so the selected
				// surviving pane opens its control writer.
				if l.resetWorkspacePaneChangeIfNeeded() && state.workspaceAttachFromProject && !l.replay.queued {
					// The surviving pane is selected synchronously, but its
					// control lease opens asynchronously. Hold real shell input
					// until that lease has been accepted.
					state.workspacePlacementInputPending = true
					l.replay.toPTY = true
					l.replay.deferred = nil
					l.replay.input <- []byte("\r")
					l.replay.queued = true
				}
			} else {
				state.dispatchPaneCommand(command)
			}
		}
		if l.control == nil {
			l.controlDone = nil
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputDirectNotes handles direct notes input.
func (l *tuiLoop) inputDirectNotes(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	if state.handleDirectNotesInput(b) {
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputProjectFiles handles the project files browser.
func (l *tuiLoop) inputProjectFiles(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	// Retire a workspace scrollbar capture before any modal route can
	// consume this report without reaching handleWorkspaceMouse.
	state.prepareWorkspaceMouseDispatch(b)
	// Project files owns mouse reports before byte-oriented modal input. This
	// prevents the SGR sequence from being swallowed as text or reaching PTY.
	if state.projectFiles.open && strings.HasPrefix(string(b), "\x1b[<") {
		if button, x, y, ok := parseSGRMouse(string(b)); ok {
			if button == 0 && strings.HasSuffix(string(b), "m") {
				state.projectFilesMouseRelease(x, y)
			} else if button == 0 && strings.HasSuffix(string(b), "M") {
				state.modalMouseInput(x, y)
			}
			state.render(os.Stdout)
			return inputDone
		}
	}
	if state.handleProjectFilesInput(b) {
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputCopyMode handles terminal copy mode.
func (l *tuiLoop) inputCopyMode(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	if state.copyMode {
		if copyModeExitInput(b) {
			state.exitCopyMode(os.Stdout)
			if state.sessionFreshlyDisplayed(state.currentSession()) {
				state.markActivitySeen(state.currentSession())
			}
			state.render(os.Stdout)
		} else if string(b) == "\x1b[A" || string(b) == "k" {
			if state.viewportTerminal() != nil {
				state.scrollPTY(1)
				state.renderCopyMode(os.Stdout)
			}
		} else if string(b) == "\x1b[B" || string(b) == "j" {
			state.scrollPTY(-1)
			state.renderCopyMode(os.Stdout)
		}
		return inputDone
	}
	return inputNext
}

// inputFocusedTerminal forwards keys to the focused PTY control.
func (l *tuiLoop) inputFocusedTerminal(f *inputFrame) inputFlow {
	state := l.state
	ctx := l.ctx
	if state.focused {
		if state.workspacePreview {
			if _, visibleErr := state.workspacePaneRect(); visibleErr != nil {
				l.controlID++
				l.attachID++
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
				state.focused = false
				state.clearAttachIdentity()
				state.outputErr = "Session pane is no longer visible; input was not sent: " + visibleErr.Error()
				state.render(os.Stdout)
				return inputDone
			}
		}
		if !state.hostIsLive(state.activePTYSession().Client) {
			state.focused = false
			state.clearAttachIdentity()
			state.outputFresh = false
			state.outputErr = "host reconnecting; input was not sent"
			state.render(os.Stdout)
			return inputDone
		}
		// Ctrl-] is the built-in PTY escape. Keep it recognized even if
		// an older or incomplete config omits the shortcut entry; under a
		// saturated PTY the escape must never be forwarded to the shell.
		if string(f.b) == "\x1d" || state.shortcut("pty_unfocus", string(f.b)) {
			if l.outputManager == nil {
				state.saveCurrentSnapshot()
			}
			l.controlID++
			if l.controlOpenCancel != nil {
				l.controlOpenCancel()
				l.controlOpenCancel = nil
			}
			if l.control != nil {
				_ = l.control.Stdin.Close()
			}
			l.control, l.controlDone = nil, nil
			if l.attach != nil {
				_ = l.attach.Stdin.Close()
			}
			if l.attachCancel != nil {
				l.attachCancel()
				l.attachCancel = nil
			}
			l.attachID++
			l.resizeInFlight = false
			l.pendingFramebufferResize = nil
			l.queuedResize = nil
			l.bufferedAttach = nil
			l.bufferedAttachBytes = 0
			l.attachOutputSource = l.attachOut
			state.focused = false
			state.clearAttachIdentity()
			// A busy PTY may have advanced the terminal while the cached
			// frame still describes the focused view. Force a complete
			// navigation repaint when returning from terminal focus.
			state.frameOutput = frameOutput{}
			if state.workspaceNav != nil && state.workspaceNav.InDetailMode() {
				state.syncDetailSelection()
			}
			l.attachCanResize = false
			l.attachInitialResizeQueued = false
			l.attachReplayEndOffset = 0
			l.attach = nil
			if paneNavigationCommand(f.paneCommand) {
				if !state.beginFocusedPaneNavigation(f.paneCommand) {
					state.render(os.Stdout)
					return inputDone
				}
				l.requestPreview(true)
				f.b = []byte("\r")
			} else {
				if strings.HasPrefix(f.paneCommand, "quick-shell:") {
					state.beginQuickShell(ctx, l.createDone, strings.TrimPrefix(f.paneCommand, "quick-shell:"))
					state.render(os.Stdout)
					return inputDone
				}
				l.requestPreview(true)
				if f.paneCommand != "" {
					state.dispatchPaneCommand(f.paneCommand)
				}
				l.queuePaneReplay()
				state.render(os.Stdout)
				return inputDone
			}
		}
		if state.focused {
			activeSession := state.activePTYSession()
			if writer := focusedPTYWriter(l.control, l.attach, activeSession, state.ownerName); writer != nil {
				_, _ = writer.Write(f.b)
			} else if l.control != nil {
				l.controlID++
				if l.controlOpenCancel != nil {
					l.controlOpenCancel()
				}
				_ = l.control.Stdin.Close()
				l.control, l.controlDone = nil, nil
				l.attachCanResize = false
				state.outputErr = "PTY control changed; input was not sent"
			}
			return inputDone
		}
	}
	return inputNext
}
