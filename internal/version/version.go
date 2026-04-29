// Package version exposes the build-time version info embedded by `go build`.
// Uses runtime/debug.ReadBuildInfo so no -ldflags wiring is required for local
// builds. Docker builds (where .git isn't readable due to UID mismatch) can
// override via -ldflags="-X .../version.Embedded=<value>".
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Embedded is set at build time via -ldflags. When non-empty it takes
// precedence over the auto-detected VCS info. Set in the Dockerfile to e.g.
// the short commit hash since Go's auto-stamping fails on bind-mounted .git.
var Embedded = ""

// Get returns a single-line version string suitable for `--version` output:
//
//	v1.2.3 (commit abc1234, built 2026-04-30, go1.25)         — go install with module version
//	commit abc1234 (built 2026-04-30, go1.25)                 — go build from a clean checkout
//	commit abc1234-dirty (built 2026-04-30, go1.25)           — uncommitted changes present
//	dev (go1.25)                                              — no VCS info available
func Get() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		if Embedded != "" {
			return Embedded
		}
		return "dev"
	}

	// If a build-time -ldflags injection set Embedded, prefer it (used for
	// docker builds where VCS auto-stamping doesn't work).
	if Embedded != "" {
		return fmt.Sprintf("%s (%s)", Embedded, info.GoVersion)
	}

	var rev, when, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			when = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}

	parts := []string{}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		parts = append(parts, info.Main.Version)
	}
	if rev != "" {
		short := rev
		if len(short) > 7 {
			short = short[:7]
		}
		if modified == "true" {
			short += "-dirty"
		}
		parts = append(parts, "commit "+short)
	}
	if when != "" {
		// vcs.time is RFC3339; trim to date only
		if i := strings.Index(when, "T"); i > 0 {
			when = when[:i]
		}
		parts = append(parts, "built "+when)
	}
	parts = append(parts, info.GoVersion)

	if len(parts) == 1 {
		return "dev (" + info.GoVersion + ")"
	}

	// Join: first part is version/commit, rest go in parentheses
	first := parts[0]
	rest := strings.Join(parts[1:], ", ")
	return fmt.Sprintf("%s (%s)", first, rest)
}
