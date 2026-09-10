//go:build darwin || freebsd || netbsd || openbsd

package supervisor

import (
	"os"

	"golang.org/x/sys/unix"
)

func pendingPTYBytes(file *os.File) (int, error) {
	const fionread = 0x4004667f
	return unix.IoctlGetInt(int(file.Fd()), fionread)
}
