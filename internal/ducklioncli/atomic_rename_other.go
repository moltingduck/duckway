//go:build !linux

package ducklioncli

import (
	"fmt"
	"os"
)

func renameNoReplace(src, dst string) error {
	i, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !i.Mode().IsRegular() {
		return fmt.Errorf("atomic no-replace directory move unsupported on this platform")
	}
	if err := os.Link(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}
