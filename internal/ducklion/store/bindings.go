package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

var ErrBindingConflict = errors.New("discord binding conflict")

type DiscordBinding struct {
	SessionID        model.SessionID `json:"session_id"`
	ChannelHandle    string          `json:"channel_handle"`
	ManagementHandle string          `json:"management_handle"`
	CreatedAtMS      int64           `json:"created_at_ms"`
}

func (s *SQLite) InsertBindingTx(ctx context.Context, tx *sql.Tx, binding DiscordBinding) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO discord_bindings(session_id,channel_handle,management_handle,created_at_ms) VALUES(?,?,?,?)`,
		binding.SessionID, binding.ChannelHandle, binding.ManagementHandle, binding.CreatedAtMS)
	if err != nil {
		if existing, getErr := s.GetBindingBySessionTx(ctx, tx, binding.SessionID); getErr == nil {
			if existing.ChannelHandle == binding.ChannelHandle && existing.ManagementHandle == binding.ManagementHandle {
				return nil
			}
			return ErrBindingConflict
		}
		if existing, getErr := s.GetBindingByChannelTx(ctx, tx, binding.ChannelHandle); getErr == nil && existing.SessionID != binding.SessionID {
			return ErrBindingConflict
		}
		return err
	}
	return nil
}

func (s *SQLite) GetBindingBySessionTx(ctx context.Context, tx *sql.Tx, id model.SessionID) (DiscordBinding, error) {
	return scanBinding(tx.QueryRowContext(ctx, `SELECT session_id,channel_handle,management_handle,created_at_ms FROM discord_bindings WHERE session_id=?`, id))
}

func (s *SQLite) GetBindingByChannelTx(ctx context.Context, tx *sql.Tx, handle string) (DiscordBinding, error) {
	return scanBinding(tx.QueryRowContext(ctx, `SELECT session_id,channel_handle,management_handle,created_at_ms FROM discord_bindings WHERE channel_handle=?`, handle))
}

func (s *SQLite) GetBindingBySession(ctx context.Context, id model.SessionID) (DiscordBinding, error) {
	return scanBinding(s.db.QueryRowContext(ctx, `SELECT session_id,channel_handle,management_handle,created_at_ms FROM discord_bindings WHERE session_id=?`, id))
}

func (s *SQLite) GetBindingByChannel(ctx context.Context, handle string) (DiscordBinding, error) {
	return scanBinding(s.db.QueryRowContext(ctx, `SELECT session_id,channel_handle,management_handle,created_at_ms FROM discord_bindings WHERE channel_handle=?`, handle))
}

type bindingScanner interface{ Scan(...any) error }

func scanBinding(row bindingScanner) (DiscordBinding, error) {
	var binding DiscordBinding
	if err := row.Scan(&binding.SessionID, &binding.ChannelHandle, &binding.ManagementHandle, &binding.CreatedAtMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DiscordBinding{}, ErrNotFound
		}
		return DiscordBinding{}, fmt.Errorf("scan discord binding: %w", err)
	}
	return binding, nil
}
