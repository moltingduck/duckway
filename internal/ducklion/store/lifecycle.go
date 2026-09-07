package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	LifecycleLaunching      LifecyclePhase = "launching"
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

type LifecycleOutcome struct {
	SessionID        model.SessionID
	Operation        LifecycleOperation
	Mode             LifecycleMode
	Requester        model.Owner
	SourceEpoch      uint64
	SourceGeneration uint64
	RequestID        string
	CompletedAtMS    int64
	Failure          string
}

func (o LifecycleOutcome) Matches(p PendingLifecycle) bool {
	return o.SessionID == p.SessionID && o.Operation == p.Operation && o.Mode == p.Mode && o.Requester == p.Requester &&
		o.SourceEpoch == p.SourceEpoch && o.SourceGeneration == p.SourceGeneration && o.RequestID == p.RequestID
}

func (p PendingLifecycle) Matches(session model.Session) bool {
	ownerMatches := (session.Kind == model.KindShell && p.Requester.Kind == model.OwnerTerminal) || (session.Writer != nil && *session.Writer == p.Requester)
	return ownerMatches && session.OwnershipEpoch == p.SourceEpoch && session.RuntimeGeneration == p.SourceGeneration
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
	case LifecycleWaiting, LifecycleStopping, LifecycleRuntimeStopped, LifecycleLaunching, LifecycleCleaning, LifecycleCompleted:
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
	if (session.Kind != model.KindAgent && session.Kind != model.KindShell) || (pending.Operation == LifecycleEnd && session.Status != model.StatusRunning) {
		return model.Session{}, false, model.ErrSessionNotRunning
	}
	if session.RuntimeGeneration != pending.SourceGeneration {
		return model.Session{}, false, model.ErrStaleGeneration
	}
	if session.OwnershipEpoch != pending.SourceEpoch {
		return model.Session{}, false, model.ErrStaleEpoch
	}
	ownerMatches := (session.Kind == model.KindShell && pending.Requester.Kind == model.OwnerTerminal) || (session.Writer != nil && *session.Writer == pending.Requester)
	if !ownerMatches {
		return model.Session{}, false, model.ErrNotOwner
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

func (s *SQLite) GetLifecycleOutcome(ctx context.Context, requester model.Owner, requestID string) (*LifecycleOutcome, error) {
	var outcome LifecycleOutcome
	outcome.Requester, outcome.RequestID = requester, requestID
	err := s.db.QueryRowContext(ctx, `SELECT session_id,operation,mode,source_epoch,source_generation,completed_at_ms,failure
		FROM lifecycle_outcomes WHERE requester_kind=? AND requester_id=? AND request_id=?`, requester.Kind, requester.ID, requestID).
		Scan(&outcome.SessionID, &outcome.Operation, &outcome.Mode, &outcome.SourceEpoch, &outcome.SourceGeneration, &outcome.CompletedAtMS, &outcome.Failure)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &outcome, nil
}

// CompleteLifecycle records a replayable result before removing the barrier.
func (s *SQLite) CompleteLifecycle(ctx context.Context, pending PendingLifecycle) error {
	if err := pending.validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO lifecycle_outcomes
		(requester_kind,requester_id,request_id,session_id,operation,mode,source_epoch,source_generation,completed_at_ms,failure)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(requester_kind,requester_id,request_id) DO NOTHING`, pending.Requester.Kind, pending.Requester.ID,
		pending.RequestID, pending.SessionID, pending.Operation, pending.Mode, pending.SourceEpoch, pending.SourceGeneration, now, "")
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted == 0 {
		var existing LifecycleOutcome
		existing.Requester, existing.RequestID = pending.Requester, pending.RequestID
		if err := tx.QueryRowContext(ctx, `SELECT session_id,operation,mode,source_epoch,source_generation,completed_at_ms,failure
			FROM lifecycle_outcomes WHERE requester_kind=? AND requester_id=? AND request_id=?`, pending.Requester.Kind, pending.Requester.ID, pending.RequestID).
			Scan(&existing.SessionID, &existing.Operation, &existing.Mode, &existing.SourceEpoch, &existing.SourceGeneration, &existing.CompletedAtMS, &existing.Failure); err != nil {
			return err
		}
		if !existing.Matches(pending) {
			return ErrIdempotencyConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_lifecycle_operations WHERE session_id=? AND request_id=?`, pending.SessionID, pending.RequestID); err != nil {
		return err
	}
	return tx.Commit()
}

// FailLifecycleRestart turns a definitive replacement-launch failure into a
// durable negative receipt, restores a usable stopped session, and releases
// the lifecycle barrier. Ambiguous supervisor outcomes never call this path.
func (s *SQLite) FailLifecycleRestart(ctx context.Context, pending PendingLifecycle, failure string) error {
	if pending.Operation != LifecycleRestart || strings.TrimSpace(failure) == "" {
		return fmt.Errorf("invalid failed lifecycle restart")
	}
	if len(failure) > 1024 {
		failure = failure[:1024]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := s.GetPendingLifecycleTx(ctx, tx, pending.SessionID)
	if err != nil || current == nil || current.RequestID != pending.RequestID || current.Phase != LifecycleLaunching {
		return fmt.Errorf("restart failure fencing conflict")
	}
	now := time.Now().UTC().UnixMilli()
	session, getErr := s.GetSessionTx(ctx, tx, pending.SessionID)
	if getErr != nil || session.RuntimeGeneration != pending.SourceGeneration+1 || session.OwnershipEpoch != pending.SourceEpoch ||
		(session.Status != model.StatusRecovering && session.Status != model.StatusStopped) {
		return fmt.Errorf("restart failure session fencing conflict")
	}
	adapter := model.AdapterUnavailable
	if session.Kind == model.KindAgent {
		adapter = model.AdapterUnhealthy
	}
	if session.Status == model.StatusRecovering {
		result, err := tx.ExecContext(ctx, `UPDATE sessions SET status='stopped',task_state='idle',adapter_state=?,exit_success=0,exit_reason=?,updated_at_ms=?
			WHERE session_id=? AND status='recovering' AND ownership_epoch=? AND runtime_generation=?`, adapter, failure, now,
			pending.SessionID, pending.SourceEpoch, pending.SourceGeneration+1)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return fmt.Errorf("restart failure session fencing conflict")
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO lifecycle_outcomes
		(requester_kind,requester_id,request_id,session_id,operation,mode,source_epoch,source_generation,completed_at_ms,failure)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(requester_kind,requester_id,request_id) DO NOTHING`, pending.Requester.Kind, pending.Requester.ID,
		pending.RequestID, pending.SessionID, pending.Operation, pending.Mode, pending.SourceEpoch, pending.SourceGeneration, now, failure)
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted != 1 {
		return fmt.Errorf("restart failure receipt conflict")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_lifecycle_operations WHERE session_id=? AND request_id=?`, pending.SessionID, pending.RequestID); err != nil {
		return err
	}
	return tx.Commit()
}

// BeginLifecycleRestart atomically advances the durable operation and fences
// the replacement runtime with a new generation and recovery identity.
func (s *SQLite) BeginLifecycleRestart(ctx context.Context, pending PendingLifecycle, publicKey []byte) (model.Session, error) {
	if pending.Operation != LifecycleRestart || len(publicKey) != ed25519.PublicKeySize {
		return model.Session{}, fmt.Errorf("invalid lifecycle restart")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Session{}, err
	}
	defer tx.Rollback()
	session, err := s.GetSessionTx(ctx, tx, pending.SessionID)
	if err != nil {
		return model.Session{}, err
	}
	current, err := s.GetPendingLifecycleTx(ctx, tx, pending.SessionID)
	if err != nil || current == nil || current.RequestID != pending.RequestID || current.Phase != LifecycleRuntimeStopped ||
		session.Status != model.StatusStopped || session.RuntimeGeneration != pending.SourceGeneration || session.OwnershipEpoch != pending.SourceEpoch {
		return model.Session{}, fmt.Errorf("restart lifecycle fencing conflict")
	}
	now := time.Now().UTC().UnixMilli()
	adapter := model.AdapterUnavailable
	if session.Kind == model.KindAgent {
		adapter = model.AdapterRecovering
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET status='recovering',runtime_generation=runtime_generation+1,task_state='idle',adapter_state=?,
		recovery_public_key=?,exit_success=NULL,exit_reason='',updated_at_ms=? WHERE session_id=? AND status='stopped' AND ownership_epoch=? AND runtime_generation=?`,
		adapter, publicKey, now, session.ID, pending.SourceEpoch, pending.SourceGeneration)
	if err != nil {
		return model.Session{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return model.Session{}, fmt.Errorf("restart session fencing conflict")
	}
	result, err = tx.ExecContext(ctx, `UPDATE pending_lifecycle_operations SET phase='launching',updated_at_ms=?,attempt=attempt+1,last_error=''
		WHERE session_id=? AND request_id=? AND phase='runtime_stopped'`, now, pending.SessionID, pending.RequestID)
	if err != nil {
		return model.Session{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return model.Session{}, fmt.Errorf("restart phase fencing conflict")
	}
	if err := tx.Commit(); err != nil {
		return model.Session{}, err
	}
	return s.GetSession(ctx, pending.SessionID)
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

func (s *SQLite) DeletePendingLifecycleRequest(ctx context.Context, id model.SessionID, requestID string, phase LifecyclePhase) (bool, error) {
	if requestID == "" || !validLifecyclePhase(phase) {
		return false, fmt.Errorf("invalid lifecycle deletion")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM pending_lifecycle_operations WHERE session_id=? AND request_id=? AND phase=?`, id, requestID, phase)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}
