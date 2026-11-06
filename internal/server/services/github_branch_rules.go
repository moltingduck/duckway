package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// GitHub branch rules (docs/github-branch-rules.md). A branch policy narrows
// what an already opened GitHub endpoint may do to a ref.

const (
	GitHubRoleRead      = "read"
	GitHubRoleProtected = "protected"
	GitHubRoleAgent     = "agent"

	// GitHubDefaultBranchToken is the rule pattern that stands for the
	// repository's current default branch.
	GitHubDefaultBranchToken = "@default"

	MaxGitHubBranchRules   = 50
	MaxGitHubTagPatterns   = 20
	MaxGitHubPatternLength = 200
)

// GitHubBranchRule is one branch-protection style rule.
type GitHubBranchRule struct {
	Branches     string `json:"branches"`
	Role         string `json:"role"`
	RunWorkflows bool   `json:"run_workflows"`
}

// GitHubTagPolicy lists the tag globs the agent may create.
type GitHubTagPolicy struct {
	Create []string `json:"create"`
}

// GitHubBranchPolicy is the per repository `branch_policy` object.
type GitHubBranchPolicy struct {
	Rules []GitHubBranchRule `json:"rules"`
	Tags  GitHubTagPolicy    `json:"tags"`
}

// GitHubRepoBranchPolicy is a branch policy plus the pull request flags that
// travel with it. It is the builder's per-repository entry
// (`github_branch_policies`) and the server's resolved view of a v2 grant.
type GitHubRepoBranchPolicy struct {
	BranchPolicy        GitHubBranchPolicy `json:"branch_policy"`
	PullRequestsMerge   bool               `json:"pull_requests_merge"`
	PullRequestsApprove bool               `json:"pull_requests_approve"`
}

func githubRoleRank(role string) int {
	switch role {
	case GitHubRoleAgent:
		return 2
	case GitHubRoleProtected:
		return 1
	default:
		return 0
	}
}

// GitHubRoleCanPush reports whether the role may push/commit/create/delete.
func GitHubRoleCanPush(role string) bool { return role == GitHubRoleAgent }

// GitHubRoleCanBeBase reports whether a pull request may target the branch.
func GitHubRoleCanBeBase(role string) bool {
	return role == GitHubRoleAgent || role == GitHubRoleProtected
}

func stripBranchPrefix(s string) string { return strings.TrimPrefix(s, "refs/heads/") }

// normalizeGitHubPattern validates one branch or tag glob and returns it
// without its refs/ prefix.
func normalizeGitHubPattern(raw, prefix string, allowDefault bool) (string, error) {
	p := strings.TrimPrefix(raw, prefix)
	if p == "" {
		return "", fmt.Errorf("empty pattern")
	}
	if len(p) > MaxGitHubPatternLength {
		return "", fmt.Errorf("pattern %q is longer than %d characters", p[:20]+"...", MaxGitHubPatternLength)
	}
	if p == GitHubDefaultBranchToken {
		if !allowDefault {
			return "", fmt.Errorf("%s is only valid for branch rules", GitHubDefaultBranchToken)
		}
		return p, nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("pattern %q must not start with /", p)
	}
	if strings.Contains(p, "..") || strings.Contains(p, "//") || strings.Contains(p, "@{") {
		return "", fmt.Errorf("pattern %q contains a sequence git does not allow", p)
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7f || c == ' ' || c == '~' || c == '^' || c == ':' || c == '[' || c == '\\' {
			return "", fmt.Errorf("pattern %q contains a character git does not allow", p)
		}
	}
	return p, nil
}

