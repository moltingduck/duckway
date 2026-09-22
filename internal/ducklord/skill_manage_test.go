package ducklord

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenameManagedSkillMigratesLegacyAndTargetState(t *testing.T) {
	repository := t.TempDir()
	oldPath := filepath.Join(repository, "old-skill")
	if err := os.Mkdir(oldPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPath, "SKILL.md"), []byte("# old"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{SkillSources: []SkillTrackingSource{{SkillID: "old-skill", URL: "https://example.test/old"}}, Clients: []Client{{
		Name:           "host",
		Host:           "example",
		SelectedSkills: []string{"old-skill", "other"},
		SkillTargets: []SkillInstallTarget{{ID: "codex", Path: "/tmp/codex", Skills: []ManagedHostSkill{
			{ID: "old-skill", Management: "push"},
			{ID: "other", Management: "none"},
		}}},
	}}}

	if err := RenameManagedSkill(repository, cfg, "old-skill", "new-skill"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, "new-skill", "SKILL.md")); err != nil {
		t.Fatalf("renamed skill missing: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old skill still exists: %v", err)
	}
	if got := cfg.Clients[0].SelectedSkills[0]; got != "new-skill" {
		t.Fatalf("legacy selected skill = %q", got)
	}
	if got := cfg.SkillSources[0].SkillID; got != "new-skill" {
		t.Fatalf("tracked source skill = %q", got)
	}
	if got := cfg.Clients[0].SkillTargets[0].Skills[0].ID; got != "new-skill" {
		t.Fatalf("target skill = %q", got)
	}
}

func TestRenameManagedSkillConflictLeavesStateUnchanged(t *testing.T) {
	repository := t.TempDir()
	for _, id := range []string{"old-skill", "new-skill"} {
		if err := os.Mkdir(filepath.Join(repository, id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{Clients: []Client{{SelectedSkills: []string{"old-skill"}, SkillTargets: []SkillInstallTarget{{Skills: []ManagedHostSkill{{ID: "old-skill", Management: "pull"}}}}}}}
	err := RenameManagedSkill(repository, cfg, "old-skill", "new-skill")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if cfg.Clients[0].SelectedSkills[0] != "old-skill" || cfg.Clients[0].SkillTargets[0].Skills[0].ID != "old-skill" {
		t.Fatal("configuration changed on rename conflict")
	}
	if _, err := os.Stat(filepath.Join(repository, "old-skill")); err != nil {
		t.Fatalf("old skill changed on conflict: %v", err)
	}
}

func TestRenameManagedSkillRejectsConfiguredDestinationID(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  Config
	}{
		{name: "source", cfg: Config{SkillSources: []SkillTrackingSource{{SkillID: "new-skill"}}}},
		{name: "selected", cfg: Config{Clients: []Client{{SelectedSkills: []string{"new-skill"}}}}},
		{name: "target", cfg: Config{Clients: []Client{{SkillTargets: []SkillInstallTarget{{Skills: []ManagedHostSkill{{ID: "new-skill"}}}}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := t.TempDir()
			if err := os.Mkdir(filepath.Join(repository, "old-skill"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg := test.cfg
			if err := RenameManagedSkill(repository, &cfg, "old-skill", "new-skill"); err == nil {
				t.Fatal("expected configured destination conflict")
			}
			if _, err := os.Stat(filepath.Join(repository, "old-skill")); err != nil {
				t.Fatalf("old skill changed on conflict: %v", err)
			}
			if _, err := os.Stat(filepath.Join(repository, "new-skill")); !os.IsNotExist(err) {
				t.Fatalf("new skill unexpectedly exists: %v", err)
			}
		})
	}
}

func TestDeleteSkillSSHValidation(t *testing.T) {
	client := Client{Name: "host", Host: "example"}
	if err := DeleteSkillSSH(t.Context(), client, "relative/path", "valid"); err == nil {
		t.Fatal("expected absolute target validation error")
	}
	if err := DeleteSkillSSH(t.Context(), client, "/absolute/path", "../unsafe"); err == nil {
		t.Fatal("expected identifier validation error")
	}
}
