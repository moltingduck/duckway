package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/hackerduck/duckway/internal/server/services"
)

// summaryRule is one branch rule row of the "what this token can do" table,
// already narrowed by what the repository's capabilities open at all.
type summaryRule struct {
	Pattern           string `json:"pattern"` // "@default" or a branch glob
	Role              string `json:"role"`    // read | protected | agent
	Push              bool   `json:"push"`
	CreateForceDelete bool   `json:"create_force_delete"`
	PRInto            bool   `json:"pr_into"`
	RunWorkflows      bool   `json:"run_workflows"`
}

type summaryRepo struct {
	Repo         string        `json:"repo"` // lower-case owner/repo
	BranchRules  bool          `json:"branch_rules"`
	Legacy       bool          `json:"legacy"`
	Rules        []summaryRule `json:"rules"`
	TagPatterns  []string      `json:"tag_patterns"`
	Tags         bool          `json:"tags"` // may create tags
	Merge        bool          `json:"merge"`
	Approve      bool          `json:"approve"`
	Clone        bool          `json:"clone"`
	Push         bool          `json:"push_capability"`
	PRWrite      bool          `json:"pull_requests_write"`
	Dispatch     bool          `json:"workflow_dispatch"`
	RefAllowlist []string      `json:"ref_allowlist"` // legacy grants only
}

type accessSummaryResponse struct {
	Service string        `json:"service"`
	Repos   []summaryRepo `json:"repos"`
}

type summaryCaps struct {
	clone, push, prWrite, dispatch bool
	refAllow                       []string
}

// githubRepoCapabilities reads each repository's effective capabilities from a
// permission config: a v2 config's capabilities, or the builder rows of a v1
// config. Repositories are keyed by lower-case owner/repo.
func githubRepoCapabilities(configJSON string) map[string]summaryCaps {
	out := map[string]summaryCaps{}
	var header struct {
		Version string `json:"version"`
	}
	if json.Unmarshal([]byte(configJSON), &header) != nil {
		return out
	}
	switch header.Version {
	case "2":
		var cfg services.GitHubCapabilityPermissionConfig
		if json.Unmarshal([]byte(configJSON), &cfg) != nil {
			return out
		}
		for repo, rp := range cfg.Repositories {
			out[strings.ToLower(repo)] = summaryCaps{
				clone: rp.Capabilities.Clone, push: rp.Capabilities.Push,
				prWrite: rp.Capabilities.PullRequestsWrite, dispatch: rp.Capabilities.WorkflowDispatch,
				refAllow: rp.RefAllowlist,
			}
		}
	case "1":
		var cfg services.PermissionConfig
		if json.Unmarshal([]byte(configJSON), &cfg) != nil {
			return out
		}
		for _, rule := range cfg.Rules {
			for repo, rows := range rule.GitHubCapabilityRows {
				contents, pulls, actions := rows["contents"], rows["pull_requests"], rows["actions"]
				out[strings.ToLower(repo)] = summaryCaps{
					clone:   contents == services.GitHubCapRead || contents == services.GitHubCapWrite,
					push:    contents == services.GitHubCapWrite,
					prWrite: pulls == services.GitHubCapWrite, dispatch: actions == services.GitHubCapWrite,
				}
			}
		}
	}
	return out
}

// buildAccessSummary computes the per-repository table of a GitHub grant.
func buildAccessSummary(configJSON string) ([]summaryRepo, error) {
	repos := []summaryRepo{}
	if strings.TrimSpace(configJSON) == "" {
		return repos, nil
	}
	policies, err := services.ParseGitHubBranchPolicies(configJSON)
	if err != nil {
		return nil, err
	}
	caps := githubRepoCapabilities(configJSON)
	names := map[string]bool{}
	for r := range caps {
		names[r] = true
	}
	for r := range policies {
		names[r] = true
	}
	sorted := make([]string, 0, len(names))
	for r := range names {
		sorted = append(sorted, r)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		c := caps[name]
		sr := summaryRepo{
			Repo: name, Rules: []summaryRule{}, TagPatterns: []string{}, RefAllowlist: []string{},
			Clone: c.clone, Push: c.push, PRWrite: c.prWrite, Dispatch: c.dispatch,
		}
		if c.refAllow != nil {
			sr.RefAllowlist = c.refAllow
		}
		pol, ok := policies[name]
		if !ok {
			sr.Legacy = true
			repos = append(repos, sr)
			continue
		}
		sr.BranchRules = true
		for _, rule := range pol.BranchPolicy.Rules {
			agent := rule.Role == services.GitHubRoleAgent
			sr.Rules = append(sr.Rules, summaryRule{
				Pattern: rule.Branches, Role: rule.Role,
				Push:              services.GitHubRoleCanPush(rule.Role) && c.push,
				CreateForceDelete: agent && c.push,
				PRInto:            services.GitHubRoleCanBeBase(rule.Role) && c.prWrite,
				RunWorkflows:      rule.RunWorkflows && c.dispatch,
			})
		}
		sr.TagPatterns = append(sr.TagPatterns, pol.BranchPolicy.Tags.Create...)
		sr.Tags = len(sr.TagPatterns) > 0 && c.push
		sr.Merge = pol.PullRequestsMerge && c.prWrite
		sr.Approve = pol.PullRequestsApprove && c.prWrite
		repos = append(repos, sr)
	}
	return repos, nil
}

// AccessSummary handles GET /api/phantom-tokens/{id}/access-summary.
func (a *AccessSimulator) AccessSummary(w http.ResponseWriter, r *http.Request) {
	ph, err := a.placeholders.GetByID(r.PathValue("id"))
	if err != nil || ph == nil {
		jsonError(w, "phantom token not found", http.StatusNotFound)
		return
	}
	svc, err := a.services.GetByID(ph.ServiceID)
	if err != nil || svc == nil {
		jsonError(w, "service not found", http.StatusNotFound)
		return
	}
	if svc.Name != "github" {
		jsonError(w, "the access summary is only available for GitHub tokens", http.StatusBadRequest)
		return
	}
	cfg := ""
	if ph.PermissionConfig != nil {
		cfg = *ph.PermissionConfig
	}
	repos, err := buildAccessSummary(cfg)
	if err != nil {
		jsonError(w, "the token's permission config is invalid: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	jsonResponse(w, accessSummaryResponse{Service: svc.Name, Repos: repos})
}
