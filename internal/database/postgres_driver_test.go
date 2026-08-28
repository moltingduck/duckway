package database

import "testing"

func TestPostgresQueryRebind(t *testing.T) {
	in := `SELECT '?', value FROM t WHERE a = ? AND note = 'it''s ?' AND b = ?`
	want := `SELECT '?', value FROM t WHERE a = $1 AND note = 'it''s ?' AND b = $2`
	if got := postgresQuery(in); got != want {
		t.Fatalf("postgresQuery() = %q, want %q", got, want)
	}
}

func TestPostgresQueryUsesBigintForSQLiteIntegerDDL(t *testing.T) {
	got := postgresQuery(`CREATE TABLE sample (expires_at INTEGER NOT NULL, id INTEGER PRIMARY KEY AUTOINCREMENT)`)
	want := `CREATE TABLE sample (expires_at BIGINT NOT NULL, id BIGSERIAL PRIMARY KEY)`
	if got != want {
		t.Fatalf("postgresQuery() = %q, want %q", got, want)
	}
}
