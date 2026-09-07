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

const (
	LifecycleEnd     LifecycleOperation = "end"
	LifecycleDestroy LifecycleOperation = "destroy"
	LifecycleRestart LifecycleOperation = "restart"

	LifecycleImmediate LifecycleMode = "immediate"
	LifecycleWait      LifecycleMode = "wait"
	LifecycleForce     LifecycleMode = "force"
)

type PendingLifecycle struct {
	SessionID        model.SessionID
	Operation        LifecycleOperation
	Mode             LifecycleMode
	Requester        model.Owner
	SourceEpoch      uint64
	SourceGeneration uint64
	RequestID        string
	CreatedAtMS      int64
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
	_, err = tx.ExecContext(ctx, `INSERT INTO pending_lifecycle_operations
		(session_id,operation,mode,requester_kind,requester_id,source_epoch,source_generation,request_id,created_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?)`, pending.SessionID, pending.Operation, pending.Mode, pending.Requester.Kind, pending.Requester.ID,
		pending.SourceEpoch, pending.SourceGeneration, pending.RequestID, pending.CreatedAtMS)
	if err != nil {
		return model.Session{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.Session{}, false, err
	}
	return session, false, nil
}

func (s *SQLite) GetPendingLifecycle(ctx context.Context, id model.SessionID) (*PendingLifecycle, error) {
	return scanPendingLifecycle(s.db.QueryRowContext(ctx, `SELECT session_id,operation,mode,requester_kind,requester_id,source_epoch,source_generation,request_id,created_at_ms
		FROM pending_lifecycle_operations WHERE session_id=?`, id))
}

func (s *SQLite) GetPendingLifecycleTx(ctx context.Context, tx *sql.Tx, id model.SessionID) (*PendingLifecycle, error) {
	return scanPendingLifecycle(tx.QueryRowContext(ctx, `SELECT session_id,operation,mode,requester_kind,requester_id,source_epoch,source_generation,request_id,created_at_ms
		FROM pending_lifecycle_operations WHERE session_id=?`, id))
}

func scanPendingLifecycle(row interface{ Scan(...any) error }) (*PendingLifecycle, error) {
	var pending PendingLifecycle
	if err := row.Scan(&pending.SessionID, &pending.Operation, &pending.Mode, &pending.Requester.Kind, &pending.Requester.ID,
		&pending.SourceEpoch, &pending.SourceGeneration, &pending.RequestID, &pending.CreatedAtMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &pending, nil
}

func (s *SQLite) DeletePendingLifecycleTx(ctx context.Context, tx *sql.Tx, id model.SessionID) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM pending_lifecycle_operations WHERE session_id=?`, id)
	return err
}

func (s *SQLite) DeletePendingLifecycle(ctx context.Context, id model.SessionID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_lifecycle_operations WHERE session_id=?`, id)
	return err
}
