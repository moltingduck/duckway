package management

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureIntegratedClaimsOnlyEmptyRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ducklion")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureIntegrated(root); err != nil {
		t.Fatal(err)
	}
	record, err := Read(root)
	if err != nil || record.Mode != Integrated || record.ManagedBy != "duckway" {
		t.Fatalf("manager=%+v err=%v", record, err)
	}
	if err := EnsureIntegrated(root); err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneAndUnmarkedStateAreNotClaimed(t *testing.T) {
	for _, name := range []string{"standalone", "unmarked"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "ducklion")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if name == "standalone" {
				if err := Write(root, Record{Mode: Standalone, ManagedBy: "ducklord"}); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(root, "state.db"), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := EnsureIntegrated(root); err == nil {
				t.Fatal("existing state was silently claimed")
			}
			if name == "standalone" {
				record, err := Read(root)
				if err != nil || record.Mode != Standalone {
					t.Fatalf("manager=%+v err=%v", record, err)
				}
			} else if _, err := Read(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected marker: %v", err)
			}
		})
	}
}

func TestManagementRejectsSymlinksAndConcurrentLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ducklion")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(root); err == nil {
		t.Fatal("concurrent management lock acquired")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), Path(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink marker accepted: %v", err)
	}
}
