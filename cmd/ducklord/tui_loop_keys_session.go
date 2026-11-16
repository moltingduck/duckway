package main

import (
	"context"
	"os"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

// inputNewSession handles the new-session modal.
func (l *tuiLoop) inputNewSession(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	runner := l.runner
	if state.newSessionMode {
		if state.newSessionStarting || state.newSessionDiscovering {
			if string(b) == "\x03" {
				if state.newSessionStarting && l.startCancel != nil {
					l.startCancel()
					l.startCancel = nil
					l.startID++ // fence a Start result racing with Ctrl+C
				}
				state.cancelCreate()
				state.render(os.Stdout)
			} else if string(b) == "\x1b" {
				if state.newSessionStarting && l.startCancel != nil {
					l.startCancel()
					l.startCancel = nil
					l.startID++
					state.newSessionStarting = false
				} else {
					if state.newSessionStep == "path" && state.newSessionProject.Path != "" {
						state.newSessionLine = state.newSessionProject.Path
					}
					creatingDirectory := state.newSessionStep == "path-confirm"
					state.cancelCreateDiscovery()
					if creatingDirectory {
						state.newSessionErr = "request canceled; the directory may have been created—recheck before retrying"
					} else {
						state.newSessionErr = "operation canceled; edit or press Esc to go back"
					}
				}
				state.render(os.Stdout)
			}
			return inputDone
		}
		beforeLine := state.newSessionLine
		action := state.handleCreateInput(b)
		if action == "" && state.newSessionStep == "path" && state.newSessionLine != beforeLine {
			state.requestPathSuggestions(ctx, l.pathSuggestionsDone)
		}
		switch action {
		case "cancel":
			state.cancelCreate()
		case "submit":
			_, clientName, args, ready, err := state.submitCreateStep(ctx, l.createDone)
			if err != nil {
				state.newSessionErr = err.Error()
				break
			}
			if !ready {
				break
			}
			if !state.hostIsLive(state.newSessionClient) {
				state.newSessionErr = "host is reconnecting; session creation is disabled until synchronization completes"
				break
			}
			c, err := mustClient(cfg, state.newSessionClient)
			if err != nil {
				f.err = err
				return inputExit
			}
			startCtx, cancel := context.WithCancel(ctx)
			l.startCancel = cancel
			l.startID++
			id := l.startID
			state.newSessionStarting = true
			state.newSessionStartGeneration, state.newSessionStartInstance = state.hostFingerprint(clientName)
			state.newSessionStartEpoch = state.hostConnectionEpoch[clientName]
			state.newSessionErr = "starting..."
			go func() {
				sessionID, err := runner.Start(startCtx, c, args)
				select {
				case l.startDone <- startDoneEvent{id: id, client: clientName, sessionID: sessionID, err: err}:
				case <-ctx.Done():
				}
			}()
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputSmallModals handles the notification, group and rename modals.
func (l *tuiLoop) inputSmallModals(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	runner := l.runner
	if state.notificationMode {
		action := state.handleNotificationInput(b)
		switch action {
		case "save":
			state.saveNotificationSettings()
		case "cancel":
			state.closeNotificationSettings()
		}
		state.render(os.Stdout)
		return inputDone
	}
	if state.groupMenu {
		state.handleGroupMenuInput(b)
		state.render(os.Stdout)
		return inputDone
	}
	if state.sessionRenameMode {
		action := state.handleSessionHandleRenameInput(b)
		if action == "rename-submit" {
			target := state.sessionRenameTarget
			client, clientErr := mustClient(cfg, target.Client)
			if clientErr != nil {
				state.sessionRenameErr = clientErr.Error()
			} else {
				_, renameErr := runner.RenameSelected(ctx, client, target, strings.TrimSpace(state.sessionRenameLine))
				if renameErr != nil {
					state.sessionRenameErr = sanitizeTerminalText(renameErr.Error())
				} else {
					state.closeSessionHandleRename()
					state.outputErr = "Session handle renamed"
					state.refreshSessions(ctx)
				}
			}
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputActionMenu handles the session action menu.
func (l *tuiLoop) inputActionMenu(f *inputFrame) inputFlow {
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	if state.actionMenu {
		action := state.handleActionMenuInput(f.b)
		state.render(os.Stdout)
		if action == "" {
			return inputDone
		}
		if action == "cancel" {
			return inputDone
		}
		// Selection is rebound to the exact captured identity before the
		// ordinary dispatch path runs. A stale menu always fails closed.
		if !state.selectActionTarget() {
			state.outputErr = "session changed while action menu was open; reopen the menu"
			state.actionTarget = ducklord.RemoteSession{}
			state.actionOperation, state.actionMode = "", ""
			state.render(os.Stdout)
			return inputDone
		}
		target := state.actionTarget
		f.selectedActionTarget = &target
		state.actionTarget = ducklord.RemoteSession{}
		if action == "lifecycle-action" {
			if state.lifecycleBusy {
				state.actionOperation, state.actionMode = "", ""
				state.outputErr = "another lifecycle operation is still pending; wait for its result"
				state.render(os.Stdout)
				return inputDone
			}
			state.lifecycleConfirm = state.actionOperation
			state.lifecycleReturnToAction = true
			state.lifecycleMode = state.actionMode
			state.lifecycleTarget = target
			state.actionOperation, state.actionMode = "", ""
			state.render(os.Stdout)
			return inputDone
		}
		if action == "notifications-action" {
			state.beginNotificationSettings()
			state.render(os.Stdout)
			return inputDone
		}
		if action == "rename-handle-action" {
			state.beginSessionHandleRename(target)
			state.render(os.Stdout)
			return inputDone
		}
		// Attach and yield reuse the canonical handlers below.
		f.b = nil
		switch action {
		case "attach":
			f.b = []byte("\r")
		case "reconnect":
			if l.outputManager == nil {
				state.refreshSelectedOutput(ctx)
				state.render(os.Stdout)
				return inputDone
			}
			client, clientErr := mustClient(cfg, target.Client)
			if clientErr != nil {
				state.outputErr = clientErr.Error()
				state.render(os.Stdout)
				return inputDone
			}
			rows, cols := state.activePTYSize()
			l.outputRequestID = l.outputManager.Reconnect(ducklord.TerminalSelection{Client: client, InstanceID: target.InstanceID, SessionID: target.SessionID,
				RuntimeGeneration: target.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
			l.reconnectRequestID = l.outputRequestID
			l.activeOutputEvent = ducklord.TerminalOutputEvent{}
			state.outputReconnecting = true
			state.outputStale = true
			state.outputFresh = false
			state.outputErr = "reconnecting PTY output from Ducklion..."
			state.render(os.Stdout)
			return inputDone
		case "yield":
			f.b = []byte("y")
		case "yield-wait":
			f.b = []byte("Y")
		}
	}
	return inputNext
}

// inputLifecycleConfirm handles the end/restart/destroy confirmation.
func (l *tuiLoop) inputLifecycleConfirm(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	runner := l.runner
	if state.lifecycleConfirm != "" {
		text := string(b)
		if text == "\x1b" || text == "q" {
			returnToAction := state.lifecycleReturnToAction
			target := state.lifecycleTarget
			state.lifecycleConfirm = ""
			state.lifecycleReturnToAction = false
			state.lifecycleTarget = ducklord.RemoteSession{}
			state.lifecycleMode = ""
			if text == "\x1b" && returnToAction {
				state.actionMenu = true
				state.actionTarget = target
			}
			state.render(os.Stdout)
			return inputDone
		}
		mode := state.lifecycleMode
		sess := state.lifecycleTarget
		if mode == "" && sess.Kind != string(model.KindShell) && state.lifecycleConfirm == protocol.SessionLifecycleRestart {
			mode = protocol.SessionLifecycleWait
		}
		if mode == "" {
			mode = protocol.SessionLifecycleImmediate
		}
		if sess.Kind == string(model.KindShell) && text != "\r" && text != "\n" {
			return inputDone
		}
		if state.lifecycleMode != "" && text != "\r" && text != "\n" {
			return inputDone
		}
		if state.lifecycleMode == "" && text == "w" && state.lifecycleConfirm != protocol.SessionLifecycleRestart {
			mode = protocol.SessionLifecycleWait
		} else if state.lifecycleMode == "" && text == "f" {
			mode = protocol.SessionLifecycleForce
		} else if text != "\r" && text != "\n" {
			return inputDone
		}
		if !state.lifecycleTargetIsCurrent() {
			state.outputErr = "session changed while awaiting confirmation; reopen the lifecycle action"
			state.lifecycleConfirm = ""
			state.lifecycleTarget = ducklord.RemoteSession{}
			state.lifecycleMode = ""
			state.render(os.Stdout)
			return inputDone
		}
		client, clientErr := mustClient(cfg, sess.Client)
		if clientErr != nil {
			state.outputErr = clientErr.Error()
			state.lifecycleConfirm = ""
			state.lifecycleTarget = ducklord.RemoteSession{}
			return inputDone
		}
		operation := state.lifecycleConfirm
		l.lifecycleRequestID++
		requestID := l.lifecycleRequestID
		generation, instance := state.hostFingerprint(sess.Client)
		epoch := l.watchedClients[sess.Client].epoch
		connectionEpoch := state.hostConnectionEpoch[sess.Client]
		state.lifecycleBusy = true
		state.lifecycleConfirm = ""
		state.lifecycleTarget = ducklord.RemoteSession{}
		state.lifecycleMode = ""
		state.outputErr = string(operation) + " accepted; Ducklion will continue it across reconnects"
		go func() {
			result, err := runner.LifecycleSelected(ctx, client, sess, operation, mode)
			select {
			case l.lifecycleDone <- lifecycleDoneEvent{id: requestID, watchEpoch: epoch, connectionEpoch: connectionEpoch,
				hostGeneration: generation, hostInstance: instance,
				operation: operation, client: sess.Client, result: result, err: err}:
			case <-ctx.Done():
			}
		}()
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}
