package services

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// GitHubCapabilityRuleName is the name of the single rule the phantom token
// builder generates from per-repository capability rows.
const GitHubCapabilityRuleName = "GitHub repositories"

// GitHubCapLevel is one of the levels a capability row can be set to.
const (
	GitHubCapNone  = "none"
	GitHubCapRead  = "read"
	GitHubCapWrite = "write"
)

// GitHubCapEndpoint is one method+path an access level opens. Path uses the
// {repo} placeholder for "owner/repo" and "{git}" for the smart-HTTP prefix
// ("/owner/repo.git").
type GitHubCapEndpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// GitHubCapability describes one capability row: which GitHub App permission
// scope it maps to (the same scope githubPermissionScope requests when a token
// is minted for such a request) and the endpoints each level opens. Write
// includes everything Read opens.
type GitHubCapability struct {
	Key   string              `json:"key"`
	Label string              `json:"label"`
	Scope string              `json:"scope"`
	Read  []GitHubCapEndpoint `json:"read"`
	Write []GitHubCapEndpoint `json:"write"`
}

// GitHubCapabilityTable is the single source of truth for the capability
// builder. The admin page receives it as JSON so the browser compiles rows
// with exactly these endpoints, and tests pin every entry to the scope and
// level githubPermissionScope derives for the same request.
var GitHubCapabilityTable = []GitHubCapability{
	{
		Key: "contents", Label: "Contents", Scope: "contents",
		Read: []GitHubCapEndpoint{
			{"GET", "{git}/info/refs"},
			{"POST", "{git}/git-upload-pack"},
			{"GET", "/repos/{repo}"},
			{"GET", "/repos/{repo}/contents/*"},
			{"GET", "/repos/{repo}/git/*"},
			{"GET", "/repos/{repo}/commits/*"},
			{"GET", "/repos/{repo}/branches/*"},
			{"GET", "/repos/{repo}/tags/*"},
			{"GET", "/repos/{repo}/compare/*"},
			{"GET", "/repos/{repo}/readme/*"},
			{"GET", "/repos/{repo}/releases/*"},
			{"GET", "/repos/{repo}/tarball/*"},
			{"GET", "/repos/{repo}/zipball/*"},
		},
		Write: []GitHubCapEndpoint{
			{"POST", "{git}/git-receive-pack"},
			{"PUT", "/repos/{repo}/contents/*"},
			{"DELETE", "/repos/{repo}/contents/*"},
			{"POST", "/repos/{repo}/git/*"},
			{"PATCH", "/repos/{repo}/git/*"},
			{"DELETE", "/repos/{repo}/git/*"},
			{"POST", "/repos/{repo}/merges/*"},
			{"POST", "/repos/{repo}/releases/*"},
			{"PATCH", "/repos/{repo}/releases/*"},
			{"DELETE", "/repos/{repo}/releases/*"},
		},
	},
	{
		Key: "pull_requests", Label: "Pull requests", Scope: "pull_requests",
		Read: []GitHubCapEndpoint{{"GET", "/repos/{repo}/pulls/*"}},
		Write: []GitHubCapEndpoint{
			{"POST", "/repos/{repo}/pulls/*"},
			{"PUT", "/repos/{repo}/pulls/*"},
			{"PATCH", "/repos/{repo}/pulls/*"},
			{"DELETE", "/repos/{repo}/pulls/*"},
		},
	},
	{
		Key: "issues", Label: "Issues", Scope: "issues",
		Read: []GitHubCapEndpoint{{"GET", "/repos/{repo}/issues/*"}},
		Write: []GitHubCapEndpoint{
			{"POST", "/repos/{repo}/issues/*"},
			{"PUT", "/repos/{repo}/issues/*"},
			{"PATCH", "/repos/{repo}/issues/*"},
			{"DELETE", "/repos/{repo}/issues/*"},
		},
	},
	{
		// Workflows live under /actions/workflows, so GitHub's "actions"
		// permission covers both runs and workflows.
		Key: "actions", Label: "Actions and workflows", Scope: "actions",
		Read: []GitHubCapEndpoint{{"GET", "/repos/{repo}/actions/*"}},
		Write: []GitHubCapEndpoint{
			{"POST", "/repos/{repo}/actions/*"},
			{"DELETE", "/repos/{repo}/actions/*"},
		},
	},
}

