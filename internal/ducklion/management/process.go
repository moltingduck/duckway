package management

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func StandalonePID(root string) string { return filepath.Join(root, "standalone.pid") }
func IntegratedPID(root string) string { return filepath.Join(root, "daemon.pid") }

func ReadPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, false
	}
	if syscall.Kill(pid, 0) != nil {
		return pid, false
	}
	if len(fields) != 2 {
		return pid, false
	}
	start, err := processStartTicks(pid)
	if err != nil || fields[1] != start {
		return pid, false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !bytes.Contains(cmdline, []byte("__ducklion_daemon")) && !bytes.Contains(cmdline, []byte{0, 'd', 'a', 'e', 'm', 'o', 'n', 0}) {
		return pid, false
	}
	return pid, true
}

func processStartTicks(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+2 >= len(data) {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+2:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("invalid process stat")
	}
	return fields[19], nil
}

func Start(root, executable string, args, env []string, pidPath, logPath string) error {
	if err := privateRoot(root); err != nil {
		return err
	}
	if pid, alive := ReadPID(pidPath); alive {
		return fmt.Errorf("ducklion daemon is already running (PID %d)", pid)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(executable, args...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	start, err := processStartTicks(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	pidText := strconv.Itoa(cmd.Process.Pid) + " " + start
	if err := os.WriteFile(pidPath, []byte(pidText), 0600); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	_ = cmd.Process.Release()
	return WaitReady(root, 5*time.Second)
}

func WaitReady(root string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", filepath.Join(root, "ducklion.sock"), 150*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("ducklion daemon did not become ready within %s", timeout)
}

func Stop(ctx context.Context, root, pidPath string) error {
	pid, alive := ReadPID(pidPath)
	if !alive {
		return fmt.Errorf("ducklion daemon is not running")
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	for {
		conn, err := net.DialTimeout("unix", filepath.Join(root, "ducklion.sock"), 100*time.Millisecond)
		if err != nil {
			lockFD, lockErr := syscall.Open(filepath.Join(root, "daemon.lock"), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if lockErr == nil {
				if flockErr := syscall.Flock(lockFD, syscall.LOCK_EX|syscall.LOCK_NB); flockErr == nil {
					_ = syscall.Flock(lockFD, syscall.LOCK_UN)
					_ = syscall.Close(lockFD)
					_ = os.Remove(pidPath)
					return nil
				}
				_ = syscall.Close(lockFD)
			}
		}
		_ = conn.Close()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
