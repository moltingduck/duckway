package clientplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openLocalPackageRoot opens every path component without following symlinks.
// Writable sticky ancestors such as /tmp are allowed; the package root itself
// must be owned by the caller and not writable by group or other users.
func openLocalPackageRoot(path string) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(abs), string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("local plugin package path is not canonical")
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, fmt.Errorf("open local plugin package directory: %w", openErr)
		}
		fd = next
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || !safeLocalOwner(stat.Uid) || stat.Mode&0022 != 0 && stat.Mode&unix.S_ISVTX == 0 {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("local plugin package path has unsafe ownership or permissions")
		}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("local plugin package directory must be owned by the current user and not writable by others")
	}
	return os.NewFile(uintptr(fd), abs), nil
}

func safeLocalOwner(uid uint32) bool {
	return uid == 0 || uid == uint32(os.Geteuid())
}

// openLocalPackageFile walks from the already-open package root using O_NOFOLLOW
// for every component. O_NONBLOCK prevents a malicious FIFO from hanging before
// fstat rejects it. The returned descriptor remains pinned to the verified file.
func openLocalPackageFile(root *os.File, relative string, expectedOrMaxSize int64) (*os.File, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.ContainsAny(relative, "\\\x00") {
		return nil, fmt.Errorf("local plugin package path is invalid")
	}
	parts := strings.Split(relative, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("local plugin package path is not canonical")
		}
	}
	parentFD := int(root.Fd())
	ownedParent := false
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(parentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if ownedParent {
			_ = unix.Close(parentFD)
		}
		if err != nil {
			return nil, fmt.Errorf("open local plugin package subdirectory: %w", err)
		}
		parentFD, ownedParent = next, true
		var stat unix.Stat_t
		if err := unix.Fstat(parentFD, &stat); err != nil {
			_ = unix.Close(parentFD)
			return nil, err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || !safeLocalOwner(stat.Uid) || stat.Mode&0022 != 0 {
			_ = unix.Close(parentFD)
			return nil, fmt.Errorf("local plugin package subdirectory has unsafe ownership or permissions")
		}
	}
	fd, err := unix.Openat(parentFD, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if ownedParent {
		_ = unix.Close(parentFD)
	}
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || !safeLocalOwner(stat.Uid) || stat.Mode&0022 != 0 || stat.Size <= 0 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("local plugin package file must be a regular, owned file not writable by others")
	}
	if expectedOrMaxSize > 0 && stat.Size > expectedOrMaxSize {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("local plugin package file exceeds its declared size")
	}
	return os.NewFile(uintptr(fd), filepath.Join(parts...)), nil
}

func localArtifactRelativePath(raw string) (string, error) {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\x00?#") {
		return "", fmt.Errorf("local plugin artifact URL must be a canonical absolute path")
	}
	relative := strings.TrimPrefix(raw, "/")
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if clean != relative || clean == "." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("local plugin artifact path escapes its package directory")
	}
	return relative, nil
}
