//go:build linux

package supervisor

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func shellHookPeerCredentials(conn *net.UnixConn) (pid int, uid uint32, ok bool) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, false
	}
	var cred *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		cred, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || socketErr != nil || cred == nil || cred.Pid <= 0 {
		return 0, 0, false
	}
	return int(cred.Pid), cred.Uid, true
}

func procParentPID(pid int) (int, bool) {
	parent, _, ok := procIdentity(pid)
	return parent, ok
}

func procIdentity(pid int) (int, uint64, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, false
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return 0, 0, false
	}
	parent, err := strconv.Atoi(fields[1])
	start, startErr := strconv.ParseUint(fields[19], 10, 64)
	return parent, start, err == nil && parent > 0 && startErr == nil && start > 0
}
