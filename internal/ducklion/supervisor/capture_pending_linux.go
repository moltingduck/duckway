//go:build linux

package supervisor

import (
	"os"

	"golang.org/x/sys/unix"
)

func pendingPTYBytes(file *os.File) (int, error) {
	return unix.IoctlGetInt(int(file.Fd()), unix.TIOCINQ)
}
