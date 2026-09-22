package client

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// These real-daemon tests need Linux procfs to find their detached runtimes.
// Each test builds a unique executable in t.TempDir: never match a shared name
// or a shell command, which could select another test or a user's session.
func registerDucklionRuntimeCleanup(t *testing.T, binary string) {
	t.Helper()
	if _, err := os.ReadDir("/proc/self"); err != nil {
		t.Skip("real-daemon runtime cleanup requires Linux procfs")
	}
	// Registered after TempDir, but before startup. Test defers stop the daemon
	// first (including its replacement after restart); cleanup then runs before
	// TempDir removal, even when a later assertion calls Fatal.
	t.Cleanup(func() { stopDucklionTestRuntimes(t, binary) })
}

func ducklionTestRuntimePIDs(binary string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil { // Processes may exit while enumerating.
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				continue
			}
			return nil, err
		}
		args := bytes.Split(cmdline, []byte{0})
		if len(args) >= 3 && string(args[0]) == binary && string(args[1]) == "__ducklion_runtime_v1" {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func stopDucklionTestRuntimes(t *testing.T, binary string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	signaled := make(map[int]bool)
	for {
		pids, err := ducklionTestRuntimePIDs(binary)
		if err != nil {
			t.Errorf("discover test runtimes: %v", err)
			return
		}
		if len(pids) == 0 {
			return
		}
		for _, pid := range pids {
			if signaled[pid] {
				continue
			}
			// SIGTERM asks the supervisor to terminate and reap its PTY process
			// group, with its own bounded SIGKILL escalation. Killing just the
			// supervisor would leave descendants behind.
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("stop test runtime %d: %v", pid, err)
			}
			signaled[pid] = true
		}
		if time.Now().After(deadline) {
			t.Errorf("test runtimes did not stop within 8s: %v", pids)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
