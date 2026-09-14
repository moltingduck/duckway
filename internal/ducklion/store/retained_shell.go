package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

// RetainedShellSession is a diagnostic index, not a selectable live Session.
// The PTY bytes remain in Ducklion's separate retained-output files.
type RetainedShellSession struct {
	SessionID         model.SessionID
	RuntimeGeneration uint64
	Handle            string
	ExitedAtMS        int64
	ExitSuccess       bool
	ExitReason        string
}

// RetireExitedShell atomically removes a naturally exited root shell from the
// active inventory while retaining its log identity. A pending lifecycle
// operation must continue through MarkRuntimeExited instead, so restart/end
// coordination keeps its existing durable Session row.
func (s *SQLite) RetireExitedShell(ctx context.Context, id model.SessionID, generation uint64, success bool, reason string) (bool, error) {
	if _, err := model.ParseSessionID(string(id)); err != nil || generation == 0 {
		return false, fmt.Errorf("invalid shell runtime identity")
	}
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	session, err := s.GetSessionTx(ctx, tx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A supervisor may retry the exit receipt after the transaction
			// committed but before its acknowledgement reached the peer.
			var exists int
			if lookupErr := tx.QueryRowContext(ctx, `SELECT 1 FROM retained_shell_sessions WHERE session_id=? AND runtime_generation=?`, id, generation).Scan(&exists); lookupErr == nil {
				return true, nil
			} else if !errors.Is(lookupErr, sql.ErrNoRows) {
				return false, lookupErr
			}
		}
		return false, err
	}
	if session.Kind != model.KindShell || session.RuntimeGeneration != generation ||
		(session.Status != model.StatusRunning && session.Status != model.StatusRecovering) {
		return false, fmt.Errorf("shell runtime exit fencing conflict")
	}
	pending, err := s.GetPendingLifecycleTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if pending != nil {
		return false, nil
	}
	exitedAt := time.Now().UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO retained_shell_sessions
		(session_id,runtime_generation,handle,exited_at_ms,exit_success,exit_reason)
		VALUES(?,?,?,?,?,?)`, id, generation, session.Handle, exitedAt, success, reason); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM sessions
		WHERE session_id=? AND kind='shell' AND runtime_generation=? AND status IN ('running','recovering')`, id, generation)
	if err != nil {
		return false, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return false, fmt.Errorf("shell runtime exit fencing conflict")
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// CompleteEndedShell commits an explicit Shell End as one durable transition:
// the replayable receipt, retained-output identity, lifecycle barrier removal,
// and active inventory deletion either all happen or none do. Restart and
// Agent End continue to use their existing stopped-session lifecycle.
func (s *SQLite) CompleteEndedShell(ctx context.Context, pending PendingLifecycle) error {
	if err := pending.validate(); err != nil {
		return err
	}
	if pending.Operation != LifecycleEnd {
		return fmt.Errorf("not a shell end lifecycle")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := s.GetPendingLifecycleTx(ctx, tx, pending.SessionID)
	if err != nil {
		return err
	}
	if current == nil {
		// A successful commit may be replayed after the acknowledgement was
		// lost. Require both the exact receipt and the retained identity.
		var outcome LifecycleOutcome
		outcome.Requester, outcome.RequestID = pending.Requester, pending.RequestID
		err := tx.QueryRowContext(ctx, `SELECT session_id,operation,mode,source_epoch,source_generation,completed_at_ms,failure
			FROM lifecycle_outcomes WHERE requester_kind=? AND requester_id=? AND request_id=?`, pending.Requester.Kind, pending.Requester.ID, pending.RequestID).
			Scan(&outcome.SessionID, &outcome.Operation, &outcome.Mode, &outcome.SourceEpoch, &outcome.SourceGeneration, &outcome.CompletedAtMS, &outcome.Failure)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("shell end lifecycle fencing conflict")
		}
		if err != nil {
			return err
		}
		if !outcome.Matches(pending) || outcome.Failure != "" {
			return ErrIdempotencyConflict
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM retained_shell_sessions WHERE session_id=? AND runtime_generation=?`, pending.SessionID, pending.SourceGeneration).Scan(&exists); err != nil {
			return fmt.Errorf("shell end retained identity missing: %w", err)
		}
		return nil
	}
	if current.Operation != LifecycleEnd || current.Mode != pending.Mode || current.Requester != pending.Requester ||
		current.SourceEpoch != pending.SourceEpoch || current.SourceGeneration != pending.SourceGeneration ||
		current.RequestID != pending.RequestID || current.Phase != LifecycleRuntimeStopped {
		return fmt.Errorf("shell end lifecycle fencing conflict")
	}
	session, err := s.GetSessionTx(ctx, tx, pending.SessionID)
	if err != nil {
		return err
	}
	if session.Kind != model.KindShell || session.Status != model.StatusStopped || session.ExitSuccess == nil ||
		session.RuntimeGeneration != pending.SourceGeneration || session.OwnershipEpoch != pending.SourceEpoch {
		return fmt.Errorf("shell end session fencing conflict")
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO retained_shell_sessions
		(session_id,runtime_generation,handle,exited_at_ms,exit_success,exit_reason) VALUES(?,?,?,?,?,?)`,
		pending.SessionID, pending.SourceGeneration, session.Handle, now, *session.ExitSuccess, session.ExitReason); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO lifecycle_outcomes
		(requester_kind,requester_id,request_id,session_id,operation,mode,source_epoch,source_generation,completed_at_ms,failure)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(requester_kind,requester_id,request_id) DO NOTHING`,
		pending.Requester.Kind, pending.Requester.ID, pending.RequestID, pending.SessionID, pending.Operation, pending.Mode,
		pending.SourceEpoch, pending.SourceGeneration, now, "")
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted != 1 {
		return ErrIdempotencyConflict
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM pending_lifecycle_operations
		WHERE session_id=? AND operation='end' AND request_id=? AND source_epoch=? AND source_generation=? AND phase='runtime_stopped'`,
		pending.SessionID, pending.RequestID, pending.SourceEpoch, pending.SourceGeneration)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("shell end lifecycle fencing conflict")
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM sessions
		WHERE session_id=? AND kind='shell' AND status='stopped' AND ownership_epoch=? AND runtime_generation=?`,
		pending.SessionID, pending.SourceEpoch, pending.SourceGeneration)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("shell end session fencing conflict")
	}
	return tx.Commit()
}

func (s *SQLite) GetRetainedShellSession(ctx context.Context, id model.SessionID, generation uint64) (RetainedShellSession, error) {
	return scanRetainedShellSession(s.db.QueryRowContext(ctx, `SELECT session_id,runtime_generation,handle,exited_at_ms,exit_success,exit_reason
		FROM retained_shell_sessions WHERE session_id=? AND runtime_generation=?`, id, generation))
}

// ListRetainedShellSessions is bounded for user-facing diagnostics. A zero
// limit selects all rows for Ducklion's internal retention sweeper.
func (s *SQLite) ListRetainedShellSessions(ctx context.Context, limit int) ([]RetainedShellSession, error) {
	if limit < 0 || limit > 512 {
		return nil, fmt.Errorf("retained shell list limit must be 0..512")
	}
	query := `SELECT session_id,runtime_generation,handle,exited_at_ms,exit_success,exit_reason
		FROM retained_shell_sessions ORDER BY exited_at_ms DESC,session_id,runtime_generation DESC`
	var args []any
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RetainedShellSession, 0)
	for rows.Next() {
		item, err := scanRetainedShellSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// DeleteRetainedShellSession removes only the diagnostic index; callers must
// separately apply the retained-output file expiry policy.
func (s *SQLite) DeleteRetainedShellSession(ctx context.Context, id model.SessionID, generation uint64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM retained_shell_sessions WHERE session_id=? AND runtime_generation=?`, id, generation)
	return err
}

type retainedShellScanner interface{ Scan(...any) error }

func scanRetainedShellSession(row retainedShellScanner) (RetainedShellSession, error) {
	var item RetainedShellSession
	var success int
	err := row.Scan(&item.SessionID, &item.RuntimeGeneration, &item.Handle, &item.ExitedAtMS, &success, &item.ExitReason)
	if errors.Is(err, sql.ErrNoRows) {
		return RetainedShellSession{}, ErrNotFound
	}
	if err != nil {
		return RetainedShellSession{}, err
	}
	item.ExitSuccess = success != 0
	return item, nil
}
