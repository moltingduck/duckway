package ducklord

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxSkillPreviewDiffBytes = 128 << 10

// SkillPreview is a validated, on-disk candidate replacement. The staged
// directory remains private to Ducklord until CommitSkillPreview is called.
type SkillPreview struct {
	Identifier string
	Info       SkillInfo
	StagedPath string
	Diff       string
	repository string
}

// PrepareSkillPreview validates source and creates a private candidate without
// changing the managed skill. Call CommitSkillPreview to install it.
func PrepareSkillPreview(repository, identifier, source string) (SkillPreview, error) {
	if !SafeIdentifier(identifier) {
		return SkillPreview{}, fmt.Errorf("unsafe skill identifier %q", identifier)
	}
	if err := ensureRepository(repository); err != nil {
		return SkillPreview{}, err
	}
	info, err := InspectSkill(source, identifier)
	if err != nil {
		return SkillPreview{}, err
	}
	container, err := os.MkdirTemp(repository, ".skill-preview-")
	if err != nil {
		return SkillPreview{}, err
	}
	preview := SkillPreview{Identifier: identifier, Info: info, StagedPath: filepath.Join(container, identifier), repository: repository}
	if err := copySkill(source, preview.StagedPath); err != nil {
		_ = os.RemoveAll(container)
		return SkillPreview{}, err
	}
	preview.Diff = skillPreviewDiff(filepath.Join(repository, identifier), preview.StagedPath, identifier)
	return preview, nil
}

// CommitSkillPreview atomically replaces the managed skill after revalidating
// the candidate. It also removes the staging directory on every outcome.
func CommitSkillPreview(preview *SkillPreview) (SkillInfo, error) {
	if preview == nil || preview.repository == "" || preview.StagedPath == "" {
		return SkillInfo{}, fmt.Errorf("skill preview is required")
	}
	defer CleanupSkillPreview(preview)
	if !SafeIdentifier(preview.Identifier) || !previewPathBelongsTo(preview.repository, preview.StagedPath, preview.Identifier) {
		return SkillInfo{}, fmt.Errorf("invalid skill preview staging path")
	}
	if err := validatePreviewPath(preview.repository, preview.StagedPath); err != nil {
		return SkillInfo{}, err
	}
	_, err := InspectSkill(preview.StagedPath, preview.Identifier)
	if err != nil {
		return SkillInfo{}, err
	}
	return ImportSkill(preview.repository, preview.Identifier, preview.StagedPath)
}

func validatePreviewPath(repository, staged string) error {
	rel, err := filepath.Rel(repository, staged)
	if err != nil || filepath.IsAbs(rel) {
		return fmt.Errorf("invalid skill preview staging path")
	}
	current := repository
	if err := validatePreviewDirectory(current); err != nil {
		return err
	}
	for _, part := range strings.Split(filepath.Clean(rel), string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		if err := validatePreviewDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func validatePreviewDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("skill preview staging path is unavailable: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("skill preview staging path contains a symlink or non-directory")
	}
	return nil
}

// CleanupSkillPreview removes a candidate that the user declined or that
// could not be committed. It is safe to call more than once.
func CleanupSkillPreview(preview *SkillPreview) {
	if preview == nil || preview.StagedPath == "" {
		return
	}
	if preview.repository == "" || !previewPathBelongsTo(preview.repository, preview.StagedPath, preview.Identifier) {
		preview.StagedPath = ""
		return
	}
	_ = os.RemoveAll(filepath.Dir(preview.StagedPath))
	preview.StagedPath = ""
}

func previewPathBelongsTo(repository, staged, identifier string) bool {
	rel, err := filepath.Rel(repository, staged)
	if err != nil || filepath.IsAbs(rel) || rel == "." {
		return false
	}
	parts := strings.Split(filepath.Clean(rel), string(os.PathSeparator))
	return len(parts) == 2 && strings.HasPrefix(parts[0], ".skill-preview-") && parts[1] == identifier
}

func skillPreviewDiff(oldPath, newPath, identifier string) string {
	oldFiles := skillFileContents(oldPath)
	newFiles := skillFileContents(newPath)
	keys := make([]string, 0, len(oldFiles)+len(newFiles))
	seen := make(map[string]bool)
	for key := range oldFiles {
		seen[key] = true
	}
	for key := range newFiles {
		seen[key] = true
	}
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out bytes.Buffer
	for _, key := range keys {
		before, beforeOK := oldFiles[key]
		after, afterOK := newFiles[key]
		if beforeOK && afterOK && bytes.Equal(before, after) {
			continue
		}
		fmt.Fprintf(&out, "--- a/%s/%s\n+++ b/%s/%s\n", identifier, key, identifier, key)
		fmt.Fprintf(&out, "@@ -%d +%d @@\n", lineCount(before), lineCount(after))
		if beforeOK {
			for _, line := range strings.Split(string(before), "\n") {
				fmt.Fprintf(&out, "-%s\n", line)
			}
		}
		for _, line := range strings.Split(string(after), "\n") {
			if afterOK {
				fmt.Fprintf(&out, "+%s\n", line)
			}
		}
		if out.Len() >= maxSkillPreviewDiffBytes {
			return out.String()[:maxSkillPreviewDiffBytes] + "\n... diff truncated ...\n"
		}
	}
	return out.String()
}

func skillFileContents(root string) map[string][]byte {
	result := make(map[string][]byte)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err == nil {
			if data, err := os.ReadFile(path); err == nil {
				result[filepath.ToSlash(rel)] = data
			}
		}
		return nil
	})
	return result
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return len(bytes.Split(data, []byte("\n")))
}
