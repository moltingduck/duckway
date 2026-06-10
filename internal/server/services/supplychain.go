package services

import (
	"fmt"
	"strconv"
	"time"
)

// Supply-chain hardening: append security settings to each package manager's
// rc/config file so agents (a) don't run install lifecycle scripts, (b) won't
// install versions newer than a cooldown window, and (c) for npm, won't pull
// dependencies straight from git or arbitrary URLs. This defends against the
// common npm/PyPI attack where a compromised maintainer publishes a malicious
// version that auto-installs (and runs postinstall scripts) within hours,
// before the registry yanks it.
//
// The registry below is the single source of truth, shared by the server
// (admin toggles + the /client endpoint). Rolling-date cutoffs are re-rendered
// on every fetch so they stay fresh.

const (
	// settingMinAgeDays is the global cooldown in days (default 3). Editable
	// from the admin panel (Supply Chain → Minimum package age).
	settingMinAgeDays = "supplychain.min_age_days"
	// settingEnabledPrefix + <id> → "1"/"0". Unset means enabled (default all-on).
	settingEnabledPrefix = "supplychain.enabled."

	defaultMinAgeDays = 3
)

// rcEntry is one config setting plus its explanatory comment. key is used to
// dedupe across managers that share a file (npm + pnpm both write ~/.npmrc and
// both set ignore-scripts — it should appear once).
type rcEntry struct {
	key     string
	comment string
	line    string
}

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

	// render produces the rc entries for this mitigation given the cooldown
	// (in days) and the current time. Unexported so it doesn't leak into JSON.
	render func(days int, now time.Time) []rcEntry
}

// RCLines returns the flat config lines (comment then setting, per entry) this
// mitigation contributes — used for the admin preview of a single manager.
func (m SupplyChainMitigation) RCLines(days int, now time.Time) []string {
	if m.render == nil {
		return nil
	}
	var out []string
	for _, e := range m.render(days, now) {
		if e.comment != "" {
			out = append(out, e.comment)
		}
		out = append(out, e.line)
	}
	return out
}

