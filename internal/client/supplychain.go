package client

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Supply-chain hardening: fetch each package manager's rc-file settings from
// the server and merge them into a duckway-managed block in the corresponding
// agent rc file (~/.npmrc, ~/.yarnrc.yml, ~/.config/uv/uv.toml, …). The block
// is delimited by markers so re-running sync replaces it in place without
// touching the user's own settings.

const (
	scBlockStart = "# >>> duckway supply-chain hardening (managed — do not edit) >>>"
	scBlockEnd   = "# <<< duckway supply-chain hardening <<<"
)

// SupplyChainRCChange records what happened to one rc file during a sync.
type SupplyChainRCChange struct {
	Path   string   // display path, e.g. "~/.npmrc"
	Action string   // "written" | "removed" | "unchanged"
	Lines  []string // settings written (block body; empty for removed/unchanged)
}

// Settings returns the bare setting lines written (comments/blank lines
// stripped), for a compact human summary.
func (c SupplyChainRCChange) Settings() []string {
	var out []string
	for _, l := range c.Lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// SyncSupplyChainRC fetches the rc settings and writes them into each agent rc
// file under $HOME, returning what changed. Best-effort: a failure on one file
// is logged and the rest proceed. Returns nil when the server returns nothing
// (older server). When all mitigations are disabled it still runs, stripping
// any block we previously wrote (reported as "removed").
func SyncSupplyChainRC(cfg *Config) []SupplyChainRCChange {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	rc, err := api.FetchSupplyChainRC()
	if err != nil || rc == nil {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	var changes []SupplyChainRCChange
	// Apply to every file the server named, plus any registry file absent from
	// the response (mitigation turned off) so disabling removes its settings.
	for _, path := range allManagedRCPaths(rc) {
		full := filepath.Join(home, filepath.FromSlash(path))
		action, err := applyManagedRCFile(full, rc[path])
		if err != nil {
			log.Printf("supply-chain: %s: %v", path, err)
			continue
		}
		changes = append(changes, SupplyChainRCChange{
			Path:   "~/" + path,
			Action: action,
			Lines:  rc[path],
		})
	}
	return changes
}

// FormatSupplyChainChanges renders the changed files as user-facing lines for
// `duckway sync`. Unchanged files are omitted; returns nil when nothing
// changed (caller can print "up to date" or stay silent).
func FormatSupplyChainChanges(changes []SupplyChainRCChange) []string {
	var out []string
	for _, c := range changes {
		switch c.Action {
		case "written":
			out = append(out, fmt.Sprintf("  wrote   %s — %s", c.Path, strings.Join(c.Settings(), ", ")))
		case "removed":
			out = append(out, fmt.Sprintf("  cleared %s (mitigation disabled)", c.Path))
		}
	}
	return out
}

// SummarizeSupplyChainChanges is a one-line summary for daemon logs.
func SummarizeSupplyChainChanges(changes []SupplyChainRCChange) string {
	var wrote, cleared []string
	for _, c := range changes {
		switch c.Action {
		case "written":
			wrote = append(wrote, c.Path)
		case "removed":
			cleared = append(cleared, c.Path)
		}
	}
	if len(wrote) == 0 && len(cleared) == 0 {
		return "up to date"
	}
	var parts []string
	if len(wrote) > 0 {
		parts = append(parts, "wrote "+strings.Join(wrote, ", "))
	}
	if len(cleared) > 0 {
		parts = append(parts, "cleared "+strings.Join(cleared, ", "))
	}
	return strings.Join(parts, "; ")
}

// knownManagedRCPaths lists every rc file the registry may write, so we can
// strip a stale managed block when a mitigation is disabled. Kept in sync with
// the server registry (services.SupplyChainMitigations) by hand — a short,
// stable list.
var knownManagedRCPaths = []string{".npmrc", ".yarnrc.yml", ".config/uv/uv.toml", ".config/go/env"}

// allManagedRCPaths returns the union of the server-provided paths and the
// known registry paths, so files whose mitigation was disabled get cleaned up.
func allManagedRCPaths(rc map[string][]string) []string {
	set := map[string]bool{}
	for p := range rc {
		set[p] = true
	}
	for _, p := range knownManagedRCPaths {
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// applyManagedRCFile writes lines into the duckway-managed block of the file at
// path, preserving user content outside the block. Empty lines removes the
// block (and the file if it becomes empty). Returns the action taken:
// "written", "removed", or "unchanged".
func applyManagedRCFile(path string, lines []string) (string, error) {
	existing := ""
	if data, err := os.ReadFile(path); err == nil {
		existing = string(data)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read: %w", err)
	}

	// TOML is order-sensitive: bare root keys must precede any [table], so
	// prepend the block there. INI/YAML rc files are order-insensitive for
	// distinct keys, so append.
	prepend := strings.HasSuffix(path, ".toml")
	updated := replaceManagedBlock(existing, lines, prepend)

	if updated == existing {
		return "unchanged", nil
	}

	if strings.TrimSpace(updated) == "" {
		// Only our block was in the file — remove it entirely.
		if err := os.Remove(path); err != nil {
			return "", fmt.Errorf("remove: %w", err)
		}
		return "removed", nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if len(lines) == 0 {
		return "removed", nil // block stripped, user content remained
	}
	return "written", nil
}

// replaceManagedBlock returns existing with the duckway-managed block replaced
// by one containing lines (removed entirely when lines is empty). User content
// outside the markers is preserved. prepend places a freshly-added block at the
// top of the file; otherwise it goes at the bottom.
func replaceManagedBlock(existing string, lines []string, prepend bool) string {
	stripped := stripManagedBlock(existing)

	if len(lines) == 0 {
		return stripped
	}

	var b strings.Builder
	b.WriteString(scBlockStart + "\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	b.WriteString(scBlockEnd + "\n")
	block := b.String()

	stripped = strings.TrimRight(stripped, "\n")
	if stripped == "" {
		return block
	}
	if prepend {
		return block + "\n" + stripped + "\n"
	}
	return stripped + "\n\n" + block
}

// stripManagedBlock removes the marked block (inclusive) from s, if present,
// and tidies the surrounding blank lines.
func stripManagedBlock(s string) string {
	start := strings.Index(s, scBlockStart)
	if start < 0 {
		return s
	}
	endMarker := strings.Index(s, scBlockEnd)
	if endMarker < 0 {
		// Corrupt/truncated block — drop from the start marker onward.
		return strings.TrimRight(s[:start], "\n")
	}
	end := endMarker + len(scBlockEnd)
	// Consume a trailing newline after the end marker if present.
	if end < len(s) && s[end] == '\n' {
		end++
	}
	before := strings.TrimRight(s[:start], "\n")
	after := strings.TrimLeft(s[end:], "\n")
	switch {
	case before == "" && after == "":
		return ""
	case before == "":
		return after
	case after == "":
		return before
	default:
		return before + "\n" + after
	}
}
