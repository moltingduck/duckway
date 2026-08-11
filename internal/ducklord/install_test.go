package ducklord

import "testing"

func TestSafeRemoteInstallPath(t *testing.T) {
	for _, path := range []string{
		"~/.local/bin/ducklion",
		"/usr/local/bin/ducklion",
		"/opt/duckway/bin/ducklion",
	} {
		if !safeRemoteInstallPath(path) {
			t.Fatalf("path %q rejected", path)
		}
	}
	for _, path := range []string{
		"",
		"ducklion",
		"-/tmp/ducklion",
		"~/bin/duck lion",
		"~/bin/ducklion;id",
		"~/bin/$(id)",
		"~/bin/`id`",
	} {
		if safeRemoteInstallPath(path) {
			t.Fatalf("path %q accepted", path)
		}
	}
}
