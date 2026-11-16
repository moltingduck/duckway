package main

import (
	"fmt"
	"os"
	"strings"
)

// inputMouse dispatches SGR mouse reports for help, panes and the session list.
func (l *tuiLoop) inputMouse(f *inputFrame) inputFlow {
	state := l.state
	mouseReport := strings.HasPrefix(string(f.b), "\x1b[<")
	var helpMouseKey []byte
	if button, x, y, ok := parseSGRMouse(string(f.b)); ok {
		if key, owned := state.handleHelpMouseReport(button, x, y, strings.HasSuffix(string(f.b), "M")); owned {
			if len(key) == 0 {
				state.render(os.Stdout)
				return inputDone
			}
			helpMouseKey = key
		} else if state.blockingModalOpen() {
			if state.projectFiles.open && button == 0 && strings.HasSuffix(string(f.b), "m") {
				f.b = state.projectFilesMouseRelease(x, y)
				if len(f.b) == 0 {
					return inputDone
				}
			} else {
				if button != 0 || !strings.HasSuffix(string(f.b), "M") {
					return inputDone
				}
				f.b = state.modalMouseInput(x, y)
				if len(f.b) == 0 {
					return inputDone
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
				return inputDone
			}
			if (button == 64 || button == 65) && !state.workspaceListWheelOwnsInput(x, y) {
				return inputDone
			}
			if state.copyMode {
				return inputDone
			}
			if state.focused {
				if !state.workspacePreview || (button != 0 && button != 2) || !strings.HasSuffix(string(f.b), "M") {
					return inputDone
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
		return inputDone
	}
	if len(helpMouseKey) != 0 {
		f.b = helpMouseKey
	}
	return inputNext
}

// inputWorkspaceMouse handles workspace mouse dragging and clicks.
func (l *tuiLoop) inputWorkspaceMouse(f *inputFrame) inputFlow {
	state := l.state
	wasWorkspaceDragging := state.workspaceDragMoved
	handledWorkspaceMouse, changedWorkspaceMouse := state.handleWorkspaceMouse(f.b)
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
			f.b = []byte("\r")
			if state.workspaceNav != nil && state.workspaceNav.InDetailMode() {
				f.b = []byte(shortcutInput(state.cfg.Shortcut("detail_focus")))
			}
		} else {
			state.render(os.Stdout)
			return inputDone
		}
	}
	return inputNext
}

// inputDetailed handles detailed workspace navigation and project input.
func (l *tuiLoop) inputDetailed(f *inputFrame) inputFlow {
	state := l.state
	wasDetailed := state.workspaceNav != nil && state.workspaceNav.InDetailMode()
	previousDetail := state.detailSelected
	if detailAction := state.handleDetailedInput(f.b); detailAction != "" {
		switch detailAction {
		case "changed":
			isDetailed := state.workspaceNav != nil && state.workspaceNav.InDetailMode()
			if l.workspaceOutput != nil && (wasDetailed != isDetailed || previousDetail != state.detailSelected) {
				l.selectPooledOutput()
			}
			state.render(os.Stdout)
			return inputDone
		case "focus", "jump":
			state.workspaceAttachFromProject = true
			state.workspaceFocusFromProject = detailAction == "jump"
			f.b = []byte("\r") // continue through the owner-gated attach action
		}
	}
	if handled, changed := state.handleWorkspaceProjectInput(f.b); handled {
		if changed && l.control != nil && !state.focused {
			_ = l.control.Stdin.Close()
			l.control, l.controlDone = nil, nil
			state.clearAttachIdentity()
		}
		if changed && l.workspaceOutput != nil {
			l.selectPooledOutput()
		}
		state.render(os.Stdout)
		return inputDone
	}
	return inputNext
}
