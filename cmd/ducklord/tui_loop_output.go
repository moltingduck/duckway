package main

import (
	"context"
	"os"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (l *tuiLoop) selectPooledOutput() {
	state := l.state
	cfg := l.cfg
	if l.outputManager == nil {
		return
	}
	if state.workspacePaneMode && state.workspacePaneStep == "notes" {
		return
	}
	if len(state.sessions) == 0 {
		if l.workspaceOutput != nil {
			l.workspaceOutput.ClearVisible()
		}
		l.outputRequestID++
		l.selectedOutputSession = ducklord.RemoteSession{}
		l.activeOutputEvent = ducklord.TerminalOutputEvent{}
		state.outputForKey, state.outputText, state.terminal = "", "", nil
		state.outputFresh = false
		return
	}
	l.reconnectRequestID = 0
	state.outputReconnecting = false
	sess := state.activePTYSession()
	var workspacePriority ducklord.TerminalSelection
	if l.workspaceOutput != nil {
		visible := state.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: sess.Client}})
		if len(visible) == 0 {
			l.workspaceOutput.ClearVisible()
			l.outputRequestID++
			l.selectedOutputSession = ducklord.RemoteSession{}
			l.activeOutputEvent = ducklord.TerminalOutputEvent{}
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
	l.selectedOutputSession = sess
	key, keyOK := terminalOutputKey(sess)
	selectedKey := sessionKey(sess)
	if state.outputForKey != selectedKey || !keyOK || l.activeOutputEvent.Key != key || l.activeOutputEvent.Revision.RuntimeGeneration != sess.RuntimeGeneration {
		state.outputText = ""
		state.terminal = nil
		state.ptyScrollOffset = 0
		l.activeOutputEvent = ducklord.TerminalOutputEvent{}
		state.outputForKey = selectedKey
	}
	if l.workspaceOutput != nil {
		l.outputRequestID = l.workspaceOutput.Select(workspacePriority)
		state.outputForKey = selectedKey
		state.outputFresh = false
		state.outputErr = "loading live PTY output..."
		return
	}
	if sess.Client == "" || sess.InstanceID == "" || sess.SessionID == "" || sess.RuntimeGeneration == 0 || !canRead(sess) || !state.hostIsLive(sess.Client) {
		l.outputRequestID++
		l.selectedOutputSession = ducklord.RemoteSession{}
		l.activeOutputEvent = ducklord.TerminalOutputEvent{}
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
	l.outputRequestID = l.outputManager.Select(ducklord.TerminalSelection{Client: client, InstanceID: sess.InstanceID, SessionID: sess.SessionID,
		RuntimeGeneration: sess.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
	state.outputForKey = selectedKey
	state.outputFresh = false
	state.outputErr = "loading live PTY output..."
}

func (l *tuiLoop) startPreview() {
	state := l.state
	ctx := l.ctx
	cfg := l.cfg
	runner := l.runner
	if l.previewInFlight || !l.previewQueued || state.focused || len(state.sessions) == 0 {
		return
	}
	l.previewQueued = false
	l.previewInFlight = true
	id := l.previewID
	sess := state.currentSession()
	key, generation := sessionKey(sess), sess.RuntimeGeneration
	client, clientErr := mustClient(cfg, sess.Client)
	previewCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	l.previewCancel = cancel
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
		case l.previewDone <- previewOutputEvent{id: id, key: key, generation: generation, text: text, err: err}:
		case <-ctx.Done():
		}
	}()
}

func (l *tuiLoop) requestPreview(fence bool) {
	state := l.state
	if l.outputManager != nil {
		l.selectPooledOutput()
		return
	}
	if state.focused || len(state.sessions) == 0 {
		return
	}
	if fence || l.previewID == 0 {
		l.previewID++
	}
	l.previewQueued = true
	l.startPreview()
}

func (l *tuiLoop) fencePreview() {
	if l.outputManager != nil {
		return
	}
	l.previewID++
	l.previewQueued = false
}

func (l *tuiLoop) renderOutput() {
	state := l.state
	if l.lastOutputRender.IsZero() || time.Since(l.lastOutputRender) >= 50*time.Millisecond {
		state.render(os.Stdout)
		l.lastOutputRender = time.Now()
	}
}
