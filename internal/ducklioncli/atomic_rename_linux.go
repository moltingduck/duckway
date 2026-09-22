//go:build linux

package ducklioncli

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameNoReplace(src, dst string) error {
	return unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
}

func renameNoReplaceAt(dir *os.File, src, dst string) error {
	return unix.Renameat2(int(dir.Fd()), src, int(dir.Fd()), dst, unix.RENAME_NOREPLACE)
}
