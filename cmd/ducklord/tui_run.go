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
			replayedInput := l.replay.queued
			if l.replay.queued {
				l.replay.queued = false
			}
			workspaceNavigationReplay := l.replay.navigation
			l.replay.navigation = false
			if shouldRouteWorkspaceReplayAsNavigation(workspaceNavigationReplay) {
				// The replacement lease may have restored focused before the
				// deferred key arrives; route it through workspace navigation.
				state.focused = false
			}
			l.inputPending = false
			// A terminal tool owns every byte while its local modal is open.
			// Route it before workspace replay, modal cancellation, or PTY focus
			// handling can consume the input.
			if state.terminalSearchMode || state.terminalBookmarkMode || state.terminalBookmarkListMode {
				state.handleTerminalToolInput(b)
				state.render(os.Stdout)
				continue
			}
			workspaceProjectNavigation := shouldRouteWorkspaceProjectInputAsNavigation(state, b, l.replay.toPTY)
			if shouldDeferWorkspaceInput(state, replayedInput) && !workspaceProjectNavigation {
				l.replay.deferred = append(l.replay.deferred, b...)
				continue
			}
			var selectedActionTarget *ducklord.RemoteSession
			// Clicking another pane cancels pending focus before changing its
			// target. Late control completions remain fenced by controlID.
			if button, _, _, ok := parseSGRMouse(string(b)); ok && (button == 0 || button == 2) && strings.HasSuffix(string(b), "M") && state.workspacePreview && !state.blockingModalOpen() {
				state.panePrefixPending = false
				if handlePendingPTYInput(state, []byte("\x1b"), l.workspaceOutput != nil, &l.control, &l.controlOpenCancel, &l.controlID) {
					l.controlDone = nil
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
					if l.control != nil && controlMatchesSession(l.control, restoreSession, state.ownerName) {
						state.focused = false
						// Keep the saved snapshot pending until the visible output lease
						// accepts input focus. The control writer may be ready first.
						if l.acceptWorkspaceControl(restoreSession) {
							l.restorePendingFocus()
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
					l.replay.deferred = nil
					if l.controlOpenCancel != nil {
						l.controlOpenCancel()
						l.controlOpenCancel = nil
						l.controlID++
					}
					if l.control != nil {
						_ = l.control.Stdin.Close()
						l.control = nil
						l.controlDone = nil
					}
					state.focused = false
					// Keep the input fence up until the restored Session pane's
					// control writer is accepted. Reopen the exact source Session
					// directly; routing Esc through Project would consume the user's
					// first shell command.
					state.workspacePlacementInputPending = true
					l.replay.deferred = nil
					if restoreSession.Client == "" {
						state.outputErr = "focused Session disappeared before PTY restore"
						state.workspacePaneFocusRestorePending = false
					} else {
						l.openControlForSession(restoreSession)
					}
				}
				state.notesFocusRestoreReselect = false
				if !state.focused && !state.workspacePaneFocusRestorePending {
					l.selectPooledOutput()
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
						l.requestPreview(true)
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
					l.replay.deferred = nil
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
					if l.control != nil && controlMatchesSession(l.control, restoreSession, state.ownerName) {
						state.focused = false
						// The saved restore snapshot remains authoritative until the
						// output focus setter accepts the handoff.
						if l.acceptWorkspaceControl(restoreSession) {
							l.restorePendingFocus()
						}
					} else {
						// The modal can close while the original writer is still
						// opening (or after it was detached). Reopen the exact
						// captured Session and keep the first shell command fenced
						// until that replacement writer is accepted.
						state.workspacePlacementInputPending = true
						l.replay.deferred = nil
						if l.controlOpenCancel != nil {
							l.controlOpenCancel()
							l.controlOpenCancel = nil
							l.controlID++
						}
						if l.control != nil {
							_ = l.control.Stdin.Close()
							l.control = nil
							l.controlDone = nil
						}
						state.focused = false
						if restoreSession.Client == "" {
							state.outputErr = "focused Session disappeared before PTY restore"
							state.workspacePaneFocusRestorePending = false
						} else {
							l.openControlForSession(restoreSession)
						}
					}
				}
				l.resetWorkspacePaneChangeIfNeeded()
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
							restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
						}
					} else if !state.scrollHelpInput(b) {
						state.handleHelpSearchInput(b)
					}
				} else if state.shortcut("help", string(b)) {
					state.dispatchHelpAction("help")
					if !state.helpMode {
						restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
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
				l.restorePendingFocus()
				drainNotesPendingInput(state, l.control)
			}
			// Deferred workspace keys are replayed only after the replacement
			// control lease is ready. Keep them on the workspace navigation route;
			// passing them through the pending PTY gate would consume navigation
			// keys such as b before handleWorkspaceProjectInput sees them.
			pendingHandled := false
			if !workspaceNavigationReplay && !workspaceProjectNavigation {
				pendingHandled = handlePendingPTYInput(state, b, l.workspaceOutput != nil, &l.control, &l.controlOpenCancel, &l.controlID)
			}
			if pendingHandled {
				// The writer may be ready even when the last output frame is stale.
				// Confirm the Help lease before interpreting a completed prefix.
				if !state.focused && state.helpFocusRestorePending {
					restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
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
						if state.beginFocusedPaneNavigation(command) && !l.replay.queued {
							l.replay.input <- []byte("\r")
							l.replay.queued = true
						}
					} else if !state.focused && state.helpFocusRestorePending {
						state.panePrefixReplay = append([]byte(shortcutInput(state.cfg.Shortcut("pane_prefix"))), []byte(command)...)
					} else if state.focused && state.dispatchFocusedPaneCommand(command) {
						// A direct focused detach bypasses the workspace modal branch.
						// Run the same lease teardown, then replay Enter so the selected
						// surviving pane opens its control writer.
						if l.resetWorkspacePaneChangeIfNeeded() && state.workspaceAttachFromProject && !l.replay.queued {
							// The surviving pane is selected synchronously, but its
							// control lease opens asynchronously. Hold real shell input
							// until that lease has been accepted.
							state.workspacePlacementInputPending = true
							l.replay.toPTY = true
							l.replay.deferred = nil
							l.replay.input <- []byte("\r")
							l.replay.queued = true
						}
					} else {
						state.dispatchPaneCommand(command)
					}
				}
				if l.control == nil {
					l.controlDone = nil
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
						l.controlID++
						if l.controlOpenCancel != nil {
							l.controlOpenCancel()
							l.controlOpenCancel = nil
						}
						if l.control != nil {
							_ = l.control.Stdin.Close()
						}
						l.control, l.controlDone = nil, nil
						if l.attach != nil {
							_ = l.attach.Stdin.Close()
						}
						if l.attachCancel != nil {
							l.attachCancel()
							l.attachCancel = nil
						}
						l.attachID++
						l.resizeInFlight = false
						l.pendingFramebufferResize, l.queuedResize = nil, nil
						l.bufferedAttach, l.bufferedAttachBytes = nil, 0
						l.attachOutputSource = l.attachOut
						state.focused = false
						state.clearAttachIdentity()
						l.attachCanResize, l.attachInitialResizeQueued = false, false
						l.attachReplayEndOffset = 0
						l.attach = nil
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
						restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
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
				l.resetPanePrefixTimer(state.panePrefixDeadline)
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
					state.beginQuickShell(ctx, l.createDone, strings.TrimPrefix(command, "quick-shell:"))
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
						l.requestPreview(true)
						b = []byte("\r")
					} else {
						dispatchPaneCommandAndRestoreHelpFocus(state, command, l.workspaceOutput, true, l.control, l.attach)
						l.queuePaneReplay()
						state.render(os.Stdout)
						continue
					}
				} else {
					if state.dispatchFocusedPaneCommand(command) {
						if l.resetWorkspacePaneChangeIfNeeded() && state.workspaceAttachFromProject && !l.replay.queued {
							// The surviving pane is selected synchronously, but its
							// control lease opens asynchronously. Hold real shell input
							// until that lease has been accepted.
							state.workspacePlacementInputPending = true
							l.replay.toPTY = true
							l.replay.deferred = nil
							l.replay.input <- []byte("\r")
							l.replay.queued = true
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
						l.controlID++
						l.attachID++
						if l.controlOpenCancel != nil {
							l.controlOpenCancel()
							l.controlOpenCancel = nil
						}
						if l.control != nil {
							_ = l.control.Stdin.Close()
							l.control, l.controlDone = nil, nil
						}
						if l.attachCancel != nil {
							l.attachCancel()
							l.attachCancel = nil
						}
						if l.attach != nil {
							_ = l.attach.Stdin.Close()
							l.attach = nil
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
					if l.outputManager == nil {
						state.saveCurrentSnapshot()
					}
					l.controlID++
					if l.controlOpenCancel != nil {
						l.controlOpenCancel()
						l.controlOpenCancel = nil
					}
					if l.control != nil {
						_ = l.control.Stdin.Close()
					}
					l.control, l.controlDone = nil, nil
					if l.attach != nil {
						_ = l.attach.Stdin.Close()
					}
					if l.attachCancel != nil {
						l.attachCancel()
						l.attachCancel = nil
					}
					l.attachID++
					l.resizeInFlight = false
					l.pendingFramebufferResize = nil
					l.queuedResize = nil
					l.bufferedAttach = nil
					l.bufferedAttachBytes = 0
					l.attachOutputSource = l.attachOut
					state.focused = false
					state.clearAttachIdentity()
					// A busy PTY may have advanced the terminal while the cached
					// frame still describes the focused view. Force a complete
					// navigation repaint when returning from terminal focus.
					state.frameOutput = frameOutput{}
					if state.workspaceNav != nil && state.workspaceNav.InDetailMode() {
						state.syncDetailSelection()
					}
					l.attachCanResize = false
					l.attachInitialResizeQueued = false
					l.attachReplayEndOffset = 0
					l.attach = nil
					if paneNavigationCommand(paneCommand) {
						if !state.beginFocusedPaneNavigation(paneCommand) {
							state.render(os.Stdout)
							continue
						}
						l.requestPreview(true)
						b = []byte("\r")
					} else {
						if strings.HasPrefix(paneCommand, "quick-shell:") {
							state.beginQuickShell(ctx, l.createDone, strings.TrimPrefix(paneCommand, "quick-shell:"))
							state.render(os.Stdout)
							continue
						}
						l.requestPreview(true)
						if paneCommand != "" {
							state.dispatchPaneCommand(paneCommand)
						}
						l.queuePaneReplay()
						state.render(os.Stdout)
						continue
					}
				}
				if state.focused {
					activeSession := state.activePTYSession()
					if writer := focusedPTYWriter(l.control, l.attach, activeSession, state.ownerName); writer != nil {
						_, _ = writer.Write(b)
					} else if l.control != nil {
						l.controlID++
						if l.controlOpenCancel != nil {
							l.controlOpenCancel()
						}
						_ = l.control.Stdin.Close()
						l.control, l.controlDone = nil, nil
						l.attachCanResize = false
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
						restorePendingHelpFocusWithAttach(state, l.workspaceOutput, true, l.control, l.attach)
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
						if l.workspaceOutput != nil && state.activeAttachKey != "" {
							if key, ok := terminalOutputKey(state.activePTYSession()); ok && setWorkspaceInputFocus(l.workspaceOutput, key) {
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
						if l.control != nil && controlMatchesSession(l.control, restoreSession, state.ownerName) {
							l.acceptWorkspaceControl()
						} else {
							if l.control != nil {
								_ = l.control.Stdin.Close()
								l.control, l.controlDone = nil, nil
							}
							if state.activeAttachKey != "" {
								_ = l.openControlForSession(restoreSession)
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
					if l.startCancel != nil {
						l.startCancel()
						l.startCancel = nil
						l.startID++
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
						l.selectPooledOutput()
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
					} else if !ok || l.outputManager == nil {
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
					epoch, instanceID := l.watchedClients[target].epoch, state.hostSync[target].InstanceID
					readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
					go func() {
						defer cancel()
						status, err := runner.HostResources(readCtx, client)
						publishHostResourceEvent(ctx, l.hostResourceDone, hostResourceEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, status: status, err: err})
					}()
				case "host-hooks-read":
					l.launchHostHookStatus(target)
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
				case "host-retention-read":
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
						break
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
				case "host-retention-save":
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
						break
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
				case "host-connect":
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
				if !state.hostMenuMode && l.hostRetentionCancel != nil {
					l.hostRetentionCancel()
					l.hostRetentionCancel = nil
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
					if err := state.startAddClient(ctx, l.addClientDone); err != nil {
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
				continue
			}
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
					continue
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
						return err
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
					if l.outputManager == nil {
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
					l.outputRequestID = l.outputManager.Reconnect(ducklord.TerminalSelection{Client: client, InstanceID: target.InstanceID, SessionID: target.SessionID,
						RuntimeGeneration: target.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
					l.reconnectRequestID = l.outputRequestID
					l.activeOutputEvent = ducklord.TerminalOutputEvent{}
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
				if changedWorkspaceMouse && l.workspaceOutput != nil {
					l.selectPooledOutput()
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
					if l.workspaceOutput != nil && (wasDetailed != isDetailed || previousDetail != state.detailSelected) {
						l.selectPooledOutput()
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
				if changed && l.control != nil && !state.focused {
					_ = l.control.Stdin.Close()
					l.control, l.controlDone = nil, nil
					state.clearAttachIdentity()
				}
				if changed && l.workspaceOutput != nil {
					l.selectPooledOutput()
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
					l.requestPreview(true)
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
				state.handleQuickSelection(&l.control, &l.controlDone, l.requestPreview)
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
				if l.control != nil && !state.focused {
					_ = l.control.Stdin.Close()
					l.control, l.controlDone = nil, nil
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
				l.fencePreview()
				if l.outputManager != nil {
					l.openControlForSession(s)
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
				l.attach = session
				l.attachCancel = cancel
				state.resizeStatus = ""
				l.attachID++
				l.resizeInFlight = false
				l.pendingFramebufferResize = nil
				l.queuedResize = nil
				l.bufferedAttach = nil
				l.bufferedAttachBytes = 0
				l.attachOutputSource = l.attachOut
				id := l.attachID
				state.activeAttachKey = ""
				state.activeAttachFresh = false
				state.pendingAttachKey = sessionKey(s)
				l.attachCanResize = state.canResizeCurrentSession() && session.ResizeBarrier != nil
				l.attachInitialResizeQueued = false
				l.attachReplayEndOffset = session.ReplayEndOffset
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
				go superviseAttach(attachCtx, id, session, l.attachOut)
				if initialReplayCaughtUp(session.StartOffset, session.ReplayEndOffset) && l.attachCanResize {
					l.attachInitialResizeQueued = true
					l.queueResize()
				}
			}
			state.render(os.Stdout)
		}
	}
}