// SupplyChainMitigations is the registry of known mitigations. Add a row here
// to support a new package manager — everything else (admin UI, toggles,
// client rc writes) is driven off this list.
func SupplyChainMitigations() []SupplyChainMitigation {
	return []SupplyChainMitigation{
		{
			ID: "npm", Name: "npm", Manager: "npm", RCPath: ".npmrc", Supported: true,
			Description: "~/.npmrc: blocks all pre/post-install scripts (ignore-scripts), refuses packages published within the cooldown window (min-release-age), and forbids git:// and direct-URL installs.",
			render: func(days int, now time.Time) []rcEntry {
				return []rcEntry{
					{key: "ignore-scripts", comment: "# 不執行 postinstall 等腳本", line: "ignore-scripts=true"},
					{key: "min-release-age", comment: fmt.Sprintf("# 不下載 %d 天內發布的套件", days), line: "min-release-age=" + strconv.Itoa(days)},
					{key: "allow-git", comment: "# 關閉 git 下載", line: "allow-git=none"},
					{key: "allow-remote", comment: "# 關閉 direct URL 下載", line: "allow-remote=none"},
				}
			},
		},
		{
			ID: "pnpm", Name: "pnpm", Manager: "pnpm", RCPath: ".npmrc", Supported: true,
			Description: "~/.npmrc: blocks all pre/post-install scripts (ignore-scripts) and refuses packages published within the cooldown window (minimum-release-age, stored in minutes). Shares the .npmrc file with npm; deduped keys appear once.",
			render: func(days int, now time.Time) []rcEntry {
				return []rcEntry{
					{key: "ignore-scripts", comment: "# 不執行 postinstall 等腳本", line: "ignore-scripts=true"},
					{key: "minimum-release-age", comment: fmt.Sprintf("# pnpm：拒絕 %d 天內發布的版本（分鐘）", days), line: "minimum-release-age=" + strconv.Itoa(days*24*60)},
				}
			},
		},
		{
			ID: "yarn", Name: "Yarn (Berry)", Manager: "yarn", RCPath: ".yarnrc.yml", Supported: true,
			Description: "~/.yarnrc.yml: blocks all pre/post-install lifecycle scripts (enableScripts: false). Yarn (Berry) has no built-in release-age gate.",
			render: func(days int, now time.Time) []rcEntry {
				return []rcEntry{
					{key: "enableScripts", comment: "# 不執行 install 腳本", line: "enableScripts: false"},
				}
			},
		},
		{
			ID: "uv", Name: "uv (Python)", Manager: "uv", RCPath: ".config/uv/uv.toml", Supported: true,
			Description: "~/.config/uv/uv.toml: refuses packages uploaded within the cooldown window (exclude-newer, relative ISO-8601 duration — refreshed each sync). Python packages do not run lifecycle scripts; also applies to `uv pip`.",
			render: func(days int, now time.Time) []rcEntry {
				return []rcEntry{
					{key: "exclude-newer", comment: fmt.Sprintf("# 排除 %d 天內上傳的套件", days), line: fmt.Sprintf("exclude-newer = \"P%dD\"", days)},
				}
			},
		},
		{
			ID: "pip", Name: "pip (Python)", Manager: "pip", RCPath: "", Supported: false,
			Description: "pip has no rc-file age or script gate. Switch to `uv pip` to get the exclude-newer cooldown from the uv mitigation above.",
		},
		{
			ID: "bun", Name: "Bun", Manager: "bun", RCPath: "", Supported: false,
			Description: "Bun already skips pre/post-install scripts for packages not listed in trustedDependencies — script blocking is on by default. No rc-file release-age gate exists; needs a registry proxy for a cooldown.",
		},
		{
			ID: "gomod", Name: "Go Modules", Manager: "go", RCPath: ".config/go/env", Supported: true,
			Description: "~/.config/go/env: explicitly clear GONOSUMDB and GOPRIVATE so all module downloads are verified via the Go checksum database with no exceptions. Go modules do not run install scripts; no release-age gate is available (the checksum DB provides immutability guarantees instead).",
			render: func(days int, now time.Time) []rcEntry {
				return []rcEntry{
					{key: "GONOSUMDB", comment: "# 所有模組必須經由 checksum DB 驗證（不允許例外）", line: "GONOSUMDB="},
					{key: "GOPRIVATE", comment: "# 不允許私有模組繞過 proxy 與 checksum DB", line: "GOPRIVATE="},
				}
			},
		},
		{
			ID: "cargo", Name: "Cargo (Rust)", Manager: "cargo", RCPath: "", Supported: false,
			Description: "Cargo runs build.rs scripts during `cargo build` and there is no config option to disable them globally. Use cargo-deny to audit and allowlist dependencies, and cargo-audit to check for known vulnerabilities.",
		},
		{
			ID: "maven", Name: "Maven (Java)", Manager: "mvn", RCPath: "", Supported: false,
			Description: "Maven plugin lifecycle phases execute arbitrary code and cannot be disabled via ~/.m2/settings.xml. Use the Maven Wrapper (mvnw) with verified checksums to pin the build tool itself.",
		},
		{
			ID: "gradle", Name: "Gradle (Java/Kotlin)", Manager: "gradle", RCPath: "", Supported: false,
			Description: "Gradle build scripts are arbitrary Groovy/Kotlin and cannot be gated via ~/.gradle/gradle.properties. Use the Gradle Wrapper (gradlew) with a verified checksum and dependency verification metadata.",
		},
		{
			ID: "composer", Name: "Composer (PHP)", Manager: "composer", RCPath: "", Supported: false,
			Description: "Composer runs pre/post-install scripts defined in composer.json. The global config is JSON (incompatible with the rc managed-block approach). Pass --no-scripts in CI and run `composer audit` to check for known vulnerabilities.",
		},
		{
			ID: "bundler", Name: "Bundler (Ruby)", Manager: "bundle", RCPath: "", Supported: false,
			Description: "RubyGems has no rc-file script gate or release-age cooldown. Gem trust-policy requires signed gems, but very few are signed. Use bundler-audit for dependency vulnerability scanning.",
		},
		{
			ID: "nuget", Name: "NuGet (.NET)", Manager: "dotnet", RCPath: "", Supported: false,
			Description: "NuGet packages do not run install scripts in the modern .NET SDK. MSBuild targets may execute code; use package lock files and central package management (Directory.Packages.props) to pin versions.",
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
// merged into one block, deduped by setting key (so ignore-scripts appears
// once) and separated by blank lines for readability. Only supported, enabled
// mitigations contribute. This is what the client fetches and writes into each
// agent rc file.
func ResolveSupplyChainRC(get func(string) string, now time.Time) map[string][]string {
	days := SupplyChainMinAgeDays(get)
	byFile := map[string][]string{}
	seen := map[string]map[string]bool{}
	for _, m := range SupplyChainMitigations() {
		if !m.Supported || m.RCPath == "" || !SupplyChainEnabled(get, m.ID) {
			continue
		}
		if seen[m.RCPath] == nil {
			seen[m.RCPath] = map[string]bool{}
		}
		for _, e := range m.render(days, now) {
			if seen[m.RCPath][e.key] {
				continue
			}
			seen[m.RCPath][e.key] = true
			if len(byFile[m.RCPath]) > 0 {
				byFile[m.RCPath] = append(byFile[m.RCPath], "") // blank separator between settings
			}
			if e.comment != "" {
				byFile[m.RCPath] = append(byFile[m.RCPath], e.comment)
			}
			byFile[m.RCPath] = append(byFile[m.RCPath], e.line)
		}
	}
	return byFile
}
