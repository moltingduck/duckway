package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type CCProject struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type CCProjectStore struct {
	path string
}

func NewCCProjectStore(configDir string) *CCProjectStore {
	return &CCProjectStore{path: filepath.Join(configDir, "cc-projects.json")}
}

func (s *CCProjectStore) List() ([]CCProject, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var projects []CCProject
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("parse cc-projects.json: %w", err)
	}
	sortProjects(projects)
	return projects, nil
}

func (s *CCProjectStore) Add(patterns []string, name string) ([]CCProject, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("missing path")
	}
	paths, err := ResolveProjectPaths(patterns)
	if err != nil {
		return nil, err
	}
	if name != "" && len(paths) != 1 {
		return nil, fmt.Errorf("--name can only be used when one directory is added")
	}

	current, err := s.List()
	if err != nil {
		return nil, err
	}
	byPath := map[string]int{}
	usedNames := map[string]bool{}
	for i, p := range current {
		byPath[p.Path] = i
		usedNames[p.Name] = true
	}

	var added []CCProject
	for _, p := range paths {
		projectName := name
		if projectName == "" {
			projectName = uniqueProjectName(filepath.Base(p), usedNames)
		}
		if idx, ok := byPath[p]; ok {
			if name != "" {
				delete(usedNames, current[idx].Name)
				current[idx].Name = projectName
				usedNames[projectName] = true
			}
			added = append(added, current[idx])
			continue
		}
		pr := CCProject{Name: projectName, Path: p}
		current = append(current, pr)
		byPath[p] = len(current) - 1
		usedNames[projectName] = true
		added = append(added, pr)
	}
	sortProjects(current)
	if err := s.save(current); err != nil {
		return nil, err
	}
	return added, nil
}

func (s *CCProjectStore) Remove(ref string) (*CCProject, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("missing project name, number, or path")
	}
	projects, err := s.List()
	if err != nil {
		return nil, err
	}
	idx := findProjectIndex(projects, ref)
	if idx < 0 {
		return nil, fmt.Errorf("project %q not found", ref)
	}
	removed := projects[idx]
	projects = append(projects[:idx], projects[idx+1:]...)
	if err := s.save(projects); err != nil {
		return nil, err
	}
	return &removed, nil
}

func (s *CCProjectStore) Clear() (int, error) {
	projects, err := s.List()
	if err != nil {
		return 0, err
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return len(projects), nil
}

func (s *CCProjectStore) Resolve(ref string) (*CCProject, error) {
	projects, err := s.List()
	if err != nil {
		return nil, err
	}
	idx := findProjectIndex(projects, ref)
	if idx < 0 {
		return nil, fmt.Errorf("project %q not found", ref)
	}
	return &projects[idx], nil
}

func (s *CCProjectStore) save(projects []CCProject) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(projects, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(s.path, data, 0600)
}

func ResolveProjectPaths(patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range patterns {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		pattern, err := normalizeProjectPattern(raw)
		if err != nil {
			return nil, err
		}
		matches := []string{pattern}
		fromGlob := hasGlobMeta(pattern)
		if fromGlob {
			matches, err = filepath.Glob(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid glob %q: %w", raw, err)
			}
			if len(matches) == 0 {
				return nil, fmt.Errorf("glob %q matched no directories", raw)
			}
		}
		addedForPattern := 0
		for _, m := range matches {
			path, err := normalizeProjectPath(m)
			if err != nil {
				if fromGlob {
					continue
				}
				return nil, fmt.Errorf("%s: %w", raw, err)
			}
			if seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
			addedForPattern++
		}
		if fromGlob && addedForPattern == 0 {
			return nil, fmt.Errorf("glob %q matched no directories", raw)
		}
	}
	sort.Strings(out)
	return out, nil
}

func normalizeProjectPattern(raw string) (string, error) {
	expanded, err := expandHome(raw)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded), nil
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func normalizeProjectPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return filepath.Clean(abs), nil
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func hasGlobMeta(path string) bool {
	return strings.ContainsAny(path, "*?[")
}

func uniqueProjectName(base string, used map[string]bool) string {
	name := sanitizeProjectName(base)
	if name == "" {
		name = "project"
	}
	if !used[name] {
		return name
	}
	for i := 2; ; i++ {
		candidate := name + "-" + strconv.Itoa(i)
		if !used[candidate] {
			return candidate
		}
	}
}

func sanitizeProjectName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = nonDiscordName.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	return name
}

func findProjectIndex(projects []CCProject, ref string) int {
	if n, err := strconv.Atoi(ref); err == nil && n >= 1 && n <= len(projects) {
		return n - 1
	}
	expanded, _ := normalizeProjectPattern(ref)
	for i, p := range projects {
		if p.Name == ref || p.Path == ref || p.Path == expanded {
			return i
		}
	}
	return -1
}

func sortProjects(projects []CCProject) {
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Name == projects[j].Name {
			return projects[i].Path < projects[j].Path
		}
		return projects[i].Name < projects[j].Name
	})
}
