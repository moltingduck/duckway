package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

type SessionProjection struct {
	Session          model.Session
	ChannelHandle    string
	ManagementHandle string
}

type SessionRevisionSnapshot struct {
	Revision         uint64
	EarliestRevision uint64
	Sessions         []SessionProjection
}

type SessionRevision struct {
	Revision  uint64
	SessionID model.SessionID
	Change    string
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
	var earliest, latest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT min(revision),max(revision) FROM session_revision_events`).Scan(&earliest, &latest); err != nil {
		return nil, 0, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revision,session_id,change_kind FROM session_revision_events WHERE revision>? ORDER BY revision LIMIT ?`, after, limit)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	var revisions []SessionRevision
	for rows.Next() {
		var revision SessionRevision
		if err := rows.Scan(&revision.Revision, &revision.SessionID, &revision.Change); err != nil {
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
	return revisions, first, last, rows.Err()
}
