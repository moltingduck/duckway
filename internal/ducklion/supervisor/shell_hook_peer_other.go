//go:build !linux && !darwin

package supervisor

import "net"

// Platforms without a verified peer-credential implementation fail closed.
func shellHookPeerCredentials(*net.UnixConn) (int, uint32, bool) {
	return 0, 0, false
}

func procParentPID(int) (int, bool) { return 0, false }

func procIdentity(int) (int, uint64, bool) { return 0, 0, false }
