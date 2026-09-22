//go:build linux

package ducklioncli

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameNoReplaceAt(dir *os.File, src, dst string) error {
	return unix.Renameat2(int(dir.Fd()), src, int(dir.Fd()), dst, unix.RENAME_NOREPLACE)
}