// ValidateGitHubBranchPolicy checks a policy and rewrites patterns into their
// stored form (no refs/heads/ prefix). dispatch says whether the repository has
// the workflow_dispatch capability, which run_workflows requires.
func ValidateGitHubBranchPolicy(p *GitHubBranchPolicy, dispatch bool) error {
	if len(p.Rules) > MaxGitHubBranchRules {
		return fmt.Errorf("branch_policy allows at most %d rules", MaxGitHubBranchRules)
	}
	seen := map[string]bool{}
	for i := range p.Rules {
		r := &p.Rules[i]
		pat, err := normalizeGitHubPattern(r.Branches, "refs/heads/", true)
		if err != nil {
			return fmt.Errorf("branch rule %d: %w", i, err)
		}
		r.Branches = pat
		switch r.Role {
		case GitHubRoleRead, GitHubRoleProtected, GitHubRoleAgent:
		default:
			return fmt.Errorf("branch rule %d: invalid role %q", i, r.Role)
		}
		if seen[pat] {
			return fmt.Errorf("branch rule %d: duplicate pattern %q", i, pat)
		}
		seen[pat] = true
		if r.RunWorkflows && !dispatch {
			return fmt.Errorf("branch rule %d: run_workflows requires the workflow_dispatch capability", i)
		}
	}
	if len(p.Tags.Create) > MaxGitHubTagPatterns {
		return fmt.Errorf("branch_policy allows at most %d tag patterns", MaxGitHubTagPatterns)
	}
	seenTags := map[string]bool{}
	for i, t := range p.Tags.Create {
		pat, err := normalizeGitHubPattern(t, "refs/tags/", false)
		if err != nil {
			return fmt.Errorf("tag pattern %d: %w", i, err)
		}
		if seenTags[pat] {
			return fmt.Errorf("tag pattern %d: duplicate pattern %q", i, pat)
		}
		seenTags[pat] = true
		p.Tags.Create[i] = pat
	}
	return nil
}

// HasRunWorkflowsRule reports whether any rule may start workflows.
func (p GitHubBranchPolicy) HasRunWorkflowsRule() bool {
	for _, r := range p.Rules {
		if r.RunWorkflows {
			return true
		}
	}
	return false
}

// HasDefaultRule reports whether a decision may depend on the default branch.
func (p GitHubBranchPolicy) HasDefaultRule() bool {
	for _, r := range p.Rules {
		if r.Branches == GitHubDefaultBranchToken {
			return true
		}
	}
	return false
}

type globTok struct {
	kind byte // 'l' literal, 's' single star, 'd' double star
	b    byte
}

func compileGitHubGlob(pattern string) []globTok {
	var toks []globTok
	for i := 0; i < len(pattern); {
		if pattern[i] != '*' {
			toks = append(toks, globTok{kind: 'l', b: pattern[i]})
			i++
			continue
		}
		j := i
		for j < len(pattern) && pattern[j] == '*' {
			j++
		}
		if j-i >= 2 {
			toks = append(toks, globTok{kind: 'd'})
		} else {
			toks = append(toks, globTok{kind: 's'})
		}
		i = j
	}
	return toks
}

// GitHubGlobMatch matches a branch or tag name against a rule glob: `*`
// matches within one `/` segment, `**` matches anything including `/`, every
// other character (including `?`) is literal.
func GitHubGlobMatch(pattern, name string) bool {
	toks := compileGitHubGlob(pattern)
	// cur[j]: toks[i:] matches name[j:]
	n := len(name)
	cur := make([]bool, n+1)
	cur[n] = true
	for i := len(toks) - 1; i >= 0; i-- {
		next := make([]bool, n+1)
		t := toks[i]
		for j := n; j >= 0; j-- {
			switch t.kind {
			case 'l':
				next[j] = j < n && name[j] == t.b && cur[j+1]
			case 's':
				next[j] = cur[j] || (j < n && name[j] != '/' && next[j+1])
			case 'd':
				next[j] = cur[j] || (j < n && next[j+1])
			}
		}
		cur = next
	}
	return cur[0]
}

func globSpecificity(pattern string) (prefixLen, stars int) {
	prefixLen = len(pattern)
	if i := strings.IndexByte(pattern, '*'); i >= 0 {
		prefixLen = i
	}
	return prefixLen, strings.Count(pattern, "*")
}

// moreSpecific reports whether rule a beats rule b for the same branch.
func moreSpecific(a, b GitHubBranchRule) bool {
	ap, as := globSpecificity(a.Branches)
	bp, bs := globSpecificity(b.Branches)
	if ap != bp {
		return ap > bp
	}
	if as != bs {
		return as < bs
	}
	if len(a.Branches) != len(b.Branches) {
		return len(a.Branches) > len(b.Branches)
	}
	ar, br := githubRoleRank(a.Role), githubRoleRank(b.Role)
	if ar != br {
		return ar < br
	}
	return !a.RunWorkflows && b.RunWorkflows
}

// SelectRule picks the single rule that governs branch (with or without a
// refs/heads/ prefix). defaultBranch is the resolved default branch, "" when
// unknown. matched is false when no rule applies (role read, no workflows).
func (p GitHubBranchPolicy) SelectRule(branch, defaultBranch string) (rule GitHubBranchRule, matched bool) {
	branch = stripBranchPrefix(branch)
	if defaultBranch != "" && branch == defaultBranch {
		for _, r := range p.Rules {
			if stripBranchPrefix(r.Branches) != GitHubDefaultBranchToken {
				continue
			}
			if !matched || moreSpecific(r, rule) {
				rule, matched = r, true
			}
		}
		if matched {
			return rule, true
		}
	}
	for _, r := range p.Rules {
		pat := stripBranchPrefix(r.Branches)
		if pat == GitHubDefaultBranchToken || !GitHubGlobMatch(pat, branch) {
			continue
		}
		r.Branches = pat
		if !matched || moreSpecific(r, rule) {
			rule, matched = r, true
		}
	}
	return rule, matched
}

