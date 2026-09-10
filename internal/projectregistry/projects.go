package projectregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"
)

var nonProjectName = regexp.MustCompile(`[^a-z0-9-]+`)

type Project struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Store struct {
	path string
}

func NewStore(configDir string) *Store {
	return &Store{path: filepath.Join(configDir, "cc-projects.json")}
}

func (s *Store) List() ([]Project, error) {
	lock, err := s.acquireLock()
	if err != nil {
		return nil, err
	}
	defer releaseLock(lock)
	return s.listUnlocked()
}

func (s *Store) listUnlocked() ([]Project, error) {
	if info, err := os.Lstat(s.path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return nil, fmt.Errorf("project registry is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var projects []Project
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("parse cc-projects.json: %w", err)
	}
	sortProjects(projects)
	return projects, nil
}

func (s *Store) Add(patterns []string, name string) ([]Project, error) {
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

	lock, err := s.acquireLock()
	if err != nil {
		return nil, err
	}
	defer releaseLock(lock)
	current, err := s.listUnlocked()
	if err != nil {
		return nil, err
	}
	byPath := map[string]int{}
	usedNames := map[string]bool{}
	for i, p := range current {
		byPath[p.Path] = i
		usedNames[p.Name] = true
	}
	if name != "" {
		for _, project := range current {
			if project.Name == name && project.Path != paths[0] {
				return nil, fmt.Errorf("project name %q is already used by %s", name, project.Path)
			}
		}
	}

	var added []Project
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
		pr := Project{Name: projectName, Path: p}
		current = append(current, pr)
		byPath[p] = len(current) - 1
		usedNames[projectName] = true
		added = append(added, pr)
	}
	sortProjects(current)
	if err := s.saveUnlocked(current); err != nil {
		return nil, err
	}
	return added, nil
}

// AddResolvedPath adds one literal directory path. Unlike Add, metacharacters
// are never interpreted as glob syntax.
func (s *Store) AddResolvedPath(path, name string) (Project, error) {
	path, err := expandHome(strings.TrimSpace(path))
	if err != nil {
		return Project{}, err
	}
	path, err = normalizeProjectPath(path)
	if err != nil {
		return Project{}, err
	}
	added, err := s.addResolved([]string{path}, name)
	if err != nil {
		return Project{}, err
	}
	return added[0], nil
}

func (s *Store) addResolved(paths []string, name string) ([]Project, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		if len(name) > 256 {
			return nil, fmt.Errorf("project name is too long")
		}
		for _, r := range name {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				return nil, fmt.Errorf("project name contains unsupported characters")
			}
		}
	}
	lock, err := s.acquireLock()
	if err != nil {
		return nil, err
	}
	defer releaseLock(lock)
	current, err := s.listUnlocked()
	if err != nil {
		return nil, err
	}
	byPath, usedNames := map[string]int{}, map[string]bool{}
	for i, project := range current {
		byPath[project.Path], usedNames[project.Name] = i, true
	}
	if name != "" {
		for _, project := range current {
			if project.Name == name && project.Path != paths[0] {
				return nil, fmt.Errorf("project name %q is already used by %s", name, project.Path)
			}
		}
	}
	added := make([]Project, 0, len(paths))
	for _, path := range paths {
		projectName := name
		if projectName == "" {
			projectName = uniqueProjectName(filepath.Base(path), usedNames)
		}
		if index, ok := byPath[path]; ok {
			if name != "" && current[index].Name != name {
				delete(usedNames, current[index].Name)
				current[index].Name = name
				usedNames[name] = true
			}
			added = append(added, current[index])
			continue
		}
		project := Project{Name: projectName, Path: path}
		current = append(current, project)
		usedNames[projectName] = true
		added = append(added, project)
	}
	sortProjects(current)
	if err := s.saveUnlocked(current); err != nil {
		return nil, err
	}
	return added, nil
}

// SuggestDirectories returns a bounded, sorted list of directories matching a
// path prefix. It performs filesystem traversal directly and never invokes a
// shell, so caller-controlled path text cannot become a command.
func SuggestDirectories(query string, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	query = strings.TrimSpace(query)
	if strings.HasPrefix(query, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		if query == "~" {
			query = home + string(os.PathSeparator)
		} else if strings.HasPrefix(query, "~/") {
			query = filepath.Join(home, strings.TrimPrefix(query, "~/"))
		}
	}
	if query == "" {
		query = "."
	}
	cleaned := filepath.Clean(query)
	parent, prefix := filepath.Dir(cleaned), filepath.Base(cleaned)
	if strings.HasSuffix(query, string(os.PathSeparator)) {
		parent, prefix = cleaned, ""
	} else if info, err := os.Stat(cleaned); err == nil && info.IsDir() {
		parent, prefix = cleaned, ""
	}
	parent, err := filepath.Abs(parent)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	results := make([]string, 0, min(limit, len(entries)))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(strings.ToLower(entry.Name()), strings.ToLower(prefix)) {
			continue
		}
		results = append(results, filepath.Join(parent, entry.Name()))
		if len(results) == limit {
			break
		}
	}
	sort.Strings(results)
	return results, nil
}

func (s *Store) Remove(ref string) (*Project, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("missing project name, number, or path")
	}
	lock, err := s.acquireLock()
	if err != nil {
		return nil, err
	}
	defer releaseLock(lock)
	projects, err := s.listUnlocked()
	if err != nil {
		return nil, err
	}
	idx := findProjectIndex(projects, ref)
	if idx < 0 {
		return nil, fmt.Errorf("project %q not found", ref)
	}
	removed := projects[idx]
	projects = append(projects[:idx], projects[idx+1:]...)
	if err := s.saveUnlocked(projects); err != nil {
		return nil, err
	}
	return &removed, nil
}

func (s *Store) Clear() (int, error) {
	lock, err := s.acquireLock()
	if err != nil {
		return 0, err
	}
	defer releaseLock(lock)
	projects, err := s.listUnlocked()
	if err != nil {
		return 0, err
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return len(projects), nil
}

func (s *Store) Resolve(ref string) (*Project, error) {
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

func (s *Store) saveUnlocked(projects []Project) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(projects, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".cc-projects-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if info, err := os.Lstat(s.path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return fmt.Errorf("project registry is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *Store) acquireLock() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

func releaseLock(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func ResolveProjectPaths(patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range patterns {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		pattern, err := NormalizeProjectPattern(raw)
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

func NormalizeProjectPattern(raw string) (string, error) {
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
	name := DefaultProjectName(base)
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

// DefaultProjectName returns the stable registry name used when --name is omitted.
func DefaultProjectName(base string) string {
	name := sanitizeProjectName(base)
	if name == "" {
		return "project"
	}
	return name
}

func sanitizeProjectName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = nonProjectName.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	return name
}

func findProjectIndex(projects []Project, ref string) int {
	if n, err := strconv.Atoi(ref); err == nil && n >= 1 && n <= len(projects) {
		return n - 1
	}
	expanded, _ := NormalizeProjectPattern(ref)
	for i, p := range projects {
		if p.Name == ref || p.Path == ref || p.Path == expanded {
			return i
		}
	}
	return -1
}

func sortProjects(projects []Project) {
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Name == projects[j].Name {
			return projects[i].Path < projects[j].Path
		}
		return projects[i].Name < projects[j].Name
	})
}
