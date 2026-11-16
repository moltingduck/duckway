package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (l *tuiLoop) onHostRetentionDone(event hostRetentionEvent) {
	state := l.state
	if event.id == state.hostMenuRequestID && l.hostRetentionCancel != nil {
		l.hostRetentionCancel()
		l.hostRetentionCancel = nil
	}
	watch := l.watchedClients[event.host]
	if !state.acceptHostRetentionEvent(event, watch.epoch) {
		return
	}
	if event.err != nil {
		state.hostMenuStep = "retention-error"
		state.hostMenuErr = sanitizeTerminalText(event.err.Error())
	} else if event.saved {
		state.hostMenuOldDays = event.days
		state.hostMenuStep = "retention-saved"
		state.hostMenuErr = ""
	} else {
		state.hostMenuOldDays = event.days
		state.hostMenuDraft = strconv.Itoa(event.days)
		state.hostMenuReplaceDraft = true
		state.hostMenuStep = "retention-edit"
		state.hostMenuErr = ""
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onHostResourceDone(event hostResourceEvent) {
	state := l.state
	watch := l.watchedClients[event.host]
	if !state.acceptHostResourceEvent(event, watch.epoch) {
		return
	}
	if event.err != nil {
		state.hostMenuErr = sanitizeTerminalText(event.err.Error())
		state.hostMenuStep = "resources-error"
	} else {
		state.hostResourceStatus = event.status
		state.hostMenuErr = ""
		state.hostMenuStep = "resources-view"
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onHostHookDone(event hostHookEvent) {
	state := l.state
	state.finishHostHookOperation(event.host, event.id)
	if event.id == state.hostMenuRequestID && l.hostRetentionCancel != nil {
		l.hostRetentionCancel()
		l.hostRetentionCancel = nil
	}
	watch := l.watchedClients[event.host]
	if !state.acceptHostHookEvent(event, watch.epoch) {
		return
	}
	if event.err != nil {
		state.hostMenuStep = "hook-error"
		state.hostMenuErr = sanitizeTerminalText(event.err.Error())
	} else {
		state.hostMenuStep = "hook-done"
		state.hostMenuErr = hostHookResultMessage(event)
		l.launchHostHookStatus(event.host)
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onHostHookStatusDone(event hostHookStatusEvent) {
	state := l.state
	if event.id != state.hostHookStatusRequestID || !state.hostMenuMode || !strings.HasPrefix(state.hostMenuStep, "hook-") ||
		event.host != state.hostMenuTarget || state.disconnectedHosts[event.host] || event.epoch != l.watchedClients[event.host].epoch ||
		event.instanceID != state.hostSync[event.host].InstanceID {
		return
	}
	state.hostHookStatusLoading = false
	if event.err != nil {
		state.hostHookStatusErr = "Host hook status unavailable"
	} else {
		state.hostHookStatuses = event.statuses
	}
	state.render(os.Stdout)
}

func (l *tuiLoop) onHostSkillsDone(event hostSkillsEvent) {
	state := l.state
	if event.action == "list" {
		if event.id != state.hostSkillsListRequestID || !state.hostMenuMode || state.hostMenuStep != "skills-dashboard" {
			ducklord.CleanupSkillPreview(event.preview)
			return
		}
		if state.hostSkillsListCancel != nil {
			state.hostSkillsListCancel()
			state.hostSkillsListCancel = nil
		}
		state.applyHostSkillsEvent(event)
		state.render(os.Stdout)
		return
	}
	if event.id != state.hostSkillsRequestID || !state.hostSkillsBusy {
		ducklord.CleanupSkillPreview(event.preview)
		return
	}
	state.applyHostSkillsEvent(event)
	state.render(os.Stdout)
}

func (l *tuiLoop) onProjectFilesDone(event projectFilesEvent) {
	state := l.state
	state.applyProjectFilesEvent(event)
	if state.projectFiles.open {
		state.render(os.Stdout)
	}
}

func (l *tuiLoop) onAddClientDone(result addClientDoneEvent) {
	state := l.state
	if result.progress {
		if state.addClientMode && state.addClientBusy && result.id == state.addClientRequestID {
			state.addClientErr = result.status
			state.render(os.Stdout)
		}
		return
	}
	if state.completeAddClient(result) {
		state.bumpHostConnectionEpoch(result.client.Name)
		delete(state.disconnectedHosts, result.client.Name)
		l.watchAllClients()
	}
	state.render(os.Stdout)
}
