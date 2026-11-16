package main

import (
	"github.com/hackerduck/duckway/internal/ducklord"
)

// dispatchPaneCommandAndRestoreHelpFocus closes the focus lease when a Help
// action is dispatched through the workspace prefix route (including a
// clicked Help action). Other close paths restore focus at their input site.
func dispatchPaneCommandAndRestoreHelpFocus(state *tuiState, command string, output workspaceInputFocusSetter, ready bool, control *ducklord.ControlSession, attach *ducklord.AttachSession) {
	state.dispatchPaneCommand(command)
	if command == "?" && !state.helpMode && state.helpFocusRestorePending {
		restorePendingHelpFocusWithAttach(state, output, ready, control, attach)
	}
}

type workspaceInputFocusSetter interface {
	SetInputFocus(ducklord.OutputKey) error
}

func setWorkspaceInputFocus(output workspaceInputFocusSetter, key ducklord.OutputKey) bool {
	return output != nil && output.SetInputFocus(key) == nil
}

// A control completion can arrive after the attach stream that was replaced
// by an Esc restore. Keep the restored Session focus fenced until that event
// is consumed; otherwise the late completion clears focus and routes the next
// byte to the workspace list.
func consumeWorkspaceRestoreControlDone(state *tuiState) bool {
	if !state.workspacePaneFocusRestoreLease && !state.workspacePaneFocusRestorePending {
		return false
	}
	state.workspacePaneFocusRestoreLease = false
	return true
}

func restorePendingWorkspacePaneFocus(state *tuiState, output workspaceInputFocusSetter, ready bool, control *ducklord.ControlSession) bool {
	if !state.workspacePaneFocusRestorePending || control == nil {
		return false
	}
	if !ready || output == nil {
		return false
	}
	session := state.activePTYSession()
	if state.workspacePaneFocusRestoreKey != "" {
		if restored, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
			session = restored
		} else if state.workspacePaneFocusRestoreSession.Client != "" {
			session = state.workspacePaneFocusRestoreSession
		}
	}
	key, ok := terminalOutputKey(session)
	if !ok || !setWorkspaceInputFocus(output, key) {
		return false
	}
	state.workspacePaneFocusRestorePending = false
	state.workspacePaneFocusRestoreKey = ""
	state.workspacePaneFocusRestoreLease = true
	state.focused = true
	return true
}

func restorePendingNotesFocus(state *tuiState, output workspaceInputFocusSetter, ready bool) bool {
	if !state.notesFocusRestorePending {
		return false
	}
	if !ready || output == nil {
		state.focused = false
		return false
	}
	key, ok := terminalOutputKey(state.activePTYSession())
	if !ok || !setWorkspaceInputFocus(output, key) {
		state.focused = false
		return false
	}
	state.notesFocusRestorePending = false
	state.focused = true
	state.outputErr = ""
	return true
}

func restorePendingHelpFocusWithAttach(state *tuiState, output workspaceInputFocusSetter, ready bool, control *ducklord.ControlSession, attach *ducklord.AttachSession) bool {
	if !state.helpFocusRestorePending || !ready || output == nil {
		return false
	}
	// ControlSession is the normal writer, but an attach stream can remain the
	// live PTY writer while control is opening or being replaced.  Treat the
	// two writers independently so a stale control event cannot block the valid
	// attach fallback.
	// Help captures the originating attach key before it relinquishes focus.
	// Use that identity when closing: selection/output bookkeeping may advance
	// while the overlay is open, so activePTYSession can already refer to a
	// different pane (or no pane at all).
	origin := state.activePTYSession()
	if state.helpPendingInputKey != "" {
		if captured, ok := state.sessionForKey(state.helpPendingInputKey); ok {
			origin = captured
		}
	}
	controlReady := control != nil && controlMatchesSession(control, origin, state.ownerName)
	attachReady := attach != nil && attach.Stdin != nil && state.helpPendingInputKey != "" && state.helpPendingInputKey == state.activeAttachKey
	if !controlReady && !attachReady {
		return false
	}
	key, ok := terminalOutputKey(origin)
	if !ok || !setWorkspaceInputFocus(output, key) {
		return false
	}
	state.helpFocusRestorePending = false
	state.helpPendingInputKey = ""
	state.focused = true
	state.outputErr = ""
	return true
}

