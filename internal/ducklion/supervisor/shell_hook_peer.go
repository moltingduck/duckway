package supervisor

import (
	"net"
	"os"
)

// shellHookPeerIsDescendant attributes a notification to this PTY process
// tree. It is an advisory signal, not proof that Codex or Claude produced it:
// any command launched inside the same shell may invoke the hook helper.
func shellHookPeerIsDescendant(conn *net.UnixConn, rootPID int, rootStart uint64) bool {
	if rootPID <= 0 || rootStart == 0 {
		return false
	}
	pid, uid, ok := shellHookPeerCredentials(conn)
	if !ok || uid != uint32(os.Geteuid()) || pid <= 0 {
		return false
	}
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
