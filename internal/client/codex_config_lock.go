package client

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var codexConfigMu sync.Mutex

// lockCodexConfig serializes Duckway's config transactions across goroutines
// and processes (including cc-watch). The lock lives beside the effective
// CODEX_HOME config and is never removed: unlinking it would split lock owners
// across different inodes. Callers must not reacquire it while holding it.
// The public sync APIs have no cancellation context, so acquisition blocks.
func lockCodexConfig() (func(), error) {
	codexConfigMu.Lock()
	unlock, err := lockCodexConfigFile()
	if err != nil {
		codexConfigMu.Unlock()
		return nil, err
	}
	return func() {
		unlock()
		codexConfigMu.Unlock()
	}, nil
}

func lockCodexConfigFile() (func(), error) {
	configPath, err := codexConfigTOMLPath()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create Codex configuration directory: %w", err)
	}
	path := filepath.Join(dir, ".duckway-config.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open Codex configuration lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (func(), error) {
		_ = file.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("stat Codex configuration lock: %w", err))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return fail(fmt.Errorf("codex configuration lock must be a regular file owned by the current user with one link"))
	}
	if err := file.Chmod(0600); err != nil {
		return fail(fmt.Errorf("secure Codex configuration lock: %w", err))
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return fail(fmt.Errorf("acquire Codex configuration lock: %w", err))
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
