package database

import "testing"

func TestPostgresQueryRebind(t *testing.T) {
	in := `SELECT '?', value FROM t WHERE a = ? AND note = 'it''s ?' AND b = ?`
	want := `SELECT '?', value FROM t WHERE a = $1 AND note = 'it''s ?' AND b = $2`
	if got := postgresQuery(in); got != want {
		t.Fatalf("postgresQuery() = %q, want %q", got, want)
	}
}
