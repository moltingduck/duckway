package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

type AuditEvent struct {
	SessionID         model.SessionID
	Principal         string
	EventType         string
	OwnershipEpoch    uint64
	RuntimeGeneration uint64
	OutcomeCode       string
	CreatedAtMS       int64
}

func (s *SQLite) MarkRuntimeConnected(ctx context.Context, id model.SessionID, generation uint64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET status='running',adapter_state='healthy',updated_at_ms=?
        WHERE session_id=? AND runtime_generation=? AND status='recovering' AND adapter_state='recovering'`, time.Now().UTC().UnixMilli(), id, generation)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("runtime connection fencing conflict")
	}
	return nil
}

func (s *SQLite) MarkRuntimeDisconnected(ctx context.Context, id model.SessionID, generation uint64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET status='recovering',adapter_state='recovering',updated_at_ms=?
        WHERE session_id=? AND runtime_generation=? AND status='running' AND adapter_state='healthy'`, time.Now().UTC().UnixMilli(), id, generation)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows > 1 {
		return fmt.Errorf("runtime disconnection updated multiple sessions")
	}
	return nil
}

func (s *SQLite) InsertAuditTx(ctx context.Context, tx *sql.Tx, event AuditEvent) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_events(session_id,principal,event_type,ownership_epoch,runtime_generation,outcome_code,created_at_ms) VALUES(?,?,?,?,?,?,?)`,
		event.SessionID, event.Principal, event.EventType, event.OwnershipEpoch, event.RuntimeGeneration, event.OutcomeCode, event.CreatedAtMS)
	return err
}

func (s *SQLite) GetSession(ctx context.Context, id model.SessionID) (model.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, sessionSelect+` WHERE session_id=?`, id))
}

func (s *SQLite) ListSessions(ctx context.Context) ([]model.Session, error) {
	rows, err := s.db.QueryContext(ctx, sessionSelect+` ORDER BY created_at_ms,session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []model.Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *SQLite) GetSessionTx(ctx context.Context, tx *sql.Tx, id model.SessionID) (model.Session, error) {
	return scanSession(tx.QueryRowContext(ctx, sessionSelect+` WHERE session_id=?`, id))
}

func (s *SQLite) InsertSessionTx(ctx context.Context, tx *sql.Tx, session model.Session) error {
	if err := session.Validate(); err != nil {
		return err
	}
	var writerKind, writerID any
	if session.Writer != nil {
		writerKind, writerID = session.Writer.Kind, session.Writer.ID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions
        (session_id,handle,kind,agent_type,cwd,shell,status,writer_kind,writer_id,ownership_epoch,runtime_generation,task_state,adapter_state,recovery_public_key,created_at_ms,updated_at_ms)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, session.ID, session.Handle, session.Kind, session.AgentType, session.CWD, session.Shell,
		session.Status, writerKind, writerID, session.OwnershipEpoch, session.RuntimeGeneration, session.TaskState, session.AdapterState,
		session.RecoveryPublicKey, session.CreatedAtMS, session.UpdatedAtMS)
	return err
}

func (s *SQLite) UpdateSessionTx(ctx context.Context, tx *sql.Tx, session model.Session, expectedEpoch, expectedGeneration uint64) error {
	if err := session.Validate(); err != nil {
		return err
	}
	var writerKind, writerID any
	if session.Writer != nil {
		writerKind, writerID = session.Writer.Kind, session.Writer.ID
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET status=?,writer_kind=?,writer_id=?,ownership_epoch=?,runtime_generation=?,task_state=?,adapter_state=?,recovery_public_key=?,updated_at_ms=?
        WHERE session_id=? AND ownership_epoch=? AND runtime_generation=?`, session.Status, writerKind, writerID, session.OwnershipEpoch,
		session.RuntimeGeneration, session.TaskState, session.AdapterState, session.RecoveryPublicKey, session.UpdatedAtMS, session.ID, expectedEpoch, expectedGeneration)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return fmt.Errorf("session fencing conflict")
	}
	return nil
}

func (s *SQLite) GetPendingYieldTx(ctx context.Context, tx *sql.Tx, id model.SessionID) (*model.PendingYield, error) {
	var pending model.PendingYield
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT session_id,requester_kind,requester_id,source_epoch,request_id,created_at_ms FROM pending_yields WHERE session_id=?`, id).
		Scan(&pending.SessionID, &kind, &pending.Requester.ID, &pending.SourceEpoch, &pending.RequestID, &pending.CreatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pending.Requester.Kind = model.OwnerKind(kind)
	return &pending, nil
}

func (s *SQLite) InsertPendingYieldTx(ctx context.Context, tx *sql.Tx, pending model.PendingYield) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO pending_yields(session_id,requester_kind,requester_id,source_epoch,request_id,created_at_ms) VALUES(?,?,?,?,?,?)`,
		pending.SessionID, pending.Requester.Kind, pending.Requester.ID, pending.SourceEpoch, pending.RequestID, pending.CreatedAtMS)
	return err
}

func (s *SQLite) DeletePendingYieldTx(ctx context.Context, tx *sql.Tx, id model.SessionID) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM pending_yields WHERE session_id=?`, id)
	return err
}

const sessionSelect = `SELECT session_id,handle,kind,agent_type,cwd,shell,status,writer_kind,writer_id,ownership_epoch,runtime_generation,task_state,adapter_state,recovery_public_key,created_at_ms,updated_at_ms FROM sessions`

type rowScanner interface{ Scan(...any) error }

func scanSession(row rowScanner) (model.Session, error) {
	var session model.Session
	var writerKind, writerID sql.NullString
	err := row.Scan(&session.ID, &session.Handle, &session.Kind, &session.AgentType, &session.CWD, &session.Shell, &session.Status,
		&writerKind, &writerID, &session.OwnershipEpoch, &session.RuntimeGeneration, &session.TaskState, &session.AdapterState,
		&session.RecoveryPublicKey, &session.CreatedAtMS, &session.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Session{}, ErrNotFound
	}
	if err != nil {
		return model.Session{}, err
	}
	if writerKind.Valid || writerID.Valid {
		if !writerKind.Valid || !writerID.Valid {
			return model.Session{}, fmt.Errorf("invalid partial writer in persisted session")
		}
		session.Writer = &model.Owner{Kind: model.OwnerKind(writerKind.String), ID: writerID.String}
	}
	if err := session.Validate(); err != nil {
		return model.Session{}, fmt.Errorf("invalid persisted session %s: %w", session.ID, err)
	}
	return session, nil
}
