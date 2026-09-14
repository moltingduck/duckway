package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const hostConfigFilename = "host-settings.json"

type hostSettings struct {
	RetainedOutputDays int `json:"pty_log_retention_days"`
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
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return 0, fmt.Errorf("host settings must be a regular file no larger than 4 KiB")
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
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("host settings path is not a regular file")
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
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Server) setRetainedOutputDays(days int) error {
	s.hostConfigMu.Lock()
	defer s.hostConfigMu.Unlock()
	if err := saveRetainedOutputDays(s.root, days); err != nil {
		return err
	}
	s.retainedOutputTTL.Store(int64(time.Duration(days) * 24 * time.Hour))
	s.requestRetainedOutputSweep()
	return nil
}
