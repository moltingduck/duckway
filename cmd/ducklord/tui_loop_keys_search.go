package main

import (
	"os"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// inputSearch handles the session search modal.
func (l *tuiLoop) inputSearch(f *inputFrame) inputFlow {
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	if state.searchMode {
		if state.searchPendingRequestID != 0 {
			if string(f.b) == "\x1b" || string(f.b) == "\x03" {
				state.searchPendingRequestID = 0
				state.closeSearch()
				l.selectPooledOutput()
				state.render(os.Stdout)
				return inputDone
			}
			state.searchErr = "switching PTY..."
			state.render(os.Stdout)
			return inputDone
		}
		action := state.handleSearchInput(f.b)
		switch action {
		case "cancel":
			state.closeSearch()
		case "activate":
			target, ok := state.searchResult()
			key := sessionKey(target)
			if ok && state.searchResultIsActivated(target) {
				if !state.prepareSearchWorkspaceSelection(key) {
					state.render(os.Stdout)
					return inputDone
				}
				state.closeSearch()
				f.b = []byte("\r")
			} else if !ok || l.outputManager == nil {
				state.searchErr = "PTY output activation is unavailable"
				state.render(os.Stdout)
				return inputDone
			} else if _, valid := terminalOutputKey(target); !valid || !canRead(target) || !state.hostIsLive(target.Client) {
				state.searchErr = "selected session is unavailable"
				state.render(os.Stdout)
				return inputDone
			} else {
				client, clientErr := mustClient(cfg, target.Client)
				if clientErr != nil {
					state.searchErr = clientErr.Error()
					state.render(os.Stdout)
					return inputDone
				}
				rows, cols := state.activePTYSize()
				requestID := l.outputManager.Select(ducklord.TerminalSelection{Client: client, InstanceID: target.InstanceID, SessionID: target.SessionID,
					RuntimeGeneration: target.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
				l.outputRequestID = requestID
				state.searchPendingRequestID = requestID
				state.searchPendingKey = key
				state.searchPendingGeneration = target.RuntimeGeneration
				state.searchPendingRevision = state.searchRevision
				state.searchErr = "switching PTY..."
				go func(id uint64) {
					timer := time.NewTimer(15 * time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
						select {
						case l.searchActivationTimeout <- id:
						case <-ctx.Done():
						}
					case <-ctx.Done():
					}
				}(requestID)
				state.render(os.Stdout)
				return inputDone
			}
		}
		if state.searchMode {
			state.render(os.Stdout)
			return inputDone
		}
	}
	return inputNext
}
