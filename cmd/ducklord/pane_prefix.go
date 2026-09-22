package main

import (
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

const quickShellRepeatWindow = 500 * time.Millisecond

// Prefix handling stays on the UI event loop and never forwards command bytes.
func (s *tuiState) handlePanePrefix(input []byte) (bool, string) {
	if !s.workspacePreview || (s.blockingModalOpen() && (!s.workspacePaneMode || s.workspacePaneStep != "notes")) || s.copyMode || s.helpSearchActive {
		s.panePrefixPending = false
		s.panePrefixSuffix = ""
		return false, ""
	}
	// Interrupt is always terminal input while a session is focused. In
	// particular, clear a stale prefix so Ctrl-C cannot be consumed as the
	// second byte of a local pane command.
	if s.focused && string(input) == "\x03" {
		s.panePrefixPending = false
		return false, ""
	}
	// Clickable help entries submit an entire local key sequence.
	prefix := shortcutInput(s.cfg.Shortcut("pane_prefix"))
	if len(input) > len(prefix) && strings.HasPrefix(string(input), prefix) {
		s.quickShellOrigin = nil
		if s.focused {
			s.quickShellOrigin = s.captureQuickShellOrigin()
		}
		if s.focused {
			s.workspaceConfigFocus = "session"
			s.workspaceConfigIdentity, _ = ducklord.IdentityFromSession(s.activePTYSession())
		}
		s.panePrefixPending = true
		input = input[len(prefix):]
	}
	if s.panePrefixPending {
		if s.panePrefixSuffix != "" && time.Now().Before(s.panePrefixDeadline) && string(input) == s.panePrefixSuffix {
			s.panePrefixPending, s.panePrefixSuffix = false, ""
			s.panePrefixDeadline = time.Time{}
			return true, "quick-shell:" + string(input)
		}
		if s.panePrefixSuffix != "" {
			first := s.panePrefixSuffix
			s.panePrefixPending, s.panePrefixSuffix = false, ""
			s.panePrefixDeadline = time.Time{}
			if len(input) != 0 {
				s.panePrefixReplay = append([]byte(nil), input...)
			}
			return true, first
		}
		quickEligible := s.quickShellOrigin != nil
		if !quickEligible && s.focused {
			session := s.activePTYSession()
			quickEligible = session.Client != "" && session.Cwd != ""
		}
		if quickEligible {
			// A PTY can deliver Ctrl-B and both suffix bytes in one read. Treat
			// the first two suffix bytes exactly like two event-loop turns, while
			// retaining any mismatched tail for normal replay.
			if len(input) >= 2 && (string(input[0]) == "-" || string(input[0]) == "\\" || string(input[0]) == "t") {
				first, second := string(input[0]), string(input[1])
				if first == second {
					s.panePrefixPending = false
					s.panePrefixSuffix = ""
					s.panePrefixDeadline = time.Time{}
					if len(input) > 2 {
						s.panePrefixReplay = append([]byte(nil), input[2:]...)
					}
					return true, "quick-shell:" + first
				}
				s.panePrefixReplay = append([]byte(nil), input[1:]...)
				return true, first
			}
			switch string(input) {
			case "-", "\\", "t":
				s.panePrefixSuffix = string(input)
				s.panePrefixDeadline = time.Now().Add(quickShellRepeatWindow)
				return true, ""
			}
		}
		s.panePrefixPending = false
		switch string(input) {
		case "-", "\\", "t", ",", "n", "p", "c", "d", "o", "O", "?", " ", "/", "m", "M", "f":
			return true, string(input)
		default:
			for _, key := range []string{"up", "down", "left", "right", "pageup", "pagedown"} {
				if string(input) == shortcutInput(key) {
					return true, key
				}
			}
			return true, ""
		}
	}
	if s.shortcut("pane_prefix", string(input)) {
		s.quickShellOrigin = nil
		if s.focused {
			s.quickShellOrigin = s.captureQuickShellOrigin()
		}
		if s.focused {
			s.workspaceConfigFocus = "session"
			s.workspaceConfigIdentity, _ = ducklord.IdentityFromSession(s.activePTYSession())
		}
		s.panePrefixPending = true
		s.panePrefixSuffix = ""
		return true, ""
	}
	if !strings.HasPrefix(string(input), "\x1b[<") {
		s.workspaceConfigFocus = ""
	}
	return false, ""
}

// takePanePrefixReplay returns the second byte captured while the first
// prefix command was being dispatched. The event loop feeds it back through
// normal input handling after that command completes.
func (s *tuiState) takePanePrefixReplay() []byte {
	replay := s.panePrefixReplay
	s.panePrefixReplay = nil
	return replay
}

// handleDirectNotesInput owns the lowercase Notes shortcut when navigation
// panes have focus. A focused terminal must receive plain `o` as PTY input.
func (s *tuiState) handleDirectNotesInput(input []byte) bool {
	if string(input) != "o" || !s.workspacePreview || s.focused || s.workspacePaneMode || s.blockingModalOpen() || s.searchMode || s.detailSearchFocused || s.copyMode || s.helpSearchActive {
		return false
	}
	if s.workspaceProjectFocus {
		s.openPrefixPane("o")
		return true
	}
	_, err := s.workspaceNavigation()
	if err != nil {
		return false
	}
	s.openPrefixPane("O")
	return true
}

func paneNavigationCommand(command string) bool {
	switch command {
	case "up", "down", "left", "right", "pageup", "pagedown", "n", "p":
		return true
	}
	return false
}

func (s *tuiState) navigatePrefixPane(command string) bool {
	s.workspaceConfigFocus = ""
	nav, err := s.workspaceNavigation()
	if err == nil {
		switch command {
		case "pageup", "p":
			err = nav.CycleTab(-1)
		case "pagedown", "n":
			err = nav.CycleTab(1)
		default:
			width, height := terminalSize()
			err = nav.MoveVisiblePane(ducklord.CalculateWorkspaceGeometry(width, height, 4), command)
		}
	}
	if err != nil {
		s.outputErr = err.Error()
		return false
	}
	s.outputErr = ""
	s.workspaceProjectFocus = true
	s.workspaceAttachFromProject = true
	s.workspaceFocusFromProject = true
	s.selectedGroupID = ""
	return true
}

// beginFocusedPaneNavigation completes the focus handoff after a prefix tab or
// pane move. The next synthetic Enter must bypass Project-list focus and attach
// the newly selected Session directly.
func (s *tuiState) beginFocusedPaneNavigation(command string) bool {
	if !s.navigatePrefixPane(command) {
		return false
	}
	s.workspaceProjectFocus = false
	s.workspaceAttachFromProject = true
	s.workspaceFocusFromProject = true
	return true
}

func (s *tuiState) openPrefixPane(command string) {
	if command == "o" || command == "O" {
		if s.workspacePaneMode && s.workspacePaneStep == "notes" {
			s.closeNotesModal()
			return
		}
		s.notesPreviousFocused = s.focused
		s.notesPreviousAttachKey = s.activeAttachKey
		_, s.notesPreviousAttachValid = s.sessionForKey(s.activeAttachKey)
		s.notesPreviousProjectFocus = s.workspaceProjectFocus
		wasProjectFocus := s.workspaceProjectFocus
		s.workspaceProjectFocus = true
		if s.hostScoped {
			s.outputErr = "Notes are unavailable in host-scoped view"
			return
		}
		s.workspacePaneMode = false
		s.workspacePaneStep = ""
		s.workspacePaneIndex = 0
		s.workspacePaneErr = ""
		s.outputErr = ""
		// `o` follows the focused pane into its Session notebook.  From the
		// project workspace focus it retains the historical Project notebook.
		s.notesScope = ducklord.NotesProject
		if command == "O" || s.focused {
			identity, ok := ducklord.IdentityFromSession(s.activePTYSession())
			if (!s.focused && !(command == "O" && !wasProjectFocus)) || !ok || identity.Key() == "" {
				s.outputErr = "Session Notes unavailable: no focused session"
				return
			}
			s.notesScope = ducklord.NotesSession
			s.notesSessionIdentity = identity
			s.notesSessionID = identity.SessionID
		}
		s.notesQuery = ""
		s.notesSearchActive = false
		if nav, err := s.workspaceNavigation(); err == nil {
			s.notesPreviousProjectID, s.notesPreviousPaneID = nav.CurrentProjectID(), nav.CurrentPaneID()
			s.notesProjectID = nav.CurrentProjectID()
			if identity, ok := s.activity().ProjectLayout.PaneSession(s.notesProjectID, nav.CurrentPaneID()); ok {
				s.notesSessionID = identity.SessionID
				s.notesSessionIdentity = identity
			} else {
				s.notesSessionID = ""
				s.notesSessionIdentity = ducklord.SessionIdentity{}
			}
			if s.notesProjectID == "" {
				s.outputErr = "No current Project for Notes"
				return
			}
		} else {
			s.outputErr = "Notes navigation unavailable: " + err.Error()
			return
		}
		s.loadNotesScope()
		s.workspacePaneMode = true
		s.workspacePaneStep = "notes"
		// Notes owns keyboard input while open. Keep the exact terminal-focus
		// state so closing the modal can return input to the prior PTY.
		s.focused = false
		return
	}
	if command == "c" {
		s.openFocusedWorkspaceConfig()
		return
	}
	s.workspaceProjectFocus = true
	if command == "," {
		s.beginWorkspaceTabRename()
		return
	}
	s.beginWorkspacePane()
	if !s.workspacePaneMode {
		return
	}
	placement := ducklord.PlaceNewTab
	switch command {
	case "-":
		placement = ducklord.PlaceHorizontal
	case "\\":
		placement = ducklord.PlaceVertical
	}
	s.workspacePaneIntent.placement = placement
	if placement != ducklord.PlaceNewTab && s.workspacePaneIntent.targetID == "" {
		s.workspacePaneErr = "choose a Project with a Session pane to split"
		return
	}
	s.workspacePaneStep, s.workspacePaneIndex = "source", 0
}

// dispatchPaneCommand applies a completed pane prefix command on the UI loop.
func (s *tuiState) dispatchFocusedPaneCommand(command string) bool {
	if command == "f" {
		s.openProjectFiles()
		return true
	}
	if command == "/" || command == "m" || command == "M" {
		return s.openTerminalTool(command)
	}
	if command == " " {
		s.openCommandPalette()
		return true
	}
	if command == "d" {
		// A focused terminal in a non-Default Project detaches its selected
		// Session pane directly. The Default Project retains the Project action
		// modal so its existing confirmation flow remains available.
		if nav, err := s.workspaceNavigation(); err == nil && nav.CurrentProjectID() != ducklord.DefaultProjectID {
			return s.detachFocusedSessionPane()
		}
		// A focused terminal still uses the workspace detach confirmation flow.
		// Restore project ownership before opening it; falling back to the
		// session-pane detach path can release focus to the Session list when a
		// prefix arrives immediately after an overlay closes.
		if _, err := s.workspaceNavigation(); err == nil {
			s.workspacePaneRestoreFocused = true
			s.workspacePaneRestoreAttachKey = s.effectiveAttachKey()
			s.workspacePaneFocusRestoreSession = ducklord.RemoteSession{}
			if captured, ok := s.sessionForKey(s.workspacePaneRestoreAttachKey); ok {
				s.workspacePaneFocusRestoreSession = captured
			}
			if s.workspacePaneFocusRestoreSession.Client == "" {
				if captured := s.activePTYSession(); captured.Client != "" {
					s.workspacePaneFocusRestoreSession = captured
				}
			}
			// The confirmation temporarily lends navigation ownership to the
			// workspace.  Do not let a stale workspace-change fence tear down
			// the focused PTY lease before Esc can restore it.
			s.workspacePaneChanged = false
			if s.workspacePaneRestoreAttachKey == "" {
				if nav, navErr := s.workspaceNavigation(); navErr == nil {
					if identity, ok := s.activity().ProjectLayout.PaneSession(nav.CurrentProjectID(), nav.CurrentPaneID()); ok {
						for _, session := range s.sessions {
							if candidate, candidateOK := ducklord.IdentityFromSession(session); candidateOK && candidate == identity {
								s.workspacePaneRestoreAttachKey = sessionKey(session)
								s.workspacePaneFocusRestoreSession = session
								break
							}
						}
					}
				}
			}
			s.workspaceProjectFocus = true
			s.beginWorkspaceDetach()
			return s.workspacePaneMode && s.workspacePaneStep == "detach-confirm"
		}
		return s.detachFocusedSessionPane()
	}
	if command == "o" || command == "O" || command == "?" {
		s.dispatchPaneCommand(command)
		return true
	}
	return false
}

func (s *tuiState) dispatchPaneCommand(command string) {
	if command == " " {
		s.openCommandPalette()
		return
	}
	if command == "d" {
		wasSessionFocus := !s.workspaceProjectFocus && s.activeAttachKey != ""
		// Preserve the Default Project's confirmation route even while a PTY
		// focus lease is being handed back after an overlay closes.
		if _, err := s.workspaceNavigation(); err == nil {
			if wasSessionFocus {
				s.workspacePaneRestoreFocused = true
				s.workspacePaneRestoreAttachKey = s.effectiveAttachKey()
				s.workspacePaneChanged = false
			}
			s.workspaceProjectFocus = true
			s.beginWorkspaceDetach()
			if s.workspacePaneMode {
				return
			}
		}
	}
	if command == "?" {
		s.dispatchHelpAction("help")
		return
	}
	if paneNavigationCommand(command) {
		s.navigatePrefixPane(command)
		return
	}
	s.openPrefixPane(command)
}

func (s *tuiState) captureQuickShellOrigin() *workspacePaneIntent {
	nav, err := s.workspaceNavigation()
	if err != nil {
		return nil
	}
	p, t, pane := nav.CurrentProjectID(), nav.CurrentTabID(), nav.CurrentPaneID()
	id, ok := s.activity().ProjectLayout.PaneSession(p, pane)
	if !ok {
		return nil
	}
	return &workspacePaneIntent{projectID: p, tabID: t, targetID: pane, originIdentity: id}
}
