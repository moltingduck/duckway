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
