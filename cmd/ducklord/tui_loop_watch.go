package main

import (
	"context"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func (l *tuiLoop) watchClient(client ducklord.Client) {
	state := l.state
	ctx := l.ctx
	if !l.eventDriven || state.disconnectedHosts[client.Name] {
		return
	}
	if _, exists := l.watchedClients[client.Name]; exists {
		return
	}
	l.watchEpoch++
	watchCtx, cancel := context.WithCancel(ctx)
	current := hostWatchState{cancel: cancel, epoch: l.watchEpoch}
	l.watchedClients[client.Name] = current
	state.eventDriven = true
	updates := l.watcher.WatchSessionUpdates(watchCtx, client)
	go func() {
		for update := range updates {
			select {
			case l.sessionUpdates <- hostSessionUpdate{update: update, epoch: current.epoch}:
			case <-watchCtx.Done():
				return
			}
		}
	}()
}

func (l *tuiLoop) watchAllClients() {
	state := l.state
	for _, client := range state.cfg.Clients {
		l.watchClient(client)
	}
}

func (l *tuiLoop) launchHostHookStatus(target string) {
	state := l.state
	ctx := l.ctx
	runner := l.runner
	state.hostHookStatusRequestID++
	requestID := state.hostHookStatusRequestID
	state.hostHookStatusLoading = true
	state.hostHookStatusErr = ""
	state.hostHookStatuses = nil
	client, ok := state.cfg.Client(target)
	if !ok || state.disconnectedHosts[target] {
		state.hostHookStatusLoading = false
		state.hostHookStatusErr = "Host is disconnected or unavailable"
		return
	}
	epoch, instanceID := l.watchedClients[target].epoch, state.hostSync[target].InstanceID
	go func() {
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		statuses := make(map[string]protocol.HostAgentHookStatus, 2)
		var readErr error
		for _, agent := range []string{"codex", "claude"} {
			statuses[agent], readErr = runner.HostAgentHookStatus(readCtx, client, agent)
			if readErr != nil {
				break
			}
		}
		select {
		case l.hostHookStatusDone <- hostHookStatusEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, statuses: statuses, err: readErr}:
		case <-ctx.Done():
		}
	}()
}
