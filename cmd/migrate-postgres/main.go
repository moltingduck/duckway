package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/database"
)

var tableOrder = []string{
	"admin_users", "services", "clients", "api_keys", "api_key_groups",
	"api_key_group_members", "key_groups", "key_group_members", "key_suites",
	"placeholder_keys", "approvals", "request_log", "request_log_detail",
	"notification_channels", "canary_settings", "canary_tokens",
	"client_runtime_status", "client_update_rollouts", "client_update_jobs",
	"oauth_credentials", "settings", "control_channels", "cc_channels",
	"discord_inbox", "cc_agent_tests", "schema_version", "conversation_usage",
	"model_pricing", "key_suite_assignments", "key_suite_entries",
}

func main() {
	dataDir := flag.String("sqlite-data", "/data", "directory containing duckway.db")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	snapshotDir, err := snapshotSQLite(*dataDir)
	if err != nil {
		log.Fatalf("snapshot SQLite source: %v", err)
	}
	defer os.RemoveAll(snapshotDir)
	source, err := database.OpenSQLite(snapshotDir)
	if err != nil {
		log.Fatalf("open SQLite source: %v", err)
	}
	defer source.Close()
	removed, err := removeOrphanRows(ctx, source)
	if err != nil {
		log.Fatalf("clean SQLite snapshot foreign keys: %v", err)
	}
	for table, count := range removed {
		log.Printf("WARNING: excluded %d orphan row(s) from %s; original SQLite backup is unchanged", count, table)
	}
	target, err := database.OpenPostgresFromEnv()
	if err != nil {
		log.Fatalf("open PostgreSQL target: %v", err)
	}
	defer target.Close()

	if err := migrate(ctx, source, target); err != nil {
		log.Fatalf("migration failed: %v", err)
	}
	fmt.Fprintln(os.Stdout, "SQLite to PostgreSQL migration completed and verified")
}

type foreignKeyViolation struct {
	table string
	rowID int64
}

func removeOrphanRows(ctx context.Context, db *sql.DB) (map[string]int, error) {
	removed := make(map[string]int)
	for {
		rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			return nil, err
		}
		var violations []foreignKeyViolation
		seen := make(map[string]bool)
		for rows.Next() {
			var table, parent string
			var rowID sql.NullInt64
			var foreignKeyID int
			if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !rowID.Valid {
				_ = rows.Close()
				return nil, fmt.Errorf("cannot safely remove orphan from %s: rowid is unavailable", table)
			}
			key := fmt.Sprintf("%s\x00%d", table, rowID.Int64)
			if !seen[key] {
				seen[key] = true
				violations = append(violations, foreignKeyViolation{table: table, rowID: rowID.Int64})
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if len(violations) == 0 {
			return removed, nil
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		for _, violation := range violations {
			table := `"` + strings.ReplaceAll(violation.table, `"`, `""`) + `"`
			result, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid = ?`, violation.rowID)
			if err != nil {
				_ = tx.Rollback()
				return nil, fmt.Errorf("remove orphan %s rowid %d: %w", violation.table, violation.rowID, err)
			}
			count, err := result.RowsAffected()
			if err != nil || count != 1 {
				_ = tx.Rollback()
				return nil, fmt.Errorf("remove orphan %s rowid %d: affected %d rows", violation.table, violation.rowID, count)
			}
			removed[violation.table]++
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
}

func snapshotSQLite(dataDir string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "duckway-sqlite-migration-*")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	for i, suffix := range []string{"", "-wal", "-shm"} {
		srcPath := filepath.Join(dataDir, "duckway.db"+suffix)
		src, openErr := os.Open(srcPath)
		if openErr != nil {
			if i > 0 && os.IsNotExist(openErr) {
				continue
			}
			return "", openErr
		}
		info, statErr := src.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			_ = src.Close()
			if statErr != nil {
				return "", statErr
			}
			return "", fmt.Errorf("%s is not a regular file", srcPath)
		}
		dst, createErr := os.OpenFile(filepath.Join(tmpDir, filepath.Base(srcPath)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if createErr != nil {
			_ = src.Close()
			return "", createErr
		}
		_, copyErr := io.Copy(dst, src)
		closeDstErr := dst.Close()
		closeSrcErr := src.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeDstErr != nil {
			return "", closeDstErr
		}
		if closeSrcErr != nil {
			return "", closeSrcErr
		}
	}
	ok = true
	return tmpDir, nil
}

func migrate(ctx context.Context, source, target *sql.DB) error {
	var integrity string
	if err := source.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("SQLite integrity check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("SQLite integrity check returned %q", integrity)
	}
	tx, err := target.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(742982032)`); err != nil {
		return fmt.Errorf("acquire import lock: %w", err)
	}
	if err := requireFreshTarget(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `TRUNCATE TABLE `+strings.Join(tableOrder, ", ")+` RESTART IDENTITY CASCADE`); err != nil {
		return fmt.Errorf("clear PostgreSQL target: %w", err)
	}
	for _, table := range tableOrder {
		columns, err := tableColumns(ctx, source, table)
		if err != nil {
			return fmt.Errorf("columns for %s: %w", table, err)
		}
		count, sourceHash, err := copyTable(ctx, source, tx, table, columns)
		if err != nil {
			return fmt.Errorf("copy %s: %w", table, err)
		}
		var targetCount int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&targetCount); err != nil {
			return fmt.Errorf("count target %s: %w", table, err)
		}
		if count != targetCount {
			return fmt.Errorf("verify %s: SQLite has %d rows, PostgreSQL has %d", table, count, targetCount)
		}
		targetHash, err := tableHash(ctx, tx, table, columns)
		if err != nil {
			return fmt.Errorf("hash target %s: %w", table, err)
		}
		if sourceHash != targetHash {
			return fmt.Errorf("verify %s: content hash mismatch", table)
		}
		log.Printf("migrated %-28s %d rows", table, count)
	}
	for _, table := range []string{"request_log", "discord_inbox", "conversation_usage"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%s','id'), COALESCE((SELECT MAX(id) FROM %s), 1), EXISTS(SELECT 1 FROM %s))`,
			table, table, table)); err != nil {
			return fmt.Errorf("reset %s sequence: %w", table, err)
		}
	}
	return tx.Commit()
}