func queueNotesPendingInput(state *tuiState, input []byte) bool {
	if !state.notesFocusRestorePending || state.notesPendingInputKey == "" || len(input) == 0 {
		return false
	}
	if len(state.notesPendingInput)+len(input) > notesPendingInputLimit {
		dropNotesPendingInput(state)
		state.notesFocusRestorePending = false
		state.outputErr = "post-Notes input was dropped while focus was restoring"
		return true
	}
	state.notesPendingInput = append(state.notesPendingInput, input...)
	return true
}

func dropNotesPendingInput(state *tuiState) {
	state.notesPendingInputKey = ""
	state.notesPendingInput = nil
}

func drainNotesPendingInput(state *tuiState, control *ducklord.ControlSession) bool {
	if state.notesFocusRestorePending || len(state.notesPendingInput) == 0 || control == nil {
		return false
	}
	if state.notesPendingInputKey == "" || state.notesPendingInputKey != state.activeAttachKey || !controlMatchesSession(control, state.activePTYSession(), state.ownerName) {
		dropNotesPendingInput(state)
		return false
	}
	queued := state.notesPendingInput
	dropNotesPendingInput(state)
	_, err := control.Stdin.Write(queued)
	return err == nil
}

// toggleHelp is the central dispatcher for opening and closing the help modal.
func (s *tuiState) toggleHelp() {
	if s.helpMode {
		s.closeHelp()
		// The output lease may have restored focus immediately before the
		// closing key is dispatched. In that case the fence is stale and must
		// not consume the first byte sent to the restored PTY. Keep it only
		// when focus is still waiting for the asynchronous lease.
		if s.helpFocusRestorePending && !s.focused {
			return
		}
		s.helpFocusRestorePending = false
		s.helpPendingInputKey = ""
		return
	}
	if s.focused {
		s.helpOriginFocused = true
		s.helpFocusRestorePending = true
		s.helpPendingInputKey = s.activeAttachKey
		s.focused = false
	} else {
		s.helpOriginFocused = false
	}
	s.helpMode = true
}

// closeHelp retires all help-local state, including the deferred focus fence.
// Help can be closed from both the pinned overlay and its search mode; keeping
// the latter as a separate assignment path can leave the next PTY byte queued
// forever after focus was restored while the close key was in flight.
func (s *tuiState) closeHelp() {
	s.helpMode = false
	s.helpOriginFocused = false
	s.helpOffset = 0
	s.helpScrollbarDrag = false
	s.helpScrollbarDragGrab = 0
	s.helpSearchActive = false
	s.helpSearchQuery = ""
	if s.helpFocusRestorePending && !s.focused {
		return
	}
	s.helpFocusRestorePending = false
	s.helpPendingInputKey = ""
}

// dispatchHelpAction is the central event-loop action for the pinned help
// overlay. The configured help key toggles it; Esc is handled centrally.
func (s *tuiState) dispatchHelpAction(action string) bool {
	if action != "help" {
		return false
	}
	s.toggleHelp()
	return true
}

// handleCentralModalCancel is the event-loop escape path for the workspace
// modals whose close command is Esc or Ctrl-C.
func (s *tuiState) handleCentralModalCancel(input []byte) bool {
	if string(input) != "\x1b" && string(input) != "\x03" {
		return false
	}
	switch {
	case s.workspacePaneMode:
		if s.workspacePaneStep == "notes" {
			if s.notesFormActive {
				s.notesFormActive, s.outputErr = false, ""
			} else {
				s.closeNotesModal()
			}
		} else {
			// A focused terminal can open the Default Project's confirmation
			// modal after temporarily lending navigation ownership to the
			// workspace.  Cancel must return that lease to the same PTY; simply
			// closing the modal leaves the UI on the Project/session list.
			restoreFocusedPTY := s.workspacePaneStep == "detach-confirm" && s.workspacePaneRestoreFocused
			restoreAttachKey := s.workspacePaneRestoreAttachKey
			s.closeWorkspacePane()
			if restoreFocusedPTY {
				s.beginFocusedDefaultDetachRestore(restoreAttachKey)
			}
		}
	default:
		return false
	}
	return true
}

func initialReplayCaughtUp(currentOffset, replayEndOffset uint64) bool {
	return currentOffset >= replayEndOffset
}
