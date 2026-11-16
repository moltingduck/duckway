package main

import (
	"os"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// inputFlow tells onInput what to do after a key-handling stage ran.
type inputFlow int

const (
	inputNext inputFlow = iota // fall through to the next stage
	inputDone                  // the key was consumed; wait for the next event
	inputExit                  // leave the run loop with inputFrame.err
)

// inputFrame carries the state one keyboard event shares between stages.
type inputFrame struct {
	b                          []byte
	err                        error
	paneCommand                string
	replayedInput              bool
	selectedActionTarget       *ducklord.RemoteSession
	workspaceNavigationReplay  bool
	workspaceProjectNavigation bool
}

// inputStages lists the key-handling stages in the order the original run
// loop evaluated them; the first stage that returns inputDone or inputExit
// ends the event.
var inputStages = []func(*tuiLoop, *inputFrame) inputFlow{
	(*tuiLoop).inputBegin,
	(*tuiLoop).inputTerminalTools,
	(*tuiLoop).inputDeferWorkspace,
	(*tuiLoop).inputCancelPendingOnClick,
	(*tuiLoop).inputCentralModalCancel,
	(*tuiLoop).inputCommandPalette,
	(*tuiLoop).inputWorkspacePaneModal,
	(*tuiLoop).inputHelpMode,
	(*tuiLoop).inputPendingFocus,
	(*tuiLoop).inputPendingPTY,
	(*tuiLoop).inputDirectNotes,
	(*tuiLoop).inputProjectFiles,
	(*tuiLoop).inputMouse,
	(*tuiLoop).inputCopyMode,
	(*tuiLoop).inputHelpSearch,
	(*tuiLoop).inputPanePrefix,
	(*tuiLoop).inputFocusedTerminal,
	(*tuiLoop).inputModalArrowAndHelp,
	(*tuiLoop).inputCentralModalCtrlC,
	(*tuiLoop).inputSearch,
	(*tuiLoop).inputSettingsModals,
	(*tuiLoop).inputHostMenu,
	(*tuiLoop).inputClientModals,
	(*tuiLoop).inputNewSession,
	(*tuiLoop).inputSmallModals,
	(*tuiLoop).inputActionMenu,
	(*tuiLoop).inputLifecycleConfirm,
	(*tuiLoop).inputWorkspaceMouse,
	(*tuiLoop).inputDetailed,
	(*tuiLoop).inputDispatchAction,
}

// onInput handles one chunk of terminal input. It reports whether the run
// loop must return, and with which error.
func (l *tuiLoop) onInput(b []byte) (exit bool, err error) {
	f := &inputFrame{b: b}
	for _, stage := range inputStages {
		switch stage(l, f) {
		case inputDone:
			return false, nil
		case inputExit:
			return true, f.err
		}
	}
	return false, nil
}

// inputBegin records replay flags and clears the pending-input marker.
func (l *tuiLoop) inputBegin(f *inputFrame) inputFlow {
	state := l.state
	f.replayedInput = l.replay.queued
	if l.replay.queued {
		l.replay.queued = false
	}
	f.workspaceNavigationReplay = l.replay.navigation
	l.replay.navigation = false
	if shouldRouteWorkspaceReplayAsNavigation(f.workspaceNavigationReplay) {
		// The replacement lease may have restored focused before the
		// deferred key arrives; route it through workspace navigation.
		state.focused = false
	}
	l.inputPending = false
	return inputNext
}

// inputTerminalTools routes every byte to an open terminal search/bookmark tool.
func (l *tuiLoop) inputTerminalTools(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	// A terminal tool owns every byte while its local modal is open.
	// Route it before workspace replay, modal cancellation, or PTY focus
	// handling can consume the input.
	if state.terminalSearchMode || state.terminalBookmarkMode || state.terminalBookmarkListMode {
		state.handleTerminalToolInput(b)
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}

// inputDeferWorkspace defers keys that arrive while a workspace placement is pending.
func (l *tuiLoop) inputDeferWorkspace(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	f.workspaceProjectNavigation = shouldRouteWorkspaceProjectInputAsNavigation(state, b, l.replay.toPTY)
	if shouldDeferWorkspaceInput(state, f.replayedInput) && !f.workspaceProjectNavigation {
		l.replay.deferred = append(l.replay.deferred, b...)
		return inputDone
	}
	return inputNext
}

// inputCancelPendingOnClick cancels a pending PTY focus when another pane is clicked.
func (l *tuiLoop) inputCancelPendingOnClick(f *inputFrame) inputFlow {
	b := f.b
	state := l.state
	// Clicking another pane cancels pending focus before changing its
	// target. Late control completions remain fenced by controlID.
	if button, _, _, ok := parseSGRMouse(string(b)); ok && (button == 0 || button == 2) && strings.HasSuffix(string(b), "M") && state.workspacePreview && !state.blockingModalOpen() {
		state.panePrefixPending = false
		if handlePendingPTYInput(state, []byte("\x1b"), l.workspaceOutput != nil, &l.control, &l.controlOpenCancel, &l.controlID) {
			l.controlDone = nil
		}
	}
	return inputNext
}
