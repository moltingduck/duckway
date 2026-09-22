package ducklord

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// DefaultSkillRepository is the local managed-skill repository used by
// Ducklord when no repository is supplied by a caller.
func DefaultSkillRepository() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".ducklord", "skills")
	}
	return filepath.Join(home, ".ducklord", "skills")
}

// EnsureSkillRepository creates the managed repository with private
// permissions. Existing symlinks and non-directories are rejected.
func EnsureSkillRepository(repository string) error {
	if repository == "" {
		repository = DefaultSkillRepository()
	}
	if err := rejectSymlinkedRepositoryParents(repository); err != nil {
		return err
	}
	if err := os.MkdirAll(repository, 0700); err != nil {
		return err
	}
	return ensureRepository(repository)
}

func rejectSymlinkedRepositoryParents(repository string) error {
	current := filepath.Clean(repository)
	for {
		info, err := os.Lstat(current)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("skill repository must be a real directory; path contains symlink %q", current)
			}
		case os.IsNotExist(err):
			// Missing descendants will be created by MkdirAll. Continue checking
			// their existing ancestors for symlinks.
		default:
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// SkillUpload is the injectable transfer seam used by DeploySelectedSkills.
type SkillUpload func(context.Context, Client, string, string, string) (SkillInfo, error)

var uploadManagedSkill SkillUpload = UploadSkillSSH

// DeploySelectedSkills uploads every selected managed skill to every named
// target configured for clientName. It validates the client and all selected
// skills before any transfer starts. If a transfer fails, earlier uploads are
// retained and returned in results; the operation is not transactional.
func DeploySelectedSkills(ctx context.Context, cfg *Config, clientName, repository string) ([]SkillInfo, error) {
	if cfg == nil {
		return nil, fmt.Errorf("ducklord config is required")
	}
	client, ok := cfg.Client(clientName)
	if !ok {
		return nil, fmt.Errorf("unknown client %q", clientName)
	}
	if err := client.Normalize(); err != nil {
		return nil, fmt.Errorf("client %q: %w", clientName, err)
	}
	if repository == "" {
		repository = DefaultSkillRepository()
	}
	if err := EnsureSkillRepository(repository); err != nil {
		return nil, err
	}
	for _, identifier := range client.SelectedSkills {
		_, err := InspectSkill(filepath.Join(repository, identifier), identifier)
		if err != nil {
			return nil, fmt.Errorf("selected skill %q: %w", identifier, err)
		}
	}
	results := make([]SkillInfo, 0, len(client.SelectedSkills)*len(client.SkillTargets))
	for _, identifier := range client.SelectedSkills {
		for _, target := range client.SkillTargets {
			info, err := uploadManagedSkill(ctx, client, repository, identifier, target.Path)
			if err != nil {
				return results, fmt.Errorf("upload skill %q to target %q: %w", identifier, target.ID, err)
			}
			results = append(results, info)
		}
	}
	return results, nil
}

// DeployTargetSkills uploads only the skills managed as push for one named
// agent target.  Older configurations that predate per-target management keep
// their selected_skills behaviour until the target receives an explicit state.
func DeployTargetSkills(ctx context.Context, cfg *Config, clientName, targetID, repository string) ([]SkillInfo, error) {
	if cfg == nil {
		return nil, fmt.Errorf("ducklord config is required")
	}
	client, ok := cfg.Client(clientName)
	if !ok {
		return nil, fmt.Errorf("unknown client %q", clientName)
	}
	if err := client.Normalize(); err != nil {
		return nil, fmt.Errorf("client %q: %w", clientName, err)
	}
	var target *SkillInstallTarget
	for i := range client.SkillTargets {
		if client.SkillTargets[i].ID == targetID {
			target = &client.SkillTargets[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("client %q has no skill target %q", clientName, targetID)
	}
	if repository == "" {
		repository = DefaultSkillRepository()
	}
	if err := EnsureSkillRepository(repository); err != nil {
		return nil, err
	}
	selected := make(map[string]bool)
	for _, skill := range target.Skills {
		if skill.Management == "push" {
			selected[skill.ID] = true
		}
	}
	// selected_skills was the original host-wide setting.  Interpret it as a
	// push default only while the target has no per-skill records.
	if len(target.Skills) == 0 {
		for _, id := range client.SelectedSkills {
			selected[id] = true
		}
	}
	identifiers := make([]string, 0, len(selected))
	for id := range selected {
		if _, err := InspectSkill(filepath.Join(repository, id), id); err != nil {
			return nil, fmt.Errorf("push skill %q: %w", id, err)
		}
		identifiers = append(identifiers, id)
	}
	sort.Strings(identifiers)
	results := make([]SkillInfo, 0, len(identifiers))
	for _, identifier := range identifiers {
		info, err := uploadManagedSkill(ctx, client, repository, identifier, target.Path)
		if err != nil {
			return results, fmt.Errorf("upload skill %q to target %q: %w", identifier, target.ID, err)
		}
		results = append(results, info)
	}
	return results, nil
}
