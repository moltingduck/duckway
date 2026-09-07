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
	return s.recordActivity(ctx, sessionID, category, coalesceWindow, 0, 0, false)
}

func (s *SQLite) RecordActivityAtOffset(ctx context.Context, sessionID model.SessionID, category model.NotificationCategory, coalesceWindow time.Duration, runtimeGeneration, sourceOffset uint64) (uint64, bool, error) {
	if runtimeGeneration == 0 || sourceOffset == 0 {
		return 0, false, errors.New("activity runtime generation and source offset must be positive")
	}
	return s.recordActivity(ctx, sessionID, category, coalesceWindow, runtimeGeneration, sourceOffset, true)
}

func (s *SQLite) recordActivity(ctx context.Context, sessionID model.SessionID, category model.NotificationCategory, coalesceWindow time.Duration, runtimeGeneration, sourceOffset uint64, fenceSource bool) (uint64, bool, error) {
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
	var updatedAt int64
	err = tx.QueryRowContext(ctx, `SELECT sequence,source_runtime_generation,last_source_offset,updated_at_ms FROM session_activity WHERE session_id=? AND category=?`, sessionID, category).Scan(&sequence, &lastRuntimeGeneration, &lastSourceOffset, &updatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if err == nil && fenceSource && (runtimeGeneration < lastRuntimeGeneration || runtimeGeneration == lastRuntimeGeneration && sourceOffset <= lastSourceOffset) {
		return sequence, false, nil
	}
	generationChanged := fenceSource && runtimeGeneration > lastRuntimeGeneration
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
		_, err = tx.ExecContext(ctx, `INSERT INTO session_activity(session_id,category,sequence,source_runtime_generation,last_source_offset,updated_at_ms) VALUES(?,?,?,?,?,?)`, sessionID, category, sequence, runtimeGeneration, sourceOffset, now)
	} else {
		sequence++
		_, err = tx.ExecContext(ctx, `UPDATE session_activity SET sequence=?,source_runtime_generation=?,last_source_offset=?,updated_at_ms=? WHERE session_id=? AND category=?`, sequence, runtimeGeneration, sourceOffset, now, sessionID, category)
	}
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_revision_events(session_id,change_kind,created_at_ms,activity_category,activity_sequence) VALUES(?,'invalidate',?,?,?)`, sessionID, now, category, sequence); err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_revision_events WHERE revision <= (SELECT max(revision)-4096 FROM session_revision_events)`); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return sequence, true, nil
}

func activityForSessionsTx(ctx context.Context, tx *sql.Tx) (map[model.SessionID]map[model.NotificationCategory]uint64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id,category,sequence FROM session_activity ORDER BY session_id,category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[model.SessionID]map[model.NotificationCategory]uint64)
	for rows.Next() {
		var sessionID model.SessionID
		var category model.NotificationCategory
		var sequence uint64
		if err := rows.Scan(&sessionID, &category, &sequence); err != nil {
			return nil, err
		}
		if err := category.Validate(); err != nil {
			return nil, err
		}
		if result[sessionID] == nil {
			result[sessionID] = make(map[model.NotificationCategory]uint64)
		}
		result[sessionID][category] = sequence
	}
	return result, rows.Err()
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
