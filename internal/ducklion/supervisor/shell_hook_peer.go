package supervisor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"net"
)

// shellHookPeerIsDescendant attributes a notification to this PTY process
// tree. It is an advisory signal, not proof that Codex or Claude produced it:
// any command launched inside the same shell may invoke the hook helper.
func shellHookPeerIsDescendant(conn *net.UnixConn, rootPID int, rootStart uint64) bool {
	if rootPID <= 0 || rootStart == 0 {
		return false
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		cred, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || socketErr != nil || cred == nil || cred.Uid != uint32(os.Geteuid()) || cred.Pid <= 0 {
		return false
	}
	pid := int(cred.Pid)
	for depth := 0; depth < 64 && pid > 1; depth++ {
		if pid == rootPID {
			_, currentStart, ok := procIdentity(pid)
			return ok && currentStart == rootStart
		}
		parent, ok := procParentPID(pid)
		if !ok || parent == pid {
			return false
		}
		pid = parent
	}
	return false
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
