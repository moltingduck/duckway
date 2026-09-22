package ducklord

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const deleteSkillScript = "delete"

func DeleteSkillSSH(ctx context.Context, client Client, target, identifier string) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("skill target path must be absolute")
	}
	if !SafeIdentifier(identifier) {
		return fmt.Errorf("unsafe skill identifier %q", identifier)
	}
	return runSkillSSH(ctx, client, deleteSkillScript, target, identifier, nil, io.Discard)
}

func RenameManagedSkill(repository string, cfg *Config, oldID, newID string) error {
	if cfg == nil {
		return fmt.Errorf("ducklord config is required")
	}
	if !SafeIdentifier(oldID) || !SafeIdentifier(newID) {
		return fmt.Errorf("skill identifiers must be safe")
	}
	if oldID == newID {
		return fmt.Errorf("old and new skill identifiers must differ")
	}
	if repository == "" {
		repository = DefaultSkillRepository()
	}
	if err := EnsureSkillRepository(repository); err != nil {
		return err
	}
	oldPath, newPath := filepath.Join(repository, oldID), filepath.Join(repository, newID)
	info, err := os.Lstat(oldPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("managed skill %q does not exist", oldID)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("managed skill %q must be a directory", oldID)
	}
	if _, err := os.Lstat(newPath); err == nil {
		return fmt.Errorf("managed skill %q already exists", newID)
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, source := range cfg.SkillSources {
		if source.SkillID == newID {
			return fmt.Errorf("managed skill %q conflicts with configured skill source", newID)
		}
	}
	for _, client := range cfg.Clients {
		for _, selected := range client.SelectedSkills {
			if selected == newID {
				return fmt.Errorf("managed skill %q conflicts with selected skill", newID)
			}
		}
		for _, target := range client.SkillTargets {
			for _, skill := range target.Skills {
				if skill.ID == newID {
					return fmt.Errorf("managed skill %q conflicts with configured target skill", newID)
				}
			}
		}
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("rename managed skill: %w", err)
	}
	for i := range cfg.SkillSources {
		if cfg.SkillSources[i].SkillID == oldID {
			cfg.SkillSources[i].SkillID = newID
		}
	}
	for i := range cfg.Clients {
		for j := range cfg.Clients[i].SelectedSkills {
			if cfg.Clients[i].SelectedSkills[j] == oldID {
				cfg.Clients[i].SelectedSkills[j] = newID
			}
		}
		for j := range cfg.Clients[i].SkillTargets {
			for k := range cfg.Clients[i].SkillTargets[j].Skills {
				if cfg.Clients[i].SkillTargets[j].Skills[k].ID == oldID {
					cfg.Clients[i].SkillTargets[j].Skills[k].ID = newID
				}
			}
		}
	}
	return nil
}