// TagCreateAllowed reports whether tag matches one of tags.create.
func (p GitHubBranchPolicy) TagCreateAllowed(tag string) bool {
	tag = strings.TrimPrefix(tag, "refs/tags/")
	for _, pat := range p.Tags.Create {
		if GitHubGlobMatch(strings.TrimPrefix(pat, "refs/tags/"), tag) {
			return true
		}
	}
	return false
}

func githubCapRowLevel(rows GitHubCapabilityRows, repo, key string) string {
	for r, row := range rows {
		if strings.EqualFold(r, repo) {
			return row[key]
		}
	}
	return ""
}

// GitHubBranchPoliciesFromConfig returns the validated, normalised branch
// policies of a permission config keyed by lower-case owner/repo: the v2
// `branch_policy` of each repository, or the builder's
// `github_branch_policies` of a v1 rule. Repositories without a policy are
// absent, so legacy configs yield an empty map.
func GitHubBranchPoliciesFromConfig(configJSON string) (map[string]GitHubRepoBranchPolicy, error) {
	out := map[string]GitHubRepoBranchPolicy{}
	if strings.TrimSpace(configJSON) == "" {
		return out, nil
	}
	var header struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(configJSON), &header); err != nil {
		return nil, fmt.Errorf("invalid permission config: %w", err)
	}
	switch header.Version {
	case "2":
		var cfg GitHubCapabilityPermissionConfig
		dec := json.NewDecoder(bytes.NewBufferString(configJSON))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("invalid github capability permission config: %w", err)
		}
		for repo, rp := range cfg.Repositories {
			if rp.BranchPolicy == nil {
				continue
			}
			bp := *rp.BranchPolicy
			bp.Rules = append([]GitHubBranchRule(nil), bp.Rules...)
			bp.Tags.Create = append([]string(nil), bp.Tags.Create...)
			if err := ValidateGitHubBranchPolicy(&bp, rp.Capabilities.WorkflowDispatch); err != nil {
				return nil, fmt.Errorf("repository %s: %w", repo, err)
			}
			out[strings.ToLower(repo)] = GitHubRepoBranchPolicy{
				BranchPolicy:        bp,
				PullRequestsMerge:   rp.Capabilities.PullRequestsMerge,
				PullRequestsApprove: rp.Capabilities.PullRequestsApprove,
			}
		}
	case "1":
		var cfg PermissionConfig
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return nil, fmt.Errorf("invalid permission config: %w", err)
		}
		for i, rule := range cfg.Rules {
			if len(rule.GitHubBranchPolicies) == 0 {
				continue
			}
			if cfg.Provider != "github" {
				return nil, fmt.Errorf("rule %d: github_branch_policies requires provider github", i)
			}
			for repo, entry := range rule.GitHubBranchPolicies {
				if !githubRepoPathPattern.MatchString(repo) {
					return nil, fmt.Errorf("rule %d: invalid github repository %q", i, repo)
				}
				key := strings.ToLower(repo)
				if _, dup := out[key]; dup {
					return nil, fmt.Errorf("rule %d: duplicate github repository %q", i, repo)
				}
				bp := entry.BranchPolicy
				bp.Rules = append([]GitHubBranchRule(nil), bp.Rules...)
				bp.Tags.Create = append([]string(nil), bp.Tags.Create...)
				actionsWrite := githubCapRowLevel(rule.GitHubCapabilityRows, repo, "actions") == GitHubCapWrite
				if err := ValidateGitHubBranchPolicy(&bp, actionsWrite); err != nil {
					return nil, fmt.Errorf("repository %s: %w", repo, err)
				}
				if (entry.PullRequestsMerge || entry.PullRequestsApprove) &&
					githubCapRowLevel(rule.GitHubCapabilityRows, repo, "pull_requests") != GitHubCapWrite {
					return nil, fmt.Errorf("repository %s: pull_requests_merge and pull_requests_approve require pull request write", repo)
				}
				entry.BranchPolicy = bp
				out[key] = entry
			}
		}
	}
	return out, nil
}
