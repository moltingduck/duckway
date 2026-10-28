//go:build linux

package client

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func codexDaemonVerificationSupported() bool { return true }
func daemonSocketRefused(err error) bool     { return errors.Is(err, syscall.ECONNREFUSED) }

func codexDaemonPeerEnvironment(conn net.Conn) ([]string, error) {
	u, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("not a Unix socket")
	}
	raw, err := u.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var peerErr error
	if err := raw.Control(func(fd uintptr) { cred, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return nil, err
	}
	if peerErr != nil {
		return nil, peerErr
	}
	if cred == nil || int(cred.Uid) != os.Geteuid() || cred.Pid <= 0 {
		return nil, fmt.Errorf("unexpected daemon owner")
	}
	env, alive, err := codexDaemonProcessEnvironment(int(cred.Pid))
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, fmt.Errorf("daemon exited during verification")
	}
	return env, nil
}

func codexDaemonProcessEnvironment(pid int) ([]string, bool, error) {
	root := "/proc/" + strconv.Itoa(pid) + "/"
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err == nil && st.Uid != uint32(os.Geteuid()) {
		return nil, true, fmt.Errorf("unexpected daemon process owner")
	}
	before, err := os.ReadFile(root + "stat")
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	// Zombies cannot own a listening backend or perform updater work.
	end := strings.LastIndexByte(string(before), ')')
	if end >= 0 && strings.HasPrefix(string(before[end+1:]), " Z ") {
		return nil, false, nil
	}
	exe, err := os.Readlink(root + "exe")
	if err != nil || exe == "" {
		return nil, true, fmt.Errorf("unverifiable daemon executable")
	}
	data, err := os.ReadFile(root + "environ")
	if err != nil {
		return nil, true, err
	}
	after, err := os.ReadFile(root + "stat")
	if err != nil || daemonStartTime(before) == "" || daemonStartTime(before) != daemonStartTime(after) {
		return nil, true, fmt.Errorf("daemon changed during verification")
	}
	exeAfter, err := os.Readlink(root + "exe")
	if err != nil || exeAfter != exe {
		return nil, true, fmt.Errorf("daemon executable changed during verification")
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"), true, nil
}

func daemonStartTime(stat []byte) string {
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return ""
	}
	return fields[19] // /proc stat field 22, after pid and parenthesized comm
}

func configureDaemonProbe(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

func cleanupDaemonProbe(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func openDaemonOwner(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0077 != 0 {
		f.Close()
		return nil, fmt.Errorf("unsafe owner file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func closeDaemonOwner(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }
