//go:build darwin

package ducklord

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameNoReplaceAt(dir *os.File, src, dst string) error {
	return unix.RenameatxNp(int(dir.Fd()), src, int(dir.Fd()), dst, unix.RENAME_EXCL)
}
