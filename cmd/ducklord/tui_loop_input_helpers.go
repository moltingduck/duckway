package main

import (
	"time"
)

func (l *tuiLoop) clearWorkspacePlacementInput() {
	state := l.state
	state.workspacePlacementInputPending = false
	l.replay.deferred = nil
	l.replay.navigation = false
	l.replay.toPTY = false
}

func (l *tuiLoop) stopPanePrefixTimer() {
	if !l.panePrefixTimer.Stop() {
		select {
		case <-l.panePrefixTimer.C:
		default:
		}
	}
}

func (l *tuiLoop) resetPanePrefixTimer(deadline time.Time) {
	l.stopPanePrefixTimer()
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d < 0 {
			d = 0
		}
		l.panePrefixTimer.Reset(d)
	}
}

func (l *tuiLoop) queuePaneReplay() {
	state := l.state
	if l.replay.queued || len(state.panePrefixReplay) == 0 {
		return
	}
	pending := state.takePanePrefixReplay()
	l.replay.input <- pending
	l.replay.queued = true
}
