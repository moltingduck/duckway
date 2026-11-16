package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// inputHostMenu handles the host menu modal.
func (l *tuiLoop) inputHostMenu(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	if state.hostMenuMode {
		action := state.handleHostMenuInput(b)
		target := state.hostMenuTarget
		targets := []string{target}
		var connectTargets []string
		if action == "host-apply-connections" {
			targets = state.changedHostMenuTargets(false)
			connectTargets = state.changedHostMenuTargets(true)
		}
		switch action {
		case "host-resources-read":
			l.hostMenuReadResources(target)
		case "host-hooks-read":
			l.launchHostHookStatus(target)
		case "host-hook-save":
			l.hostMenuSaveHook(target)
		case "host-retention-read":
			l.hostMenuReadRetention(target)
		case "host-retention-save":
			l.hostMenuSaveRetention(target)
		case "host-notification-settings":
			state.hostMenuMode = false
			state.beginNotificationConfig("host", target)
		case "host-add":
			state.hostMenuMode = false
			state.beginAddClient()
		case "host-remove":
			state.hostMenuMode = false
			state.beginRemoveClient()
		case "host-disconnect", "host-reconnect", "host-apply-connections":
			l.hostMenuApplyConnections(target, targets, connectTargets, action)
		case "host-connect":
			l.hostMenuConnect(target, targets)
		}
		if !state.hostMenuMode && l.hostRetentionCancel != nil {
			l.hostRetentionCancel()
			l.hostRetentionCancel = nil
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// hostMenuConnect connects the selected Host.
func (l *tuiLoop) hostMenuConnect(target string, targets []string) {
	state := l.state
	ctx := l.ctx
	for _, target := range targets {
		if !state.disconnectedHosts[target] {
			continue
		}
		state.bumpHostConnectionEpoch(target)
		delete(state.disconnectedHosts, target)
		state.hostSync[target] = ducklord.SessionUpdate{Client: target, State: "connecting"}
		if client, ok := state.cfg.Client(target); ok {
			l.watchClient(client)
		}
		if !l.eventDriven {
			state.refreshSessions(ctx)
		}
		state.hostMenuMode = false
		state.outputErr = "connecting " + target
	}
}

// hostMenuApplyConnections disconnects, reconnects or applies Host connection changes.
func (l *tuiLoop) hostMenuApplyConnections(target string, targets []string, connectTargets []string, action string) {
	state := l.state
	ctx := l.ctx
	for _, target := range targets {
		if target == state.hostMenuTarget && l.hostRetentionCancel != nil {
			l.hostRetentionCancel()
			l.hostRetentionCancel = nil
		}
		wasActive := state.hostOwnsActivePTY(target, l.control)
		if current, ok := l.watchedClients[target]; ok {
			current.cancel()
			delete(l.watchedClients, target)
		}
		previous := state.hostSync[target]
		state.bumpHostConnectionEpoch(target)
		state.disconnectedHosts[target] = true
		if state.newSessionMode && state.newSessionClient == target {
			state.cancelCreateDiscovery()
			state.cancelPathSuggestions()
			state.newSessionErr = "host disconnected; pending session operation was canceled"
		}
		if l.outputManager != nil && previous.InstanceID != "" {
			// A user disconnect is reversible and keeps the desired LRU
			// membership. ForgetHost is reserved for daemon replacement
			// and permanent host removal.
			l.outputManager.SyncHost(target, previous.InstanceID, false, nil)
		}
		if wasActive {
			if l.attachCancel != nil {
				l.attachCancel()
				l.attachCancel = nil
			}
			if l.controlOpenCancel != nil {
				l.controlOpenCancel()
				l.controlOpenCancel = nil
			}
			if l.control != nil {
				_ = l.control.Stdin.Close()
				l.control, l.controlDone = nil, nil
			}
			state.focused = false
			state.clearAttachIdentity()
		}
		state.hostSync[target] = ducklord.SessionUpdate{Client: target, InstanceID: previous.InstanceID, Generation: previous.Generation, Revision: previous.Revision, State: "disconnected"}
		state.hostMenuMode = false
		state.outputErr = "disconnected " + target + "; its sessions and notifications are detached"
		if action == "host-reconnect" {
			delete(state.disconnectedHosts, target)
			state.hostSync[target] = ducklord.SessionUpdate{Client: target, State: "reconnecting"}
			if client, ok := state.cfg.Client(target); ok {
				l.watchClient(client)
			}
			if !l.eventDriven {
				state.refreshSessions(ctx)
			}
			state.outputErr = "reconnecting " + target
		}
	}
	if l.outputManager != nil {
		l.selectPooledOutput()
	}
	if action == "host-apply-connections" {
		for _, target := range connectTargets {
			state.bumpHostConnectionEpoch(target)
			delete(state.disconnectedHosts, target)
			state.hostSync[target] = ducklord.SessionUpdate{Client: target, State: "connecting"}
			if client, ok := state.cfg.Client(target); ok {
				l.watchClient(client)
			}
			if !l.eventDriven {
				state.refreshSessions(ctx)
			}
		}
		state.hostMenuMode = false
		state.outputErr = fmt.Sprintf("host connections updated: %d connecting, %d disconnected", len(connectTargets), len(targets))
	}
}

// hostMenuSaveRetention saves the Host retention setting.
func (l *tuiLoop) hostMenuSaveRetention(target string) {
	state := l.state
	ctx := l.ctx
	runner := l.runner
	if l.hostRetentionCancel != nil {
		l.hostRetentionCancel()
	}
	state.hostMenuRequestID++
	requestID := state.hostMenuRequestID
	epoch, instanceID := l.watchedClients[target].epoch, state.hostSync[target].InstanceID
	client, ok := state.cfg.Client(target)
	if !ok || state.disconnectedHosts[target] {
		state.hostMenuStep = "retention-error"
		state.hostMenuErr = "Host is disconnected or unavailable"
		return
	}
	days, _ := strconv.Atoi(state.hostMenuDraft)
	saveCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	l.hostRetentionCancel = cancel
	go func() {
		defer cancel()
		err := runner.SetHostLogRetention(saveCtx, client, days)
		if err == nil {
			var actual int
			actual, err = runner.HostLogRetention(saveCtx, client)
			if err == nil && actual != days {
				err = fmt.Errorf("host reported %d retention days after update, expected %d", actual, days)
			}
		}
		select {
		case l.hostRetentionDone <- hostRetentionEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, days: days, err: err, saved: true}:
		case <-ctx.Done():
		}
	}()
}

// hostMenuReadRetention starts reading the Host retention setting.
func (l *tuiLoop) hostMenuReadRetention(target string) {
	state := l.state
	ctx := l.ctx
	runner := l.runner
	if l.hostRetentionCancel != nil {
		l.hostRetentionCancel()
	}
	state.hostMenuRequestID++
	requestID := state.hostMenuRequestID
	epoch, instanceID := l.watchedClients[target].epoch, state.hostSync[target].InstanceID
	client, ok := state.cfg.Client(target)
	if !ok || state.disconnectedHosts[target] {
		state.hostMenuStep = "retention-error"
		state.hostMenuErr = "Host is disconnected or unavailable"
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	l.hostRetentionCancel = cancel
	go func() {
		defer cancel()
		days, err := runner.HostLogRetention(readCtx, client)
		select {
		case l.hostRetentionDone <- hostRetentionEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, days: days, err: err}:
		case <-ctx.Done():
		}
	}()
}

// hostMenuSaveHook saves a Host agent hook change.
func (l *tuiLoop) hostMenuSaveHook(target string) {
	state := l.state
	ctx := l.ctx
	runner := l.runner
	client, ok := state.cfg.Client(target)
	if !ok || state.disconnectedHosts[target] {
		state.hostMenuStep = "hook-error"
		state.hostMenuErr = "Host is disconnected or unavailable"
		return
	}
	requestID := state.hostMenuRequestID + 1
	if !state.beginHostHookOperation(target, requestID) {
		state.hostMenuStep = "hook-error"
		state.hostMenuErr = "Another hook update is still running on this Host"
		return
	}
	if l.hostRetentionCancel != nil {
		l.hostRetentionCancel()
	}
	state.hostMenuRequestID = requestID
	epoch, instanceID := l.watchedClients[target].epoch, state.hostSync[target].InstanceID
	agent, operation := state.hostHookAgent, state.hostHookAction
	saveCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	l.hostRetentionCancel = cancel
	go func() {
		defer cancel()
		result, err := runner.ConfigureHostAgentHook(saveCtx, client, agent, operation)
		select {
		case l.hostHookDone <- hostHookEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, agent: agent, action: operation, result: result, err: err}:
		case <-ctx.Done():
		}
	}()
}

// hostMenuReadResources starts reading Host resource usage.
func (l *tuiLoop) hostMenuReadResources(target string) {
	state := l.state
	ctx := l.ctx
	runner := l.runner
	state.hostMenuRequestID++
	requestID := state.hostMenuRequestID
	client, ok := state.cfg.Client(target)
	if !ok || state.disconnectedHosts[target] {
		state.hostMenuStep = "resources-error"
		state.hostMenuErr = "Host is disconnected or unavailable"
		return
	}
	epoch, instanceID := l.watchedClients[target].epoch, state.hostSync[target].InstanceID
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	go func() {
		defer cancel()
		status, err := runner.HostResources(readCtx, client)
		publishHostResourceEvent(ctx, l.hostResourceDone, hostResourceEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, status: status, err: err})
	}()
}

