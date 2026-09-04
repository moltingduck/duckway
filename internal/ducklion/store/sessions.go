package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

func (s *SQLite) CreateSessionIdempotent(ctx context.Context, principal, requestID string, fingerprint [32]byte, session model.Session) (model.SessionID, bool, error) {
	key := MutationKey{Principal: principal, RequestID: requestID, Operation: "create_session", Fingerprint: fingerprint}
	result, err := s.RunMutation(ctx, key, func(tx *sql.Tx) (json.RawMessage, error) {
		if err := s.InsertSessionTx(ctx, tx, session); err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			SessionID model.SessionID `json:"session_id"`
		}{session.ID})
	})
	if err != nil {
		return "", false, err
	}
	var value struct {
		SessionID model.SessionID `json:"session_id"`
	}
	if err := json.Unmarshal(result.JSON, &value); err != nil {
		return "", false, err
	}
	return value.SessionID, result.Replayed, nil
}

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
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET status='running',adapter_state=CASE kind WHEN 'agent' THEN 'healthy' ELSE 'unavailable' END,updated_at_ms=?
		WHERE session_id=? AND runtime_generation=? AND status IN ('recovering','running') AND
		((kind='agent' AND adapter_state IN ('recovering','healthy')) OR (kind='shell' AND adapter_state='unavailable'))`, time.Now().UTC().UnixMilli(), id, generation)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("runtime connection fencing conflict")
	}
	return nil
}

func (s *SQLite) MarkRuntimeDisconnected(ctx context.Context, id model.SessionID, generation uint64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET status='recovering',adapter_state=CASE kind WHEN 'agent' THEN 'recovering' ELSE 'unavailable' END,updated_at_ms=?
		WHERE session_id=? AND runtime_generation=? AND status='running' AND
		((kind='agent' AND adapter_state='healthy') OR (kind='shell' AND adapter_state='unavailable'))`, time.Now().UTC().UnixMilli(), id, generation)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows > 1 {
		return fmt.Errorf("runtime disconnection updated multiple sessions")
	}
	return nil
}

// PrepareRuntimeRecovery clears connection-derived state after the daemon has
// acquired its singleton lock. Independent supervisors keep running and prove
// possession of their recovery keys before becoming healthy again.
func (s *SQLite) PrepareRuntimeRecovery(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET status='recovering',adapter_state=CASE kind WHEN 'agent' THEN 'recovering' ELSE 'unavailable' END,updated_at_ms=?
		WHERE status='running'`, time.Now().UTC().UnixMilli())
	return err
}

func (s *SQLite) MarkRuntimeStopped(ctx context.Context, id model.SessionID, generation uint64) error {
	return s.MarkRuntimeExited(ctx, id, generation, false, "runtime launch or stop failed")
}

func (s *SQLite) MarkRuntimeExited(ctx context.Context, id model.SessionID, generation uint64, success bool, reason string) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	session, err := s.GetSessionTx(ctx, tx, id)
	if err != nil || session.RuntimeGeneration != generation || session.Status != model.StatusRunning && session.Status != model.StatusRecovering {
		return fmt.Errorf("runtime stop fencing conflict")
	}
	pending, err := s.GetPendingYieldTx(ctx, tx, id)
	if err != nil {
		return err
	}
	oldEpoch := session.OwnershipEpoch
	session.TaskState = model.TaskIdle
	if pending != nil {
		if _, err := session.ApplyPendingYield(*pending); err != nil && !errors.Is(err, model.ErrStaleEpoch) {
			return err
		}
	}
	session.Status = model.StatusStopped
	if session.Kind == model.KindAgent {
		session.AdapterState = model.AdapterUnhealthy
	} else {
		session.AdapterState = model.AdapterUnavailable
	}
	session.ExitSuccess = &success
	session.ExitReason = reason
	session.UpdatedAtMS = time.Now().UTC().UnixMilli()
	var writerKind, writerID any
	if session.Writer != nil {
		writerKind, writerID = session.Writer.Kind, session.Writer.ID
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET status=?,writer_kind=?,writer_id=?,ownership_epoch=?,task_state=?,adapter_state=?,exit_success=?,exit_reason=?,updated_at_ms=?
		WHERE session_id=? AND ownership_epoch=? AND runtime_generation=? AND status IN ('running','recovering')`, session.Status, writerKind, writerID,
		session.OwnershipEpoch, session.TaskState, session.AdapterState, success, reason, session.UpdatedAtMS, id, oldEpoch, generation)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("runtime stop fencing conflict")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE managed_tasks SET status='failed',error_category='runtime_exit',updated_at_ms=?
		WHERE session_id=? AND runtime_generation=? AND status IN ('prepared','running','replying')`, session.UpdatedAtMS, id, generation); err != nil {
		return err
	}
	if pending != nil {
		if err := s.DeletePendingYieldTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		(session_id,handle,kind,agent_type,cwd,shell,status,writer_kind,writer_id,ownership_epoch,runtime_generation,task_state,adapter_state,recovery_public_key,created_at_ms,updated_at_ms,exit_success,exit_reason)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, session.ID, session.Handle, session.Kind, session.AgentType, session.CWD, session.Shell,
		session.Status, writerKind, writerID, session.OwnershipEpoch, session.RuntimeGeneration, session.TaskState, session.AdapterState,
		session.RecoveryPublicKey, session.CreatedAtMS, session.UpdatedAtMS, session.ExitSuccess, session.ExitReason)
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

const sessionSelect = `SELECT session_id,handle,kind,agent_type,cwd,shell,status,writer_kind,writer_id,ownership_epoch,runtime_generation,task_state,adapter_state,recovery_public_key,created_at_ms,updated_at_ms,exit_success,exit_reason FROM sessions`

type rowScanner interface{ Scan(...any) error }

func scanSession(row rowScanner) (model.Session, error) {
	var session model.Session
	var writerKind, writerID sql.NullString
	var exitSuccess sql.NullBool
	err := row.Scan(&session.ID, &session.Handle, &session.Kind, &session.AgentType, &session.CWD, &session.Shell, &session.Status,
		&writerKind, &writerID, &session.OwnershipEpoch, &session.RuntimeGeneration, &session.TaskState, &session.AdapterState,
		&session.RecoveryPublicKey, &session.CreatedAtMS, &session.UpdatedAtMS, &exitSuccess, &session.ExitReason)
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
	if exitSuccess.Valid {
		value := exitSuccess.Bool
		session.ExitSuccess = &value
	}
	if err := session.Validate(); err != nil {
		return model.Session{}, fmt.Errorf("invalid persisted session %s: %w", session.ID, err)
	}
	return session, nil
}