func requireFreshTarget(ctx context.Context, target *sql.Tx) error {
	allowed := make(map[string]bool, len(tableOrder))
	for _, table := range tableOrder {
		allowed[table] = true
	}
	rows, err := target.QueryContext(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return err
		}
		if !allowed[table] {
			return fmt.Errorf("PostgreSQL target contains unknown table %q; refusing destructive import", table)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, table := range tableOrder {
		if table == "services" {
			var unexpected int
			if err := target.QueryRowContext(ctx, `SELECT COUNT(*) FROM services WHERE id NOT IN ('svc-openai-default','svc-xai-default')`).Scan(&unexpected); err != nil {
				return err
			}
			if unexpected != 0 {
				return fmt.Errorf("PostgreSQL target already contains service data; refusing destructive import")
			}
			continue
		}
		var count int64
		if err := target.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("PostgreSQL target table %s already contains %d rows; refusing destructive import", table, count)
		}
	}
	return nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func copyTable(ctx context.Context, source *sql.DB, target *sql.Tx, table string, columns []string) (int64, [32]byte, error) {
	rows, err := source.QueryContext(ctx, `SELECT `+strings.Join(columns, ",")+` FROM `+table)
	if err != nil {
		return 0, [32]byte{}, err
	}
	defer rows.Close()
	marks := strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")
	query := `INSERT INTO ` + table + ` (` + strings.Join(columns, ",") + `) VALUES (` + marks + `)`
	var count int64
	var canonicalRows []string
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return count, [32]byte{}, err
		}
		canonicalRows = append(canonicalRows, canonicalRow(values))
		if _, err := target.ExecContext(ctx, query, values...); err != nil {
			return count, [32]byte{}, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, [32]byte{}, err
	}
	return count, hashRows(canonicalRows), nil
}

func tableHash(ctx context.Context, db queryer, table string, columns []string) ([32]byte, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+strings.Join(columns, ",")+` FROM `+table)
	if err != nil {
		return [32]byte{}, err
	}
	defer rows.Close()
	var canonicalRows []string
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return [32]byte{}, err
		}
		canonicalRows = append(canonicalRows, canonicalRow(values))
	}
	if err := rows.Err(); err != nil {
		return [32]byte{}, err
	}
	return hashRows(canonicalRows), nil
}

func tableColumns(ctx context.Context, db queryer, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT * FROM `+table+` LIMIT 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return rows.Columns()
}

func canonicalRow(values []any) string {
	for i, value := range values {
		if bytes, ok := value.([]byte); ok {
			values[i] = string(bytes)
		}
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func hashRows(rows []string) [32]byte {
	sort.Strings(rows)
	hash := sha256.New()
	for _, row := range rows {
		hash.Write([]byte(row))
		hash.Write([]byte{'\n'})
	}
	return [32]byte(hash.Sum(nil))
}
