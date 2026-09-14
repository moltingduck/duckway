package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

// RecordActivity durably advances a category cursor and appends its projection
// invalidation in the same transaction. A positive coalesceWindow suppresses
// additional advances within that window.
func (s *SQLite) RecordActivity(ctx context.Context, sessionID model.SessionID, category model.NotificationCategory, coalesceWindow time.Duration) (uint64, bool, error) {
	return s.recordActivity(ctx, sessionID, category, coalesceWindow, 0, 0, 0, "", false, false)
}

func (s *SQLite) RecordActivityAtOffset(ctx context.Context, sessionID model.SessionID, category model.NotificationCategory, coalesceWindow time.Duration, runtimeGeneration, sourceOffset uint64) (uint64, bool, error) {
	if runtimeGeneration == 0 || sourceOffset == 0 {
		return 0, false, errors.New("activity runtime generation and source offset must be positive")
	}
	return s.recordActivity(ctx, sessionID, category, coalesceWindow, runtimeGeneration, sourceOffset, 0, "", true, false)
}

func (s *SQLite) RecordAgentActivity(ctx context.Context, sessionID model.SessionID, category model.NotificationCategory, runtimeGeneration, eventID, sourceOffset uint64) (uint64, bool, error) {
	return s.RecordAgentActivityWithSource(ctx, sessionID, category, runtimeGeneration, eventID, sourceOffset, "")
}

func (s *SQLite) RecordAgentActivityWithSource(ctx context.Context, sessionID model.SessionID, category model.NotificationCategory, runtimeGeneration, eventID, sourceOffset uint64, source string) (uint64, bool, error) {
	if runtimeGeneration == 0 || eventID == 0 {
		return 0, false, errors.New("agent activity runtime generation and event id must be positive")
	}
	if source != "" && source != "codex" && source != "claude" {
		return 0, false, errors.New("unsupported agent hook callback source")
	}
	return s.recordActivity(ctx, sessionID, category, 0, runtimeGeneration, sourceOffset, eventID, source, false, true)
}

type HookCallback struct {
	SessionID         model.SessionID
	RuntimeGeneration uint64
	LastEventID       uint64
	UpdatedAtMS       int64
}

// LastHookCallback reports the newest admitted callback on this Host. The
// source is self-declared by the Session process tree,
// so this is observability, never agent identity or authorization evidence.
func (s *SQLite) LastHookCallback(ctx context.Context, source string) (HookCallback, bool, error) {
	if source != "codex" && source != "claude" {
		return HookCallback{}, false, errors.New("unsupported agent hook callback source")
	}
	var callback HookCallback
	err := s.db.QueryRowContext(ctx, `SELECT session_id,runtime_generation,last_event_id,updated_at_ms
		FROM host_hook_callbacks WHERE source=?`, source).
		Scan(&callback.SessionID, &callback.RuntimeGeneration, &callback.LastEventID, &callback.UpdatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return HookCallback{}, false, nil
	}
	return callback, err == nil, err
}

