package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceManagedBlock_AppendToExisting(t *testing.T) {
	existing := "registry=https://my.registry/\n//my.registry/:_authToken=secret\n"
	out := replaceManagedBlock(existing, []string{"ignore-scripts=true", "before=2026-06-04T12:00:00Z"}, false)

	// User content preserved.
	if !strings.Contains(out, "registry=https://my.registry/") || !strings.Contains(out, "_authToken=secret") {
		t.Fatalf("user content lost:\n%s", out)
	}
	// Block present with markers and lines.
	if !strings.Contains(out, scBlockStart) || !strings.Contains(out, scBlockEnd) {
		t.Fatalf("markers missing:\n%s", out)
	}
	if !strings.Contains(out, "ignore-scripts=true") || !strings.Contains(out, "before=2026-06-04T12:00:00Z") {
		t.Fatalf("managed lines missing:\n%s", out)
	}
	// User content comes before the managed block (append).
	if strings.Index(out, "registry=") > strings.Index(out, scBlockStart) {
		t.Fatalf("expected user content before block:\n%s", out)
	}
}

func TestReplaceManagedBlock_Idempotent(t *testing.T) {
	existing := "registry=https://my.registry/\n"
	lines := []string{"ignore-scripts=true"}
	once := replaceManagedBlock(existing, lines, false)
	twice := replaceManagedBlock(once, lines, false)
	if once != twice {
		t.Fatalf("not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	// Exactly one block.
	if strings.Count(twice, scBlockStart) != 1 {
		t.Fatalf("expected one block, got %d:\n%s", strings.Count(twice, scBlockStart), twice)
	}
}

func TestReplaceManagedBlock_UpdateChangesOnlyBlock(t *testing.T) {
	base := "user=1\n"
	v1 := replaceManagedBlock(base, []string{"before=2026-06-01T00:00:00Z"}, false)
	v2 := replaceManagedBlock(v1, []string{"before=2026-06-05T00:00:00Z"}, false)
	if strings.Contains(v2, "2026-06-01") {
		t.Fatalf("stale value not replaced:\n%s", v2)
	}
	if !strings.Contains(v2, "user=1") {
		t.Fatalf("user content lost on update:\n%s", v2)
	}
	if strings.Count(v2, scBlockStart) != 1 {
		t.Fatalf("expected one block after update:\n%s", v2)
	}
}

func TestReplaceManagedBlock_EmptyRemovesBlock(t *testing.T) {
	withBlock := replaceManagedBlock("user=1\n", []string{"ignore-scripts=true"}, false)
	cleared := replaceManagedBlock(withBlock, nil, false)
	if strings.Contains(cleared, scBlockStart) || strings.Contains(cleared, "ignore-scripts") {
		t.Fatalf("block should be gone:\n%q", cleared)
	}
	if !strings.Contains(cleared, "user=1") {
		t.Fatalf("user content lost:\n%q", cleared)
	}
}

func TestReplaceManagedBlock_PrependForToml(t *testing.T) {
	existing := "[tool.uv]\nindex-url = \"https://pypi.org/simple\"\n"
	out := replaceManagedBlock(existing, []string{`exclude-newer = "2026-06-04T12:00:00Z"`}, true)
	// The managed root key must come BEFORE the [tool.uv] table for valid TOML.
	if strings.Index(out, scBlockStart) > strings.Index(out, "[tool.uv]") {
		t.Fatalf("toml block must be prepended before tables:\n%s", out)
	}
}

func TestApplyManagedRCFile_CreatesAndClears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", ".npmrc") // parent dir doesn't exist yet

	action, err := applyManagedRCFile(path, []string{"ignore-scripts=true", "min-release-age=3"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if action != "written" {
		t.Errorf("action = %q, want written", action)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !strings.Contains(string(data), "ignore-scripts=true") {
		t.Fatalf("settings not written:\n%s", data)
	}

	// Re-applying the same lines is a no-op ("unchanged").
	if action, _ := applyManagedRCFile(path, []string{"ignore-scripts=true", "min-release-age=3"}); action != "unchanged" {
		t.Errorf("re-apply action = %q, want unchanged", action)
	}

	// Clearing (empty lines) on a file that only held our block removes the file.
	action, err = applyManagedRCFile(path, nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if action != "removed" {
		t.Errorf("clear action = %q, want removed", action)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should be removed when only the managed block remained")
	}
}

func TestApplyManagedRCFile_PreservesUserContentOnClear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".npmrc")
	if err := os.WriteFile(path, []byte("registry=https://r/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := applyManagedRCFile(path, []string{"ignore-scripts=true"}); err != nil {
		t.Fatal(err)
	}
	if action, err := applyManagedRCFile(path, nil); err != nil {
		t.Fatal(err)
	} else if action != "removed" {
		t.Errorf("clear-with-user-content action = %q, want removed", action)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "registry=https://r/") {
		t.Fatalf("user content lost on clear:\n%s", data)
	}
	if strings.Contains(string(data), scBlockStart) {
		t.Fatalf("managed block should be gone:\n%s", data)
	}
}

func TestFormatAndSummarizeChanges(t *testing.T) {
	changes := []SupplyChainRCChange{
		{Path: "~/.npmrc", Action: "written", Lines: []string{"# c", "ignore-scripts=true", "", "min-release-age=3"}},
		{Path: "~/.config/uv/uv.toml", Action: "unchanged"},
		{Path: "~/.yarnrc.yml", Action: "removed"},
	}

	// Settings() strips comments/blanks.
	got := changes[0].Settings()
	if len(got) != 2 || got[0] != "ignore-scripts=true" || got[1] != "min-release-age=3" {
		t.Errorf("Settings() = %v", got)
	}

	// Format: written shows settings, removed shows cleared, unchanged omitted.
	lines := FormatSupplyChainChanges(changes)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "wrote   ~/.npmrc — ignore-scripts=true, min-release-age=3") {
		t.Errorf("written line wrong:\n%s", joined)
	}
	if !strings.Contains(joined, "cleared ~/.yarnrc.yml") {
		t.Errorf("removed line missing:\n%s", joined)
	}
	if strings.Contains(joined, "uv.toml") {
		t.Errorf("unchanged file should be omitted:\n%s", joined)
	}

	if s := SummarizeSupplyChainChanges(changes); s != "wrote ~/.npmrc; cleared ~/.yarnrc.yml" {
		t.Errorf("summary = %q", s)
	}
	if s := SummarizeSupplyChainChanges([]SupplyChainRCChange{{Action: "unchanged"}}); s != "up to date" {
		t.Errorf("all-unchanged summary = %q, want 'up to date'", s)
	}
}
