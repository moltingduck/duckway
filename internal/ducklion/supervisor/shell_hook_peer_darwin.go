//go:build darwin

package supervisor

import (
	"net"

	"golang.org/x/sys/unix"
)

func shellHookPeerCredentials(conn *net.UnixConn) (pid int, uid uint32, ok bool) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, false
	}
	var cred *unix.Xucred
	var peerPID int
	var credErr, pidErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		peerPID, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil || credErr != nil || pidErr != nil || cred == nil || peerPID <= 0 {
		return 0, 0, false
	}
	return peerPID, cred.Uid, true
}

func procParentPID(pid int) (int, bool) {
	parent, _, ok := procIdentity(pid)
	return parent, ok
}

func procIdentity(pid int) (int, uint64, bool) {
	if pid <= 0 {
		return 0, 0, false
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info == nil || int(info.Proc.P_pid) != pid || info.Eproc.Ppid <= 0 {
		return 0, 0, false
	}
	started := info.Proc.P_starttime
	if started.Sec <= 0 || started.Usec < 0 {
		return 0, 0, false
	}
	startToken := uint64(started.Sec)*1_000_000 + uint64(started.Usec)
	if startToken == 0 {
		return 0, 0, false
	}
	return int(info.Eproc.Ppid), startToken, true
}
