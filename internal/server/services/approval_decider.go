package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// Decider surfaces recorded in approvals.decided_via.
const (
	ViaWeb      = "web"
	ViaTUI      = "tui"
	ViaDiscord  = "discord"
	ViaTelegram = "telegram"
	ViaSweeper  = "sweeper"
)

// DecisionHook is called once after every decision commits, from every path
// (web API, console forms, approver API, Discord, Telegram, sweeper). status is
// the new status: approved, rejected or ignored. Hooks run synchronously on the
// deciding goroutine, so they must be quick and must never panic; an audit
// recorder should log its own errors rather than return them.
type DecisionHook func(ctx context.Context, approvalID, decidedBy, via, status string)

// ErrApprovalNotFound means the approval ID does not exist.
var ErrApprovalNotFound = errors.New("approval not found")

// AlreadyDecidedError reports a lost race: the approval was decided before this
// attempt, with the existing decision attached so a UI can show "already
// approved by ...". It unwraps to queries.ErrApprovalNotPending.
type AlreadyDecidedError struct{ Decision queries.ApprovalDecision }

func (e *AlreadyDecidedError) Error() string {
	msg := "approval already decided (" + e.Decision.Status + ")"
	if e.Decision.DecidedBy != "" {
		msg += " by " + e.Decision.DecidedBy
	}
	return msg
}
func (e *AlreadyDecidedError) Unwrap() error { return queries.ErrApprovalNotPending }

// ApprovalDecider is the single place that changes an approval's status. It
// attributes the decision, turns a concurrent decision into AlreadyDecidedError
// and notifies observers.
type ApprovalDecider struct {
	approvals *queries.ApprovalQueries
	observers []DecisionHook
}

func NewApprovalDecider(a *queries.ApprovalQueries) *ApprovalDecider {
	return &ApprovalDecider{approvals: a}
}

// AddObserver registers a hook. Call it during setup, before decisions flow.
func (d *ApprovalDecider) AddObserver(h DecisionHook) {
	if h != nil {
		d.observers = append(d.observers, h)
	}
}

func (d *ApprovalDecider) notify(ctx context.Context, id, by, via, status string) {
	for _, h := range d.observers {
		h(ctx, id, by, via, status)
	}
}

// conflict builds the error for an update that matched no pending row.
func (d *ApprovalDecider) conflict(id string) error {
	dec, err := d.approvals.GetDecision(id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrApprovalNotFound
	}
	if err != nil {
		return queries.ErrApprovalNotPending
	}
	if dec.Status == "pending" { // vanished and came back: should not happen; keep generic
		return queries.ErrApprovalNotPending
	}
	return &AlreadyDecidedError{Decision: dec}
}

// Approve grants a pending approval for minutes (<= 0: the phantom token's
// configured approval time limit).
func (d *ApprovalDecider) Approve(ctx context.Context, id string, minutes int, by, via string) error {
	if minutes <= 0 {
		if a, err := d.approvals.GetByID(id); err == nil {
			minutes = d.approvals.PlaceholderApprovalMinutes(a.PlaceholderID)
		} else {
			minutes = queries.DefaultApprovalMinutes
		}
	}
	exp := queries.ApprovalExpiry(time.Duration(queries.ClampApprovalMinutes(minutes)) * time.Minute)
	if err := d.approvals.ApproveBy(id, exp, by, via); err != nil {
		if errors.Is(err, queries.ErrApprovalNotPending) {
			return d.conflict(id)
		}
		return err
	}
	d.notify(ctx, id, by, via, "approved")
	return nil
}

// Reject rejects a pending approval.
func (d *ApprovalDecider) Reject(ctx context.Context, id string, by, via string) error {
	if err := d.approvals.RejectBy(id, by, via); err != nil {
		if errors.Is(err, queries.ErrApprovalNotPending) {
			return d.conflict(id)
		}
		return err
	}
	d.notify(ctx, id, by, via, "rejected")
	return nil
}

// Expire marks pending approvals older than ttlMinutes ignored (system:sweeper).
func (d *ApprovalDecider) Expire(ctx context.Context, ttlMinutes int) (int, error) {
	ids, err := d.approvals.ExpirePending(ttlMinutes)
	for _, id := range ids {
		d.notify(ctx, id, queries.SweeperDecidedBy, ViaSweeper, "ignored")
	}
	return len(ids), err
}

// ChatActor identifies who pressed a chat button, reacted or typed a command.
type ChatActor struct {
	Kind     string   // "discord" or "telegram"
	UserID   string   // platform user ID
	Username string   // display only
	RoleIDs  []string // Discord guild member roles, when the event carries them
}

// DecidedBy is the approvals.decided_by value for this actor.
func (a ChatActor) DecidedBy() string { return a.Kind + ":" + a.UserID }

// DecisionFunc applies an approve (minutes <= 0: the token's default limit) or
// reject decision made from a chat surface.
type DecisionFunc func(approvalID string, approve bool, minutes int, actor ChatActor) error

// NewChatDecision adapts the decider to a DecisionFunc for chat surfaces.
func (d *ApprovalDecider) NewChatDecision() DecisionFunc {
	return func(id string, approve bool, minutes int, actor ChatActor) error {
		ctx := context.Background()
		if approve {
			return d.Approve(ctx, id, minutes, actor.DecidedBy(), actor.Kind)
		}
		return d.Reject(ctx, id, actor.DecidedBy(), actor.Kind)
	}
}

// describeDecision is the one-line result shown on chat messages.
func describeDecision(status, decidedBy string, minutes int, at time.Time) string {
	verb := map[string]string{"approved": "Approved", "rejected": "Rejected", "ignored": "Expired (ignored)"}[status]
	if verb == "" {
		verb = status
	}
	line := verb
	if status == "approved" && minutes > 0 {
		line += " for " + fmtMinutes(minutes)
	}
	if decidedBy != "" {
		line += " by " + decidedBy
	}
	return fmt.Sprintf("%s at %s UTC", line, at.UTC().Format("2006-01-02 15:04"))
}

func fmtMinutes(m int) string {
	switch {
	case m%1440 == 0:
		return fmt.Sprintf("%d d", m/1440)
	case m%60 == 0:
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d min", m)
}