// GitHubCapabilityRows maps owner/repo to capability key to level
// (none, read or write). Missing keys mean none.
type GitHubCapabilityRows map[string]map[string]string

func githubCapPath(tpl, repo string) string {
	tpl = strings.ReplaceAll(tpl, "{git}", "/"+repo+".git")
	return strings.ReplaceAll(tpl, "{repo}", repo)
}

// githubCapEndpoints compiles one repository's capabilities into ordered
// endpoints (capability table order, read before write).
func githubCapEndpoints(repo string, caps map[string]string) []EndpointRule {
	var out []EndpointRule
	for _, c := range GitHubCapabilityTable {
		level := caps[c.Key]
		if level != GitHubCapRead && level != GitHubCapWrite {
			continue
		}
		eps := append([]GitHubCapEndpoint(nil), c.Read...)
		if level == GitHubCapWrite {
			eps = append(eps, c.Write...)
		}
		for _, e := range eps {
			out = append(out, EndpointRule{Method: e.Method, Path: githubCapPath(e.Path, repo), Allow: true})
		}
	}
	return out
}

// ValidateGitHubCapabilities checks repository names, capability keys and
// levels.
func ValidateGitHubCapabilities(caps GitHubCapabilityRows) error {
	known := map[string]bool{}
	for _, c := range GitHubCapabilityTable {
		known[c.Key] = true
	}
	seen := map[string]bool{}
	for repo, row := range caps {
		if !githubRepoPathPattern.MatchString(repo) {
			return fmt.Errorf("invalid github repository %q", repo)
		}
		if seen[strings.ToLower(repo)] {
			return fmt.Errorf("duplicate github repository %q", repo)
		}
		seen[strings.ToLower(repo)] = true
		for k, level := range row {
			if !known[k] {
				return fmt.Errorf("unknown github capability %q", k)
			}
			if level != GitHubCapNone && level != GitHubCapRead && level != GitHubCapWrite {
				return fmt.Errorf("invalid level %q for %s %s", level, repo, k)
			}
		}
	}
	return nil
}

// CompileGitHubCapabilityRule builds the single deny_all_other rule the
// builder stores. Repositories are emitted in sorted order so the output is
// deterministic. The capabilities map is kept on the rule so the admin page
// can show the rows again; the engine only ever looks at Endpoints.
func CompileGitHubCapabilityRule(caps GitHubCapabilityRows) (PermissionRule, error) {
	if err := ValidateGitHubCapabilities(caps); err != nil {
		return PermissionRule{}, err
	}
	repos := make([]string, 0, len(caps))
	for r := range caps {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	rule := PermissionRule{Name: GitHubCapabilityRuleName, GitHubCapabilityRows: caps, DenyAllOther: true}
	for _, repo := range repos {
		rule.Endpoints = append(rule.Endpoints, githubCapEndpoints(repo, caps[repo])...)
	}
	return rule, nil
}

// RecognizeGitHubCapabilityRule reports the capability rows of a rule the
// builder generated. A rule only counts when its endpoints are exactly what
// the table compiles from its capabilities, so a hand-edited rule is never
// mistaken for builder output.
func RecognizeGitHubCapabilityRule(rule PermissionRule) (GitHubCapabilityRows, bool) {
	if len(rule.GitHubCapabilityRows) == 0 || !rule.DenyAllOther || rule.RateLimit != nil {
		return nil, false
	}
	want, err := CompileGitHubCapabilityRule(rule.GitHubCapabilityRows)
	if err != nil || len(want.Endpoints) == 0 {
		return nil, false
	}
	if !reflect.DeepEqual(want.Endpoints, rule.Endpoints) {
		return nil, false
	}
	return rule.GitHubCapabilityRows, true
}
