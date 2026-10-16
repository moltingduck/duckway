package management

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStopReturnsContextErrorWhenSocketDialFails(t *testing.T) {
	root := t.TempDir()
	readyPath := filepath.Join(root, "helper.ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStopReturnsContextErrorWhenSocketDialFailsHelper$", "__ducklion_daemon")
	cmd.Env = append(os.Environ(), "DUCKLION_STOP_HELPER=1", "DUCKLION_STOP_READY="+readyPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("managed helper did not become ready: %v", err)
	}

	pidPath := StandalonePID(root)
	start, err := processStartTicks(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d %s", cmd.Process.Pid, start)), 0600); err != nil {
		t.Fatal(err)
	}
	if pid, alive := ReadPID(pidPath); !alive || pid != cmd.Process.Pid {
		t.Fatalf("helper is not recognized as a managed PID: pid=%d alive=%v", pid, alive)
	}

	// Keep the lock unavailable so Stop retries after the absent socket fails to dial.
	lockFD, err := syscall.Open(filepath.Join(root, "daemon.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(lockFD, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = syscall.Close(lockFD)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(lockFD, syscall.LOCK_UN)
		_ = syscall.Close(lockFD)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := Stop(ctx, root, pidPath); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop error = %v, want context deadline", err)
	}
}

func TestStopReturnsContextErrorWhenSocketDialFailsHelper(t *testing.T) {
	if os.Getenv("DUCKLION_STOP_HELPER") != "1" {
		return
	}
	_ = os.WriteFile(os.Getenv("DUCKLION_STOP_READY"), []byte("ready"), 0600)
	select {}
}

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
