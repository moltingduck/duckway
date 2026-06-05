package services

import (
	"strings"
	"testing"
	"time"
)

func mapGet(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func mitByID(id string) SupplyChainMitigation {
	for _, m := range SupplyChainMitigations() {
		if m.ID == id {
			return m
		}
	}
	return SupplyChainMitigation{}
}

func TestRCLines_RelativeAndCommented(t *testing.T) {
	now := time.Now() // not used by any renderer — all values are relative

	npm := strings.Join(mitByID("npm").RCLines(3, now), "\n")
	for _, want := range []string{"ignore-scripts=true", "min-release-age=3", "allow-git=none", "allow-remote=none"} {
		if !strings.Contains(npm, want) {
			t.Errorf("npm missing %q in:\n%s", want, npm)
		}
	}
	// No absolute timestamp anywhere.
	if strings.Contains(npm, "T") && strings.Contains(npm, "Z") {
		t.Errorf("npm should carry no absolute timestamp:\n%s", npm)
	}

	pnpm := strings.Join(mitByID("pnpm").RCLines(3, now), "\n")
	if !strings.Contains(pnpm, "minimum-release-age=4320") { // 3 days → minutes
		t.Errorf("pnpm minutes wrong:\n%s", pnpm)
	}

	uv := strings.Join(mitByID("uv").RCLines(3, now), "\n")
	if !strings.Contains(uv, `exclude-newer = "P3D"`) { // relative ISO-8601 duration
		t.Errorf("uv should use relative duration P3D:\n%s", uv)
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

func TestResolveSupplyChainRC_MergeDedupeByKey(t *testing.T) {
	now := time.Now()

	npmrc := strings.Join(ResolveSupplyChainRC(mapGet(map[string]string{}), now)[".npmrc"], "\n")

	// ignore-scripts shared by npm+pnpm appears exactly once (dedupe by key).
	if c := strings.Count(npmrc, "ignore-scripts=true"); c != 1 {
		t.Errorf("ignore-scripts should appear once, got %d:\n%s", c, npmrc)
	}
	for _, want := range []string{
		"min-release-age=1", "allow-git=none", "allow-remote=none", "minimum-release-age=1440",
	} {
		if !strings.Contains(npmrc, want) {
			t.Errorf(".npmrc missing %q:\n%s", want, npmrc)
		}
	}

	rc := ResolveSupplyChainRC(mapGet(map[string]string{}), now)
	if strings.Join(rc[".yarnrc.yml"], "\n") != "# 不執行 install 腳本\nenableScripts: false" {
		t.Errorf(".yarnrc.yml = %v", rc[".yarnrc.yml"])
	}
	if !strings.Contains(strings.Join(rc[".config/uv/uv.toml"], "\n"), `exclude-newer = "P1D"`) {
		t.Errorf("uv.toml = %v", rc[".config/uv/uv.toml"])
	}

	// Disable npm: its keys drop, pnpm still supplies ignore-scripts + min age.
	npmrc = strings.Join(ResolveSupplyChainRC(mapGet(map[string]string{
		SettingKeySupplyChainEnabled("npm"): "0",
	}), now)[".npmrc"], "\n")
	for _, gone := range []string{"min-release-age=1", "allow-git=none", "allow-remote=none"} {
		if strings.Contains(npmrc, gone) {
			t.Errorf("%q should be gone when npm disabled:\n%s", gone, npmrc)
		}
	}
	if !strings.Contains(npmrc, "ignore-scripts=true") || !strings.Contains(npmrc, "minimum-release-age=1440") {
		t.Errorf("pnpm settings should remain when npm disabled:\n%s", npmrc)
	}
}