// inputClientModals handles the add/remove host modals.
func (l *tuiLoop) inputClientModals(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	ctx := l.ctx
	if state.addClientMode {
		if state.addClientBusy {
			if string(b) == "\x03" || string(b) == "\x1b" || string(b) == "q" {
				state.cancelAddClient()
				state.render(os.Stdout)
			}
			return inputDone
		}
		action := state.handleAddClientInput(b)
		switch action {
		case "cancel":
			state.cancelAddClient()
		case "submit":
			if err := state.startAddClient(ctx, l.addClientDone); err != nil {
				state.addClientErr = err.Error()
			}
		}
		state.render(os.Stdout)
		return inputDone
	}
	if state.removeClientMode {
		action := state.handleRemoveClientInput(b)
		if action == "remove" {
			name := state.removeClientConfirm
			instance := state.hostSync[name].InstanceID
			if err := state.removeClient(ctx, name); err != nil {
				state.outputErr = err.Error()
			} else {
				if current, ok := l.watchedClients[name]; ok {
					current.cancel()
					delete(l.watchedClients, name)
				}
				state.disconnectedHosts[name] = true
				state.bumpHostConnectionEpoch(name)
				if l.outputManager != nil {
					l.outputManager.ForgetHost(name, instance)
					l.selectPooledOutput()
				}
			}
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}
