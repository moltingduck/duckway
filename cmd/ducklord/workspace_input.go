package main

import (
	"github.com/hackerduck/duckway/internal/ducklord"
)

// workspaceInputReplay holds the dispatcher's existing local replay slot and
// the bytes fenced while a replacement workspace PTY lease becomes ready.
type workspaceInputReplay struct {
	input      chan []byte
	queued     bool
	deferred   []byte
	navigation bool
	toPTY      bool
}

func (r *workspaceInputReplay) selectInput(input <-chan []byte) <-chan []byte {
	if r.queued {
		return r.input
	}
	return input
}

func (r *workspaceInputReplay) releaseWorkspacePlacement(s *tuiState) {
	if !s.workspacePlacementInputPending {
		return
	}
	if len(r.deferred) != 0 {
		if r.queued {
			return // retain the fence until the older local replay is consumed
		}
		r.input <- r.deferred
		r.deferred = nil
		r.navigation = !r.toPTY
		r.queued = true
	}
	r.toPTY = false
	s.workspacePlacementInputPending = false
}

func (r *workspaceInputReplay) retryWorkspacePlacement(s *tuiState, control *ducklord.ControlSession) {
	if s.workspacePlacementInputPending && s.focused && !s.centralModalOpen() && !s.helpMode && !r.queued && controlMatchesSession(control, s.activePTYSession(), s.ownerName) {
		r.releaseWorkspacePlacement(s)
	}
}

// Every workspace readiness path completes the same handoff: only the exact
// writer and visible output lease may gain focus and release fenced bytes.
func acceptWorkspaceControlInput(s *tuiState, output workspaceInputFocusSetter, control *ducklord.ControlSession, session ducklord.RemoteSession, replay *workspaceInputReplay) bool {
	if !s.workspaceControlMayAccept() || !controlMatchesSession(control, session, s.ownerName) {
		return false
	}
	key, ok := terminalOutputKey(session)
	if !ok || !setWorkspaceInputFocus(output, key) {
		return false
	}
	s.focused = true
	replay.releaseWorkspacePlacement(s)
	return true
}

func shouldDeferWorkspaceInput(s *tuiState, replayed bool) bool {
	// A placement fence belongs to the asynchronous PTY handoff. Keep the first
	// workspace key queued until the replacement control lease is accepted; the
	// replay is then routed through workspace navigation (for example Ctrl-]
	// followed by b, then p) instead of being consumed by the old PTY gate.
	return s.workspacePlacementInputPending && !s.focused && !replayed && !s.centralModalOpen() &&
		!s.workspaceProjectFocus
}

// shouldRouteWorkspaceProjectInputAsNavigation identifies the project-focus
// shortcut while a replacement PTY lease is still fenced. Route this key
// locally so the user can reach the Project pane during restoration.
//
// A fence armed for a focused-PTY handoff (for example a direct Ctrl-B d detach
// whose surviving pane is still opening its writer) holds typed shell input,
// not navigation. A "b" inside that command must stay behind the fence;
// treating it as the Project shortcut drops the lease and sends the rest of the
// command to the Project pane.
func shouldRouteWorkspaceProjectInputAsNavigation(s *tuiState, input []byte, fencedForPTY bool) bool {
	if fencedForPTY && s.workspacePlacementInputPending {
		return false
	}
	return s.workspacePreview && !s.focused && !s.newSessionMode && !s.workspacePaneMode &&
		s.shortcut("project_focus", string(input))
}

func shouldRouteWorkspaceReplayAsNavigation(deferredReplay bool) bool {
	return deferredReplay
}
