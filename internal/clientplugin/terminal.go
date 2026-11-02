package clientplugin

import (
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// The host is temporarily in the background while restoring the terminal.
// Ignore SIGTTOU only for this ioctl and restore its prior default disposition.
func restoreTerminal(fd, pgid int) {
	ignored := signal.Ignored(syscall.SIGTTOU)
	signal.Ignore(syscall.SIGTTOU)
	_ = unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, pgid)
	if !ignored {
		signal.Reset(syscall.SIGTTOU)
	}
}
