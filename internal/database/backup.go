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
	"syscall"
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
	outDir := filepath.Dir(absOut)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	// VACUUM INTO creates its file with the process umask, which can be
	// world-readable. Write into a private (0700) temp directory next to the
	// destination, restrict the file, then move it into place. The temp
	// directory is always removed, including on failure.
	tmpDir, err := os.MkdirTemp(outDir, ".duckway-backup-*")
	if err != nil {
		return fmt.Errorf("create private temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	tmpOut := filepath.Join(tmpDir, "backup.db")

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
	lit := "'" + strings.ReplaceAll(tmpOut, "'", "''") + "'"
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+lit); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "locked") || strings.Contains(msg, "busy") || strings.Contains(msg, "disk i/o") || strings.Contains(msg, "protocol") {
			return fmt.Errorf("vacuum into: database is busy or locked (%w); online backup needs the data directory on a local filesystem shared by one kernel (not NFS or a network/virtiofs bind mount); retry, or stop the app services and copy the database offline", err)
		}
		return fmt.Errorf("vacuum into: %w", err)
	}
	if err := os.Chmod(tmpOut, 0o600); err != nil {
		return fmt.Errorf("chmod backup: %w", err)
	}
	// Link (not rename) so an existing destination is never overwritten.
	if err := os.Link(tmpOut, absOut); err != nil {
		switch {
		case errors.Is(err, os.ErrExist):
			return fmt.Errorf("refusing to overwrite existing file %s", absOut)
		case linkUnsupported(err):
			// Filesystem without hard links: no-clobber copy (O_EXCL).
			if cerr := copyNoClobber(tmpOut, absOut); cerr != nil {
				return cerr
			}
		default:
			return fmt.Errorf("move backup into place: %w", err)
		}
	}
	return nil
}

func linkUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EOPNOTSUPP)
}

// copyNoClobber copies src to dst with mode 0600, failing if dst exists. A
// partial dst is removed on error.
func copyNoClobber(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open temp backup: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing file %s", dst)
		}
		return fmt.Errorf("create backup file: %w", err)
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("copy backup: %w", err)
	}
	return out.Sync()
}

// RunBackupCommand implements `duckway-server backup --out PATH` (also used by
// duckway-admin). dataDirDefault is used when --data is not given.
func RunBackupCommand(args []string, dataDirDefault string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "Path of the SQLite backup file to create (must not exist)")
	data := fs.String("data", "", "Data directory (default: DUCKWAY_DATA_DIR)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: backup --out PATH [--data DIR]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
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
