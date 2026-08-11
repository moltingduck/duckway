package client

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func findDucklionExecutable() (string, error) {
	if path, err := exec.LookPath("ducklion"); err == nil {
		return path, nil
	}
	exe, err := os.Executable()
	if err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "ducklion")
		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0 {
			return sibling, nil
		}
	}
	return "", fmt.Errorf("ducklion not found in PATH or beside duckway; install standalone ducklion or run cc watch with --tmux/DUCKWAY_CC_USE_TMUX=1")
}
