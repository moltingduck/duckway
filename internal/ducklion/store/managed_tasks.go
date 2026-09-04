package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

type ManagedTaskStatus string

const (
	ManagedTaskPrepared  ManagedTaskStatus = "prepared"
	ManagedTaskRunning   ManagedTaskStatus = "running"
	ManagedTaskReplying  ManagedTaskStatus = "replying"
	ManagedTaskCompleted ManagedTaskStatus = "completed"
	ManagedTaskFailed    ManagedTaskStatus = "failed"
)

type ManagedTask struct {
	SessionID         model.SessionID
	TaskID            string
	PromptDigest      [32]byte
	Owner             model.Owner
	OwnershipEpoch    uint64
	RuntimeGeneration uint64
	Status            ManagedTaskStatus
	LastEventSeq      uint64
	OutputStart       uint64
	OutputEnd         *uint64
	ErrorCategory     string
	CreatedAtMS       int64
	UpdatedAtMS       int64
}

// PrepareManagedTask persists only correlation and fence metadata. Prompt
// bytes remain in Duckway's durable inbox and the supervisor's bounded memory.
func (s *SQLite) PrepareManagedTask(ctx context.Context, task ManagedTask) (ManagedTask, bool, error) {
	if task.Owner.Kind != model.OwnerCC || task.Owner.ID == "" || !protocol.ValidTaskID(task.TaskID) || task.OwnershipEpoch == 0 || task.RuntimeGeneration == 0 {
		return ManagedTask{}, false, fmt.Errorf("invalid managed task metadata")
	}
	now := time.Now().UTC().UnixMilli()
	if task.CreatedAtMS == 0 {
		task.CreatedAtMS = now
	}
	task.UpdatedAtMS = now
	task.Status = ManagedTaskPrepared
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ManagedTask{}, false, err
	}
	defer tx.Rollback()
	if existing, getErr := getManagedTaskTx(ctx, tx, task.SessionID, task.TaskID); getErr == nil {
		if existing.PromptDigest != task.PromptDigest || existing.Owner != task.Owner || existing.OwnershipEpoch != task.OwnershipEpoch || existing.RuntimeGeneration != task.RuntimeGeneration {
			return ManagedTask{}, true, ErrIdempotencyConflict
		}
		return existing, true, nil
	} else if !errors.Is(getErr, ErrNotFound) {
		return ManagedTask{}, false, getErr
	}
	if err := s.validateManagedTaskTx(ctx, tx, task); err != nil {
		return ManagedTask{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO managed_tasks(
		session_id,task_id,prompt_digest,owner_kind,owner_id,ownership_epoch,runtime_generation,status,last_event_seq,output_start,created_at_ms,updated_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, task.SessionID, task.TaskID, task.PromptDigest[:], task.Owner.Kind, task.Owner.ID,
		task.OwnershipEpoch, task.RuntimeGeneration, task.Status, task.LastEventSeq, task.OutputStart, task.CreatedAtMS, task.UpdatedAtMS)
	if err != nil {
		return ManagedTask{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET task_state='running',updated_at_ms=?
		WHERE session_id=? AND ownership_epoch=? AND runtime_generation=? AND task_state='idle'`, now, task.SessionID, task.OwnershipEpoch, task.RuntimeGeneration)
	if err != nil {
		return ManagedTask{}, false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ManagedTask{}, false, model.ErrTaskActive
	}
	if err := tx.Commit(); err != nil {
		return ManagedTask{}, false, err
	}
	return task, false, nil
}

func (s *SQLite) ValidateManagedTask(ctx context.Context, task ManagedTask) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if existing, getErr := getManagedTaskTx(ctx, tx, task.SessionID, task.TaskID); getErr == nil {
		if existing.PromptDigest == task.PromptDigest && existing.Owner == task.Owner && existing.OwnershipEpoch == task.OwnershipEpoch && existing.RuntimeGeneration == task.RuntimeGeneration {
			return nil
		}
		return ErrIdempotencyConflict
	} else if !errors.Is(getErr, ErrNotFound) {
		return getErr
	}
	return s.validateManagedTaskTx(ctx, tx, task)
}

func (s *SQLite) validateManagedTaskTx(ctx context.Context, tx *sql.Tx, task ManagedTask) error {
	session, err := s.GetSessionTx(ctx, tx, task.SessionID)
	if err != nil {
		return err
	}
	if err := session.AuthorizeAgentInput(task.Owner, task.OwnershipEpoch, task.RuntimeGeneration); err != nil {
		return err
	}
	if session.AdapterState != model.AdapterHealthy {
		return model.ErrAdapterNotHealthy
	}
	var boundHandle string
	if err := tx.QueryRowContext(ctx, `SELECT channel_handle FROM discord_bindings WHERE session_id=?`, task.SessionID).Scan(&boundHandle); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if boundHandle != task.Owner.ID {
		return model.ErrNotOwner
	}
	if session.TaskState != model.TaskIdle {
		return model.ErrTaskActive
	}
	pending, err := s.GetPendingYieldTx(ctx, tx, task.SessionID)
	if err != nil {
		return err
	}
	if pending != nil {
		return model.ErrPendingYield
	}
	return nil
}

func (s *SQLite) FailManagedTask(ctx context.Context, sessionID model.SessionID, taskID, category string, generation uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE managed_tasks SET status='failed',error_category=?,updated_at_ms=?
		WHERE session_id=? AND task_id=? AND runtime_generation=? AND status='prepared'`, category, time.Now().UTC().UnixMilli(), sessionID, taskID, generation)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrNotFound
	}
	result, err = tx.ExecContext(ctx, `UPDATE sessions SET task_state='idle',updated_at_ms=? WHERE session_id=? AND runtime_generation=? AND task_state='running'`, time.Now().UTC().UnixMilli(), sessionID, generation)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *SQLite) GetManagedTask(ctx context.Context, sessionID model.SessionID, taskID string) (ManagedTask, error) {
	return scanManagedTask(s.db.QueryRowContext(ctx, managedTaskSelect+` WHERE session_id=? AND task_id=?`, sessionID, taskID))
}

func (s *SQLite) MarkManagedTaskRunning(ctx context.Context, sessionID model.SessionID, taskID string, generation uint64) (ManagedTask, error) {
	now := time.Now().UTC().UnixMilli()
	result, err := s.db.ExecContext(ctx, `UPDATE managed_tasks SET status='running',updated_at_ms=?
		WHERE session_id=? AND task_id=? AND runtime_generation=? AND status IN ('prepared','running')`, now, sessionID, taskID, generation)
	if err != nil {
		return ManagedTask{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ManagedTask{}, ErrNotFound
	}
	return s.GetManagedTask(ctx, sessionID, taskID)
}

const managedTaskSelect = `SELECT session_id,task_id,prompt_digest,owner_kind,owner_id,ownership_epoch,runtime_generation,status,
	last_event_seq,output_start,output_end,error_category,created_at_ms,updated_at_ms FROM managed_tasks`

func getManagedTaskTx(ctx context.Context, tx *sql.Tx, sessionID model.SessionID, taskID string) (ManagedTask, error) {
	return scanManagedTask(tx.QueryRowContext(ctx, managedTaskSelect+` WHERE session_id=? AND task_id=?`, sessionID, taskID))
}

func scanManagedTask(row interface{ Scan(...any) error }) (ManagedTask, error) {
	var task ManagedTask
	var digest []byte
	var ownerKind string
	var outputEnd sql.NullInt64
	if err := row.Scan(&task.SessionID, &task.TaskID, &digest, &ownerKind, &task.Owner.ID, &task.OwnershipEpoch, &task.RuntimeGeneration,
		&task.Status, &task.LastEventSeq, &task.OutputStart, &outputEnd, &task.ErrorCategory, &task.CreatedAtMS, &task.UpdatedAtMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ManagedTask{}, ErrNotFound
		}
		return ManagedTask{}, err
	}
	if len(digest) != len(task.PromptDigest) {
		return ManagedTask{}, fmt.Errorf("invalid managed task digest")
	}
	copy(task.PromptDigest[:], digest)
	task.Owner.Kind = model.OwnerKind(ownerKind)
	if outputEnd.Valid {
		value := uint64(outputEnd.Int64)
		task.OutputEnd = &value
	}
	return task, nil
}
