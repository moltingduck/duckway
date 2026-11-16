package main

import (
	"os"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// inputCentralModalCancel handles Esc/Ctrl-C for central modals, including detach-confirm focus restore.
func (l *tuiLoop) inputCentralModalCancel(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	// Central modals take ownership of Esc/Ctrl-C even when they were
	// opened from a focused terminal. Handle this before the pending
	// PTY gate so modal cancellation cannot be consumed by it.
	if state.handleCentralModalCancel(b) {
		if state.workspacePaneFocusRestorePending {
			// The original control lease is still valid when a focused
			// terminal opens the detach modal.  Keep that writer and hand
			// its visible output lease back immediately; closing it here
			// creates a needless asynchronous gap that can drop the first
			// command after Esc.
			restoreSession := state.activePTYSession()
			if state.workspacePaneFocusRestoreKey != "" {
				if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
					restoreSession = captured
				}
				if restoreSession.Client == "" {
					restoreSession = state.workspacePaneFocusRestoreSession
				}
			}
			if l.control != nil && controlMatchesSession(l.control, restoreSession, state.ownerName) {
				state.focused = false
				// Keep the saved snapshot pending until the visible output lease
				// accepts input focus. The control writer may be ready first.
				if l.acceptWorkspaceControl(restoreSession) {
					l.restorePendingFocus()
				}
				state.notesFocusRestoreReselect = false
				state.render(os.Stdout)
				return inputDone
			}
			// The modal may have been opened while the previous control
			// lease was still connecting. Cancel that stale request before
			// replaying Enter, otherwise the replay is consumed by the
			// pending-input gate instead of reopening the saved PTY.
			// A replacement control really is asynchronous, so fence the
			// first command only on this path.
			state.workspacePlacementInputPending = true
			l.replay.deferred = nil
			if l.controlOpenCancel != nil {
				l.controlOpenCancel()
				l.controlOpenCancel = nil
				l.controlID++
			}
			if l.control != nil {
				_ = l.control.Stdin.Close()
				l.control = nil
				l.controlDone = nil
			}
			state.focused = false
			// Keep the input fence up until the restored Session pane's
			// control writer is accepted. Reopen the exact source Session
			// directly; routing Esc through Project would consume the user's
			// first shell command.
			state.workspacePlacementInputPending = true
			l.replay.deferred = nil
			if restoreSession.Client == "" {
				state.outputErr = "focused Session disappeared before PTY restore"
				state.workspacePaneFocusRestorePending = false
			} else {
				l.openControlForSession(restoreSession)
			}
		}
		state.notesFocusRestoreReselect = false
		if !state.focused && !state.workspacePaneFocusRestorePending {
			l.selectPooledOutput()
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputCommandPalette handles the command palette and its resulting actions.
func (l *tuiLoop) inputCommandPalette(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	ctx := l.ctx
	if state.handleCommandPaletteInput(b) {
		switch state.commandPaletteAction {
		case "host-add":
			state.commandPaletteAction = ""
			state.beginAddClient()
		case "create-session":
			state.commandPaletteAction = ""
			state.workspaceNewSessionIntent = nil
			state.beginCreate()
		case "refresh":
			state.commandPaletteAction = ""
			state.refreshSessions(ctx)
			if state.activeAttachKey == "" {
				l.requestPreview(true)
			}
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputWorkspacePaneModal routes keys to the workspace pane modal.
func (l *tuiLoop) inputWorkspacePaneModal(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	// A workspace pane modal owns keyboard input for its entire
	// lifetime, including when it was opened from a focused terminal.
	// Route this before the pending PTY gate and focused-terminal path so
	// navigation and confirmation keys cannot leak to the shell. Mouse
	// reports continue through the modal hit-test below.
	if state.workspacePaneMode && !strings.HasPrefix(string(b), "\x1b[<") {
		notesInput := state.workspacePaneStep == "notes"
		if state.handleWorkspacePaneInput(b) && !notesInput && state.workspacePaneStep != "notes" {
			// The create wizard takes ownership of the next byte immediately.
			// A focused Default pane may still have a deferred restore lease;
			// leave that lease attached to the workspace modal instead of
			// allowing it to consume the host selector's first Enter.
			state.workspacePaneFocusRestorePending = false
			state.workspacePaneRestoreFocused = false
			state.workspacePlacementInputPending = false
			l.replay.deferred = nil
			state.beginCreate()
		}
		// Esc from a focused detach confirmation is handled by the
		// workspace-pane dispatcher, so hand the still-matching control
		// lease back immediately just as the central modal path does.
		if state.workspacePaneFocusRestorePending {
			restoreSession := state.activePTYSession()
			if state.workspacePaneFocusRestoreKey != "" {
				if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
					restoreSession = captured
				}
				if restoreSession.Client == "" {
					restoreSession = state.workspacePaneFocusRestoreSession
				}
			}
			if l.control != nil && controlMatchesSession(l.control, restoreSession, state.ownerName) {
				state.focused = false
				// The saved restore snapshot remains authoritative until the
				// output focus setter accepts the handoff.
				if l.acceptWorkspaceControl(restoreSession) {
					l.restorePendingFocus()
				}
			} else {
				// The modal can close while the original writer is still
				// opening (or after it was detached). Reopen the exact
				// captured Session and keep the first shell command fenced
				// until that replacement writer is accepted.
				state.workspacePlacementInputPending = true
				l.replay.deferred = nil
				if l.controlOpenCancel != nil {
					l.controlOpenCancel()
					l.controlOpenCancel = nil
					l.controlID++
				}
				if l.control != nil {
					_ = l.control.Stdin.Close()
					l.control = nil
					l.controlDone = nil
				}
				state.focused = false
				if restoreSession.Client == "" {
					state.outputErr = "focused Session disappeared before PTY restore"
					state.workspacePaneFocusRestorePending = false
				} else {
					l.openControlForSession(restoreSession)
				}
			}
		}
		l.resetWorkspacePaneChangeIfNeeded()
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputHelpMode routes keys to Help when it owns the keyboard.
func (l *tuiLoop) inputHelpMode(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	// Help owns textual input for its entire lifetime, including while
	// focus restoration is pending or has already completed. Handle this
	// before the pending PTY gate so a focused close key cannot leak to the
	// originating shell. Mouse reports continue through the modal hit-test
	// below.
	if state.helpMode && !state.blockingModalOpen() && !strings.HasPrefix(string(b), "\x1b[<") {
		if state.helpSearchActive {
			if state.shortcut("help", string(b)) {
				state.closeHelp()
				if !state.helpMode {
					restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
				}
			} else if !state.scrollHelpInput(b) {
				state.handleHelpSearchInput(b)
			}
		} else if state.shortcut("help", string(b)) {
			state.dispatchHelpAction("help")
			if !state.helpMode {
				restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
			}
		} else if string(b) == "/" {
			state.helpSearchActive = true
		} else {
			state.scrollHelpInput(b)
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputHelpSearch handles Help search entry.
func (l *tuiLoop) inputHelpSearch(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	if state.helpSearchActive && !state.blockingModalOpen() {
		if state.shortcut("help", string(b)) {
			state.closeHelp()
			if !state.helpMode {
				// Closing Help must release its input fence even when the
				// last framebuffer event was marked stale. The visible output
				// lease can still be valid, and waiting for another event would
				// consume the first command sent to the originating PTY.
				restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
			}
		} else {
			state.handleHelpSearchInput(b)
		}
		state.render(os.Stdout)
		return inputDone
	}
	if state.helpMode && !state.focused && !state.blockingModalOpen() && string(b) == "/" && !state.shortcut("help", string(b)) {
		state.helpSearchActive = true
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputModalArrowAndHelp discards stray modal arrows and routes Help keys.
func (l *tuiLoop) inputModalArrowAndHelp(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	if state.shouldDiscardModalArrow(b) {
		state.render(os.Stdout)
		return inputDone
	}
	// Help is a pinned overlay: only its configured binding may toggle it.
	// Esc, quit, and Ctrl-C do not dismiss it.
	if state.helpMode && !state.blockingModalOpen() {
		if state.shortcut("help", string(b)) {
			state.dispatchHelpAction("help")
			if !state.helpMode {
				// See the search-close path above: focus restoration depends
				// on the lease, rather than on a fresh repaint arriving after
				// the close key.
				restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
			}
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputCentralModalCtrlC handles Ctrl-C inside central modals.
func (l *tuiLoop) inputCentralModalCtrlC(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	if string(b) == "\x03" && state.centralModalOpen() {
		switch {
		case state.projectFiles.open:
			state.handleProjectFilesInput(b)
		case state.workspacePaneMode:
			if state.workspacePaneStep == "notes" {
				state.closeNotesModal()
			} else {
				state.closeWorkspacePane()
			}
		case state.shortcutMode:
			state.shortcutMode = false
		case state.notificationConfigMode:
			state.closeNotificationConfig()
		case state.searchMode:
			state.closeSearch()
		case state.terminalSearchMode, state.terminalBookmarkMode, state.terminalBookmarkListMode:
			state.closeTerminalTool()
		case state.hostMenuMode:
			if strings.HasPrefix(state.hostMenuStep, "skills-") {
				state.closeHostMenuWithCtrlC()
				// Host Skills is opened from the workspace navigation route while
				// the existing PTY control remains alive. Restore that exact
				// output key here so the next byte reaches the original shell.
				if l.workspaceOutput != nil && state.activeAttachKey != "" {
					if key, ok := terminalOutputKey(state.activePTYSession()); ok && setWorkspaceInputFocus(l.workspaceOutput, key) {
						// The pooled output lease can be restored before the PTY
						// writer. Keep keyboard ownership fenced until both are
						// ready; otherwise the first command after Ctrl-C is
						// accepted visually and dropped by a nil writer.
						state.focused = false
						state.workspacePlacementInputPending = true
					}
				}
				// Keep an already-matching writer. Host Skills normally leaves
				// that writer alive while its pooled output lease is restored;
				// closing it here creates a replacement window in which the next
				// shell command can be lost.
				restoreSession := state.activePTYSession()
				if l.control != nil && controlMatchesSession(l.control, restoreSession, state.ownerName) {
					l.acceptWorkspaceControl()
				} else {
					if l.control != nil {
						_ = l.control.Stdin.Close()
						l.control, l.controlDone = nil, nil
					}
					if state.activeAttachKey != "" {
						_ = l.openControlForSession(restoreSession)
					}
				}
			} else {
				state.hostMenuMode = false
			}
		case state.addClientMode:
			state.cancelAddClient()
		case state.removeClientMode:
			state.cancelRemoveClient()
		case state.newSessionMode:
			if l.startCancel != nil {
				l.startCancel()
				l.startCancel = nil
				l.startID++
			}
			state.cancelCreate()
		case state.notificationMode:
			state.closeNotificationSettings()
		case state.groupMenu:
			state.closeGroupMenu()
		case state.actionMenu:
			state.closeActionMenu()
		case state.sessionRenameMode:
			state.closeSessionHandleRename()
		case state.lifecycleConfirm != "":
			state.lifecycleConfirm = ""
			state.lifecycleReturnToAction = false
			state.lifecycleTarget = ducklord.RemoteSession{}
			state.lifecycleMode = ""
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputSettingsModals handles the legacy-restart, shortcut and notification config modals.
func (l *tuiLoop) inputSettingsModals(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	if !state.workspacePaneMode && state.workspacePreview {
		if state.syncCurrentNotes() {
			if state.handleNotesInput(b) {
				state.render(os.Stdout)
				return inputDone
			}
		}
	}
	if state.shortcutMode {
		if state.handleShortcutInput(b) == "restart-tui" {
			f.err = errRestartTUI
			return inputExit
		}
		state.render(os.Stdout)
		return inputDone
	}
	if state.notificationConfigMode {
		if state.handleNotificationConfigInput(b) == "restart-tui" {
			f.err = errRestartTUI
			return inputExit
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}
