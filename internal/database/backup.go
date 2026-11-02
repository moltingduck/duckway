package database

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BackupSQLite writes a transactionally consistent copy of the SQLite database
// in dataDir to outPath using VACUUM INTO. It is safe to run while the gateway
// and admin keep serving: it takes a read snapshot and never runs migrations or
// writes to the live database. It refuses to overwrite an existing file.
func BackupSQLite(ctx context.Context, dataDir, outPath string) error {
	if strings.TrimSpace(outPath) == "" {
		return errors.New("backup output path is required")
	}
	absOut, err := filepath.Abs(outPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	if _, err := os.Lstat(absOut); err == nil {
		return fmt.Errorf("refusing to overwrite existing file %s", absOut)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check output path: %w", err)
	}
	dbPath := filepath.Join(dataDir, "duckway.db")
	if st, err := os.Stat(dbPath); err != nil || st.IsDir() {
		return fmt.Errorf("sqlite database not found at %s", dbPath)
	}
	if err := os.MkdirAll(filepath.Dir(absOut), 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	u := url.URL{Scheme: "file", Path: dbPath}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(30000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Quote the path for the SQL literal; VACUUM INTO does not take parameters
	// in every SQLite build.
	lit := "'" + strings.ReplaceAll(absOut, "'", "''") + "'"
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+lit); err != nil {
		_ = os.Remove(absOut)
		return fmt.Errorf("vacuum into: %w", err)
	}
	if err := os.Chmod(absOut, 0o600); err != nil {
		return fmt.Errorf("chmod backup: %w", err)
	}
	return nil
}

// RunBackupCommand implements `duckway-server backup --out PATH` (also used by
// duckway-admin). dataDirDefault is used when --data is not given.
func RunBackupCommand(args []string, dataDirDefault string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "Path of the SQLite backup file to create (must not exist)")
	data := fs.String("data", "", "Data directory (default: DUCKWAY_DATA_DIR)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("DUCKWAY_DATABASE_DRIVER")), "postgres") {
		fmt.Fprintln(stderr, "backup: PostgreSQL is configured; use pg_dump (scripts/prod.sh upgrade does this for you)")
		return 2
	}
	if *out == "" {
		fmt.Fprintln(stderr, "backup: --out PATH is required")
		return 2
	}
	dir := *data
	if dir == "" {
		dir = dataDirDefault
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := BackupSQLite(ctx, dir, *out); err != nil {
		fmt.Fprintf(stderr, "backup: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "backup written: %s\n", *out)
	return 0
}
