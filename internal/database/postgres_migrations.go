package database

import (
	"database/sql"
	"fmt"
)

var postgresCompatibilityFunctions = []string{
	`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
	`CREATE OR REPLACE FUNCTION datetime(value text) RETURNS text
	 LANGUAGE sql STABLE AS $$
	 SELECT CASE WHEN lower(value) = 'now'
	   THEN to_char(CURRENT_TIMESTAMP AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS')
	   ELSE to_char(value::timestamptz AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') END
	 $$`,
	`CREATE OR REPLACE FUNCTION datetime(value text, modifier text) RETURNS text
	 LANGUAGE sql STABLE AS $$
	 SELECT to_char(
	   ((CASE WHEN lower(value) = 'now' THEN CURRENT_TIMESTAMP ELSE value::timestamptz END)
	   + modifier::interval) AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS')
	 $$`,
	`CREATE OR REPLACE FUNCTION strftime(format text, value text) RETURNS bigint
	 LANGUAGE sql STABLE AS $$
	 SELECT CASE WHEN format = '%s' THEN
	   extract(epoch FROM CASE WHEN lower(value) = 'now' THEN CURRENT_TIMESTAMP ELSE value::timestamptz END)::bigint
	 ELSE NULL END
	 $$`,
	`CREATE OR REPLACE FUNCTION json_extract(value text, path text) RETURNS text
	 LANGUAGE sql IMMUTABLE AS $$
	 SELECT CASE WHEN value = '' OR value IS NULL THEN NULL
	   ELSE value::jsonb #>> string_to_array(trim(leading '$.' from path), '.') END
	 $$`,
	`CREATE OR REPLACE FUNCTION randomblob(size integer) RETURNS bytea
	 LANGUAGE sql VOLATILE AS $$
	 SELECT gen_random_bytes(size)
	 $$`,
	`CREATE OR REPLACE FUNCTION hex(value bytea) RETURNS text
	 LANGUAGE sql IMMUTABLE AS $$ SELECT encode(value, 'hex') $$`,
	`CREATE OR REPLACE FUNCTION max(left_value bigint, right_value bigint) RETURNS bigint
	 LANGUAGE sql IMMUTABLE AS $$ SELECT greatest(left_value, right_value) $$`,
	`CREATE OR REPLACE FUNCTION max(left_value integer, right_value integer) RETURNS integer
	 LANGUAGE sql IMMUTABLE AS $$ SELECT greatest(left_value, right_value) $$`,
	`CREATE OR REPLACE FUNCTION max(left_value real, right_value real) RETURNS real
	 LANGUAGE sql IMMUTABLE AS $$ SELECT greatest(left_value, right_value) $$`,
}

func runPostgresMigrations(db *sql.DB) error {
	// A single connection keeps the session-level advisory lock attached to
	// every migration statement while split admin/gateway start concurrently.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(5)
	}()
	if _, err := db.Exec(`SELECT pg_advisory_lock(742982031)`); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() { _, _ = db.Exec(`SELECT pg_advisory_unlock(742982031)`) }()

	for i, statement := range postgresCompatibilityFunctions {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("compatibility function %d: %w", i, err)
		}
	}
	return runMigrations(db)
}
