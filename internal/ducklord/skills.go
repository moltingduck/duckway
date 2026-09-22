package ducklord

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	MaxSkillFiles = 1024
	MaxSkillBytes = 16 << 20
)

type SkillInfo struct {
	Identifier string
	Files      int
	Bytes      int64
	Digest     string
}

// skillTransferTestHook is set only by package tests to exercise the
// validation-to-transfer boundary.
var skillTransferTestHook func(string)

// InspectSkill validates a skill directory without following symlinks and
// returns a stable digest over sorted relative paths and file contents.
func InspectSkill(dir, identifier string) (SkillInfo, error) {
	if !SafeIdentifier(identifier) {
		return SkillInfo{}, fmt.Errorf("unsafe skill identifier %q", identifier)
	}
	root, err := os.Lstat(dir)
	if err != nil {
		return SkillInfo{}, err
	}
	if !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return SkillInfo{}, fmt.Errorf("skill must be a real directory")
	}
	paths := make([]string, 0)
	var total int64
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			rel, _ := filepath.Rel(dir, path)
			return fmt.Errorf("skill contains symlink %q", rel)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			rel, _ := filepath.Rel(dir, path)
			return fmt.Errorf("skill contains non-regular file %q", rel)
		}
		if len(paths) >= MaxSkillFiles {
			return fmt.Errorf("skill exceeds %d files", MaxSkillFiles)
		}
		if total > MaxSkillBytes-info.Size() {
			return fmt.Errorf("skill exceeds %d bytes", MaxSkillBytes)
		}
		total += info.Size()
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return SkillInfo{}, err
	}
	sort.Strings(paths)
	foundManifest := false
	h := sha256.New()
	rfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return SkillInfo{}, err
	}
	defer unix.Close(rfd)
	var actualTotal int64
	for _, rel := range paths {
		if rel == "SKILL.md" {
			foundManifest = true
		}
		data, err := readSkillFile(rfd, rel, MaxSkillBytes-actualTotal)
		if err != nil {
			return SkillInfo{}, err
		}
		actualTotal += int64(len(data))
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		h.Write(data)
	}
	if !foundManifest {
		return SkillInfo{}, fmt.Errorf("skill must contain SKILL.md")
	}
	if actualTotal != total {
		return SkillInfo{}, fmt.Errorf("skill changed during inspection")
	}
	return SkillInfo{Identifier: identifier, Files: len(paths), Bytes: total, Digest: hex.EncodeToString(h.Sum(nil))}, nil
}

// ImportSkill atomically installs a prevalidated skill beneath repository.
func ImportSkill(repository, identifier, source string) (SkillInfo, error) {
	if !SafeIdentifier(identifier) {
		return SkillInfo{}, fmt.Errorf("unsafe skill identifier %q", identifier)
	}
	if err := ensureRepository(repository); err != nil {
		return SkillInfo{}, err
	}
	info, err := InspectSkill(source, identifier)
	if err != nil {
		return SkillInfo{}, err
	}
	tmp, err := os.MkdirTemp(repository, ".skill-import-")
	if err != nil {
		return SkillInfo{}, err
	}
	tmpName := tmp
	defer os.RemoveAll(tmpName)
	stage := filepath.Join(tmp, identifier)
	if err := copySkill(source, stage); err != nil {
		return SkillInfo{}, err
	}
	staged, err := InspectSkill(stage, identifier)
	if err != nil {
		return SkillInfo{}, err
	}
	if staged.Files != info.Files || staged.Bytes != info.Bytes || staged.Digest != info.Digest {
		return SkillInfo{}, fmt.Errorf("skill source changed during import")
	}
	target := filepath.Join(repository, identifier)
	old, err := os.MkdirTemp(repository, ".skill-old-")
	if err != nil {
		return SkillInfo{}, err
	}
	if err := os.Remove(old); err != nil {
		return SkillInfo{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, old); err != nil {
			return SkillInfo{}, err
		}
	} else if !os.IsNotExist(err) {
		return SkillInfo{}, err
	}
	if err := os.Rename(stage, target); err != nil {
		if _, statErr := os.Lstat(old); statErr == nil {
			if restoreErr := os.Rename(old, target); restoreErr != nil {
				return SkillInfo{}, fmt.Errorf("install skill: %w; preserve backup %q after restore failure: %v", err, old, restoreErr)
			}
		}
		return SkillInfo{}, err
	}
	_ = os.RemoveAll(old)
	return info, nil
}

func ensureRepository(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skill repository must be a real directory")
	}
	return nil
}

func skillPathHasNoSymlinkComponents(path string) error {
	clean := filepath.Clean(path)
	for current := clean; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill path contains symlink %q", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

// readSkillFile opens every component relative to a descriptor for the
// original root. A rename of any ancestor therefore cannot redirect the read.
func readSkillFile(rfd int, rel string, limit int64) ([]byte, error) {
	components := strings.Split(filepath.ToSlash(rel), "/")
	fd := rfd
	for i, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("invalid skill path %q", rel)
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		nfd, openErr := unix.Openat(fd, component, flags, 0)
		if fd != rfd {
			unix.Close(fd)
		}
		if openErr != nil {
			return nil, openErr
		}
		fd = nfd
	}
	f := os.NewFile(uintptr(fd), rel)
	data, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("skill exceeds %d bytes", MaxSkillBytes)
	}
	return data, nil
}

func copySkill(src, dst string) error {
	if err := skillPathHasNoSymlinkComponents(src); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		return err
	}
	files := 0
	var total int64
	rfd, err := unix.Open(src, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(rfd)
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill contains symlink %q", rel)
		}
		dest := filepath.Join(dst, rel)
		if entry.IsDir() {
			if err := skillPathHasNoSymlinkComponents(path); err != nil {
				return err
			}
			return os.Mkdir(dest, 0700)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill contains symlink %q", rel)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill contains non-regular file %q", rel)
		}
		if skillTransferTestHook != nil {
			skillTransferTestHook(rel)
		}
		files++
		if files > MaxSkillFiles {
			return fmt.Errorf("skill exceeds %d files", MaxSkillFiles)
		}
		if err := skillPathHasNoSymlinkComponents(path); err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		remaining := MaxSkillBytes - total
		if remaining < 0 {
			remaining = 0
		}
		data, cpErr := readSkillFile(rfd, rel, remaining)
		n := int64(len(data))
		if cpErr == nil {
			_, cpErr = out.Write(data)
		}
		closeErr := out.Close()
		if cpErr == nil && n > remaining {
			return fmt.Errorf("skill exceeds %d bytes", MaxSkillBytes)
		}
		if cpErr == nil {
			total += n
		}
		if cpErr == nil {
			latest, statErr := os.Stat(path)
			if statErr != nil {
				return statErr
			}
			if latest.Size() != info.Size() {
				return fmt.Errorf("skill source changed during import")
			}
		}
		if cpErr != nil {
			return cpErr
		}
		return closeErr
	})
}
