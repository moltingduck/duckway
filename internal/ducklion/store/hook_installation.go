package store

import (
	"context"
	"database/sql"
	"errors"
)

// InvalidateHookInstallation starts a new epoch before changing settings. A
// failed or interrupted write stays unverified, even if old settings remain.
func (s *SQLite) InvalidateHookInstallation(ctx context.Context, source string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO host_hook_installations(source,epoch,installed,verified_epoch) VALUES(?,1,0,0)
		ON CONFLICT(source) DO UPDATE SET epoch=epoch+1,installed=0,verified_epoch=0`, source)
	return err
}

// SetHookInstallation completes a settings operation. Repeating an unchanged
// install preserves its epoch; callbacks received while absent cannot verify it.
func (s *SQLite) SetHookInstallation(ctx context.Context, source string, installed bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO host_hook_installations(source,epoch,installed,verified_epoch) VALUES(?,1,?,0)
		ON CONFLICT(source) DO UPDATE SET installed=excluded.installed,
		verified_epoch=CASE WHEN excluded.installed=0 THEN 0 ELSE verified_epoch END`, source, installed)
	return err
}

func (s *SQLite) HookActivation(ctx context.Context, source string) (string, error) {
	var installed bool
	var epoch, verified uint64
	err := s.db.QueryRowContext(ctx, `SELECT installed,epoch,verified_epoch FROM host_hook_installations WHERE source=?`, source).Scan(&installed, &epoch, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return "pending", nil
	}
	if err != nil {
		return "", err
	}
	if installed && verified == epoch {
		return "operational", nil
	}
	return "pending", nil
}
