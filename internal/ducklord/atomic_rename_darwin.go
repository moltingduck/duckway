//go:build darwin

package ducklord

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameNoReplace(src, dst string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_EXCL)
}
func renameNoReplaceAt(dir *os.File, src, dst string) error {
	return unix.RenameatxNp(int(dir.Fd()), src, int(dir.Fd()), dst, unix.RENAME_EXCL)
}
