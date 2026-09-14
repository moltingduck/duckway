package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const hostConfigFilename = "host-settings.json"

type hostSettings struct {
	RetainedOutputDays int `json:"pty_log_retention_days"`
}

type hostConfigCommittedError struct{ cause error }

func (e *hostConfigCommittedError) Error() string {
	return "host setting applied but directory sync failed: " + e.cause.Error()
}
func (e *hostConfigCommittedError) Unwrap() error { return e.cause }

func secureHostSettingsFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0 && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func validRetainedOutputDays(days int) bool { return days >= 1 && days <= 3650 }

func loadRetainedOutputTTL(root string, fallback time.Duration) (time.Duration, error) {
	if fallback <= 0 {
		fallback = defaultRetainedOutputTTL
	}
	path := filepath.Join(root, hostConfigFilename)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return fallback, nil
	}
	if err != nil {
		return 0, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !secureHostSettingsFile(info) || info.Size() > 4096 {
		return 0, fmt.Errorf("host settings must be a private regular file no larger than 4 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return 0, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var settings hostSettings
	if err := decoder.Decode(&settings); err != nil {
		return 0, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || !validRetainedOutputDays(settings.RetainedOutputDays) {
		return 0, fmt.Errorf("invalid host retention settings")
	}
	return time.Duration(settings.RetainedOutputDays) * 24 * time.Hour, nil
}

func saveRetainedOutputDays(root string, days int) error {
	if !validRetainedOutputDays(days) {
		return fmt.Errorf("pty_log_retention_days must be between 1 and 3650")
	}
	path := filepath.Join(root, hostConfigFilename)
	if info, err := os.Lstat(path); err == nil {
		if !secureHostSettingsFile(info) {
			return fmt.Errorf("host settings path is not a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, _ := json.Marshal(hostSettings{RetainedOutputDays: days})
	temp, err := os.CreateTemp(root, ".host-settings-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return &hostConfigCommittedError{cause: err}
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return &hostConfigCommittedError{cause: err}
	}
	return nil
}

func (s *Server) setRetainedOutputDays(days int) error {
	s.hostConfigMu.Lock()
	defer s.hostConfigMu.Unlock()
	err := saveRetainedOutputDays(s.root, days)
	var committed *hostConfigCommittedError
	if err != nil && !errors.As(err, &committed) {
		return err
	}
	s.retainedOutputTTL.Store(int64(time.Duration(days) * 24 * time.Hour))
	s.requestRetainedOutputSweep()
	return err
}
