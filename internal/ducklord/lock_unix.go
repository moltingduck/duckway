//go:build unix

package ducklord

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type InstanceLock struct {
	mu   sync.Mutex
	file *os.File
}

func AcquireInstanceLock() (*InstanceLock, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, ".ducklord")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create ducklord directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("ducklord directory is not a directory")
	}
	if info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("ducklord directory %s must not be group/world writable", dir)
	}
	path := filepath.Join(dir, "ducklord.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open ducklord lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf("another ducklord TUI is already running")
		}
		return nil, fmt.Errorf("lock ducklord instance: %w", err)
	}
	return &InstanceLock{file: file}, nil
}

func (l *InstanceLock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
