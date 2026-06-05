package services

import (
	"testing"
	"time"
)

func mapGet(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestRCLines(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	by := map[string]SupplyChainMitigation{}
	for _, m := range SupplyChainMitigations() {
		by[m.ID] = m
	}

	// npm: ignore-scripts + before=<cutoff>
	npm := by["npm"].RCLines(3, now)
	if len(npm) != 2 || npm[0] != "ignore-scripts=true" || npm[1] != "before=2026-06-02T12:00:00Z" {
		t.Errorf("npm rc lines = %v", npm)
	}
	// pnpm: minutes = days*1440
	pnpm := by["pnpm"].RCLines(3, now)
	if pnpm[1] != "minimum-release-age=4320" {
		t.Errorf("pnpm rc lines = %v", pnpm)
	}
	// uv: TOML exclude-newer
	uv := by["uv"].RCLines(1, now)
	if uv[0] != `exclude-newer = "2026-06-04T12:00:00Z"` {
		t.Errorf("uv rc lines = %v", uv)
	}
	// unsupported managers contribute nothing
	if l := by["pip"].RCLines(1, now); l != nil {
		t.Errorf("pip should render no lines, got %v", l)
	}
}

func TestSupplyChainDefaultsAllOn(t *testing.T) {
	get := mapGet(map[string]string{})
	if SupplyChainMinAgeDays(get) != 1 {
		t.Errorf("default min age = %d, want 1", SupplyChainMinAgeDays(get))
	}
	for _, m := range SupplyChainMitigations() {
		if !SupplyChainEnabled(get, m.ID) {
			t.Errorf("mitigation %s should default to enabled", m.ID)
		}
	}
}

func TestSupplyChainToggleAndMinAge(t *testing.T) {
	get := mapGet(map[string]string{
		SettingKeySupplyChainEnabled("npm"): "0",
		SettingKeySupplyChainMinAgeDays():   "3",
	})
	if SupplyChainEnabled(get, "npm") {
		t.Error("npm should be disabled when set to 0")
	}
	if !SupplyChainEnabled(get, "pnpm") {
		t.Error("pnpm should still be enabled (unset)")
	}
	if SupplyChainMinAgeDays(get) != 3 {
		t.Errorf("min age = %d, want 3", SupplyChainMinAgeDays(get))
	}
}

func TestResolveSupplyChainRC(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	rc := ResolveSupplyChainRC(mapGet(map[string]string{}), now)

	// npm + pnpm share .npmrc; ignore-scripts=true is deduped to one line.
	npmrc := rc[".npmrc"]
	count := 0
	for _, l := range npmrc {
		if l == "ignore-scripts=true" {
			count++
		}
	}
	if count != 1 {
		t.Errorf(".npmrc should contain ignore-scripts=true exactly once, got %v", npmrc)
	}
	wantInNpmrc := map[string]bool{
		"ignore-scripts=true":         false,
		"before=2026-06-04T12:00:00Z": false,
		"minimum-release-age=1440":    false,
	}
	for _, l := range npmrc {
		if _, ok := wantInNpmrc[l]; ok {
			wantInNpmrc[l] = true
		}
	}
	for l, found := range wantInNpmrc {
		if !found {
			t.Errorf(".npmrc missing %q (got %v)", l, npmrc)
		}
	}

	if rc[".yarnrc.yml"][0] != "enableScripts: false" {
		t.Errorf(".yarnrc.yml = %v", rc[".yarnrc.yml"])
	}
	if rc[".config/uv/uv.toml"][0] != `exclude-newer = "2026-06-04T12:00:00Z"` {
		t.Errorf("uv.toml = %v", rc[".config/uv/uv.toml"])
	}

	// Disabling npm drops before= but pnpm keeps ignore-scripts + minimum-release-age.
	rc = ResolveSupplyChainRC(mapGet(map[string]string{
		SettingKeySupplyChainEnabled("npm"): "0",
	}), now)
	for _, l := range rc[".npmrc"] {
		if l == "before=2026-06-04T12:00:00Z" {
			t.Error("before= should be gone when npm disabled")
		}
	}
	hasMinAge := false
	for _, l := range rc[".npmrc"] {
		if l == "minimum-release-age=1440" {
			hasMinAge = true
		}
	}
	if !hasMinAge {
		t.Errorf("pnpm minimum-release-age should remain, got %v", rc[".npmrc"])
	}
}
