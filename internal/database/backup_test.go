package database

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func seedDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := openSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE backup_probe (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := db.Exec(`INSERT INTO backup_probe (v) VALUES ('row')`); err != nil {
			t.Fatal(err)
		}
	}
	return dir, db
}

func countProbe(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM backup_probe`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, err %v", check, err)
	}
	return n
}

func TestBackupSQLiteConsistentAndOpenable(t *testing.T) {
	dir, _ := seedDB(t)
	out := filepath.Join(t.TempDir(), "sub", "b.db")
	if err := BackupSQLite(context.Background(), dir, out); err != nil {
		t.Fatal(err)
	}
	if n := countProbe(t, out); n != 50 {
		t.Fatalf("rows = %d", n)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
}

func TestBackupSQLiteRefusesExisting(t *testing.T) {
	dir, _ := seedDB(t)
	out := filepath.Join(t.TempDir(), "b.db")
	if err := os.WriteFile(out, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := BackupSQLite(context.Background(), dir, out); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "keep" {
		t.Fatal("existing file modified")
	}
}

func TestBackupSQLiteMissingDB(t *testing.T) {
	if err := BackupSQLite(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "b.db")); err == nil {
		t.Fatal("expected error")
	}
}

func TestBackupSQLiteWhileWriting(t *testing.T) {
	dir, db := seedDB(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = db.Exec(`INSERT INTO backup_probe (v) VALUES ('w')`)
			}
		}
	}()
	out := filepath.Join(t.TempDir(), "b.db")
	err := BackupSQLite(context.Background(), dir, out)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if n := countProbe(t, out); n < 50 {
		t.Fatalf("rows = %d", n)
	}
}

func TestRunBackupCommand(t *testing.T) {
	dir, _ := seedDB(t)
	out := filepath.Join(t.TempDir(), "b.db")
	var so, se strings.Builder
	if rc := RunBackupCommand([]string{"--out", out, "--data", dir}, "", &so, &se); rc != 0 {
		t.Fatalf("rc=%d %s", rc, se.String())
	}
	if rc := RunBackupCommand([]string{"--out", out, "--data", dir}, "", &so, &se); rc != 1 {
		t.Fatalf("second rc=%d", rc)
	}
	if rc := RunBackupCommand([]string{}, dir, &so, &se); rc != 2 {
		t.Fatalf("missing out rc=%d", rc)
	}
	t.Setenv("DUCKWAY_DATABASE_DRIVER", "postgres")
	se.Reset()
	if rc := RunBackupCommand([]string{"--out", out + "2"}, dir, &so, &se); rc != 2 || !strings.Contains(se.String(), "pg_dump") {
		t.Fatalf("pg rc=%d %s", rc, se.String())
	}
}

func TestBackupSQLiteFilePrivateAndNoTempLeftover(t *testing.T) {
	dir, _ := seedDB(t)
	oldMask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldMask) })
	outDir := t.TempDir()
	out := filepath.Join(outDir, "b.db")
	if err := BackupSQLite(context.Background(), dir, out); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o want 600", st.Mode().Perm())
	}
	ents, _ := os.ReadDir(outDir)
	if len(ents) != 1 {
		t.Fatalf("leftover entries in %s: %v", outDir, ents)
	}
	// Failure path (database missing) also leaves nothing behind.
	if err := BackupSQLite(context.Background(), t.TempDir(), filepath.Join(outDir, "c.db")); err == nil {
		t.Fatal("expected error")
	}
	if ents, _ := os.ReadDir(outDir); len(ents) != 1 {
		t.Fatalf("leftover after failure: %v", ents)
	}
}

func TestRunBackupCommandHelpExitsZeroAndMentionsOut(t *testing.T) {
	var so, se strings.Builder
	if rc := RunBackupCommand([]string{"--help"}, "", &so, &se); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(so.String()+se.String(), "--out") {
		t.Fatalf("help does not mention --out: %q", se.String())
	}
}
