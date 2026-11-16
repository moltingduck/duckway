package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (l *tuiLoop) onSessionUpdate(envelope hostSessionUpdate) {
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	update := envelope.update
	currentWatch, current := l.watchedClients[update.Client]
	if !current || currentWatch.epoch != envelope.epoch || state.disconnectedHosts[update.Client] {
		return
	}
	previousHost := state.hostSync[update.Client]
	previousInstance := previousHost.InstanceID
	if previousInstance == "" {
		for _, session := range state.sessions {
			if session.Client == update.Client && session.InstanceID != "" {
				previousInstance = session.InstanceID
				break
			}
		}
	}
	previousActive := state.effectiveAttachKey()
	var previousWorkspaceSelections []ducklord.TerminalSelection
	if l.workspaceOutput != nil && update.ChangedSessionID != "" {
		selected := state.currentSession()
		previousWorkspaceSelections = state.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: selected.Client}})
	}
	if !state.applySessionUpdate(update) {
		return
	}
	state.retryWorkspaceCreateDiscovery(ctx, l.createDone, previousHost, update)
	if state.invalidateCreateStart(update) {
		if l.startCancel != nil {
			l.startCancel()
			l.startCancel = nil
		}
		l.startID++ // fence a completion racing with cancellation
	}
	if l.outputManager != nil {
		if previousInstance != "" && update.InstanceID != "" && update.InstanceID != previousInstance {
			l.outputManager.ForgetHost(update.Client, previousInstance)
		} else if previousHost.InstanceID != "" && previousHost.State == "live" && update.State != "live" {
			l.outputManager.SyncHost(update.Client, previousHost.InstanceID, false, nil)
		}
		if update.State == "live" && update.InstanceID != "" {
			// Only an authoritative live update from the current watch epoch
			// may clear a disconnect tombstone for this instance.
			rows, cols := state.activePTYSize()
			selections := make([]ducklord.TerminalSelection, 0, len(update.Sessions))
			if client, clientErr := mustClient(cfg, update.Client); clientErr == nil {
				for _, session := range update.Sessions {
					if session.SessionID != "" && session.RuntimeGeneration != 0 && canRead(session) {
						selections = append(selections, ducklord.TerminalSelection{Client: client, InstanceID: session.InstanceID, SessionID: session.SessionID,
							RuntimeGeneration: session.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
					}
				}
			}
			l.outputManager.SyncHost(update.Client, update.InstanceID, true, selections)
		}
		workspaceSelectionsChangedByUpdate := false
		if l.workspaceOutput != nil && update.ChangedSessionID != "" {
			selected := state.currentSession()
			workspaceSelectionsChangedByUpdate = workspaceSelectionsChanged(previousWorkspaceSelections,
				state.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: selected.Client}}))
		}
		if l.workspaceOutput != nil && (previousHost.State != update.State || previousInstance != update.InstanceID || workspaceSelectionsChangedByUpdate) {
			l.selectPooledOutput()
		}
	}
	if l.control != nil && (!state.hostIsLive(state.activePTYSession().Client) || !controlMatchesSession(l.control, state.activePTYSession(), state.ownerName)) {
		l.controlID++
		if l.controlOpenCancel != nil {
			l.controlOpenCancel()
			l.controlOpenCancel = nil
		}
		_ = l.control.Stdin.Close()
		l.control, l.controlDone = nil, nil
		l.attachCanResize = false
		state.focused = false
		state.clearAttachIdentity()
		state.outputErr = "PTY control disconnected; reconnect and focus it again"
	}
	if cancelRemovedPTYControl(state, previousActive, &l.control, &l.controlOpenCancel, &l.controlID) {
		l.controlDone = nil
		if l.attachCancel != nil {
			l.attachCancel()
			l.attachCancel = nil
		}
		if l.attach != nil {
			_ = l.attach.Stdin.Close()
		}
		l.attachID++
		l.resizeInFlight = false
		l.pendingFramebufferResize = nil
		l.queuedResize = nil
		l.bufferedAttach = nil
		l.bufferedAttachBytes = 0
		l.attachOutputSource = l.attachOut
		l.attach = nil
		l.attachCanResize = false
		l.attachInitialResizeQueued = false
		l.attachReplayEndOffset = 0
	}
	if l.outputManager != nil && !state.searchMode {
		l.selectPooledOutput()
	} else if !state.focused && state.activeAttachKey == "" {
		l.requestPreview(false)
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onCreateDiscovery(result createDiscoveryEvent) {
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	runner := l.runner
	clientName, _, args, ready := state.applyCreateDiscovery(result)
	if ready {
		client, clientErr := mustClient(cfg, clientName)
		if clientErr != nil {
			state.newSessionErr = clientErr.Error()
		} else {
			startCtx, cancel := context.WithCancel(ctx)
			l.startCancel = cancel
			l.startID++
			id := l.startID
			state.newSessionStarting = true
			state.newSessionStartGeneration, state.newSessionStartInstance = state.hostFingerprint(clientName)
			state.newSessionStartEpoch = state.hostConnectionEpoch[clientName]
			state.newSessionErr = "starting..."
			go func() {
				sessionID, startErr := runner.Start(startCtx, client, args)
				select {
				case l.startDone <- startDoneEvent{id: id, client: clientName, sessionID: sessionID, err: startErr}:
				case <-ctx.Done():
				}
			}()
		}
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onPathSuggestions(result pathSuggestionEvent) {
	state := l.state
	generation, instance := state.hostFingerprint(result.client)
	if state.newSessionMode && state.newSessionStep == "path" && result.id == state.newSessionPathRequestID &&
		result.query == state.newSessionLine && result.generation == generation && result.instance == instance && state.hostIsLive(result.client) {
		state.newSessionPathBusy = false
		state.newSessionPathCancel = nil
		if result.err != nil {
			state.newSessionPathSuggestions = nil
			state.newSessionErr = sanitizeTerminalText(result.err.Error())
		} else {
			state.newSessionPathSuggestions = append([]string(nil), result.paths...)
			state.newSessionPathSelected = min(state.newSessionPathSelected, max(0, len(result.paths)-1))
			state.newSessionErr = fmt.Sprintf("%d matching remote directories", len(result.paths))
		}
		state.render(os.Stdout)
	}
}

func (l *tuiLoop) onStartDone(result startDoneEvent) {
	state := l.state
	ctx := l.ctx
	if result.id != l.startID {
		return
	}
	if state.disconnectedHosts[result.client] {
		l.startCancel = nil
		state.newSessionStarting = false
		if state.workspaceNewSessionIntent != nil {
			state.newSessionErr = "host disconnected; shell may have started—resync, then reopen Add Session pane"
		} else {
			state.newSessionErr = "host disconnected; remote start result was discarded"
		}
		state.workspaceNewSessionIntent = nil
		state.render(os.Stdout)
		return
	}
	l.startCancel = nil
	state.completeNewSessionStart(ctx, result.client, result.sessionID, result.err)
	if l.outputManager != nil && result.err == nil {
		l.selectPooledOutput()
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onLifecycleDone(result lifecycleDoneEvent) {
	state := l.state
	ctx := l.ctx
	currentWatch := l.watchedClients[result.client]
	if result.id != l.lifecycleRequestID {
		return
	}
	state.lifecycleBusy = false
	if !state.lifecycleResultCurrent(result, l.lifecycleRequestID, currentWatch.epoch) {
		state.outputErr = string(result.operation) + " outcome unknown after Host connection changed; resync before retrying"
		state.render(os.Stdout)
		return
	}
	state.lifecycleConfirm = ""
	state.lifecycleTarget = ducklord.RemoteSession{}
	if result.err != nil {
		state.outputErr = string(result.operation) + " failed: " + sanitizeTerminalText(result.err.Error())
	} else {
		state.outputErr = fmt.Sprintf("%s completed for %s (generation %d)", result.operation, result.result.SessionID, result.result.RuntimeGeneration)
	}
	state.refreshSessions(ctx)
	if state.activeAttachKey == "" {
		l.requestPreview(false)
	}
	state.render(os.Stdout)
}
