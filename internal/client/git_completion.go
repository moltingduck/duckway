package client

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/hackerduck/duckway/internal/server/services"
)

const gitCompletionFile = "git-completion.json"
const maxGitCompletionSize = 1 << 20

var completionRepoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func safeCompletionRepo(repo string) bool {
	if !completionRepoPattern.MatchString(repo) {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." || strings.HasSuffix(part, ".git") {
			return false
		}
	}
	return true
}

func completionRepos(keys []PlaceholderKeyInfo) []string {
	var repos []string
	for _, key := range keys {
		if key.ServiceName != "github" || strings.TrimSpace(key.Placeholder) == "" {
			continue
		}
		var config struct {
			Version      string `json:"version"`
			Provider     string `json:"provider"`
			Repositories map[string]struct {
				Capabilities struct {
					Clone bool `json:"clone"`
				} `json:"capabilities"`
			} `json:"repositories"`
			Rules []struct {
				Endpoints []struct {
					Method string `json:"method"`
					Path   string `json:"path"`
					Allow  bool   `json:"allow"`
				} `json:"endpoints"`
			} `json:"rules"`
		}
		if json.Unmarshal([]byte(key.PermissionConfig), &config) != nil || config.Provider != "github" {
			continue
		}
		parsed, err := services.ParsePermissionConfig(key.PermissionConfig)
		if err != nil {
			continue
		}
		if config.Version == "2" {
			for repo, assignment := range config.Repositories {
				if assignment.Capabilities.Clone {
					repos = append(repos, repo)
				}
			}
			continue
		}
		if config.Version != "" && config.Version != "1" {
			continue
		}
		// Hints describe assigned permissions, independent of transient request
		// quotas. Remove rate accounting from this local copy before reusing
		// the server's endpoint matching and deny/constraint semantics.
		for i := range parsed.Rules {
			parsed.Rules[i].RateLimit = nil
		}
		permissionOnly, err := json.Marshal(parsed)
		if err != nil {
			continue
		}
		checker := services.NewPermissionChecker()
		for _, rule := range config.Rules {
			for _, ep := range rule.Endpoints {
				// Only upload-pack grants identify repositories available to clone.
				if !ep.Allow || !strings.HasSuffix(ep.Path, ".git/git-upload-pack") || !strings.EqualFold(ep.Method, "POST") {
					continue
				}
				if repo, ok := githubRepoFromACLEndpoint(ep.Method, ep.Path, ep.Allow); ok && safeCompletionRepo(repo) {
					if checker.Check(string(permissionOnly), "completion", "GET", "/"+repo+".git/info/refs", nil).Allowed && checker.Check(string(permissionOnly), "completion", "POST", "/"+repo+".git/git-upload-pack", nil).Allowed {
						repos = append(repos, repo)
					}
				}
			}
		}
	}
	return cleanCompletionRepos(repos)
}

func cleanCompletionRepos(repos []string) []string {
	sort.Strings(repos)
	seen := map[string]bool{}
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		lower := strings.ToLower(repo)
		if safeCompletionRepo(repo) && !seen[lower] {
			seen[lower] = true
			out = append(out, repo)
		}
	}
	return out
}

func saveGitCompletionRepos(configDir string, keys []PlaceholderKeyInfo) error {
	data, err := json.Marshal(completionRepos(keys))
	if err != nil {
		return err
	}
	if err = os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(configDir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return os.ErrPermission
	}
	if err = os.Chmod(configDir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(configDir, ".git-completion-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(configDir, gitCompletionFile))
}

// LocalGitCompletionRepos reads an advisory, secret-free sync snapshot. Missing,
// malformed, oversized or special files simply provide no completion hints.
func LocalGitCompletionRepos(configDir string) []string {
	path := filepath.Join(configDir, gitCompletionFile)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxGitCompletionSize {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(file, maxGitCompletionSize+1))
	if err != nil || len(data) > maxGitCompletionSize {
		return nil
	}
	var repos []string
	if json.Unmarshal(data, &repos) != nil {
		return nil
	}
	return cleanCompletionRepos(repos)
}
