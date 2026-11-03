package handlers

import (
	"database/sql"
	"errors"
	"testing"
)

var errUnique = errors.New("UNIQUE constraint failed: approvals.placeholder_id, approvals.scope")

func TestResolveScopedApprovalRaceOutcomes(t *testing.T) {
	noRows := func() (string, error) { return "", sql.ErrNoRows }
	t.Run("reuses an existing pending approval", func(t *testing.T) {
		id, created, fwd, err := resolveScopedApproval(scopedApprovalOps{
			pending: func() (string, error) { return "p1", nil },
			valid:   func() error { return sql.ErrNoRows },
			create:  func() (string, error) { t.Fatal("must not create"); return "", nil },
		})
		if id != "p1" || created || fwd || err != nil {
			t.Fatalf("%s %v %v %v", id, created, fwd, err)
		}
	})
	t.Run("creates when nothing exists", func(t *testing.T) {
		id, created, fwd, err := resolveScopedApproval(scopedApprovalOps{pending: noRows, valid: func() error { return sql.ErrNoRows }, create: func() (string, error) { return "n1", nil }})
		if id != "n1" || !created || fwd || err != nil {
			t.Fatalf("%s %v %v %v", id, created, fwd, err)
		}
	})
	t.Run("lost the insert race: returns the winner's pending, not created", func(t *testing.T) {
		calls := 0
		id, created, fwd, err := resolveScopedApproval(scopedApprovalOps{
			pending: func() (string, error) {
				calls++
				if calls == 1 {
					return "", sql.ErrNoRows
				}
				return "winner", nil
			},
			valid:  func() error { return sql.ErrNoRows },
			create: func() (string, error) { return "", errUnique },
		})
		if id != "winner" || created || fwd || err != nil {
			t.Fatalf("%s %v %v %v", id, created, fwd, err)
		}
	})
	t.Run("winner already approved: forward, never 500", func(t *testing.T) {
		_, created, fwd, err := resolveScopedApproval(scopedApprovalOps{
			pending: noRows,
			valid:   func() error { return nil },
			create:  func() (string, error) { return "", errUnique },
		})
		if created || !fwd || err != nil {
			t.Fatalf("created=%v forward=%v err=%v", created, fwd, err)
		}
	})
	t.Run("winner settled otherwise: creates a fresh one", func(t *testing.T) {
		n := 0
		id, created, _, err := resolveScopedApproval(scopedApprovalOps{
			pending: noRows,
			valid:   func() error { return sql.ErrNoRows },
			create: func() (string, error) {
				n++
				if n == 1 {
					return "", errUnique
				}
				return "fresh", nil
			},
		})
		if id != "fresh" || !created || err != nil {
			t.Fatalf("%s %v %v", id, created, err)
		}
	})
	t.Run("persistent contention is a retryable condition, not a failure", func(t *testing.T) {
		_, _, _, err := resolveScopedApproval(scopedApprovalOps{pending: noRows, valid: func() error { return sql.ErrNoRows }, create: func() (string, error) { return "", errUnique }})
		if !errors.Is(err, errApprovalContention) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("other errors are reported", func(t *testing.T) {
		boom := errors.New("disk full")
		_, _, _, err := resolveScopedApproval(scopedApprovalOps{pending: noRows, valid: func() error { return sql.ErrNoRows }, create: func() (string, error) { return "", boom }})
		if !errors.Is(err, boom) {
			t.Fatalf("err=%v", err)
		}
	})
}
