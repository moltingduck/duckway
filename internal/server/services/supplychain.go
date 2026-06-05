package services

import (
	"fmt"
	"strconv"
	"time"
)

// Supply-chain hardening: append security settings to each package manager's
// rc/config file so agents (a) don't run install lifecycle scripts and (b)
// won't install versions newer than a cooldown window. This defends against
// the common npm/PyPI attack where a compromised maintainer publishes a
// malicious version that auto-installs (and runs postinstall scripts) within
// hours, before the registry yanks it. Skipping freshly-published versions and
// disabling install scripts removes the two highest-leverage vectors.
//
// The registry below is the single source of truth, shared by the server
// (admin toggles + the /client endpoint). Rolling-date cutoffs are re-rendered
// on every fetch so they stay fresh.

const (
	// settingMinAgeDays is the global cooldown in days (default 1).
	settingMinAgeDays = "supplychain.min_age_days"
	// settingEnabledPrefix + <id> → "1"/"0". Unset means enabled (default all-on).
	settingEnabledPrefix = "supplychain.enabled."

	defaultMinAgeDays = 1
)

// SupplyChainMitigation describes one package manager's hardening and where to
// write it.
type SupplyChainMitigation struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Manager string `json:"manager"`
	// RCPath is the config file (relative to $HOME) the client appends to,
	// e.g. ".npmrc". Empty for unsupported managers.
	RCPath string `json:"rc_path"`
	// Supported is false for managers we list so admins know we considered
	// them, but which have no rc-file age/script gate — they need a registry
	// proxy instead, so we never write a bogus setting for them.
	Supported   bool   `json:"supported"`
	Description string `json:"description"`

	// render produces the rc lines for this mitigation given the cooldown (in
	// days) and the current time. Unexported so it doesn't leak into JSON.
	render func(days int, now time.Time) []string
}

// RCLines returns the config lines this mitigation contributes to its rc file.
func (m SupplyChainMitigation) RCLines(days int, now time.Time) []string {
	if m.render == nil {
		return nil
	}
	return m.render(days, now)
}

func cutoffRFC3339(days int, now time.Time) string {
	return now.Add(-time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)
}

// SupplyChainMitigations is the registry of known mitigations. Add a row here
// to support a new package manager — everything else (admin UI, toggles,
// client rc writes) is driven off this list.
func SupplyChainMitigations() []SupplyChainMitigation {
	return []SupplyChainMitigation{
		{
			ID: "npm", Name: "npm", Manager: "npm", RCPath: ".npmrc", Supported: true,
			Description: "~/.npmrc: disable install scripts and pin resolution to versions published before the cutoff (before=).",
			render: func(days int, now time.Time) []string {
				return []string{
					"ignore-scripts=true",
					"before=" + cutoffRFC3339(days, now),
				}
			},
		},
		{
			ID: "pnpm", Name: "pnpm", Manager: "pnpm", RCPath: ".npmrc", Supported: true,
			Description: "~/.npmrc: disable install scripts and refuse versions published in the last N days (minimum-release-age, minutes).",
			render: func(days int, now time.Time) []string {
				return []string{
					"ignore-scripts=true",
					"minimum-release-age=" + strconv.Itoa(days*24*60),
				}
			},
		},
		{
			ID: "yarn", Name: "Yarn (Berry)", Manager: "yarn", RCPath: ".yarnrc.yml", Supported: true,
			Description: "~/.yarnrc.yml: disable install scripts (enableScripts: false). Yarn has no release-age gate.",
			render: func(days int, now time.Time) []string {
				return []string{"enableScripts: false"}
			},
		},
		{
			ID: "uv", Name: "uv (Python)", Manager: "uv", RCPath: ".config/uv/uv.toml", Supported: true,
			Description: "~/.config/uv/uv.toml: exclude packages uploaded after the cutoff (exclude-newer). Also applies to `uv pip`.",
			render: func(days int, now time.Time) []string {
				return []string{fmt.Sprintf("exclude-newer = %q", cutoffRFC3339(days, now))}
			},
		},
		{
			ID: "pip", Name: "pip (Python)", Manager: "pip", RCPath: "", Supported: false,
			Description: "pip has no rc-file age or script gate. Use `uv pip` (exclude-newer) for a cooldown instead.",
		},
		{
			ID: "bun", Name: "Bun", Manager: "bun", RCPath: "", Supported: false,
			Description: "Bun has no documented release-age rc setting (it already skips lifecycle scripts for untrusted deps). Needs a registry proxy for a cooldown.",
		},
	}
}

// SupplyChainMinAgeDays reads the configured cooldown in days, defaulting to 1.
// get is typically settingsQ.Get.
func SupplyChainMinAgeDays(get func(string) string) int {
	if v := get(settingMinAgeDays); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMinAgeDays
}

// SupplyChainEnabled reports whether a mitigation is on. Unset defaults to
// enabled, so the feature is all-on out of the box.
func SupplyChainEnabled(get func(string) string, id string) bool {
	switch get(settingEnabledPrefix + id) {
	case "0":
		return false
	default:
		return true
	}
}

// SettingKeySupplyChainEnabled returns the settings key for a mitigation's
// enabled flag, so callers (toggle endpoints) write the same key this package
// reads.
func SettingKeySupplyChainEnabled(id string) string { return settingEnabledPrefix + id }

// SettingKeySupplyChainMinAgeDays returns the settings key for the cooldown.
func SettingKeySupplyChainMinAgeDays() string { return settingMinAgeDays }

// ResolveSupplyChainRC returns the rc lines to write, grouped by rc file path
// (relative to $HOME). Managers that share a file (npm + pnpm → .npmrc) are
// merged into one block with duplicate lines (e.g. ignore-scripts=true)
// collapsed. Only supported, enabled mitigations contribute. This is what the
// client fetches and writes into each agent rc file.
func ResolveSupplyChainRC(get func(string) string, now time.Time) map[string][]string {
	days := SupplyChainMinAgeDays(get)
	byFile := map[string][]string{}
	seen := map[string]map[string]bool{}
	for _, m := range SupplyChainMitigations() {
		if !m.Supported || m.RCPath == "" || !SupplyChainEnabled(get, m.ID) {
			continue
		}
		for _, line := range m.RCLines(days, now) {
			if seen[m.RCPath] == nil {
				seen[m.RCPath] = map[string]bool{}
			}
			if seen[m.RCPath][line] {
				continue
			}
			seen[m.RCPath][line] = true
			byFile[m.RCPath] = append(byFile[m.RCPath], line)
		}
	}
	return byFile
}
