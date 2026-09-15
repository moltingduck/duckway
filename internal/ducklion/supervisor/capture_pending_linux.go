//go:build linux

package supervisor

import (
	"os"

	"golang.org/x/sys/unix"
)

func pendingPTYBytes(file *os.File) (int, error) {
	conn, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pending int
	var ioctlErr error
	if err := conn.Control(func(fd uintptr) {
		pending, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCINQ)
	}); err != nil {
		return 0, err
	}
	return pending, ioctlErr
}
