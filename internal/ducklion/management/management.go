package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Mode string

const (
	Standalone    Mode = "standalone"
	Integrated    Mode = "integrated"
	Transitioning Mode = "transitioning"
)

type Record struct {
	Mode      Mode   `json:"mode"`
	ManagedBy string `json:"managed_by"`
	Version   string `json:"version,omitempty"`
	Binary    string `json:"binary,omitempty"`
}

func (r Record) Valid() bool {
	return r.Mode == Standalone && r.ManagedBy == "ducklord" || r.Mode == Integrated && r.ManagedBy == "duckway" || r.Mode == Transitioning && r.ManagedBy == "duckway"
}

func Path(root string) string { return filepath.Join(root, "management.json") }

func Read(root string) (Record, error) {
	path := Path(root)
	info, err := os.Lstat(path)
	if err != nil {
		return Record{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return Record{}, fmt.Errorf("ducklion management marker must be a private regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return Record{}, fmt.Errorf("ducklion management marker must belong to the current user")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil || !record.Valid() {
		return Record{}, fmt.Errorf("invalid Ducklion management marker")
	}
	return record, nil
}

func Write(root string, record Record) error {
	if !record.Valid() {
		return fmt.Errorf("invalid Ducklion manager")
	}
	if err := privateRoot(root); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".management-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), Path(root)); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func privateRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("ducklion state root must be a private real directory")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("ducklion state root must belong to the current user")
	}
	return nil
}

type Lock struct{ file *os.File }

func Acquire(root string) (*Lock, error) {
	if err := privateRoot(root); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(filepath.Join(root, "management.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "management.lock")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("ducklion management lock must be a private regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		_ = file.Close()
		return nil, fmt.Errorf("ducklion management lock must belong to the current user")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("ducklion management operation already running: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}

// EnsureIntegrated claims only a fresh state root. An unmarked root with
// existing data is ambiguous and must not be silently taken over.
func EnsureIntegrated(root string) error {
	if err := privateRoot(root); err != nil {
		return err
	}
	record, err := Read(root)
	if err == nil {
		if record.Mode != Integrated {
			return fmt.Errorf("ducklion is standalone; run duckway integrate ducklion on this host")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "management.lock" {
			return fmt.Errorf("unmarked Ducklion state exists; explicit integration is required")
		}
	}
	return Write(root, Record{Mode: Integrated, ManagedBy: "duckway"})
}

func EnsureStandalone(root, binary string) error {
	if err := privateRoot(root); err != nil {
		return err
	}
	record, err := Read(root)
	if err == nil {
		if record.Mode != Standalone {
			return fmt.Errorf("ducklion is managed by Duckway; standalone install is forbidden")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "management.lock" {
			return fmt.Errorf("unmarked Ducklion state exists; refusing standalone ownership")
		}
	}
	return Write(root, Record{Mode: Standalone, ManagedBy: "ducklord", Binary: binary})
}
