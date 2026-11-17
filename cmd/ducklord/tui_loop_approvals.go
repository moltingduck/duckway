package main

import (
	"context"
	"errors"
	"os"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// approvalsEvent reports asynchronous approver API work to the run loop.
type approvalsEvent struct {
	kind     string // list | decision | stream
	gen      uint64
	items    []ducklord.ApproverApproval
	history  []ducklord.ApproverApproval
	hasHist  bool
	err      error
	id       string
	approved bool
	decision *ducklord.ApproverDecision
	stream   ducklord.ApproverEvent
	epoch    uint64 // watch epoch the stream event came from
}

// startApprovals loads the stored approver login, if any, and starts the live
// events watch. It is safe to call again after `ducklord approver login`.
func (l *tuiLoop) startApprovals() {
	state := l.state
	a := &state.approvals
	if l.approvalsCancel != nil {
		l.approvalsCancel()
		l.approvalsCancel = nil
	}
	a.loggedIn, a.client, a.loadErr, a.conn = false, nil, "", ""
	a.watchEpoch++
	a.generation++ // list responses of the previous login are void
	a.loading, a.listInFlight, a.listDirty = false, false, false
	a.localWarning = localAgentWarning()
	creds, err := ducklord.LoadApproverCredentials(approverCredentialsFor(state.cfgPath))
	if err != nil {
		if !errors.Is(err, ducklord.ErrApproverNotLoggedIn) {
			a.loadErr = oneLine(err.Error())
		}
		return
	}
	client, err := ducklord.NewApproverClient(creds.Server, creds.Token, nil)
	if err != nil {
		a.loadErr = oneLine(err.Error())
		return
	}
	a.loggedIn, a.client, a.server, a.conn = true, client, client.Server(), "connecting"
	if l.approvalsDone == nil {
		return // tests drive events by hand
	}
	ctx, cancel := context.WithCancel(l.ctx)
	l.approvalsCancel = cancel
	events := client.WatchEvents(ctx, ducklord.DefaultApproverBackoff)
	done, epoch := l.approvalsDone, a.watchEpoch
	go func() {
		for ev := range events {
			select {
			case done <- approvalsEvent{kind: "stream", stream: ev, epoch: epoch}:
			case <-ctx.Done():
				return
			}
		}
	}()
	l.refreshApprovals()
}

// openApprovals shows the panel (re-reading the login when there is none).
func (l *tuiLoop) openApprovals() {
	a := &l.state.approvals
	if !a.loggedIn {
		l.startApprovals()
	}
	a.open, a.detail, a.picker, a.message = true, false, false, ""
	a.localWarning = localAgentWarning()
	a.guardKeys()
	if a.loggedIn {
		l.refreshApprovals()
	}
}

func (l *tuiLoop) sendApprovals(ev approvalsEvent) {
	select {
	case l.approvalsDone <- ev:
	case <-l.ctx.Done():
	}
}

// refreshApprovals refetches the pending list (and history when shown).
func (l *tuiLoop) refreshApprovals() {
	a := &l.state.approvals
	if !a.loggedIn || a.client == nil || l.approvalsDone == nil {
		return
	}
	if a.listInFlight {
		a.listDirty = true // one more fetch once the current one lands
		return
	}
	a.listInFlight, a.loading = true, true
	gen, client, history := a.generation, a.client, a.showHistory
	go func() {
		items, err := client.List(l.ctx, []string{"pending"}, 200)
		ev := approvalsEvent{kind: "list", gen: gen, items: items, err: err}
		if err == nil && history {
			ev.history, ev.err = client.List(l.ctx, []string{"approved", "rejected", "ignored"}, 50)
			ev.hasHist = ev.err == nil
		}
		l.sendApprovals(ev)
	}()
}

// runApprovalsCmd performs the action a key press asked for.
func (l *tuiLoop) runApprovalsCmd(cmd approvalsCmd) {
	a := &l.state.approvals
	switch cmd.kind {
	case "close":
		a.open, a.picker, a.detail = false, false, false
	case "refresh":
		l.refreshApprovals()
	case "reload":
		l.startApprovals()
		if a.loggedIn {
			l.refreshApprovals()
		}
	case "approve", "reject":
		if l.approvalsDone == nil || a.client == nil {
			return
		}
		client, id, minutes, approve := a.client, cmd.id, cmd.minutes, cmd.kind == "approve"
		go func() {
			ev := approvalsEvent{kind: "decision", id: id, approved: approve}
			if approve {
				ev.decision, ev.err = client.Approve(l.ctx, id, minutes)
			} else {
				ev.decision, ev.err = client.Reject(l.ctx, id)
			}
			l.sendApprovals(ev)
		}()
	}
}

// inputApprovals owns every key while the approvals panel is open. It sits
// right after Help in inputStages: a Help overlay or any earlier modal still
// wins, and the panel is only opened when no other modal is.
func (l *tuiLoop) inputApprovals(f *inputFrame) inputFlow {
	state := l.state
	if !state.approvals.open {
		return inputNext
	}
	l.runApprovalsCmd(state.handleApprovalsInput(f.b))
	state.render(os.Stdout)
	return inputDone
}

// onApprovalsEvent applies one completed API call or stream update.
func (l *tuiLoop) onApprovalsEvent(ev approvalsEvent) {
	state := l.state
	a := &state.approvals
	defer state.render(os.Stdout)
	fail := func(err error) {
		if errors.Is(err, ducklord.ErrApproverUnauthorized) {
			a.conn = "revoked"
			a.message, a.messageErr = oneLine(err.Error()), true
			return
		}
		a.message, a.messageErr = "approver API: "+oneLine(err.Error()), true
	}
	switch ev.kind {
	case "list":
		if ev.gen != a.generation {
			return // from a previous login
		}
		a.loading, a.listInFlight = false, false
		if a.listDirty {
			a.listDirty = false
			defer l.refreshApprovals()
		}
		if ev.err != nil {
			fail(ev.err)
			return
		}
		a.setItems(ev.items)
		if ev.hasHist {
			a.setHistory(ev.history)
		}
	case "decision":
		a.busy = ""
		switch {
		case ev.err == nil && ev.approved:
			a.message, a.messageErr = "approved "+shortApprovalID(ev.id), false
		case ev.err == nil:
			a.message, a.messageErr = "rejected "+shortApprovalID(ev.id), false
		default:
			var decided *ducklord.AlreadyDecidedError
			if errors.As(ev.err, &decided) {
				a.message, a.messageErr = oneLine(ev.err.Error()), true
			} else {
				fail(ev.err)
			}
		}
		l.refreshApprovals()
	case "stream":
		if ev.epoch != a.watchEpoch {
			return // an earlier watch (before a re-login)
		}
		switch ev.stream.Kind {
		case ducklord.ApproverEventConnected:
			a.conn = "live"
			l.refreshApprovals()
		case ducklord.ApproverEventApprovals:
			a.conn = "live"
			a.pendingCount, a.countKnown = int(ev.stream.Pending), true
			if a.open {
				l.refreshApprovals()
			}
		case ducklord.ApproverEventDisconnected:
			a.conn = "reconnecting"
		case ducklord.ApproverEventUnauthorized:
			fail(ducklord.ErrApproverUnauthorized)
		}
	}
}

func shortApprovalID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// maybeHintApprovalsFromNotification reacts to an agent "approval required"
// notification. It never opens the panel: a panel appearing by itself while the
// operator types could turn a stray y or n into a decision on a request they
// never saw. It only leaves a status-line hint; the panel opens on an explicit
// shortcut (or the command palette entry).
func (l *tuiLoop) maybeHintApprovalsFromNotification() {
	state := l.state
	if !state.approvalNotifyPending {
		return
	}
	state.approvalNotifyPending = false
	a := &state.approvals
	if !a.loggedIn || a.open || (a.countKnown && a.pendingCount == 0) {
		return
	}
	state.outputErr = state.approvalsHint()
}
