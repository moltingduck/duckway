package ducklioncli

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// hostLoginShell runs on the SSH host. Resolve the account database before
// consulting SHELL, which may have been overridden in the SSH command's environment.
func hostLoginShell() string {
	uid := strconv.Itoa(os.Geteuid())
	return resolveHostLoginShell(uid, runtime.GOOS, os.Getenv("SHELL"), os.ReadFile, func(path string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, path, args...).Output()
	}, func() (string, error) {
		account, err := user.LookupId(uid)
		if err != nil {
			return "", err
		}
		return account.Username, nil
	})
}

func resolveHostLoginShell(uid, goos, environmentShell string, readFile func(string) ([]byte, error), run func(string, ...string) ([]byte, error), username func() (string, error)) string {
	// getent includes directory-service/NSS accounts absent from /etc/passwd.
	for _, path := range []string{"/usr/bin/getent", "/bin/getent"} {
		if data, err := run(path, "passwd", uid); err == nil {
			if shell, ok := passwdLoginShell(string(data), uid); ok {
				return shell
			}
		}
	}
	if goos == "darwin" {
		if name, err := username(); err == nil && name != "" {
			if data, err := run("/usr/bin/dscl", ".", "-read", "/Users/"+name, "UserShell"); err == nil {
				if shell, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "UserShell:"); ok && filepath.IsAbs(strings.TrimSpace(shell)) {
					return strings.TrimSpace(shell)
				}
			}
		}
	}
	if data, err := readFile("/etc/passwd"); err == nil {
		if shell, ok := passwdLoginShell(string(data), uid); ok {
			return shell
		}
	}
	if filepath.IsAbs(environmentShell) {
		return environmentShell
	}
	return "/bin/sh"
}

func passwdLoginShell(data, uid string) (string, bool) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 7 || fields[2] != uid {
			continue
		}
		if fields[6] == "" {
			return "/bin/sh", true
		}
		if filepath.IsAbs(fields[6]) {
			return fields[6], true
		}
	}
	return "", false
}
