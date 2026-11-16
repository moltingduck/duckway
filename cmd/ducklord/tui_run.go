package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
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
	defer state.notificationSink.Close()
	var outputManager tuiOutputManager
	var workspaceOutput *ducklord.WorkspaceOutputAdapter
	var outputEvents <-chan ducklord.TerminalOutputEvent
	var workspaceRepaint <-chan struct{}
	if source, ok := runner.(ducklord.TerminalOutputSource); ok {
		if state.workspacePreview {
			workspaceOutput, err = ducklord.NewWorkspaceOutputAdapter(ctx, cfg.RawOutputSubscriptionLimit(), source, state.snapshotStore,
				func(selected ducklord.TerminalSelection) []ducklord.TerminalSelection {
					return state.workspaceVisibleSelections(selected)
				})
			outputManager = workspaceOutput
			workspaceRepaint = workspaceOutput.RepaintReady()
			state.workspaceOutput = workspaceOutput
			state.clearOutputFocus = workspaceOutput.ClearInputFocus
		} else {
			outputManager, err = ducklord.NewTerminalOutputManager(ctx, cfg.RawOutputSubscriptionLimit(), source, state.snapshotStore)
		}
		if err != nil {
			return err
		}
		outputEvents = outputManager.Events()
		state.pooledOutput = true
		defer outputManager.Close()
	} else {
		defer state.saveCurrentSnapshot()
	}
	sessionUpdates := make(chan hostSessionUpdate, 32)
	watchedClients := make(map[string]hostWatchState)
	var watchEpoch uint64
	watcher, eventDriven := runner.(interface {
		WatchSessionUpdates(context.Context, ducklord.Client) <-chan ducklord.SessionUpdate
	})
	watchClient := func(client ducklord.Client) {
		if !eventDriven || state.disconnectedHosts[client.Name] {
			return
		}
		if _, exists := watchedClients[client.Name]; exists {
			return
		}
		watchEpoch++
		watchCtx, cancel := context.WithCancel(ctx)
		current := hostWatchState{cancel: cancel, epoch: watchEpoch}
		watchedClients[client.Name] = current
		state.eventDriven = true
		updates := watcher.WatchSessionUpdates(watchCtx, client)
		go func() {
			for update := range updates {
				select {
				case sessionUpdates <- hostSessionUpdate{update: update, epoch: current.epoch}:
				case <-watchCtx.Done():
					return
				}
			}
		}()
	}
	watchAllClients := func() {
		for _, client := range state.cfg.Clients {
			watchClient(client)
		}
	}
	watchAllClients()
	state.refreshSessions(ctx)
	if outputManager == nil {
		state.refreshSelectedOutput(ctx)
	}
	state.render(os.Stdout)
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	resizeSignals := make(chan os.Signal, 1)
	signal.Notify(resizeSignals, syscall.SIGWINCH)
	defer signal.Stop(resizeSignals)
	input := make(chan []byte, 8)
	inputReady := make(chan struct{}, 1)
	attachOut := make(chan attachOutputEvent, 32)
	attachOutputSource := (<-chan attachOutputEvent)(attachOut)
	resizeRequests := make(chan resizeRequest, 1)
	resizeResults := make(chan resizeDoneEvent, 1)
	startDone := make(chan startDoneEvent, 1)
	hostRetentionDone := make(chan hostRetentionEvent, 1)
	hostResourceDone := make(chan hostResourceEvent, 1)
	hostHookDone := make(chan hostHookEvent, 1)
	hostHookStatusDone := make(chan hostHookStatusEvent, 1)
	hostSkillsDone := make(chan hostSkillsEvent)
	state.hostSkillsDone = hostSkillsDone
	projectFilesDone := make(chan projectFilesEvent, 8)
	state.projectFiles.done = projectFilesDone
	state.projectFiles.lifecycle = ctx
	var hostRetentionCancel context.CancelFunc
	launchHostHookStatus := func(target string) {
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
		epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
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
			case hostHookStatusDone <- hostHookStatusEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, statuses: statuses, err: readErr}:
			case <-ctx.Done():
			}
		}()
	}
	createDone := make(chan createDiscoveryEvent, 1)
	pathSuggestionsDone := make(chan pathSuggestionEvent, 1)
	addClientDone := make(chan addClientDoneEvent, 1)
	lifecycleDone := make(chan lifecycleDoneEvent, 1)
	var lifecycleRequestID uint64
	previewDone := make(chan previewOutputEvent, 1)
	searchActivationTimeout := make(chan uint64, 1)
	var previewID uint64
	previewInFlight := false
	previewQueued := false
	var previewCancel context.CancelFunc
	var outputRequestID uint64
	var reconnectRequestID uint64
	var activeOutputEvent ducklord.TerminalOutputEvent
	var selectedOutputSession ducklord.RemoteSession
	selectPooledOutput := func() {
		if outputManager == nil {
			return
		}
		if state.workspacePaneMode && state.workspacePaneStep == "notes" {
			return
		}
		if len(state.sessions) == 0 {
			if workspaceOutput != nil {
				workspaceOutput.ClearVisible()
			}
			outputRequestID++
			selectedOutputSession = ducklord.RemoteSession{}
			activeOutputEvent = ducklord.TerminalOutputEvent{}
			state.outputForKey, state.outputText, state.terminal = "", "", nil
			state.outputFresh = false
			return
		}
		reconnectRequestID = 0
		state.outputReconnecting = false
		sess := state.activePTYSession()
		var workspacePriority ducklord.TerminalSelection
		if workspaceOutput != nil {
			visible := state.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: sess.Client}})
			if len(visible) == 0 {
				workspaceOutput.ClearVisible()
				outputRequestID++
				selectedOutputSession = ducklord.RemoteSession{}
				activeOutputEvent = ducklord.TerminalOutputEvent{}
				state.outputForKey = ""
				state.outputText = ""
				state.terminal = nil
				state.outputFresh = false
				state.outputErr = "current Project has no live Session pane"
				return
			}
			workspacePriority = state.workspacePreferredSelection(visible, sess)
			for _, candidate := range state.sessions {
				if candidate.Client == workspacePriority.Client.Name && candidate.InstanceID == workspacePriority.InstanceID && candidate.SessionID == workspacePriority.SessionID && candidate.RuntimeGeneration == workspacePriority.RuntimeGeneration {
					sess = candidate
					break
				}
			}
		}
		selectedOutputSession = sess
		key, keyOK := terminalOutputKey(sess)
		selectedKey := sessionKey(sess)
		if state.outputForKey != selectedKey || !keyOK || activeOutputEvent.Key != key || activeOutputEvent.Revision.RuntimeGeneration != sess.RuntimeGeneration {
			state.outputText = ""
			state.terminal = nil
			state.ptyScrollOffset = 0
			activeOutputEvent = ducklord.TerminalOutputEvent{}
			state.outputForKey = selectedKey
		}
		if workspaceOutput != nil {
			outputRequestID = workspaceOutput.Select(workspacePriority)
			state.outputForKey = selectedKey
			state.outputFresh = false
			state.outputErr = "loading live PTY output..."
			return
		}
		if sess.Client == "" || sess.InstanceID == "" || sess.SessionID == "" || sess.RuntimeGeneration == 0 || !canRead(sess) || !state.hostIsLive(sess.Client) {
			outputRequestID++
			selectedOutputSession = ducklord.RemoteSession{}
			activeOutputEvent = ducklord.TerminalOutputEvent{}
			state.outputForKey, state.outputText, state.terminal = "", "", nil
			state.outputFresh = false
			state.outputErr = "PTY output is unavailable until the host session is synchronized"
			return
		}
		client, clientErr := mustClient(cfg, sess.Client)
		if clientErr != nil {
			state.outputErr = clientErr.Error()
			return
		}
		rows, cols := state.activePTYSize()
		outputRequestID = outputManager.Select(ducklord.TerminalSelection{Client: client, InstanceID: sess.InstanceID, SessionID: sess.SessionID,
			RuntimeGeneration: sess.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
		state.outputForKey = selectedKey
		state.outputFresh = false
		state.outputErr = "loading live PTY output..."
	}
	startPreview := func() {
		if previewInFlight || !previewQueued || state.focused || len(state.sessions) == 0 {
			return
		}
		previewQueued = false
		previewInFlight = true
		id := previewID
		sess := state.currentSession()
		key, generation := sessionKey(sess), sess.RuntimeGeneration
		client, clientErr := mustClient(cfg, sess.Client)
		previewCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		previewCancel = cancel
		ref := sess.SessionID
		if ref == "" {
			ref = sess.Name
		}
		go func() {
			var text string
			err := clientErr
			if err == nil {
				if isolated, ok := runner.(interface {
					ReadPreview(context.Context, ducklord.Client, string, int) (string, error)
				}); ok {
					text, err = isolated.ReadPreview(previewCtx, client, ref, 80)
				} else {
					text, err = runner.Read(previewCtx, client, ref, 80)
				}
			}
			select {
			case previewDone <- previewOutputEvent{id: id, key: key, generation: generation, text: text, err: err}:
			case <-ctx.Done():
			}
		}()
	}
	requestPreview := func(fence bool) {
		if outputManager != nil {
			selectPooledOutput()
			return
		}
		if state.focused || len(state.sessions) == 0 {
			return
		}
		if fence || previewID == 0 {
			previewID++
		}
		previewQueued = true
		startPreview()
	}
	fencePreview := func() {
		if outputManager != nil {
			return
		}
		previewID++
		previewQueued = false
	}
	var attach *ducklord.AttachSession
	var control *ducklord.ControlSession
	var controlDone <-chan error
	var pendingControlSince time.Time
	controlOpened := make(chan controlOpenEvent, 1)
	controlID := 0
	var controlOpenCancel context.CancelFunc
	var attachCancel context.CancelFunc
	attachID := 0
	var attachCanResize bool
	var attachInitialResizeQueued bool
	var attachReplayEndOffset uint64
	var pendingFramebufferResize *resizeDoneEvent
	var resizeInFlight bool
	var queuedResize *[2]uint16
	openControlForSession := func(s ducklord.RemoteSession) bool {
		if outputManager == nil || !canAttach(s) || !state.hostIsLive(s.Client) {
			return false
		}
		c, err := mustClient(cfg, s.Client)
		if err != nil {
			state.outputErr = err.Error()
			return false
		}
		fencePreview()
		state.focused = false
		state.activeAttachKey = sessionKey(s)
		if workspaceOutput != nil {
			selectPooledOutput()
		}
		state.activeAttachFresh = state.outputFresh
		state.pendingAttachKey = ""
		controlID++
		attachID++
		resizeInFlight = false
		queuedResize = nil
		id := controlID
		if !state.canResizeCurrentSession() {
			state.focused = true
			state.outputErr = "read-only PTY view; yield control before sending input"
			return false
		}
		controlRunner, ok := runner.(interface {
			OpenControlSession(context.Context, ducklord.Client, string) (*ducklord.ControlSession, error)
		})
		if !ok {
			state.outputErr = "PTY control is unavailable"
			state.clearAttachIdentity()
			return false
		}
		state.outputErr = "opening PTY control..."
		key := sessionKey(s)
		if controlOpenCancel != nil {
			controlOpenCancel()
		}
		controlCtx, cancelControlOpen := context.WithCancel(ctx)
		controlOpenCancel = cancelControlOpen
		go func() {
			opened, openErr := controlRunner.OpenControlSession(controlCtx, c, s.SessionID)
			select {
			case controlOpened <- controlOpenEvent{id: id, key: key, control: opened, err: openErr}:
			case <-ctx.Done():
				if opened != nil {
					_ = opened.Stdin.Close()
				}
			}
		}()
		return true
	}
	var bufferedAttach []attachOutputEvent
	bufferedAttachBytes := 0
	resizeWorkerDone := make(chan struct{})
	go func() {
		defer close(resizeWorkerDone)
		runResizeWorker(ctx, resizeRequests, resizeResults)
	}()
	defer func() {
		stop()
		state.cancelCreateDiscovery()
		state.cancelPathSuggestions()
		state.createWorkers.Wait()
		<-resizeWorkerDone
	}()
	resizeCapability := func() func(uint16, uint16) (uint64, error) {
		var resize func(uint16, uint16) (uint64, error)
		expectedKey, keyOK := terminalOutputKey(state.activePTYSession())
		if outputManager != nil && control != nil && control.ResizeBarrier != nil && activeOutputEvent.Lease != 0 && keyOK && activeOutputEvent.Key == expectedKey &&
			activeOutputEvent.Revision.RuntimeGeneration == control.RuntimeGeneration && control.RuntimeGeneration == state.activePTYSession().RuntimeGeneration {
			event := activeOutputEvent
			controlSnapshot := control
			resize = func(rows, cols uint16) (uint64, error) {
				return outputManager.Resize(event, rows, cols, controlSnapshot.ResizeBarrier)
			}
		} else if attach != nil {
			resize = attach.ResizeBarrier
		}
		return resize
	}
	queueResize := func() {
		if !attachCanResize {
			return
		}
		resize := resizeCapability()
		if resize == nil {
			return
		}
		rows, cols := state.activePTYSize()
		if !attachInitialResizeQueued && !initialReplayCaughtUp(state.terminalOffset, attachReplayEndOffset) {
			// SIGWINCH may arrive while the initial replay is still being
			// consumed. Remember only the latest dimensions; starting a barrier
			// here would hide the replay until the network RPC returned.
			queuedResize = &[2]uint16{rows, cols}
			return
		}
		if resizeInFlight || pendingFramebufferResize != nil {
			queuedResize = &[2]uint16{rows, cols}
			return
		}
		request := resizeRequest{id: attachID, rows: rows, cols: cols, resize: resize}
		select {
		case resizeRequests <- request:
			resizeInFlight = true
		default:
			select {
			case <-resizeRequests:
			default:
			}
			select {
			case resizeRequests <- request:
				resizeInFlight = true
			default:
			}
		}
	}
	resetWorkspacePaneChange := func() bool {
		if !state.workspacePaneChanged {
			return false
		}
		state.workspacePaneChanged = false
		controlID++
		attachID++
		resizeInFlight = false
		pendingFramebufferResize = nil
		queuedResize = nil
		bufferedAttach = nil
		bufferedAttachBytes = 0
		attachOutputSource = attachOut
		attachCanResize = false
		attachInitialResizeQueued = false
		attachReplayEndOffset = 0
		if controlOpenCancel != nil {
			controlOpenCancel()
			controlOpenCancel = nil
		}
		if control != nil {
			_ = control.Stdin.Close()
			control, controlDone = nil, nil
		}
		if attachCancel != nil {
			attachCancel()
			attachCancel = nil
		}
		if attach != nil {
			_ = attach.Stdin.Close()
			attach = nil
		}
		state.clearAttachIdentity()
		state.focused = false
		if workspaceOutput != nil {
			selectPooledOutput()
		}
		return true
	}
	resetWorkspacePaneChangeIfNeeded := func() bool {
		// Closing a focused detach confirmation with Esc leaves the modal closed
		// while the exact source Session control is being rebound.  Do not let
		// the generic workspace reset invalidate that pending restore; once the
		// lease is accepted, normal changes can still reset as usual.
		if state.workspacePaneFocusRestorePending && !state.workspacePaneChanged {
			return false
		}
		// A focused Default detach confirmation temporarily owns the keyboard,
		// but must keep the originating PTY lease available for Esc restoration.
		// Confirming the detach closes the modal first, so the normal reset still
		// tears down the detached pane's lease.
		if state.workspacePaneMode && state.workspacePaneStep == "detach-confirm" && state.workspacePaneRestoreFocused {
			state.workspacePaneChanged = false
			return false
		}
		return resetWorkspacePaneChange()
	}
	replay := workspaceInputReplay{input: make(chan []byte, 1)}
	acceptWorkspaceControl := func(restored ...ducklord.RemoteSession) bool {
		// A workspace modal owns input while it is open.  Async output/control
		// readiness must not reclaim the PTY focus that opening the modal
		// deliberately released; closeNotesModal restores it through the
		// deferred focus lease.
		if workspaceOutput == nil || control == nil || !state.workspaceControlMayAccept() {
			return false
		}
		session := state.activePTYSession()
		if len(restored) != 0 {
			session = restored[0]
		}
		// A focused Default detach modal is closed before its original PTY
		// control is restored, so the workspace pane rectangle is temporarily
		// unavailable.  The captured source session is authoritative for this
		// handoff; requiring the modal rectangle here strands Esc on the list.
		if (!state.workspacePaneFocusRestorePending && func() bool {
			_, err := state.workspacePaneRect()
			return err != nil
		}()) || !state.hostIsLive(session.Client) || !controlMatchesSession(control, session, state.ownerName) {
			_ = control.Stdin.Close()
			control, controlDone = nil, nil
			state.clearAttachIdentity()
			state.outputErr = "Session pane or writer changed before PTY control became ready"
			return false
		}
		if !acceptWorkspaceControlInput(state, workspaceOutput, control, session, &replay) {
			return false // the visible raw-output lease may still be opening
		}
		pendingControlSince = time.Time{}
		attachCanResize = control.ResizeBarrier != nil
		if nav, err := state.workspaceNavigation(); err == nil {
			width, height := terminalSize()
			_, _ = nav.FocusVisiblePane(ducklord.CalculateWorkspaceGeometry(width, height, 4))
		}
		state.outputErr = ""
		if state.sessionFreshlyDisplayed(session) {
			state.markActivitySeen(session)
		}
		if resizeCapability() != nil && !attachInitialResizeQueued {
			attachInitialResizeQueued = true
			queueResize()
		}
		return true
	}
	restorePendingFocus := func() bool {
		restored := restorePendingNotesFocus(state, workspaceOutput, state.outputFresh)
		// The output lease and the PTY writer become ready independently.  Help
		// must not advertise terminal focus until the writer has arrived as well;
		// otherwise the first byte after closing the overlay is silently discarded
		// while control is still nil.
		restored = restorePendingHelpFocusWithAttach(state, workspaceOutput, state.outputFresh, control, attach) || restored
		if restorePendingWorkspacePaneFocus(state, workspaceOutput, state.outputFresh, control) {
			restored = true
		}
		return restored
	}
	finishAttach := func(event attachOutputEvent) {
		// Closing a focused Default detach modal can restore the existing
		// control lease while the old attach stream is still finishing.  Its
		// terminal event belongs to that old stream and must not tear down the
		// focus that Esc just returned to the source Session.
		preserveRestoredFocus := state.workspacePaneFocusRestoreLease || state.workspacePaneFocusRestorePending
		attachID++
		resizeInFlight = false
		pendingFramebufferResize = nil
		queuedResize = nil
		bufferedAttach = nil
		bufferedAttachBytes = 0
		attachOutputSource = attachOut
		wasFocused := state.focused
		if !preserveRestoredFocus {
			state.focused = false
			state.clearAttachIdentity()
		}
		attachCanResize = false
		attachInitialResizeQueued = false
		attachReplayEndOffset = 0
		attach = nil
		if attachCancel != nil {
			attachCancel()
			attachCancel = nil
		}
		requestPreview(true)
		if event.err != nil && wasFocused {
			state.outputErr = event.err.Error()
		}
	}
	var startCancel context.CancelFunc
	startID := 0
	selectPooledOutput()
	go readInput(ctx, input, inputReady)
	inputPending := false
	clearWorkspacePlacementInput := func() {
		state.workspacePlacementInputPending = false
		replay.deferred = nil
		replay.navigation = false
		replay.toPTY = false
	}
	panePrefixTimer := time.NewTimer(time.Hour)
	if !panePrefixTimer.Stop() {
		<-panePrefixTimer.C
	}
	stopPanePrefixTimer := func() {
		if !panePrefixTimer.Stop() {
			select {
			case <-panePrefixTimer.C:
			default:
			}
		}
	}
	resetPanePrefixTimer := func(deadline time.Time) {
		stopPanePrefixTimer()
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d < 0 {
				d = 0
			}
			panePrefixTimer.Reset(d)
		}
	}
	queuePaneReplay := func() {
		if replay.queued || len(state.panePrefixReplay) == 0 {
			return
		}
		pending := state.takePanePrefixReplay()
		replay.input <- pending
		replay.queued = true
	}
	defer stopPanePrefixTimer()
	lastOutputRender := time.Time{}
	renderOutput := func() {
		if lastOutputRender.IsZero() || time.Since(lastOutputRender) >= 50*time.Millisecond {
			state.render(os.Stdout)
			lastOutputRender = time.Now()
		}
	}
	for {
		// An earlier local replay can occupy the single replay slot when a
		// writer/output completion accepts focus. Retry after that replay is
		// consumed, before selecting any newer keyboard bytes.
		replay.retryWorkspacePlacement(state, control)
		// Inventory placement selects a newly created Session asynchronously. Queue
		// the same local attach action used by Enter so the selected pane acquires
		// a PTY control lease before the next user bytes are handled.
		if state.workspacePlacementFocusPending && !state.workspacePaneFocusRestorePending && !replay.queued {
			state.workspacePlacementFocusPending = false
			// A quick-shell placement replaces the pane owned by the current
			// control session. Release that old writer before replaying the local
			// attach action; otherwise the replayed Enter is delivered to the old
			// PTY while activeAttachKey already names the newly created Session.
			if control != nil {
				controlID++
				if controlOpenCancel != nil {
					controlOpenCancel()
					controlOpenCancel = nil
				}
				_ = control.Stdin.Close()
				control = nil
				controlDone = nil
			}
			state.focused = false
			if state.clearOutputFocus != nil {
				state.clearOutputFocus()
			}
			// Placement changes the selected workspace pane after the output
			// manager has already selected the old pane.  Select the created
			// Session before opening its writer lease, otherwise the lease
			// completion cannot transfer raw input focus to the new PTY.
			selectPooledOutput()
			replay.input <- []byte("\r")
			replay.queued = true
		}
		// A prefix command can finish while Help's asynchronous PTY focus lease
		// is still pending.  Keep it queued until the lease restores terminal
		// focus; dispatching it immediately would apply the command to the
		// navigation pane.
		if state.focused && !state.helpFocusRestorePending && !replay.queued && len(state.panePrefixReplay) != 0 {
			replay.input <- state.takePanePrefixReplay()
			replay.queued = true
		}
		// A continuously producing PTY can keep its output channel ready and
		// starve keyboard input in a Go select. Once input is queued, temporarily
		// remove the output case so controls such as Ctrl-C and Ctrl-] run first.
		attachOutputSelect := attachOutputSource
		outputEventsSelect := outputEvents
		inputSelect := replay.selectInput(input)
		if inputPending || len(input) != 0 || replay.queued {
			attachOutputSelect = nil
			outputEventsSelect = nil
		}
		select {
		case <-panePrefixTimer.C:
			if state.panePrefixPending && state.panePrefixSuffix != "" && !time.Now().Before(state.panePrefixDeadline) {
				command := state.panePrefixSuffix
				state.panePrefixPending, state.panePrefixSuffix = false, ""
				state.panePrefixDeadline = time.Time{}
				state.dispatchPaneCommand(command)
				if pending := state.takePanePrefixReplay(); len(pending) != 0 {
					replay.input <- pending
					replay.queued = true
				}
				state.render(os.Stdout)
			}
		case <-inputReady:
			inputPending = true
		case event := <-hostRetentionDone:
			if event.id == state.hostMenuRequestID && hostRetentionCancel != nil {
				hostRetentionCancel()
				hostRetentionCancel = nil
			}
			watch := watchedClients[event.host]
			if !state.acceptHostRetentionEvent(event, watch.epoch) {
				continue
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
		case event := <-hostResourceDone:
			watch := watchedClients[event.host]
			if !state.acceptHostResourceEvent(event, watch.epoch) {
				continue
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
		case event := <-hostHookDone:
			state.finishHostHookOperation(event.host, event.id)
			if event.id == state.hostMenuRequestID && hostRetentionCancel != nil {
				hostRetentionCancel()
				hostRetentionCancel = nil
			}
			watch := watchedClients[event.host]
			if !state.acceptHostHookEvent(event, watch.epoch) {
				continue
			}
			if event.err != nil {
				state.hostMenuStep = "hook-error"
				state.hostMenuErr = sanitizeTerminalText(event.err.Error())
			} else {
				state.hostMenuStep = "hook-done"
				state.hostMenuErr = hostHookResultMessage(event)
				launchHostHookStatus(event.host)
			}
			state.render(os.Stdout)
		case event := <-hostHookStatusDone:
			if event.id != state.hostHookStatusRequestID || !state.hostMenuMode || !strings.HasPrefix(state.hostMenuStep, "hook-") ||
				event.host != state.hostMenuTarget || state.disconnectedHosts[event.host] || event.epoch != watchedClients[event.host].epoch ||
				event.instanceID != state.hostSync[event.host].InstanceID {
				continue
			}
			state.hostHookStatusLoading = false
			if event.err != nil {
				state.hostHookStatusErr = "Host hook status unavailable"
			} else {
				state.hostHookStatuses = event.statuses
			}
			state.render(os.Stdout)
		case event := <-hostSkillsDone:
			if event.action == "list" {
				if event.id != state.hostSkillsListRequestID || !state.hostMenuMode || state.hostMenuStep != "skills-dashboard" {
					ducklord.CleanupSkillPreview(event.preview)
					continue
				}
				if state.hostSkillsListCancel != nil {
					state.hostSkillsListCancel()
					state.hostSkillsListCancel = nil
				}
				state.applyHostSkillsEvent(event)
				state.render(os.Stdout)
				continue
			}
			if event.id != state.hostSkillsRequestID || !state.hostSkillsBusy {
				ducklord.CleanupSkillPreview(event.preview)
				continue
			}
			state.applyHostSkillsEvent(event)
			state.render(os.Stdout)
		case event := <-projectFilesDone:
			state.applyProjectFilesEvent(event)
			if state.projectFiles.open {
				state.render(os.Stdout)
			}
		case <-workspaceRepaint:
			if control != nil && !state.focused {
				if state.workspacePaneFocusRestorePending {
					restoreSession := state.activePTYSession()
					if state.workspacePaneFocusRestoreKey != "" {
						if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
							restoreSession = captured
						} else if state.workspacePaneFocusRestoreSession.Client != "" {
							restoreSession = state.workspacePaneFocusRestoreSession
						}
					}
					if acceptWorkspaceControl(restoreSession) {
						restorePendingFocus()
					}
				} else {
					acceptWorkspaceControl()
				}
			}
			state.render(os.Stdout)
		case <-ctx.Done():
			state.cleanupHostSkills()
			state.cancelAddClientWork()
			if controlOpenCancel != nil {
				controlOpenCancel()
			}
			if control != nil {
				_ = control.Stdin.Close()
			}
			if attachCancel != nil {
				attachCancel()
			}
			if startCancel != nil {
				startCancel()
			}
			state.cancelCreateDiscovery()
			return nil
		case result := <-addClientDone:
			if result.progress {
				if state.addClientMode && state.addClientBusy && result.id == state.addClientRequestID {
					state.addClientErr = result.status
					state.render(os.Stdout)
				}
				continue
			}
			if state.completeAddClient(result) {
				state.bumpHostConnectionEpoch(result.client.Name)
				delete(state.disconnectedHosts, result.client.Name)
				watchAllClients()
			}
			state.render(os.Stdout)
		case <-ticker.C:
			// Start and inventory are separate RPCs. While a new pane is
			// awaiting its Session, poll even in event-driven mode so a missed
			// update cannot strand the placement or its deadline forever.
			hadPendingPlacement := len(state.pendingWorkspacePlacements) > 0
			if hadPendingPlacement {
				state.refreshSessions(ctx)
			}
			if workspaceOutput != nil && control != nil && !state.focused {
				if !pendingControlSince.IsZero() && time.Since(pendingControlSince) >= 15*time.Second {
					_ = control.Stdin.Close()
					control, controlDone = nil, nil
					state.clearAttachIdentity()
					state.outputErr = "PTY focus timed out waiting for Session output"
				} else {
					if state.workspacePaneFocusRestorePending {
						restoreSession := state.activePTYSession()
						if state.workspacePaneFocusRestoreKey != "" {
							if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
								restoreSession = captured
							} else if state.workspacePaneFocusRestoreSession.Client != "" {
								restoreSession = state.workspacePaneFocusRestoreSession
							}
						}
						if acceptWorkspaceControl(restoreSession) {
							restorePendingFocus()
						}
					} else {
						acceptWorkspaceControl()
					}
				}
				state.render(os.Stdout)
				continue
			}
			if !state.focused && !state.newSessionMode && !state.searchMode {
				if !state.eventDriven && !hadPendingPlacement {
					state.refreshSessions(ctx)
				}
				requestPreview(false)
			}
			state.render(os.Stdout)
		case requestID := <-searchActivationTimeout:
			if state.searchMode && state.searchPendingRequestID == requestID {
				state.searchPendingRequestID = 0
				state.searchErr = "PTY activation timed out; try again"
				selectPooledOutput()
				state.render(os.Stdout)
			}
		case event := <-outputEventsSelect:
			expectedSession := state.activePTYSession()
			if workspaceOutput != nil {
				expectedSession = selectedOutputSession
			}
			if state.searchMode && state.searchActivatedKey != "" {
				if activated, ok := state.sessionForKey(state.searchActivatedKey); ok {
					expectedSession = activated
				}
			}
			searchActivation := state.searchMode && state.searchPendingRequestID != 0 && event.RequestID == state.searchPendingRequestID
			if searchActivation {
				if state.searchPendingRevision != state.searchRevision || !state.selectableSessionMatches(state.searchPendingKey, state.searchPendingGeneration) {
					state.searchPendingRequestID = 0
					state.searchErr = "session changed while switching; try again"
					selectPooledOutput()
					state.render(os.Stdout)
					continue
				}
				expectedSession, _ = state.sessionForKey(state.searchPendingKey)
			}
			expectedKey, expectedOK := terminalOutputKey(expectedSession)
			if searchActivation && (!expectedOK || event.Key != expectedKey || event.Revision.RuntimeGeneration != expectedSession.RuntimeGeneration) {
				state.searchPendingRequestID = 0
				state.searchErr = "PTY activation returned stale session output; try again"
				selectPooledOutput()
				state.render(os.Stdout)
				continue
			}
			if event.RequestID != outputRequestID || !expectedOK || event.Key != expectedKey || event.Revision.RuntimeGeneration != expectedSession.RuntimeGeneration || event.Err != nil {
				if event.RequestID == outputRequestID && event.Err != nil {
					state.outputReconnecting = false
					if event.RequestID == reconnectRequestID {
						reconnectRequestID = 0
						state.outputStale = true
						state.outputErr = "PTY reconnect failed; previous live view retained"
						state.render(os.Stdout)
						continue
					}
					if searchActivation {
						state.searchPendingRequestID = 0
						state.searchErr = "PTY output stream unavailable"
						selectPooledOutput()
					} else {
						state.outputFresh = false
						state.outputErr = "PTY output stream unavailable"
					}
					state.render(os.Stdout)
				}
				continue
			}
			var view ducklord.PooledTerminalView
			var viewErr error
			if event.FinalView != nil {
				view = *event.FinalView
			} else {
				view, viewErr = outputManager.View(event)
				if viewErr != nil {
					if searchActivation {
						state.searchPendingRequestID = 0
						state.searchErr = "PTY framebuffer is unavailable; try again"
						selectPooledOutput()
						state.render(os.Stdout)
					}
					continue
				}
			}
			if searchActivation && (!view.Ready || view.Disconnected || view.Ended) {
				state.searchPendingRequestID = 0
				state.searchErr = "PTY output is not ready; try again"
				selectPooledOutput()
				state.render(os.Stdout)
				continue
			}
			terminal, valid := ducklord.NewTerminalFromState(view.Framebuffer, ducklord.DefaultTerminalScrollback)
			if !valid {
				if searchActivation {
					state.searchPendingRequestID = 0
					state.searchErr = "PTY framebuffer is invalid"
					selectPooledOutput()
				} else {
					state.outputFresh = false
					state.outputErr = "PTY framebuffer is invalid"
				}
				state.render(os.Stdout)
				continue
			}
			if searchActivation && !state.prepareSearchWorkspaceSelection(state.searchPendingKey) {
				selectPooledOutput()
				state.render(os.Stdout)
				continue
			}
			state.terminal = terminal
			state.ptyScrollOffset = 0
			activeOutputEvent = event
			activeOutputEvent.FinalView = nil
			state.outputText = terminal.Text()
			state.terminalGeneration = view.RuntimeGeneration
			state.terminalOffset = view.OutputOffset
			state.terminalCursorValid = view.Ready && !view.Disconnected
			state.outputStale = false
			state.outputReconnecting = false
			if event.RequestID == reconnectRequestID {
				reconnectRequestID = 0
			}
			state.outputFresh = view.Ready && !view.Disconnected
			state.outputErr = view.Error
			if view.Ended {
				state.outputErr = "PTY process ended"
			}
			if state.outputFresh {
				restorePendingFocus()
				drainNotesPendingInput(state, control)
			}
			if searchActivation {
				state.outputForKey = state.searchPendingKey
				state.activeAttachKey = state.searchPendingKey
				state.activeAttachFresh = state.outputFresh
				state.searchActivatedKey = state.searchPendingKey
				state.searchActivatedRevision = state.searchPendingRevision
				state.searchActivatedGeneration = state.searchPendingGeneration
				state.searchPendingRequestID = 0
				state.searchErr = "Active · Enter again to focus"
			}
			if control != nil && !state.focused {
				acceptWorkspaceControl()
			}
			if state.sessionFreshlyDisplayed(state.activePTYSession()) {
				state.markActivitySeen(state.activePTYSession())
			}
			if state.focused && control != nil && !attachInitialResizeQueued && resizeCapability() != nil {
				attachInitialResizeQueued = true
				queueResize()
			}
			renderOutput()
		case opened := <-controlOpened:
			paneVisible := true
			if state.workspacePreview {
				_, paneErr := state.workspacePaneRect()
				paneVisible = paneErr == nil
			}
			openedForCurrentAttach := opened.id == controlID && opened.key == state.activeAttachKey
			restoringFocusedPane := state.workspacePaneFocusRestorePending && openedForCurrentAttach
			restoreSession := state.activePTYSession()
			if restoringFocusedPane && state.workspacePaneFocusRestoreKey != "" {
				if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
					restoreSession = captured
				} else if state.workspacePaneFocusRestoreSession.Client != "" {
					restoreSession = state.workspacePaneFocusRestoreSession
				}
			}
			if state.workspacePaneMode || state.workspaceProjectFocus || !paneVisible && !restoringFocusedPane || !openedForCurrentAttach || opened.control != nil && !controlMatchesSession(opened.control, restoreSession, state.ownerName) {
				if opened.control != nil {
					_ = opened.control.Stdin.Close()
				}
				// A late result from an older attach must never clear the identity
				// captured by a newly placed quick-shell pane.  The id alone is not
				// sufficient when a replacement attach reuses the current lifecycle.
				if openedForCurrentAttach {
					if controlOpenCancel != nil {
						controlOpenCancel()
						controlOpenCancel = nil
					}
					clearWorkspacePlacementInput()
					state.clearAttachIdentity()
				}
				if !paneVisible && opened.id == controlID {
					state.clearAttachIdentity()
					state.focused = false
					state.outputErr = "Session pane became hidden; PTY control was not opened"
					state.render(os.Stdout)
				}
				continue
			}
			controlOpenCancel = nil
			if opened.err != nil {
				state.outputErr = sanitizeTerminalText(opened.err.Error())
				if openedForCurrentAttach {
					clearWorkspacePlacementInput()
				}
				state.clearAttachIdentity()
				state.render(os.Stdout)
				continue
			}
			if opened.control == nil {
				state.outputErr = "PTY control unavailable"
				if openedForCurrentAttach {
					clearWorkspacePlacementInput()
				}
				state.clearAttachIdentity()
				state.render(os.Stdout)
				continue
			}
			control = opened.control
			controlDone = control.Done
			if workspaceOutput != nil {
				var restoreSession ducklord.RemoteSession
				if state.workspacePaneFocusRestorePending && state.workspacePaneFocusRestoreKey != "" {
					if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
						restoreSession = captured
					}
					if restoreSession.Client == "" {
						restoreSession = state.workspacePaneFocusRestoreSession
					}
				}
				if restoreSession.Client == "" {
					if !acceptWorkspaceControl() {
						// Keep the placement fence until the output lease is accepted.
						// Host Skills can restore pooled output before its replacement
						// writer; clearing here drops the first command after Ctrl-C.
						if control != nil {
							pendingControlSince = time.Now()
							state.outputErr = "waiting for Session pane output before PTY input"
						}
						state.render(os.Stdout)
						continue
					}
				} else if !acceptWorkspaceControl(restoreSession) {
					// A restored focused pane may receive its control writer before
					// the output lease is ready. Keep the first post-Esc command
					// fenced until the lease is accepted; otherwise it falls through
					// to the Session list and is lost.
					if state.workspacePlacementInputPending && !restoringFocusedPane {
						clearWorkspacePlacementInput()
					}
					if control != nil {
						pendingControlSince = time.Now()
						state.outputErr = "waiting for Session pane output before PTY input"
					}
					state.render(os.Stdout)
					continue
				}
				restorePendingFocus()
				drainNotesPendingInput(state, control)
				state.render(os.Stdout)
				continue
			}
			state.focused = true
			replay.releaseWorkspacePlacement(state)
			attachCanResize = control.ResizeBarrier != nil
			state.outputErr = ""
			if state.sessionFreshlyDisplayed(state.activePTYSession()) {
				state.markActivitySeen(state.activePTYSession())
			}
			if resizeCapability() != nil {
				attachInitialResizeQueued = true
				queueResize()
			}
			state.render(os.Stdout)
		case controlErr := <-controlDone:
			controlDone = nil
			// The previous control stream can finish after Esc has handed the
			// saved Session back from the detach modal. Its completion belongs to
			// that stale stream; do not tear down the newly restored focus lease.
			if consumeWorkspaceRestoreControlDone(state) {
				// The restored writer itself may be the stream that just completed.
				// Reopen it against the captured Session before accepting keyboard
				// input; otherwise the UI advertises focus with no live writer.
				if state.activeAttachKey != "" {
					restoreSession := state.activePTYSession()
					if state.workspacePaneFocusRestoreKey != "" {
						if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
							restoreSession = captured
						}
					}
					state.workspacePaneFocusRestorePending = true
					state.workspacePaneFocusRestoreKey = sessionKey(restoreSession)
					state.workspacePaneFocusRestoreSession = restoreSession
					state.focused = false
					control = nil
					state.workspacePlacementInputPending = true
					openControlForSession(restoreSession)
				}
				continue
			}
			control = nil
			attachCanResize = false
			if state.focused || workspaceOutput != nil && state.activeAttachKey != "" {
				state.focused = false
				state.clearAttachIdentity()
			}
			if controlErr != nil {
				state.outputErr = "PTY control connection closed"
			}
			state.render(os.Stdout)
		case result := <-previewDone:
			if previewCancel != nil {
				previewCancel()
				previewCancel = nil
			}
			previewInFlight = false
			if state.applyPreviewOutput(result, previewID) {
				state.render(os.Stdout)
			}
			startPreview()
		case <-resizeSignals:
			queueResize()
			state.render(os.Stdout)
		case result := <-resizeResults:
			if result.id == attachID {
				resizeInFlight = false
				state.resizeStatus = ""
				if outputManager != nil {
					if result.err != nil {
						state.resizeStatus = "resize failed: " + sanitizeTerminalText(result.err.Error())
					}
					if queuedResize != nil {
						queuedResize = nil
						queueResize()
					}
					state.render(os.Stdout)
					continue
				}
				if result.err != nil {
					state.resizeStatus = "resize failed: " + sanitizeTerminalText(result.err.Error())
				} else if state.terminal != nil && result.barrier > state.terminalOffset {
					pendingFramebufferResize = &result
					state.resizeStatus = "resize pending output drain"
				} else if state.terminal != nil {
					state.terminal.Resize(int(result.rows), int(result.cols))
				}
				completion := drainBufferedAttach(state, bufferedAttach, &pendingFramebufferResize)
				bufferedAttach = nil
				bufferedAttachBytes = 0
				attachOutputSource = attachOut
				if completion != nil {
					finishAttach(*completion)
				} else if pendingFramebufferResize == nil && queuedResize != nil {
					queued := *queuedResize
					queuedResize = nil
					if resize := resizeCapability(); resize != nil {
						resizeRequests <- resizeRequest{id: attachID, rows: queued[0], cols: queued[1], resize: resize}
						resizeInFlight = true
					}
				}
				state.render(os.Stdout)
			}
		case envelope := <-sessionUpdates:
			update := envelope.update
			currentWatch, current := watchedClients[update.Client]
			if !current || currentWatch.epoch != envelope.epoch || state.disconnectedHosts[update.Client] {
				continue
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
			if workspaceOutput != nil && update.ChangedSessionID != "" {
				selected := state.currentSession()
				previousWorkspaceSelections = state.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: selected.Client}})
			}
			if !state.applySessionUpdate(update) {
				continue
			}
			state.retryWorkspaceCreateDiscovery(ctx, createDone, previousHost, update)
			if state.invalidateCreateStart(update) {
				if startCancel != nil {
					startCancel()
					startCancel = nil
				}
				startID++ // fence a completion racing with cancellation
			}
			if outputManager != nil {
				if previousInstance != "" && update.InstanceID != "" && update.InstanceID != previousInstance {
					outputManager.ForgetHost(update.Client, previousInstance)
				} else if previousHost.InstanceID != "" && previousHost.State == "live" && update.State != "live" {
					outputManager.SyncHost(update.Client, previousHost.InstanceID, false, nil)
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
					outputManager.SyncHost(update.Client, update.InstanceID, true, selections)
				}
				workspaceSelectionsChangedByUpdate := false
				if workspaceOutput != nil && update.ChangedSessionID != "" {
					selected := state.currentSession()
					workspaceSelectionsChangedByUpdate = workspaceSelectionsChanged(previousWorkspaceSelections,
						state.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: selected.Client}}))
				}
				if workspaceOutput != nil && (previousHost.State != update.State || previousInstance != update.InstanceID || workspaceSelectionsChangedByUpdate) {
					selectPooledOutput()
				}
			}
			if control != nil && (!state.hostIsLive(state.activePTYSession().Client) || !controlMatchesSession(control, state.activePTYSession(), state.ownerName)) {
				controlID++
				if controlOpenCancel != nil {
					controlOpenCancel()
					controlOpenCancel = nil
				}
				_ = control.Stdin.Close()
				control, controlDone = nil, nil
				attachCanResize = false
				state.focused = false
				state.clearAttachIdentity()
				state.outputErr = "PTY control disconnected; reconnect and focus it again"
			}
			if cancelRemovedPTYControl(state, previousActive, &control, &controlOpenCancel, &controlID) {
				controlDone = nil
				if attachCancel != nil {
					attachCancel()
					attachCancel = nil
				}
				if attach != nil {
					_ = attach.Stdin.Close()
				}
				attachID++
				resizeInFlight = false
				pendingFramebufferResize = nil
				queuedResize = nil
				bufferedAttach = nil
				bufferedAttachBytes = 0
				attachOutputSource = attachOut
				attach = nil
				attachCanResize = false
				attachInitialResizeQueued = false
				attachReplayEndOffset = 0
			}
			if outputManager != nil && !state.searchMode {
				selectPooledOutput()
			} else if !state.focused && state.activeAttachKey == "" {
				requestPreview(false)
			}
			state.render(os.Stdout)
		case result := <-createDone:
			clientName, _, args, ready := state.applyCreateDiscovery(result)
			if ready {
				client, clientErr := mustClient(cfg, clientName)
				if clientErr != nil {
					state.newSessionErr = clientErr.Error()
				} else {
					startCtx, cancel := context.WithCancel(ctx)
					startCancel = cancel
					startID++
					id := startID
					state.newSessionStarting = true
					state.newSessionStartGeneration, state.newSessionStartInstance = state.hostFingerprint(clientName)
					state.newSessionStartEpoch = state.hostConnectionEpoch[clientName]
					state.newSessionErr = "starting..."
					go func() {
						sessionID, startErr := runner.Start(startCtx, client, args)
						select {
						case startDone <- startDoneEvent{id: id, client: clientName, sessionID: sessionID, err: startErr}:
						case <-ctx.Done():
						}
					}()
				}
			}
			state.render(os.Stdout)
		case result := <-pathSuggestionsDone:
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
		case result := <-startDone:
			if result.id != startID {
				continue
			}
			if state.disconnectedHosts[result.client] {
				startCancel = nil
				state.newSessionStarting = false
				if state.workspaceNewSessionIntent != nil {
					state.newSessionErr = "host disconnected; shell may have started—resync, then reopen Add Session pane"
				} else {
					state.newSessionErr = "host disconnected; remote start result was discarded"
				}
				state.workspaceNewSessionIntent = nil
				state.render(os.Stdout)
				continue
			}
			startCancel = nil
			state.completeNewSessionStart(ctx, result.client, result.sessionID, result.err)
			if outputManager != nil && result.err == nil {
				selectPooledOutput()
			}
			state.render(os.Stdout)
		case result := <-lifecycleDone:
			currentWatch := watchedClients[result.client]
			if result.id != lifecycleRequestID {
				continue
			}
			state.lifecycleBusy = false
			if !state.lifecycleResultCurrent(result, lifecycleRequestID, currentWatch.epoch) {
				state.outputErr = string(result.operation) + " outcome unknown after Host connection changed; resync before retrying"
				state.render(os.Stdout)
				continue
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
				requestPreview(false)
			}
			state.render(os.Stdout)
		case chunk := <-attachOutputSelect:
			if chunk.id != attachID {
				continue
			}
			if resizeInFlight {
				bufferedAttach = append(bufferedAttach, chunk)
				bufferedAttachBytes += len(chunk.text)
				if bufferedAttachBytes >= maxResizeBufferedBytes {
					attachOutputSource = nil
				}
				continue
			}
			if chunk.done {
				finishAttach(chunk)
				// A burst of PTY output can keep this select case ready. Avoid
				// adding another potentially blocking frame write when keyboard
				// input is already queued; the next iteration services input first.
				if len(input) == 0 {
					renderOutput()
				}
				continue
			}
			applyOrderedAttachChunk(state, chunk, &pendingFramebufferResize)
			if !attachInitialResizeQueued && attachCanResize && initialReplayCaughtUp(state.terminalOffset, attachReplayEndOffset) {
				// Show the initial replay before waiting on the remote resize
				// barrier. Bytes emitted after this point remain ordered by the
				// existing barrier buffering path.
				attachInitialResizeQueued = true
				queuedResize = nil
				queueResize()
			}
			if pendingFramebufferResize == nil && queuedResize != nil {
				queuedResize = nil
				queueResize()
			}
			// Rendering each output chunk can monopolize the event loop while a
			// command such as `yes` is producing output. Let queued controls (in
			// particular Ctrl-C and the unfocus key) run before another frame.
			if len(input) == 0 {
				renderOutput()
			}
		case b := <-inputSelect:
			replayedInput := replay.queued
			if replay.queued {
				replay.queued = false
			}
			workspaceNavigationReplay := replay.navigation
			replay.navigation = false
			if shouldRouteWorkspaceReplayAsNavigation(workspaceNavigationReplay) {
				// The replacement lease may have restored focused before the
				// deferred key arrives; route it through workspace navigation.
				state.focused = false
			}
			inputPending = false
			// A terminal tool owns every byte while its local modal is open.
			// Route it before workspace replay, modal cancellation, or PTY focus
			// handling can consume the input.
			if state.terminalSearchMode || state.terminalBookmarkMode || state.terminalBookmarkListMode {
				state.handleTerminalToolInput(b)
				state.render(os.Stdout)
				continue
			}
			workspaceProjectNavigation := shouldRouteWorkspaceProjectInputAsNavigation(state, b, replay.toPTY)
			if shouldDeferWorkspaceInput(state, replayedInput) && !workspaceProjectNavigation {
				replay.deferred = append(replay.deferred, b...)
				continue
			}
			var selectedActionTarget *ducklord.RemoteSession
			// Clicking another pane cancels pending focus before changing its
			// target. Late control completions remain fenced by controlID.
			if button, _, _, ok := parseSGRMouse(string(b)); ok && (button == 0 || button == 2) && strings.HasSuffix(string(b), "M") && state.workspacePreview && !state.blockingModalOpen() {
				state.panePrefixPending = false
				if handlePendingPTYInput(state, []byte("\x1b"), workspaceOutput != nil, &control, &controlOpenCancel, &controlID) {
					controlDone = nil
				}
			}
			// Central modals take ownership of Esc/Ctrl-C even when they were
			// opened from a focused terminal. Handle this before the pending
			// PTY gate so modal cancellation cannot be consumed by it.
			if state.handleCentralModalCancel(b) {
				if state.workspacePaneFocusRestorePending {
					// The original control lease is still valid when a focused
					// terminal opens the detach modal.  Keep that writer and hand
					// its visible output lease back immediately; closing it here
					// creates a needless asynchronous gap that can drop the first
					// command after Esc.
					restoreSession := state.activePTYSession()
					if state.workspacePaneFocusRestoreKey != "" {
						if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
							restoreSession = captured
						}
						if restoreSession.Client == "" {
							restoreSession = state.workspacePaneFocusRestoreSession
						}
					}
					if control != nil && controlMatchesSession(control, restoreSession, state.ownerName) {
						state.focused = false
						// Keep the saved snapshot pending until the visible output lease
						// accepts input focus. The control writer may be ready first.
						if acceptWorkspaceControl(restoreSession) {
							restorePendingFocus()
						}
						state.notesFocusRestoreReselect = false
						state.render(os.Stdout)
						continue
					}
					// The modal may have been opened while the previous control
					// lease was still connecting. Cancel that stale request before
					// replaying Enter, otherwise the replay is consumed by the
					// pending-input gate instead of reopening the saved PTY.
					// A replacement control really is asynchronous, so fence the
					// first command only on this path.
					state.workspacePlacementInputPending = true
					replay.deferred = nil
					if controlOpenCancel != nil {
						controlOpenCancel()
						controlOpenCancel = nil
						controlID++
					}
					if control != nil {
						_ = control.Stdin.Close()
						control = nil
						controlDone = nil
					}
					state.focused = false
					// Keep the input fence up until the restored Session pane's
					// control writer is accepted. Reopen the exact source Session
					// directly; routing Esc through Project would consume the user's
					// first shell command.
					state.workspacePlacementInputPending = true
					replay.deferred = nil
					if restoreSession.Client == "" {
						state.outputErr = "focused Session disappeared before PTY restore"
						state.workspacePaneFocusRestorePending = false
					} else {
						openControlForSession(restoreSession)
					}
				}
				state.notesFocusRestoreReselect = false
				if !state.focused && !state.workspacePaneFocusRestorePending {
					selectPooledOutput()
				}
				state.render(os.Stdout)
				continue
			}
			if state.handleCommandPaletteInput(b) {
				switch state.commandPaletteAction {
				case "host-add":
					state.commandPaletteAction = ""
					state.beginAddClient()
				case "create-session":
					state.commandPaletteAction = ""
					state.workspaceNewSessionIntent = nil
					state.beginCreate()
				case "refresh":
					state.commandPaletteAction = ""
					state.refreshSessions(ctx)
					if state.activeAttachKey == "" {
						requestPreview(true)
					}
				}
				state.render(os.Stdout)
				continue
			}
			// A workspace pane modal owns keyboard input for its entire
			// lifetime, including when it was opened from a focused terminal.
			// Route this before the pending PTY gate and focused-terminal path so
			// navigation and confirmation keys cannot leak to the shell. Mouse
			// reports continue through the modal hit-test below.
			if state.workspacePaneMode && !strings.HasPrefix(string(b), "\x1b[<") {
				notesInput := state.workspacePaneStep == "notes"
				if state.handleWorkspacePaneInput(b) && !notesInput && state.workspacePaneStep != "notes" {
					// The create wizard takes ownership of the next byte immediately.
					// A focused Default pane may still have a deferred restore lease;
					// leave that lease attached to the workspace modal instead of
					// allowing it to consume the host selector's first Enter.
					state.workspacePaneFocusRestorePending = false
					state.workspacePaneRestoreFocused = false
					state.workspacePlacementInputPending = false
					replay.deferred = nil
					state.beginCreate()
				}
				// Esc from a focused detach confirmation is handled by the
				// workspace-pane dispatcher, so hand the still-matching control
				// lease back immediately just as the central modal path does.
				if state.workspacePaneFocusRestorePending {
					restoreSession := state.activePTYSession()
					if state.workspacePaneFocusRestoreKey != "" {
						if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
							restoreSession = captured
						}
						if restoreSession.Client == "" {
							restoreSession = state.workspacePaneFocusRestoreSession
						}
					}
					if control != nil && controlMatchesSession(control, restoreSession, state.ownerName) {
						state.focused = false
						// The saved restore snapshot remains authoritative until the
						// output focus setter accepts the handoff.
						if acceptWorkspaceControl(restoreSession) {
							restorePendingFocus()
						}
					} else {
						// The modal can close while the original writer is still
						// opening (or after it was detached). Reopen the exact
						// captured Session and keep the first shell command fenced
						// until that replacement writer is accepted.
						state.workspacePlacementInputPending = true
						replay.deferred = nil
						if controlOpenCancel != nil {
							controlOpenCancel()
							controlOpenCancel = nil
							controlID++
						}
						if control != nil {
							_ = control.Stdin.Close()
							control = nil
							controlDone = nil
						}
						state.focused = false
						if restoreSession.Client == "" {
							state.outputErr = "focused Session disappeared before PTY restore"
							state.workspacePaneFocusRestorePending = false
						} else {
							openControlForSession(restoreSession)
						}
					}
				}
				resetWorkspacePaneChangeIfNeeded()
				state.render(os.Stdout)
				continue
			}
			// Help owns textual input for its entire lifetime, including while
			// focus restoration is pending or has already completed. Handle this
			// before the pending PTY gate so a focused close key cannot leak to the
			// originating shell. Mouse reports continue through the modal hit-test
			// below.
			if state.helpMode && !state.blockingModalOpen() && !strings.HasPrefix(string(b), "\x1b[<") {
				if state.helpSearchActive {
					if state.shortcut("help", string(b)) {
						state.closeHelp()
						if !state.helpMode {
							restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
						}
					} else if !state.scrollHelpInput(b) {
						state.handleHelpSearchInput(b)
					}
				} else if state.shortcut("help", string(b)) {
					state.dispatchHelpAction("help")
					if !state.helpMode {
						restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
					}
				} else if string(b) == "/" {
					state.helpSearchActive = true
				} else {
					state.scrollHelpInput(b)
				}
				state.render(os.Stdout)
				continue
			}
			if !workspaceNavigationReplay && (state.notesFocusRestorePending || state.helpFocusRestorePending || state.workspacePaneFocusRestorePending) && state.outputFresh {
				restorePendingFocus()
				drainNotesPendingInput(state, control)
			}
			// Deferred workspace keys are replayed only after the replacement
			// control lease is ready. Keep them on the workspace navigation route;
			// passing them through the pending PTY gate would consume navigation
			// keys such as b before handleWorkspaceProjectInput sees them.
			pendingHandled := false
			if !workspaceNavigationReplay && !workspaceProjectNavigation {
				pendingHandled = handlePendingPTYInput(state, b, workspaceOutput != nil, &control, &controlOpenCancel, &controlID)
			}
			if pendingHandled {
				// The writer may be ready even when the last output frame is stale.
				// Confirm the Help lease before interpreting a completed prefix.
				if !state.focused && state.helpFocusRestorePending {
					restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
				}
				// Remember a prefix even if the writer opens between its two keys.
				if consumed, command := state.handlePanePrefix(b); consumed && command != "" {
					// Command palette is a local modal even when the prefix arrived
					// while a focused PTY control was being handed off.  Open it before
					// the generic focused-command path tears down PTY ownership; the
					// suffix must never be replayed to the shell.
					if command == " " {
						state.openCommandPalette()
						state.render(os.Stdout)
						continue
					}
					// A prefix command completed while PTY focus was still opening
					// must remain a local UI action. In particular, never forward
					// Ctrl-B followed by ? or o to the PTY.
					if paneNavigationCommand(command) {
						// Control readiness can race the two bytes of a focused
						// prefix command.  Preserve the focused handoff contract even
						// when the pending-input gate has just observed the writer as
						// unavailable; dispatchPaneCommand would leave the selection
						// in the list and let the next PTY sentinel be consumed there.
						if state.beginFocusedPaneNavigation(command) && !replay.queued {
							replay.input <- []byte("\r")
							replay.queued = true
						}
					} else if !state.focused && state.helpFocusRestorePending {
						state.panePrefixReplay = append([]byte(shortcutInput(state.cfg.Shortcut("pane_prefix"))), []byte(command)...)
					} else if state.focused && state.dispatchFocusedPaneCommand(command) {
						// A direct focused detach bypasses the workspace modal branch.
						// Run the same lease teardown, then replay Enter so the selected
						// surviving pane opens its control writer.
						if resetWorkspacePaneChangeIfNeeded() && state.workspaceAttachFromProject && !replay.queued {
							// The surviving pane is selected synchronously, but its
							// control lease opens asynchronously. Hold real shell input
							// until that lease has been accepted.
							state.workspacePlacementInputPending = true
							replay.toPTY = true
							replay.deferred = nil
							replay.input <- []byte("\r")
							replay.queued = true
						}
					} else {
						state.dispatchPaneCommand(command)
					}
				}
				if control == nil {
					controlDone = nil
				}
				state.render(os.Stdout)
				continue
			}
			if state.handleDirectNotesInput(b) {
				state.render(os.Stdout)
				continue
			}
			// Retire a workspace scrollbar capture before any modal route can
			// consume this report without reaching handleWorkspaceMouse.
			state.prepareWorkspaceMouseDispatch(b)
			// Project files owns mouse reports before byte-oriented modal input. This
			// prevents the SGR sequence from being swallowed as text or reaching PTY.
			if state.projectFiles.open && strings.HasPrefix(string(b), "\x1b[<") {
				if button, x, y, ok := parseSGRMouse(string(b)); ok {
					if button == 0 && strings.HasSuffix(string(b), "m") {
						state.projectFilesMouseRelease(x, y)
					} else if button == 0 && strings.HasSuffix(string(b), "M") {
						state.modalMouseInput(x, y)
					}
					state.render(os.Stdout)
					continue
				}
			}
			if state.handleProjectFilesInput(b) {
				state.render(os.Stdout)
				continue
			}
			mouseReport := strings.HasPrefix(string(b), "\x1b[<")
			var helpMouseKey []byte
			if button, x, y, ok := parseSGRMouse(string(b)); ok {
				if key, owned := state.handleHelpMouseReport(button, x, y, strings.HasSuffix(string(b), "M")); owned {
					if len(key) == 0 {
						state.render(os.Stdout)
						continue
					}
					helpMouseKey = key
				} else if state.blockingModalOpen() {
					if state.projectFiles.open && button == 0 && strings.HasSuffix(string(b), "m") {
						b = state.projectFilesMouseRelease(x, y)
						if len(b) == 0 {
							continue
						}
					} else {
						if button != 0 || !strings.HasSuffix(string(b), "M") {
							continue
						}
						b = state.modalMouseInput(x, y)
						if len(b) == 0 {
							continue
						}
					}
				} else {
					// Mouse reports are always local UI input. Never inject their
					// escape sequences into a focused remote PTY.
					if (button == 64 || button == 65) && state.contentPanePoint(x, y) && state.viewportTerminal() != nil {
						if button == 64 {
							state.scrollPTY(3)
						} else {
							state.scrollPTY(-3)
						}
						if state.copyMode {
							state.renderCopyMode(os.Stdout)
						} else {
							state.render(os.Stdout)
						}
						continue
					}
					if (button == 64 || button == 65) && !state.workspaceListWheelOwnsInput(x, y) {
						continue
					}
					if state.copyMode {
						continue
					}
					if state.focused {
						if !state.workspacePreview || (button != 0 && button != 2) || !strings.HasSuffix(string(b), "M") {
							continue
						}
						controlID++
						if controlOpenCancel != nil {
							controlOpenCancel()
							controlOpenCancel = nil
						}
						if control != nil {
							_ = control.Stdin.Close()
						}
						control, controlDone = nil, nil
						if attach != nil {
							_ = attach.Stdin.Close()
						}
						if attachCancel != nil {
							attachCancel()
							attachCancel = nil
						}
						attachID++
						resizeInFlight = false
						pendingFramebufferResize, queuedResize = nil, nil
						bufferedAttach, bufferedAttachBytes = nil, 0
						attachOutputSource = attachOut
						state.focused = false
						state.clearAttachIdentity()
						attachCanResize, attachInitialResizeQueued = false, false
						attachReplayEndOffset = 0
						attach = nil
					}
				}
			} else if mouseReport {
				continue
			}
			if len(helpMouseKey) != 0 {
				b = helpMouseKey
			}
			if state.copyMode {
				if copyModeExitInput(b) {
					state.exitCopyMode(os.Stdout)
					if state.sessionFreshlyDisplayed(state.currentSession()) {
						state.markActivitySeen(state.currentSession())
					}
					state.render(os.Stdout)
				} else if string(b) == "\x1b[A" || string(b) == "k" {
					if state.viewportTerminal() != nil {
						state.scrollPTY(1)
						state.renderCopyMode(os.Stdout)
					}
				} else if string(b) == "\x1b[B" || string(b) == "j" {
					state.scrollPTY(-1)
					state.renderCopyMode(os.Stdout)
				}
				continue
			}
			if state.helpSearchActive && !state.blockingModalOpen() {
				if state.shortcut("help", string(b)) {
					state.closeHelp()
					if !state.helpMode {
						// Closing Help must release its input fence even when the
						// last framebuffer event was marked stale. The visible output
						// lease can still be valid, and waiting for another event would
						// consume the first command sent to the originating PTY.
						restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
					}
				} else {
					state.handleHelpSearchInput(b)
				}
				state.render(os.Stdout)
				continue
			}
			if state.helpMode && !state.focused && !state.blockingModalOpen() && string(b) == "/" && !state.shortcut("help", string(b)) {
				state.helpSearchActive = true
				state.render(os.Stdout)
				continue
			}
			paneCommand := ""
			if consumed, command := state.handlePanePrefix(b); consumed {
				resetPanePrefixTimer(state.panePrefixDeadline)
				paneCommand = command
				if command == "" {
					state.render(os.Stdout)
					continue
				}
				if command == " " {
					// Space completes the local command-palette route. Keep the
					// focused control intact so Esc can restore the exact PTY origin.
					state.openCommandPalette()
					state.render(os.Stdout)
					continue
				}
				if strings.HasPrefix(command, "quick-shell:") {
					state.beginQuickShell(ctx, createDone, strings.TrimPrefix(command, "quick-shell:"))
					state.render(os.Stdout)
					continue
				}
				if state.focused && paneNavigationCommand(command) {
					nav, err := state.workspaceNavigation()
					if err == nil {
						width, height := terminalSize()
						var target string
						navigationKey := command
						switch command {
						case "n":
							navigationKey = "pagedown"
						case "p":
							navigationKey = "pageup"
						}
						target, err = nav.PaneNavigationTarget(ducklord.CalculateWorkspaceGeometry(width, height, 4), navigationKey)
						if err == nil && target == nav.CurrentPaneID() {
							state.render(os.Stdout)
							continue
						}
					}
					if err != nil {
						state.outputErr = err.Error()
						state.render(os.Stdout)
						continue
					}
				}
				if !state.focused {
					if paneNavigationCommand(command) {
						if !state.navigatePrefixPane(command) {
							state.render(os.Stdout)
							continue
						}
						requestPreview(true)
						b = []byte("\r")
					} else {
						dispatchPaneCommandAndRestoreHelpFocus(state, command, workspaceOutput, true, control, attach)
						queuePaneReplay()
						state.render(os.Stdout)
						continue
					}
				} else {
					if state.dispatchFocusedPaneCommand(command) {
						if resetWorkspacePaneChangeIfNeeded() && state.workspaceAttachFromProject && !replay.queued {
							// The surviving pane is selected synchronously, but its
							// control lease opens asynchronously. Hold real shell input
							// until that lease has been accepted.
							state.workspacePlacementInputPending = true
							replay.toPTY = true
							replay.deferred = nil
							replay.input <- []byte("\r")
							replay.queued = true
						}
						state.render(os.Stdout)
						continue
					}
					b = []byte(shortcutInput(state.cfg.Shortcut("pty_unfocus")))
				}
			}
			if state.focused {
				if state.workspacePreview {
					if _, visibleErr := state.workspacePaneRect(); visibleErr != nil {
						controlID++
						attachID++
						if controlOpenCancel != nil {
							controlOpenCancel()
							controlOpenCancel = nil
						}
						if control != nil {
							_ = control.Stdin.Close()
							control, controlDone = nil, nil
						}
						if attachCancel != nil {
							attachCancel()
							attachCancel = nil
						}
						if attach != nil {
							_ = attach.Stdin.Close()
							attach = nil
						}
						state.focused = false
						state.clearAttachIdentity()
						state.outputErr = "Session pane is no longer visible; input was not sent: " + visibleErr.Error()
						state.render(os.Stdout)
						continue
					}
				}
				if !state.hostIsLive(state.activePTYSession().Client) {
					state.focused = false
					state.clearAttachIdentity()
					state.outputFresh = false
					state.outputErr = "host reconnecting; input was not sent"
					state.render(os.Stdout)
					continue
				}
				// Ctrl-] is the built-in PTY escape. Keep it recognized even if
				// an older or incomplete config omits the shortcut entry; under a
				// saturated PTY the escape must never be forwarded to the shell.
				if string(b) == "\x1d" || state.shortcut("pty_unfocus", string(b)) {
					if outputManager == nil {
						state.saveCurrentSnapshot()
					}
					controlID++
					if controlOpenCancel != nil {
						controlOpenCancel()
						controlOpenCancel = nil
					}
					if control != nil {
						_ = control.Stdin.Close()
					}
					control, controlDone = nil, nil
					if attach != nil {
						_ = attach.Stdin.Close()
					}
					if attachCancel != nil {
						attachCancel()
						attachCancel = nil
					}
					attachID++
					resizeInFlight = false
					pendingFramebufferResize = nil
					queuedResize = nil
					bufferedAttach = nil
					bufferedAttachBytes = 0
					attachOutputSource = attachOut
					state.focused = false
					state.clearAttachIdentity()
					// A busy PTY may have advanced the terminal while the cached
					// frame still describes the focused view. Force a complete
					// navigation repaint when returning from terminal focus.
					state.frameOutput = frameOutput{}
					if state.workspaceNav != nil && state.workspaceNav.InDetailMode() {
						state.syncDetailSelection()
					}
					attachCanResize = false
					attachInitialResizeQueued = false
					attachReplayEndOffset = 0
					attach = nil
					if paneNavigationCommand(paneCommand) {
						if !state.beginFocusedPaneNavigation(paneCommand) {
							state.render(os.Stdout)
							continue
						}
						requestPreview(true)
						b = []byte("\r")
					} else {
						if strings.HasPrefix(paneCommand, "quick-shell:") {
							state.beginQuickShell(ctx, createDone, strings.TrimPrefix(paneCommand, "quick-shell:"))
							state.render(os.Stdout)
							continue
						}
						requestPreview(true)
						if paneCommand != "" {
							state.dispatchPaneCommand(paneCommand)
						}
						queuePaneReplay()
						state.render(os.Stdout)
						continue
					}
				}
				if state.focused {
					activeSession := state.activePTYSession()
					if writer := focusedPTYWriter(control, attach, activeSession, state.ownerName); writer != nil {
						_, _ = writer.Write(b)
					} else if control != nil {
						controlID++
						if controlOpenCancel != nil {
							controlOpenCancel()
						}
						_ = control.Stdin.Close()
						control, controlDone = nil, nil
						attachCanResize = false
						state.outputErr = "PTY control changed; input was not sent"
					}
					continue
				}
			}
			if state.shouldDiscardModalArrow(b) {
				state.render(os.Stdout)
				continue
			}
			// Help is a pinned overlay: only its configured binding may toggle it.
			// Esc, quit, and Ctrl-C do not dismiss it.
			if state.helpMode && !state.blockingModalOpen() {
				if state.shortcut("help", string(b)) {
					state.dispatchHelpAction("help")
					if !state.helpMode {
						// See the search-close path above: focus restoration depends
						// on the lease, rather than on a fresh repaint arriving after
						// the close key.
						restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
					}
				}
				state.render(os.Stdout)
				continue
			}
			if string(b) == "\x03" && state.centralModalOpen() {
				switch {
				case state.projectFiles.open:
					state.handleProjectFilesInput(b)
				case state.workspacePaneMode:
					if state.workspacePaneStep == "notes" {
						state.closeNotesModal()
					} else {
						state.closeWorkspacePane()
					}
				case state.shortcutMode:
					state.shortcutMode = false
				case state.notificationConfigMode:
					state.closeNotificationConfig()
				case state.searchMode:
					state.closeSearch()
				case state.terminalSearchMode, state.terminalBookmarkMode, state.terminalBookmarkListMode:
					state.closeTerminalTool()
				case state.hostMenuMode:
					if strings.HasPrefix(state.hostMenuStep, "skills-") {
						state.closeHostMenuWithCtrlC()
						// Host Skills is opened from the workspace navigation route while
						// the existing PTY control remains alive. Restore that exact
						// output key here so the next byte reaches the original shell.
						if workspaceOutput != nil && state.activeAttachKey != "" {
							if key, ok := terminalOutputKey(state.activePTYSession()); ok && setWorkspaceInputFocus(workspaceOutput, key) {
								// The pooled output lease can be restored before the PTY
								// writer. Keep keyboard ownership fenced until both are
								// ready; otherwise the first command after Ctrl-C is
								// accepted visually and dropped by a nil writer.
								state.focused = false
								state.workspacePlacementInputPending = true
							}
						}
						// Keep an already-matching writer. Host Skills normally leaves
						// that writer alive while its pooled output lease is restored;
						// closing it here creates a replacement window in which the next
						// shell command can be lost.
						restoreSession := state.activePTYSession()
						if control != nil && controlMatchesSession(control, restoreSession, state.ownerName) {
							acceptWorkspaceControl()
						} else {
							if control != nil {
								_ = control.Stdin.Close()
								control, controlDone = nil, nil
							}
							if state.activeAttachKey != "" {
								_ = openControlForSession(restoreSession)
							}
						}
					} else {
						state.hostMenuMode = false
					}
				case state.addClientMode:
					state.cancelAddClient()
				case state.removeClientMode:
					state.cancelRemoveClient()
				case state.newSessionMode:
					if startCancel != nil {
						startCancel()
						startCancel = nil
						startID++
					}
					state.cancelCreate()
				case state.notificationMode:
					state.closeNotificationSettings()
				case state.groupMenu:
					state.closeGroupMenu()
				case state.actionMenu:
					state.closeActionMenu()
				case state.sessionRenameMode:
					state.closeSessionHandleRename()
				case state.lifecycleConfirm != "":
					state.lifecycleConfirm = ""
					state.lifecycleReturnToAction = false
					state.lifecycleTarget = ducklord.RemoteSession{}
					state.lifecycleMode = ""
				}
				state.render(os.Stdout)
				continue
			}
			if state.searchMode {
				if state.searchPendingRequestID != 0 {
					if string(b) == "\x1b" || string(b) == "\x03" {
						state.searchPendingRequestID = 0
						state.closeSearch()
						selectPooledOutput()
						state.render(os.Stdout)
						continue
					}
					state.searchErr = "switching PTY..."
					state.render(os.Stdout)
					continue
				}
				action := state.handleSearchInput(b)
				if action == "cancel" {
					state.closeSearch()
				} else if action == "activate" {
					target, ok := state.searchResult()
					key := sessionKey(target)
					if ok && state.searchResultIsActivated(target) {
						if !state.prepareSearchWorkspaceSelection(key) {
							state.render(os.Stdout)
							continue
						}
						state.closeSearch()
						b = []byte("\r")
					} else if !ok || outputManager == nil {
						state.searchErr = "PTY output activation is unavailable"
						state.render(os.Stdout)
						continue
					} else if _, valid := terminalOutputKey(target); !valid || !canRead(target) || !state.hostIsLive(target.Client) {
						state.searchErr = "selected session is unavailable"
						state.render(os.Stdout)
						continue
					} else {
						client, clientErr := mustClient(cfg, target.Client)
						if clientErr != nil {
							state.searchErr = clientErr.Error()
							state.render(os.Stdout)
							continue
						}
						rows, cols := state.activePTYSize()
						requestID := outputManager.Select(ducklord.TerminalSelection{Client: client, InstanceID: target.InstanceID, SessionID: target.SessionID,
							RuntimeGeneration: target.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
						outputRequestID = requestID
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
								case searchActivationTimeout <- id:
								case <-ctx.Done():
								}
							case <-ctx.Done():
							}
						}(requestID)
						state.render(os.Stdout)
						continue
					}
				}
				if state.searchMode {
					state.render(os.Stdout)
					continue
				}
			}
			if !state.workspacePaneMode && state.workspacePreview {
				if state.syncCurrentNotes() {
					if state.handleNotesInput(b) {
						state.render(os.Stdout)
						continue
					}
				}
			}
			if state.shortcutMode {
				if state.handleShortcutInput(b) == "restart-tui" {
					return errRestartTUI
				}
				state.render(os.Stdout)
				continue
			}
			if state.notificationConfigMode {
				if state.handleNotificationConfigInput(b) == "restart-tui" {
					return errRestartTUI
				}
				state.render(os.Stdout)
				continue
			}
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
					state.hostMenuRequestID++
					requestID := state.hostMenuRequestID
					client, ok := state.cfg.Client(target)
					if !ok || state.disconnectedHosts[target] {
						state.hostMenuStep = "resources-error"
						state.hostMenuErr = "Host is disconnected or unavailable"
						break
					}
					epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
					readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
					go func() {
						defer cancel()
						status, err := runner.HostResources(readCtx, client)
						publishHostResourceEvent(ctx, hostResourceDone, hostResourceEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, status: status, err: err})
					}()
				case "host-hooks-read":
					launchHostHookStatus(target)
				case "host-hook-save":
					client, ok := state.cfg.Client(target)
					if !ok || state.disconnectedHosts[target] {
						state.hostMenuStep = "hook-error"
						state.hostMenuErr = "Host is disconnected or unavailable"
						break
					}
					requestID := state.hostMenuRequestID + 1
					if !state.beginHostHookOperation(target, requestID) {
						state.hostMenuStep = "hook-error"
						state.hostMenuErr = "Another hook update is still running on this Host"
						break
					}
					if hostRetentionCancel != nil {
						hostRetentionCancel()
					}
					state.hostMenuRequestID = requestID
					epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
					agent, operation := state.hostHookAgent, state.hostHookAction
					saveCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
					hostRetentionCancel = cancel
					go func() {
						defer cancel()
						result, err := runner.ConfigureHostAgentHook(saveCtx, client, agent, operation)
						select {
						case hostHookDone <- hostHookEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, agent: agent, action: operation, result: result, err: err}:
						case <-ctx.Done():
						}
					}()
				case "host-retention-read":
					if hostRetentionCancel != nil {
						hostRetentionCancel()
					}
					state.hostMenuRequestID++
					requestID := state.hostMenuRequestID
					epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
					client, ok := state.cfg.Client(target)
					if !ok || state.disconnectedHosts[target] {
						state.hostMenuStep = "retention-error"
						state.hostMenuErr = "Host is disconnected or unavailable"
						break
					}
					readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
					hostRetentionCancel = cancel
					go func() {
						defer cancel()
						days, err := runner.HostLogRetention(readCtx, client)
						select {
						case hostRetentionDone <- hostRetentionEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, days: days, err: err}:
						case <-ctx.Done():
						}
					}()
				case "host-retention-save":
					if hostRetentionCancel != nil {
						hostRetentionCancel()
					}
					state.hostMenuRequestID++
					requestID := state.hostMenuRequestID
					epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
					client, ok := state.cfg.Client(target)
					if !ok || state.disconnectedHosts[target] {
						state.hostMenuStep = "retention-error"
						state.hostMenuErr = "Host is disconnected or unavailable"
						break
					}
					days, _ := strconv.Atoi(state.hostMenuDraft)
					saveCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
					hostRetentionCancel = cancel
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
						case hostRetentionDone <- hostRetentionEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, days: days, err: err, saved: true}:
						case <-ctx.Done():
						}
					}()
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
					for _, target := range targets {
						if target == state.hostMenuTarget && hostRetentionCancel != nil {
							hostRetentionCancel()
							hostRetentionCancel = nil
						}
						wasActive := state.hostOwnsActivePTY(target, control)
						if current, ok := watchedClients[target]; ok {
							current.cancel()
							delete(watchedClients, target)
						}
						previous := state.hostSync[target]
						state.bumpHostConnectionEpoch(target)
						state.disconnectedHosts[target] = true
						if state.newSessionMode && state.newSessionClient == target {
							state.cancelCreateDiscovery()
							state.cancelPathSuggestions()
							state.newSessionErr = "host disconnected; pending session operation was canceled"
						}
						if outputManager != nil && previous.InstanceID != "" {
							// A user disconnect is reversible and keeps the desired LRU
							// membership. ForgetHost is reserved for daemon replacement
							// and permanent host removal.
							outputManager.SyncHost(target, previous.InstanceID, false, nil)
						}
						if wasActive {
							if attachCancel != nil {
								attachCancel()
								attachCancel = nil
							}
							if controlOpenCancel != nil {
								controlOpenCancel()
								controlOpenCancel = nil
							}
							if control != nil {
								_ = control.Stdin.Close()
								control, controlDone = nil, nil
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
								watchClient(client)
							}
							if !eventDriven {
								state.refreshSessions(ctx)
							}
							state.outputErr = "reconnecting " + target
						}
					}
					if outputManager != nil {
						selectPooledOutput()
					}
					if action == "host-apply-connections" {
						for _, target := range connectTargets {
							state.bumpHostConnectionEpoch(target)
							delete(state.disconnectedHosts, target)
							state.hostSync[target] = ducklord.SessionUpdate{Client: target, State: "connecting"}
							if client, ok := state.cfg.Client(target); ok {
								watchClient(client)
							}
							if !eventDriven {
								state.refreshSessions(ctx)
							}
						}
						state.hostMenuMode = false
						state.outputErr = fmt.Sprintf("host connections updated: %d connecting, %d disconnected", len(connectTargets), len(targets))
					}
				case "host-connect":
					for _, target := range targets {
						if !state.disconnectedHosts[target] {
							continue
						}
						state.bumpHostConnectionEpoch(target)
						delete(state.disconnectedHosts, target)
						state.hostSync[target] = ducklord.SessionUpdate{Client: target, State: "connecting"}
						if client, ok := state.cfg.Client(target); ok {
							watchClient(client)
						}
						if !eventDriven {
							state.refreshSessions(ctx)
						}
						state.hostMenuMode = false
						state.outputErr = "connecting " + target
					}
				}
				if !state.hostMenuMode && hostRetentionCancel != nil {
					hostRetentionCancel()
					hostRetentionCancel = nil
				}
				state.render(os.Stdout)
				continue
			}
			if state.addClientMode {
				if state.addClientBusy {
					if string(b) == "\x03" || string(b) == "\x1b" || string(b) == "q" {
						state.cancelAddClient()
						state.render(os.Stdout)
					}
					continue
				}
				action := state.handleAddClientInput(b)
				switch action {
				case "cancel":
					state.cancelAddClient()
				case "submit":
					if err := state.startAddClient(ctx, addClientDone); err != nil {
						state.addClientErr = err.Error()
					}
				}
				state.render(os.Stdout)
				continue
			}
			if state.removeClientMode {
				action := state.handleRemoveClientInput(b)
				if action == "remove" {
					name := state.removeClientConfirm
					instance := state.hostSync[name].InstanceID
					if err := state.removeClient(ctx, name); err != nil {
						state.outputErr = err.Error()
					} else {
						if current, ok := watchedClients[name]; ok {
							current.cancel()
							delete(watchedClients, name)
						}
						state.disconnectedHosts[name] = true
						state.bumpHostConnectionEpoch(name)
						if outputManager != nil {
							outputManager.ForgetHost(name, instance)
							selectPooledOutput()
						}
					}
				}
				state.render(os.Stdout)
				continue
			}
			if state.newSessionMode {
				if state.newSessionStarting || state.newSessionDiscovering {
					if string(b) == "\x03" {
						if state.newSessionStarting && startCancel != nil {
							startCancel()
							startCancel = nil
							startID++ // fence a Start result racing with Ctrl+C
						}
						state.cancelCreate()
						state.render(os.Stdout)
					} else if string(b) == "\x1b" {
						if state.newSessionStarting && startCancel != nil {
							startCancel()
							startCancel = nil
							startID++
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
					continue
				}
				beforeLine := state.newSessionLine
				action := state.handleCreateInput(b)
				if action == "" && state.newSessionStep == "path" && state.newSessionLine != beforeLine {
					state.requestPathSuggestions(ctx, pathSuggestionsDone)
				}
				switch action {
				case "cancel":
					state.cancelCreate()
				case "submit":
					_, clientName, args, ready, err := state.submitCreateStep(ctx, createDone)
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
						return err
					}
					startCtx, cancel := context.WithCancel(ctx)
					startCancel = cancel
					startID++
					id := startID
					state.newSessionStarting = true
					state.newSessionStartGeneration, state.newSessionStartInstance = state.hostFingerprint(clientName)
					state.newSessionStartEpoch = state.hostConnectionEpoch[clientName]
					state.newSessionErr = "starting..."
					go func() {
						sessionID, err := runner.Start(startCtx, c, args)
						select {
						case startDone <- startDoneEvent{id: id, client: clientName, sessionID: sessionID, err: err}:
						case <-ctx.Done():
						}
					}()
				}
				state.render(os.Stdout)
				continue
			}
			if state.notificationMode {
				action := state.handleNotificationInput(b)
				switch action {
				case "save":
					state.saveNotificationSettings()
				case "cancel":
					state.closeNotificationSettings()
				}
				state.render(os.Stdout)
				continue
			}
			if state.groupMenu {
				state.handleGroupMenuInput(b)
				state.render(os.Stdout)
				continue
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
				continue
			}
			if state.actionMenu {
				action := state.handleActionMenuInput(b)
				state.render(os.Stdout)
				if action == "" {
					continue
				}
				if action == "cancel" {
					continue
				}
				// Selection is rebound to the exact captured identity before the
				// ordinary dispatch path runs. A stale menu always fails closed.
				if !state.selectActionTarget() {
					state.outputErr = "session changed while action menu was open; reopen the menu"
					state.actionTarget = ducklord.RemoteSession{}
					state.actionOperation, state.actionMode = "", ""
					state.render(os.Stdout)
					continue
				}
				target := state.actionTarget
				selectedActionTarget = &target
				state.actionTarget = ducklord.RemoteSession{}
				if action == "lifecycle-action" {
					if state.lifecycleBusy {
						state.actionOperation, state.actionMode = "", ""
						state.outputErr = "another lifecycle operation is still pending; wait for its result"
						state.render(os.Stdout)
						continue
					}
					state.lifecycleConfirm = state.actionOperation
					state.lifecycleReturnToAction = true
					state.lifecycleMode = state.actionMode
					state.lifecycleTarget = target
					state.actionOperation, state.actionMode = "", ""
					state.render(os.Stdout)
					continue
				}
				if action == "notifications-action" {
					state.beginNotificationSettings()
					state.render(os.Stdout)
					continue
				}
				if action == "rename-handle-action" {
					state.beginSessionHandleRename(target)
					state.render(os.Stdout)
					continue
				}
				// Attach and yield reuse the canonical handlers below.
				b = nil
				switch action {
				case "attach":
					b = []byte("\r")
				case "reconnect":
					if outputManager == nil {
						state.refreshSelectedOutput(ctx)
						state.render(os.Stdout)
						continue
					}
					client, clientErr := mustClient(cfg, target.Client)
					if clientErr != nil {
						state.outputErr = clientErr.Error()
						state.render(os.Stdout)
						continue
					}
					rows, cols := state.activePTYSize()
					outputRequestID = outputManager.Reconnect(ducklord.TerminalSelection{Client: client, InstanceID: target.InstanceID, SessionID: target.SessionID,
						RuntimeGeneration: target.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
					reconnectRequestID = outputRequestID
					activeOutputEvent = ducklord.TerminalOutputEvent{}
					state.outputReconnecting = true
					state.outputStale = true
					state.outputFresh = false
					state.outputErr = "reconnecting PTY output from Ducklion..."
					state.render(os.Stdout)
					continue
				case "yield":
					b = []byte("y")
				case "yield-wait":
					b = []byte("Y")
				}
			}
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
					continue
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
					continue
				}
				if state.lifecycleMode != "" && text != "\r" && text != "\n" {
					continue
				}
				if state.lifecycleMode == "" && text == "w" && state.lifecycleConfirm != protocol.SessionLifecycleRestart {
					mode = protocol.SessionLifecycleWait
				} else if state.lifecycleMode == "" && text == "f" {
					mode = protocol.SessionLifecycleForce
				} else if text != "\r" && text != "\n" {
					continue
				}
				if !state.lifecycleTargetIsCurrent() {
					state.outputErr = "session changed while awaiting confirmation; reopen the lifecycle action"
					state.lifecycleConfirm = ""
					state.lifecycleTarget = ducklord.RemoteSession{}
					state.lifecycleMode = ""
					state.render(os.Stdout)
					continue
				}
				client, clientErr := mustClient(cfg, sess.Client)
				if clientErr != nil {
					state.outputErr = clientErr.Error()
					state.lifecycleConfirm = ""
					state.lifecycleTarget = ducklord.RemoteSession{}
					continue
				}
				operation := state.lifecycleConfirm
				lifecycleRequestID++
				requestID := lifecycleRequestID
				generation, instance := state.hostFingerprint(sess.Client)
				epoch := watchedClients[sess.Client].epoch
				connectionEpoch := state.hostConnectionEpoch[sess.Client]
				state.lifecycleBusy = true
				state.lifecycleConfirm = ""
				state.lifecycleTarget = ducklord.RemoteSession{}
				state.lifecycleMode = ""
				state.outputErr = string(operation) + " accepted; Ducklion will continue it across reconnects"
				go func() {
					result, err := runner.LifecycleSelected(ctx, client, sess, operation, mode)
					select {
					case lifecycleDone <- lifecycleDoneEvent{id: requestID, watchEpoch: epoch, connectionEpoch: connectionEpoch,
						hostGeneration: generation, hostInstance: instance,
						operation: operation, client: sess.Client, result: result, err: err}:
					case <-ctx.Done():
					}
				}()
				state.render(os.Stdout)
				continue
			}
			wasWorkspaceDragging := state.workspaceDragMoved
			handledWorkspaceMouse, changedWorkspaceMouse := state.handleWorkspaceMouse(b)
			if wasWorkspaceDragging != state.workspaceDragMoved {
				shape := "default"
				if state.workspaceDragMoved {
					shape = "grabbing"
				}
				fmt.Fprint(os.Stdout, mouseCursorShape(shape))
			}
			if handledWorkspaceMouse {
				if changedWorkspaceMouse && workspaceOutput != nil {
					selectPooledOutput()
				}
				if state.workspaceMouseFocus {
					state.workspaceMouseFocus = false
					state.workspaceAttachFromProject = true
					state.workspaceFocusFromProject = true
					state.selectedGroupID = ""
					b = []byte("\r")
					if state.workspaceNav != nil && state.workspaceNav.InDetailMode() {
						b = []byte(shortcutInput(state.cfg.Shortcut("detail_focus")))
					}
				} else {
					state.render(os.Stdout)
					continue
				}
			}
			wasDetailed := state.workspaceNav != nil && state.workspaceNav.InDetailMode()
			previousDetail := state.detailSelected
			if detailAction := state.handleDetailedInput(b); detailAction != "" {
				switch detailAction {
				case "changed":
					isDetailed := state.workspaceNav != nil && state.workspaceNav.InDetailMode()
					if workspaceOutput != nil && (wasDetailed != isDetailed || previousDetail != state.detailSelected) {
						selectPooledOutput()
					}
					state.render(os.Stdout)
					continue
				case "focus", "jump":
					state.workspaceAttachFromProject = true
					state.workspaceFocusFromProject = detailAction == "jump"
					b = []byte("\r") // continue through the owner-gated attach action
				}
			}
			if handled, changed := state.handleWorkspaceProjectInput(b); handled {
				if changed && control != nil && !state.focused {
					_ = control.Stdin.Close()
					control, controlDone = nil, nil
					state.clearAttachIdentity()
				}
				if changed && workspaceOutput != nil {
					selectPooledOutput()
				}
				state.render(os.Stdout)
				continue
			}
			wasDragging := state.dragSession.Key() != ""
			action := state.handleInput(b)
			isDragging := state.dragSession.Key() != ""
			if wasDragging != isDragging {
				shape := "default"
				if isDragging {
					shape = "grabbing"
				}
				fmt.Fprint(os.Stdout, mouseCursorShape(shape))
			}
			switch action {
			case "quit":
				return nil
			case "help":
				state.dispatchHelpAction(action)
			case "project-files":
				state.openProjectFiles()
			case "shortcut-settings":
				state.beginShortcutSettings()
			case "notification-settings":
				state.beginNotificationConfig("global", "")
			case "host-actions":
				state.beginHostMenu()
			case "refresh":
				state.refreshSessions(ctx)
				if state.activeAttachKey == "" {
					requestPreview(true)
				}
			case "organize":
				state.cycleOrganizationMode()
			case "quick-sort":
				state.cycleQuickSort()
			case "quick-sort-direction":
				state.reverseQuickSortTime()
			case "groups":
				state.beginGroupMenu()
			case "reorder-up":
				state.moveSelectedSession(-1)
			case "reorder-down":
				state.moveSelectedSession(1)
			case "select":
				state.handleQuickSelection(&control, &controlDone, requestPreview)
			case "new":
				state.workspaceNewSessionIntent = nil
				state.beginCreate()
			case "search":
				state.beginSearch()
			case "notifications":
				state.beginNotificationSettings()
			case "copy-mode":
				state.enterCopyMode(os.Stdout)
				continue
			case "actions":
				state.beginActionMenu()
			case "end", "restart", "destroy":
				if state.lifecycleBusy {
					state.outputErr = "another lifecycle operation is still pending; wait for its result"
					break
				}
				sess := state.currentSession()
				if sess.SessionID == "" || !state.hostIsLive(sess.Client) {
					state.outputErr = "session lifecycle is unavailable while the host is reconnecting"
					break
				}
				if sess.Kind == string(model.KindAgent) && (sess.WriterKind != string(model.OwnerTerminal) || sess.WriterID != state.ownerName) {
					state.outputErr = "read-only session; yield control to this Ducklord before changing its lifecycle"
					break
				}
				state.lifecycleConfirm = protocol.SessionLifecycleOperation(action)
				state.lifecycleReturnToAction = false
				state.lifecycleTarget = sess
				state.lifecycleMode = ""
				state.outputErr = ""
			case "add-client":
				state.beginAddClient()
			case "remove-client":
				state.beginRemoveClient()
			case "yield", "yield-wait":
				if len(state.sessions) == 0 {
					break
				}
				sess := state.sessions[state.selected]
				if !state.hostIsLive(sess.Client) {
					state.outputErr = "host is reconnecting; yield is disabled until session state is synchronized"
					break
				}
				client, err := mustClient(cfg, sess.Client)
				if err != nil {
					state.outputErr = err.Error()
					break
				}
				var result protocol.SessionYieldResult
				if selectedActionTarget != nil {
					result, err = runner.YieldSelected(ctx, client, *selectedActionTarget, action == "yield-wait")
				} else {
					ref := sess.SessionID
					if ref == "" {
						ref = sess.Name
					}
					result, err = runner.Yield(ctx, client, ref, action == "yield-wait")
				}
				if err != nil {
					state.outputErr = err.Error()
					break
				}
				state.refreshSessions(ctx)
				state.outputErr = fmt.Sprintf("yield %s (epoch %d)", result.Decision, result.OwnershipEpoch)
			case "attach":
				if control != nil && !state.focused {
					_ = control.Stdin.Close()
					control, controlDone = nil, nil
					state.clearAttachIdentity()
				}
				if len(state.sessions) == 0 {
					continue
				}
				s := state.currentSession()
				if state.workspaceAttachFromProject || state.workspacePaneFocusRestorePending && state.workspacePaneFocusRestoreKey != "" {
					state.workspaceAttachFromProject = false
					if state.workspacePaneFocusRestorePending && state.workspacePaneFocusRestoreKey != "" {
						var ok bool
						s, ok = state.sessionForKey(state.workspacePaneFocusRestoreKey)
						if !ok {
							if state.workspacePaneFocusRestoreSession.Client == "" {
								state.outputErr = "focused Session disappeared before PTY restore"
								break
							}
							s = state.workspacePaneFocusRestoreSession
						}
					} else {
						var targetErr error
						s, targetErr = state.workspaceSelectedPaneSession()
						if targetErr != nil {
							state.outputErr = targetErr.Error()
							break
						}
					}
					state.activeAttachKey = sessionKey(s)
				}
				if state.workspacePreview {
					if _, visibleErr := state.workspacePaneRect(); visibleErr != nil {
						state.outputErr = "cannot focus hidden Session pane: " + visibleErr.Error()
						state.clearAttachIdentity()
						break
					}
				}
				if !canAttach(s) || !state.hostIsLive(s.Client) {
					if !state.hostIsLive(s.Client) {
						state.outputErr = "host is reconnecting; PTY controls are disabled"
					}
					state.clearAttachIdentity()
					continue
				}
				c, err := mustClient(cfg, s.Client)
				if err != nil {
					return err
				}
				fencePreview()
				if outputManager != nil {
					openControlForSession(s)
					break
				}
				attachCtx, cancel := context.WithCancel(ctx)
				sessionRef := s.SessionID
				if sessionRef == "" {
					sessionRef = s.Name
				}
				var session *ducklord.AttachSession
				if resumer, ok := runner.(interface {
					AttachStreamFrom(context.Context, ducklord.Client, string, ducklord.AttachResume) (*ducklord.AttachSession, error)
				}); ok && state.terminal != nil && state.outputForKey == sessionKey(s) && state.terminalCursorValid {
					session, err = resumer.AttachStreamFrom(attachCtx, c, sessionRef, ducklord.AttachResume{RuntimeGeneration: state.terminalGeneration, OutputOffset: state.terminalOffset})
				} else {
					session, err = runner.AttachStream(attachCtx, c, sessionRef)
				}
				if err != nil {
					cancel()
					state.outputErr = err.Error()
					break
				}
				attach = session
				attachCancel = cancel
				state.resizeStatus = ""
				attachID++
				resizeInFlight = false
				pendingFramebufferResize = nil
				queuedResize = nil
				bufferedAttach = nil
				bufferedAttachBytes = 0
				attachOutputSource = attachOut
				id := attachID
				state.activeAttachKey = ""
				state.activeAttachFresh = false
				state.pendingAttachKey = sessionKey(s)
				attachCanResize = state.canResizeCurrentSession() && session.ResizeBarrier != nil
				attachInitialResizeQueued = false
				attachReplayEndOffset = session.ReplayEndOffset
				state.outputForKey = state.pendingAttachKey
				if !session.ExactResume {
					state.outputText = ""
					state.terminal = nil
					state.terminalGeneration = session.RuntimeGeneration
					state.terminalOffset = session.StartOffset
					state.terminalCursorValid = false
				} else {
					state.outputStale = false
				}
				state.outputErr = ""
				state.outputStale = false
				state.outputFresh = false
				// A detach modal can reopen the saved PTY while its focus lease is
				// still pending. Keep the UI unfocused until the control writer is
				// accepted; otherwise workspaceControlMayAccept rejects that writer
				// and the next input falls through to the Session list.
				state.focused = !state.workspacePaneFocusRestorePending
				go superviseAttach(attachCtx, id, session, attachOut)
				if initialReplayCaughtUp(session.StartOffset, session.ReplayEndOffset) && attachCanResize {
					attachInitialResizeQueued = true
					queueResize()
				}
			}
			state.render(os.Stdout)
		}
	}
}
