package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

var ErrVisibilityRuntimeChanged = errors.New("session runtime changed before visibility update")

// InvalidateVisibility appends a revision for an ephemeral, generation-fenced
// visibility change. It never changes task, ownership, or notification state.
func (s *SQLite) InvalidateVisibility(ctx context.Context, sessionID model.SessionID, generation uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO session_revision_events(session_id,change_kind,created_at_ms)
		SELECT session_id,'invalidate',unixepoch('subsec')*1000 FROM sessions
		WHERE session_id=? AND runtime_generation=? AND status='running'`, sessionID, generation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrVisibilityRuntimeChanged
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_revision_events WHERE revision <= (SELECT max(revision)-4096 FROM session_revision_events)`); err != nil {
		return err
	}
	return tx.Commit()
}

type SessionProjection struct {
	Session             model.Session
	ChannelHandle       string
	ManagementHandle    string
	ActivitySequences   map[model.NotificationCategory]uint64
	ActivityUpdatedAtMS map[model.NotificationCategory]int64
}

type SessionRevisionSnapshot struct {
	Revision         uint64
	EarliestRevision uint64
	Sessions         []SessionProjection
}

type SessionRevision struct {
	Revision         uint64
	SessionID        model.SessionID
	Change           string
	ActivityCategory model.NotificationCategory
	ActivitySequence uint64
}

// SessionSnapshot reads the journal watermark and complete session projection
// from one SQLite read transaction.
func (s *SQLite) SessionSnapshot(ctx context.Context) (SessionRevisionSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SessionRevisionSnapshot{}, err
	}
	defer tx.Rollback()
	var latest, earliest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(revision),min(revision) FROM session_revision_events`).Scan(&latest, &earliest); err != nil {
		return SessionRevisionSnapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, sessionSelect+` ORDER BY created_at_ms,session_id`)
	if err != nil {
		return SessionRevisionSnapshot{}, err
	}
	var projections []SessionProjection
	for rows.Next() {
		session, scanErr := scanSession(rows)
		if scanErr != nil {
			_ = rows.Close()
			return SessionRevisionSnapshot{}, scanErr
		}
		projections = append(projections, SessionProjection{Session: session})
	}
	if err := rows.Close(); err != nil {
		return SessionRevisionSnapshot{}, err
	}
	if err := rows.Err(); err != nil {
		return SessionRevisionSnapshot{}, err
	}
	for i := range projections {
		binding, bindingErr := s.GetBindingBySessionTx(ctx, tx, projections[i].Session.ID)
		if bindingErr == nil {
			projections[i].ChannelHandle = binding.ChannelHandle
			projections[i].ManagementHandle = binding.ManagementHandle
		} else if !errors.Is(bindingErr, ErrNotFound) {
			return SessionRevisionSnapshot{}, bindingErr
		}
	}
	activity, timestamps, err := activityForSessionsTx(ctx, tx)
	if err != nil {
		return SessionRevisionSnapshot{}, err
	}
	for i := range projections {
		projections[i].ActivitySequences = activity[projections[i].Session.ID]
		projections[i].ActivityUpdatedAtMS = timestamps[projections[i].Session.ID]
	}
	if err := tx.Commit(); err != nil {
		return SessionRevisionSnapshot{}, err
	}
	result := SessionRevisionSnapshot{Sessions: projections}
	if latest.Valid {
		result.Revision = uint64(latest.Int64)
	}
	if earliest.Valid {
		result.EarliestRevision = uint64(earliest.Int64)
	}
	return result, nil
}

func (s *SQLite) SessionRevisionsAfter(ctx context.Context, after uint64, limit int) ([]SessionRevision, uint64, uint64, error) {
	if limit < 1 || limit > 512 {
		limit = 256
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, 0, err
	}
	defer tx.Rollback()
	var earliest, latest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT min(revision),max(revision) FROM session_revision_events`).Scan(&earliest, &latest); err != nil {
		return nil, 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,session_id,change_kind,coalesce(activity_category,''),coalesce(activity_sequence,0) FROM session_revision_events WHERE revision>? ORDER BY revision LIMIT ?`, after, limit)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	var revisions []SessionRevision
	for rows.Next() {
		var revision SessionRevision
		if err := rows.Scan(&revision.Revision, &revision.SessionID, &revision.Change, &revision.ActivityCategory, &revision.ActivitySequence); err != nil {
			return nil, 0, 0, err
		}
		revisions = append(revisions, revision)
	}
	var first, last uint64
	if earliest.Valid {
		first = uint64(earliest.Int64)
	}
	if latest.Valid {
		last = uint64(latest.Int64)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, 0, err
	}
	return revisions, first, last, nil
}
