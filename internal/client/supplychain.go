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

// SyncSupplyChainRC fetches the rc settings and writes them into each agent rc
// file under $HOME. Best-effort: a failure on one file is logged and the rest
// proceed. No-op when the server returns nothing (older server or all disabled
// — in the all-disabled case it strips any block we previously wrote).
func SyncSupplyChainRC(cfg *Config) {
	api := NewAPIClient(cfg.ServerURL, cfg.Token)
	rc, err := api.FetchSupplyChainRC()
	if err != nil || rc == nil {
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	// Apply to every file the server named. Also clear our block from rc files
	// known to the registry but absent from the response (mitigation turned
	// off), so disabling actually removes the settings.
	for _, path := range allManagedRCPaths(rc) {
		full := filepath.Join(home, filepath.FromSlash(path))
		if err := applyManagedRCFile(full, rc[path]); err != nil {
			log.Printf("supply-chain: %s: %v", path, err)
		}
	}
}

// knownManagedRCPaths lists every rc file the registry may write, so we can
// strip a stale managed block when a mitigation is disabled. Kept in sync with
// the server registry (services.SupplyChainMitigations) by hand — a short,
// stable list.
var knownManagedRCPaths = []string{".npmrc", ".yarnrc.yml", ".config/uv/uv.toml"}

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
// path. Empty lines removes the managed block entirely (and the file if it
// becomes empty). Existing user content outside the block is preserved.
func applyManagedRCFile(path string, lines []string) error {
	existing := ""
	if data, err := os.ReadFile(path); err == nil {
		existing = string(data)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read: %w", err)
	}

	// TOML is order-sensitive: bare root keys must precede any [table], so
	// prepend the block there. INI/YAML rc files are order-insensitive for
	// distinct keys, so append.
	prepend := strings.HasSuffix(path, ".toml")
	updated := replaceManagedBlock(existing, lines, prepend)

	if strings.TrimSpace(updated) == "" {
		// Nothing left — remove the file if it exists (don't leave an empty one).
		if existing != "" {
			_ = os.Remove(path)
		}
		return nil
	}
	if updated == existing {
		return nil // no change
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	log.Printf("supply-chain: updated %s", path)
	return nil
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
