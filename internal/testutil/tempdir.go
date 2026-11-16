// Package testutil holds helpers shared by Duckway tests.
package testutil

import (
	"os"
	"testing"
)

// maxTempRoot is the longest temp root that still leaves room for the
// per-test directory plus a socket name inside the 104-108 byte sun_path
// limit of unix domain sockets.
const maxTempRoot = 24

// ShortTempRoot points TMPDIR at a short directory when the current temp root
// is long enough to push unix socket paths over the sun_path limit. It returns
// a cleanup function that restores TMPDIR and removes the directory. Call it
// from TestMain, before m.Run, and defer or call the cleanup before os.Exit.
func ShortTempRoot() func() {
	if len(os.TempDir()) <= maxTempRoot {
		return func() {}
	}
	dir, err := os.MkdirTemp("/tmp", "dw-")
	if err != nil {
		return func() {}
	}
	old, had := os.LookupEnv("TMPDIR")
	_ = os.Setenv("TMPDIR", dir)
	return func() {
		if had {
			_ = os.Setenv("TMPDIR", old)
		} else {
			_ = os.Unsetenv("TMPDIR")
		}
		_ = os.RemoveAll(dir)
	}
}

// RunWithShortTempRoot runs the package tests with a short temp root. Use it
// as the last statement of TestMain: os.Exit(testutil.RunWithShortTempRoot(m)).
func RunWithShortTempRoot(m *testing.M) int {
	cleanup := ShortTempRoot()
	defer cleanup()
	return m.Run()
}

// ShortDir returns a short per-test directory suitable for unix socket paths,
// removed on test cleanup. It uses t.TempDir() when that path is short enough.
func ShortDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir) <= 60 {
		return dir
	}
	short, err := os.MkdirTemp("/tmp", "dw-")
	if err != nil {
		return dir
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	return short
}
