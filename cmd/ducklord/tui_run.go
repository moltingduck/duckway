package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func runTUIWithOptions(cfg *ducklord.Config, runner remoteRunner, cfgPath string, refresh time.Duration, hostScoped bool, owner string) error {
	if limiter, ok := runner.(interface{ SetOutputSubscriptionLimit(int) error }); ok {
		if err := limiter.SetOutputSubscriptionLimit(cfg.RawOutputSubscriptionLimit()); err != nil {
			return err
		}
	}
	oldState, err := makeRaw()
	if err != nil {
		return err
	}
	defer restore(oldState)
	fmt.Print("\033[?1049h\033[?25l\033[?1002h\033[?1006h")
	defer fmt.Print(mouseCursorShape("default") + "\033[?1006l\033[?1002l\033[?25h\033[?1049l")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	activityStore := ducklord.ActivityStateStore{}
	activityState, err := activityStore.Load()
	stateWarning := ""
	var recovered *ducklord.CorruptStateRecoveredError
	var stateLoadWarning *ducklord.StateLoadWarning
	if errors.As(err, &recovered) && activityState != nil {
		stateWarning = recovered.Error()
	} else if errors.As(err, &stateLoadWarning) && activityState != nil {
		stateWarning = stateLoadWarning.Error()
	} else if err != nil {
		return fmt.Errorf("load Ducklord activity state: %w", err)
	}
	if warning := soundSetupWarning(cfg); warning != "" {
		if stateWarning != "" {
			stateWarning += "; "
		}
		stateWarning += warning
	}
	if warning := desktopSetupWarning(cfg, activityState); warning != "" {
		if stateWarning != "" {
			stateWarning += "; "
		}
		stateWarning += warning
	}
	state := &tuiState{cfg: cfg, cfgPath: cfgPath, runner: runner, refresh: refresh, hashes: map[string]string{}, hostScoped: hostScoped, ownerName: owner,
		terminalCookedState: oldState,
		snapshotStore:       ducklord.SnapshotStore{}, activityStore: activityStore, activityState: activityState, hostSync: make(map[string]ducklord.SessionUpdate), localWarning: stateWarning,
		listPaneWidth: cfg.SessionListPaneWidth(), autoHideList: cfg.SessionListAutoHide(), disconnectedHosts: make(map[string]bool),
		workspacePreview: os.Getenv("DUCKLORD_LEGACY_TUI") != "1"}
	state.notificationSink = newLocalNotificationSink()
	l := &tuiLoop{ctx: ctx, stop: stop, cfg: cfg, runner: runner, state: state}
	defer state.notificationSink.Close()
	if source, ok := runner.(ducklord.TerminalOutputSource); ok {
		if state.workspacePreview {
			l.workspaceOutput, err = ducklord.NewWorkspaceOutputAdapter(ctx, cfg.RawOutputSubscriptionLimit(), source, state.snapshotStore,
				func(selected ducklord.TerminalSelection) []ducklord.TerminalSelection {
					return state.workspaceVisibleSelections(selected)
				})
			l.outputManager = l.workspaceOutput
			l.workspaceRepaint = l.workspaceOutput.RepaintReady()
			state.workspaceOutput = l.workspaceOutput
			state.clearOutputFocus = l.workspaceOutput.ClearInputFocus
		} else {
			l.outputManager, err = ducklord.NewTerminalOutputManager(ctx, cfg.RawOutputSubscriptionLimit(), source, state.snapshotStore)
		}
		if err != nil {
			return err
		}
		l.outputEvents = l.outputManager.Events()
		state.pooledOutput = true
		defer l.outputManager.Close()
	} else {
		defer state.saveCurrentSnapshot()
	}
	l.sessionUpdates = make(chan hostSessionUpdate, 32)
	l.watchedClients = make(map[string]hostWatchState)
	l.watcher, l.eventDriven = runner.(interface {
		WatchSessionUpdates(context.Context, ducklord.Client) <-chan ducklord.SessionUpdate
	})
	l.watchAllClients()
	state.refreshSessions(ctx)
	if l.outputManager == nil {
		state.refreshSelectedOutput(ctx)
	}
	state.render(os.Stdout)
	l.ticker = time.NewTicker(refresh)
	defer l.ticker.Stop()
	l.resizeSignals = make(chan os.Signal, 1)
	signal.Notify(l.resizeSignals, syscall.SIGWINCH)
	defer signal.Stop(l.resizeSignals)
	l.input = make(chan []byte, 8)
	l.inputReady = make(chan struct{}, 1)
	l.attachOut = make(chan attachOutputEvent, 32)
	l.attachOutputSource = (<-chan attachOutputEvent)(l.attachOut)
	l.resizeRequests = make(chan resizeRequest, 1)
	l.resizeResults = make(chan resizeDoneEvent, 1)
	l.startDone = make(chan startDoneEvent, 1)
	l.hostRetentionDone = make(chan hostRetentionEvent, 1)
	l.hostResourceDone = make(chan hostResourceEvent, 1)
	l.hostHookDone = make(chan hostHookEvent, 1)
	l.hostHookStatusDone = make(chan hostHookStatusEvent, 1)
	l.hostSkillsDone = make(chan hostSkillsEvent)
	state.hostSkillsDone = l.hostSkillsDone
	l.projectFilesDone = make(chan projectFilesEvent, 8)
	state.projectFiles.done = l.projectFilesDone
	state.projectFiles.lifecycle = ctx
	l.createDone = make(chan createDiscoveryEvent, 1)
	l.pathSuggestionsDone = make(chan pathSuggestionEvent, 1)
	l.addClientDone = make(chan addClientDoneEvent, 1)
	l.lifecycleDone = make(chan lifecycleDoneEvent, 1)
	l.previewDone = make(chan previewOutputEvent, 1)
	l.searchActivationTimeout = make(chan uint64, 1)
	l.previewInFlight = false
	l.previewQueued = false
	l.controlOpened = make(chan controlOpenEvent, 1)
	l.controlID = 0
	l.attachID = 0
	l.bufferedAttachBytes = 0
	l.resizeWorkerDone = make(chan struct{})
	go func() {
		defer close(l.resizeWorkerDone)
		runResizeWorker(ctx, l.resizeRequests, l.resizeResults)
	}()
	defer func() {
		stop()
		state.cancelCreateDiscovery()
		state.cancelPathSuggestions()
		state.createWorkers.Wait()
		<-l.resizeWorkerDone
	}()
	l.replay = workspaceInputReplay{input: make(chan []byte, 1)}
	l.startID = 0
	l.selectPooledOutput()
	go readInput(ctx, l.input, l.inputReady)
	l.inputPending = false
	l.panePrefixTimer = time.NewTimer(time.Hour)
	if !l.panePrefixTimer.Stop() {
		<-l.panePrefixTimer.C
	}
	defer l.stopPanePrefixTimer()
	l.lastOutputRender = time.Time{}
	for {
		l.queueDeferredReplays()
		// A continuously producing PTY can keep its output channel ready and
		// starve keyboard input in a Go select. Once input is queued, temporarily
		// remove the output case so controls such as Ctrl-C and Ctrl-] run first.
		attachOutputSelect := l.attachOutputSource
		outputEventsSelect := l.outputEvents
		inputSelect := l.replay.selectInput(l.input)
		if l.inputPending || len(l.input) != 0 || l.replay.queued {
			attachOutputSelect = nil
			outputEventsSelect = nil
		}
		select {
		case <-l.panePrefixTimer.C:
			l.onPanePrefixTimer()
		case <-l.inputReady:
			l.inputPending = true
		case event := <-l.hostRetentionDone:
			l.onHostRetentionDone(event)
		case event := <-l.hostResourceDone:
			l.onHostResourceDone(event)
		case event := <-l.hostHookDone:
			l.onHostHookDone(event)
		case event := <-l.hostHookStatusDone:
			l.onHostHookStatusDone(event)
		case event := <-l.hostSkillsDone:
			l.onHostSkillsDone(event)
		case event := <-l.projectFilesDone:
			l.onProjectFilesDone(event)
		case <-l.workspaceRepaint:
			l.onWorkspaceRepaint()
		case <-ctx.Done():
			l.shutdown()
			return nil
		case result := <-l.addClientDone:
			l.onAddClientDone(result)
		case <-l.ticker.C:
			l.onTick()
		case requestID := <-l.searchActivationTimeout:
			l.onSearchActivationTimeout(requestID)
		case event := <-outputEventsSelect:
			l.onOutputEvent(event)
		case opened := <-l.controlOpened:
			l.onControlOpened(opened)
		case controlErr := <-l.controlDone:
			l.onControlClosed(controlErr)
		case result := <-l.previewDone:
			l.onPreviewDone(result)
		case <-l.resizeSignals:
			l.onResizeSignal()
		case result := <-l.resizeResults:
			l.onResizeResult(result)
		case envelope := <-l.sessionUpdates:
			l.onSessionUpdate(envelope)
		case result := <-l.createDone:
			l.onCreateDiscovery(result)
		case result := <-l.pathSuggestionsDone:
			l.onPathSuggestions(result)
		case result := <-l.startDone:
			l.onStartDone(result)
		case result := <-l.lifecycleDone:
			l.onLifecycleDone(result)
		case chunk := <-attachOutputSelect:
			l.onAttachOutput(chunk)
		case b := <-inputSelect:
			if exit, err := l.onInput(b); exit {
				return err
			}
		}
	}
}
