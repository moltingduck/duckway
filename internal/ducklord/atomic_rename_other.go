//go:build !linux && !darwin

package ducklord

import (
	"fmt"
	"os"
)

// Non-Linux platforms have no portable atomic no-replace rename. Hard-linking
// regular files preserves no-clobber semantics; directory replacement is
// rejected because it cannot be made atomic portably.
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

func renameNoReplaceAt(dir *os.File, src, dst string) error { return renameNoReplace(src, dst) }
