package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

type LifecycleOperation string
type LifecycleMode string
type LifecyclePhase string

const (
	LifecycleEnd     LifecycleOperation = "end"
	LifecycleDestroy LifecycleOperation = "destroy"
	LifecycleRestart LifecycleOperation = "restart"

	LifecycleImmediate LifecycleMode = "immediate"
	LifecycleWait      LifecycleMode = "wait"
	LifecycleForce     LifecycleMode = "force"

	LifecycleWaiting        LifecyclePhase = "waiting"
	LifecycleStopping       LifecyclePhase = "stopping"
	LifecycleRuntimeStopped LifecyclePhase = "runtime_stopped"
	LifecycleCleaning       LifecyclePhase = "cleaning"
	LifecycleCompleted      LifecyclePhase = "completed"
)

type PendingLifecycle struct {
	SessionID        model.SessionID
	Operation        LifecycleOperation
	Mode             LifecycleMode
	Requester        model.Owner
	SourceEpoch      uint64
	SourceGeneration uint64
	RequestID        string
	Phase            LifecyclePhase
	CreatedAtMS      int64
	UpdatedAtMS      int64
	Attempt          uint64
	LastError        string
}

func (p PendingLifecycle) Matches(session model.Session) bool {
	return session.Writer != nil && *session.Writer == p.Requester && session.OwnershipEpoch == p.SourceEpoch && session.RuntimeGeneration == p.SourceGeneration
}

func (p PendingLifecycle) validate() error {
	if p.Operation != LifecycleEnd && p.Operation != LifecycleDestroy && p.Operation != LifecycleRestart {
		return fmt.Errorf("invalid lifecycle operation")
	}
	if p.Mode != LifecycleImmediate && p.Mode != LifecycleWait && p.Mode != LifecycleForce {
		return fmt.Errorf("invalid lifecycle mode")
	}
	if err := p.Requester.Validate(); err != nil || p.SourceEpoch == 0 || p.SourceGeneration == 0 || p.RequestID == "" {
		return fmt.Errorf("invalid lifecycle request")
	}
	return nil
}

func validLifecyclePhase(phase LifecyclePhase) bool {
	switch phase {
	case LifecycleWaiting, LifecycleStopping, LifecycleRuntimeStopped, LifecycleCleaning, LifecycleCompleted:
		return true
	default:
		return false
	}
}

