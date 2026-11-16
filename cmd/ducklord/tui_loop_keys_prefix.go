package main

import (
	"os"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// inputPanePrefix handles the pane prefix key and its commands.
func (l *tuiLoop) inputPanePrefix(f *inputFrame) inputFlow {
	state := l.state
	ctx := l.ctx
	f.paneCommand = ""
	if consumed, command := state.handlePanePrefix(f.b); consumed {
		l.resetPanePrefixTimer(state.panePrefixDeadline)
		f.paneCommand = command
		if command == "" {
			state.render(os.Stdout)
			return inputDone
		}
		if command == " " {
			// Space completes the local command-palette route. Keep the
			// focused control intact so Esc can restore the exact PTY origin.
			state.openCommandPalette()
			state.render(os.Stdout)
			return inputDone
		}
		if strings.HasPrefix(command, "quick-shell:") {
			state.beginQuickShell(ctx, l.createDone, strings.TrimPrefix(command, "quick-shell:"))
			state.render(os.Stdout)
			return inputDone
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
					return inputDone
				}
			}
			if err != nil {
				state.outputErr = err.Error()
				state.render(os.Stdout)
				return inputDone
			}
		}
		if !state.focused {
			if paneNavigationCommand(command) {
				if !state.navigatePrefixPane(command) {
					state.render(os.Stdout)
					return inputDone
				}
				l.requestPreview(true)
				f.b = []byte("\r")
			} else {
				dispatchPaneCommandAndRestoreHelpFocus(state, command, l.workspaceOutput, true, l.control, l.attach)
				l.queuePaneReplay()
				state.render(os.Stdout)
				return inputDone
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
				return inputDone
			}
			f.b = []byte(shortcutInput(state.cfg.Shortcut("pty_unfocus")))
		}
	}
	return inputNext
}