func (s *SQLite) recordActivity(ctx context.Context, sessionID model.SessionID, category model.NotificationCategory, coalesceWindow time.Duration, runtimeGeneration, sourceOffset, sourceEventID uint64, source string, fenceSource, fenceEvent bool) (uint64, bool, error) {
	if _, err := model.ParseSessionID(string(sessionID)); err != nil {
		return 0, false, err
	}
	if err := category.Validate(); err != nil {
		return 0, false, err
	}
	now := time.Now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE session_id=?`, sessionID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrNotFound
	} else if err != nil {
		return 0, false, err
	}
	var sequence uint64
	var lastRuntimeGeneration uint64
	var lastSourceOffset uint64
	var lastSourceEventID uint64
	var updatedAt int64
	err = tx.QueryRowContext(ctx, `SELECT sequence,source_runtime_generation,last_source_offset,last_source_event_id,updated_at_ms FROM session_activity WHERE session_id=? AND category=?`, sessionID, category).Scan(&sequence, &lastRuntimeGeneration, &lastSourceOffset, &lastSourceEventID, &updatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if fenceEvent {
		var globalGeneration, globalEventID uint64
		globalErr := tx.QueryRowContext(ctx, `SELECT source_runtime_generation,last_source_event_id FROM session_activity WHERE session_id=? AND last_source_event_id>0 ORDER BY source_runtime_generation DESC,last_source_event_id DESC LIMIT 1`, sessionID).Scan(&globalGeneration, &globalEventID)
		if globalErr != nil && !errors.Is(globalErr, sql.ErrNoRows) {
			return 0, false, globalErr
		}
		if globalErr == nil && (runtimeGeneration < globalGeneration || runtimeGeneration == globalGeneration && sourceEventID <= globalEventID) {
			return sequence, false, nil
		}
	}
	if err == nil && fenceSource && (runtimeGeneration < lastRuntimeGeneration || runtimeGeneration == lastRuntimeGeneration && sourceOffset <= lastSourceOffset) {
		return sequence, false, nil
	}
	if err == nil && fenceEvent && (runtimeGeneration < lastRuntimeGeneration || runtimeGeneration == lastRuntimeGeneration && sourceEventID <= lastSourceEventID) {
		return sequence, false, nil
	}
	generationChanged := (fenceSource || fenceEvent) && runtimeGeneration > lastRuntimeGeneration
	if err == nil && !generationChanged && coalesceWindow > 0 && now-updatedAt < coalesceWindow.Milliseconds() {
		if fenceSource {
			if _, err := tx.ExecContext(ctx, `UPDATE session_activity SET source_runtime_generation=?,last_source_offset=? WHERE session_id=? AND category=?`, runtimeGeneration, sourceOffset, sessionID, category); err != nil {
				return 0, false, err
			}
			if err := tx.Commit(); err != nil {
				return 0, false, err
			}
		}
		return sequence, false, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		sequence = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO session_activity(session_id,category,sequence,source_runtime_generation,last_source_offset,last_source_event_id,updated_at_ms) VALUES(?,?,?,?,?,?,?)`, sessionID, category, sequence, runtimeGeneration, sourceOffset, sourceEventID, now)
	} else {
		sequence++
		_, err = tx.ExecContext(ctx, `UPDATE session_activity SET sequence=?,source_runtime_generation=?,last_source_offset=?,last_source_event_id=?,updated_at_ms=? WHERE session_id=? AND category=?`, sequence, runtimeGeneration, sourceOffset, sourceEventID, now, sessionID, category)
	}
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_revision_events(session_id,change_kind,created_at_ms,activity_category,activity_sequence) VALUES(?,'invalidate',?,?,?)`, sessionID, now, category, sequence); err != nil {
		return 0, false, err
	}
	if source != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO host_hook_callbacks(source,session_id,runtime_generation,last_event_id,updated_at_ms) VALUES(?,?,?,?,?)
			ON CONFLICT(source) DO UPDATE SET session_id=excluded.session_id,runtime_generation=excluded.runtime_generation,last_event_id=excluded.last_event_id,updated_at_ms=excluded.updated_at_ms`,
			source, sessionID, runtimeGeneration, sourceEventID, now); err != nil {
			return 0, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_revision_events WHERE revision <= (SELECT max(revision)-4096 FROM session_revision_events)`); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return sequence, true, nil
}

func activityForSessionsTx(ctx context.Context, tx *sql.Tx) (map[model.SessionID]map[model.NotificationCategory]uint64, map[model.SessionID]map[model.NotificationCategory]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id,category,sequence,updated_at_ms FROM session_activity ORDER BY session_id,category`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	result := make(map[model.SessionID]map[model.NotificationCategory]uint64)
	timestamps := make(map[model.SessionID]map[model.NotificationCategory]int64)
	for rows.Next() {
		var sessionID model.SessionID
		var category model.NotificationCategory
		var sequence uint64
		var updatedAtMS int64
		if err := rows.Scan(&sessionID, &category, &sequence, &updatedAtMS); err != nil {
			return nil, nil, err
		}
		if err := category.Validate(); err != nil {
			return nil, nil, err
		}
		if result[sessionID] == nil {
			result[sessionID] = make(map[model.NotificationCategory]uint64)
		}
		result[sessionID][category] = sequence
		if timestamps[sessionID] == nil {
			timestamps[sessionID] = make(map[model.NotificationCategory]int64)
		}
		timestamps[sessionID][category] = updatedAtMS
	}
	return result, timestamps, rows.Err()
}

func recordActivityTx(ctx context.Context, tx *sql.Tx, sessionID model.SessionID, category model.NotificationCategory, now int64) (uint64, error) {
	if err := category.Validate(); err != nil {
		return 0, err
	}
	var sequence uint64
	err := tx.QueryRowContext(ctx, `SELECT sequence FROM session_activity WHERE session_id=? AND category=?`, sessionID, category).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		sequence = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO session_activity(session_id,category,sequence,source_runtime_generation,last_source_offset,updated_at_ms) VALUES(?,?,?,0,0,?)`, sessionID, category, sequence, now)
	} else if err == nil {
		sequence++
		_, err = tx.ExecContext(ctx, `UPDATE session_activity SET sequence=?,updated_at_ms=? WHERE session_id=? AND category=?`, sequence, now, sessionID, category)
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_revision_events(session_id,change_kind,created_at_ms,activity_category,activity_sequence) VALUES(?,'invalidate',?,?,?)`, sessionID, now, category, sequence); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM session_revision_events WHERE revision <= (SELECT max(revision)-4096 FROM session_revision_events)`)
	return sequence, err
}