// ReserveLifecycle installs the durable admission barrier in the same
// transaction that revalidates owner, fences, and immediate-idle semantics.
func (s *SQLite) ReserveLifecycle(ctx context.Context, pending PendingLifecycle) (model.Session, bool, error) {
	if err := pending.validate(); err != nil {
		return model.Session{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Session{}, false, err
	}
	defer tx.Rollback()
	session, err := s.GetSessionTx(ctx, tx, pending.SessionID)
	if err != nil {
		return model.Session{}, false, err
	}
	existing, err := s.GetPendingLifecycleTx(ctx, tx, pending.SessionID)
	if err != nil {
		return model.Session{}, false, err
	}
	if existing != nil {
		if !existing.Matches(session) {
			if err := s.DeletePendingLifecycleTx(ctx, tx, pending.SessionID); err != nil {
				return model.Session{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return model.Session{}, false, err
			}
			if session.RuntimeGeneration != existing.SourceGeneration {
				return model.Session{}, false, model.ErrStaleGeneration
			}
			if session.OwnershipEpoch != existing.SourceEpoch {
				return model.Session{}, false, model.ErrStaleEpoch
			}
			return model.Session{}, false, model.ErrNotOwner
		}
		if existing.Operation == pending.Operation && existing.Mode == pending.Mode && existing.Requester == pending.Requester &&
			existing.SourceEpoch == pending.SourceEpoch && existing.SourceGeneration == pending.SourceGeneration && existing.RequestID == pending.RequestID {
			return session, true, nil
		}
		return model.Session{}, false, model.ErrLifecyclePending
	}
	if err := session.AuthorizeAgentInput(pending.Requester, pending.SourceEpoch, pending.SourceGeneration); err != nil {
		return model.Session{}, false, err
	}
	if existingYield, err := s.GetPendingYieldTx(ctx, tx, pending.SessionID); err != nil {
		return model.Session{}, false, err
	} else if existingYield != nil {
		return model.Session{}, false, model.ErrPendingYield
	}
	if pending.Mode == LifecycleImmediate && session.TaskState != model.TaskIdle {
		return model.Session{}, false, model.ErrTaskActive
	}
	pending.CreatedAtMS = time.Now().UTC().UnixMilli()
	pending.UpdatedAtMS = pending.CreatedAtMS
	pending.Phase = LifecycleWaiting
	_, err = tx.ExecContext(ctx, `INSERT INTO pending_lifecycle_operations
		(session_id,operation,mode,requester_kind,requester_id,source_epoch,source_generation,request_id,phase,created_at_ms,updated_at_ms,attempt,last_error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, pending.SessionID, pending.Operation, pending.Mode, pending.Requester.Kind, pending.Requester.ID,
		pending.SourceEpoch, pending.SourceGeneration, pending.RequestID, pending.Phase, pending.CreatedAtMS, pending.UpdatedAtMS, pending.Attempt, pending.LastError)
	if err != nil {
		return model.Session{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.Session{}, false, err
	}
	return session, false, nil
}

func (s *SQLite) GetPendingLifecycle(ctx context.Context, id model.SessionID) (*PendingLifecycle, error) {
	return scanPendingLifecycle(s.db.QueryRowContext(ctx, `SELECT session_id,operation,mode,requester_kind,requester_id,source_epoch,source_generation,request_id,phase,created_at_ms,updated_at_ms,attempt,last_error
		FROM pending_lifecycle_operations WHERE session_id=?`, id))
}

func (s *SQLite) GetPendingLifecycleTx(ctx context.Context, tx *sql.Tx, id model.SessionID) (*PendingLifecycle, error) {
	return scanPendingLifecycle(tx.QueryRowContext(ctx, `SELECT session_id,operation,mode,requester_kind,requester_id,source_epoch,source_generation,request_id,phase,created_at_ms,updated_at_ms,attempt,last_error
		FROM pending_lifecycle_operations WHERE session_id=?`, id))
}

func scanPendingLifecycle(row interface{ Scan(...any) error }) (*PendingLifecycle, error) {
	var pending PendingLifecycle
	if err := row.Scan(&pending.SessionID, &pending.Operation, &pending.Mode, &pending.Requester.Kind, &pending.Requester.ID,
		&pending.SourceEpoch, &pending.SourceGeneration, &pending.RequestID, &pending.Phase, &pending.CreatedAtMS,
		&pending.UpdatedAtMS, &pending.Attempt, &pending.LastError); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &pending, nil
}

// ListPendingLifecycles returns every unfinished lifecycle intent in stable
// session order so daemon recovery can deterministically recreate executors.
func (s *SQLite) ListPendingLifecycles(ctx context.Context) ([]PendingLifecycle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id,operation,mode,requester_kind,requester_id,source_epoch,source_generation,request_id,phase,created_at_ms,updated_at_ms,attempt,last_error
		FROM pending_lifecycle_operations WHERE phase<>'completed' ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []PendingLifecycle
	for rows.Next() {
		item, err := scanPendingLifecycle(rows)
		if err != nil {
			return nil, err
		}
		pending = append(pending, *item)
	}
	return pending, rows.Err()
}

// CompareAndSwapLifecyclePhase advances one exact lifecycle request. Attempts
// count executor claims, including same-phase retries that only update error
// diagnostics.
func (s *SQLite) CompareAndSwapLifecyclePhase(ctx context.Context, id model.SessionID, requestID string, from, to LifecyclePhase, lastError string) (PendingLifecycle, error) {
	if requestID == "" || !validLifecyclePhase(from) || !validLifecyclePhase(to) || len(lastError) > 1024 {
		return PendingLifecycle{}, fmt.Errorf("invalid lifecycle phase transition")
	}
	now := time.Now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PendingLifecycle{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE pending_lifecycle_operations
		SET phase=?,updated_at_ms=?,attempt=attempt+1,last_error=?
		WHERE session_id=? AND request_id=? AND phase=?`, to, now, lastError, id, requestID, from)
	if err != nil {
		return PendingLifecycle{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return PendingLifecycle{}, ErrNotFound
	}
	pending, err := s.GetPendingLifecycleTx(ctx, tx, id)
	if err != nil {
		return PendingLifecycle{}, err
	}
	if pending == nil {
		return PendingLifecycle{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return PendingLifecycle{}, err
	}
	return *pending, nil
}

// CancelWaitingLifecycle removes only the caller's exact, not-yet-executing
// request. Once stopping begins cancellation must fail closed.
func (s *SQLite) CancelWaitingLifecycle(ctx context.Context, id model.SessionID, requestID string, requester model.Owner, epoch, generation uint64) (bool, error) {
	if requestID == "" || requester.Validate() != nil || epoch == 0 || generation == 0 {
		return false, fmt.Errorf("invalid lifecycle cancellation")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM pending_lifecycle_operations
		WHERE session_id=? AND request_id=? AND requester_kind=? AND requester_id=? AND source_epoch=? AND source_generation=? AND phase='waiting'`,
		id, requestID, requester.Kind, requester.ID, epoch, generation)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *SQLite) DeletePendingLifecycleTx(ctx context.Context, tx *sql.Tx, id model.SessionID) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM pending_lifecycle_operations WHERE session_id=?`, id)
	return err
}

func (s *SQLite) DeletePendingLifecycle(ctx context.Context, id model.SessionID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_lifecycle_operations WHERE session_id=?`, id)
	return err
}
