package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

	// MaxGitHubRefNameBytes bounds a concrete branch, tag or ref name.
	MaxGitHubRefNameBytes = 255

	// MaxGitHubMatchNameBytes bounds names handed to the glob matcher (defence
	// in depth; the proxy refuses names above 255 bytes before matching).
	MaxGitHubMatchNameBytes = 1024
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

// truncateRunes shortens s to at most n runes without splitting a UTF-8
// sequence.
func truncateRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// GitHubRefNameProblem is the one ref-name check shared by pattern validation
// and request-time name checks. pattern allows `*` (a glob) and does not
// require a trailing-slash-free name; a concrete name must be a valid git ref
// name as far as the proxy can tell. It returns "" for an acceptable name.
func GitHubRefNameProblem(name string, pattern bool) string {
	if name == "" {
		return "is empty"
	}
	if len(name) > MaxGitHubRefNameBytes && !pattern {
		return fmt.Sprintf("is longer than %d bytes", MaxGitHubRefNameBytes)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f || c == ' ' || c == '~' || c == '^' || c == ':' || c == '[' || c == '\\' || c == '?' {
			return "contains a character git does not allow"
		}
		if c == '*' && !pattern {
			return "contains a wildcard"
		}
	}
	switch {
	case strings.HasPrefix(name, "/"):
		return "must not start with /"
	case strings.Contains(name, ".."), strings.Contains(name, "//"), strings.Contains(name, "@{"):
		return "contains a sequence git does not allow"
	case !pattern && (strings.HasSuffix(name, "/") || name == "@"):
		return "is not a valid ref name"
	}
	return ""
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
		return "", fmt.Errorf("pattern %q is longer than %d characters", truncateRunes(p, 20)+"...", MaxGitHubPatternLength)
	}
	if p == GitHubDefaultBranchToken {
		if !allowDefault {
			return "", fmt.Errorf("%s is only valid for branch rules", GitHubDefaultBranchToken)
		}
		return p, nil
	}
	if prob := GitHubRefNameProblem(p, true); prob != "" {
		return "", fmt.Errorf("pattern %q %s", p, prob)
	}
	return p, nil
}

// ValidateGitHubBranchPolicy checks a policy and rewrites patterns into their
// stored form (no refs/heads/ prefix). dispatch says whether the repository has
// the workflow_dispatch capability, which run_workflows requires.
func ValidateGitHubBranchPolicy(p *GitHubBranchPolicy, dispatch bool) error {
	return validateGitHubBranchPolicy(p, dispatch, "the workflow_dispatch capability")
}

// cloneAndValidate validates a copy of p (so the caller's slices are never
// rewritten) and returns the normalised copy. runCap names what run_workflows
// requires, for the error message.
func (p GitHubBranchPolicy) cloneAndValidate(dispatch bool, runCap string) (GitHubBranchPolicy, error) {
	bp := p
	bp.Rules = append([]GitHubBranchRule(nil), p.Rules...)
	bp.Tags.Create = append([]string(nil), p.Tags.Create...)
	if err := validateGitHubBranchPolicy(&bp, dispatch, runCap); err != nil {
		return GitHubBranchPolicy{}, err
	}
	return bp, nil
}

func validateGitHubBranchPolicy(p *GitHubBranchPolicy, dispatch bool, runCap string) error {
	if len(p.Rules) > MaxGitHubBranchRules {
		return fmt.Errorf("branch_policy allows at most %d rules", MaxGitHubBranchRules)
	}
	seen := map[string]bool{}
	for i := range p.Rules {
		r := &p.Rules[i]
		pat, err := normalizeGitHubPattern(r.Branches, "refs/heads/", true)
		if err != nil {
			return fmt.Errorf("branch rule %q: %w", truncateRunes(r.Branches, 40), err)
		}
		r.Branches = pat
		switch r.Role {
		case GitHubRoleRead, GitHubRoleProtected, GitHubRoleAgent:
		default:
			return fmt.Errorf("branch rule %q: invalid role %q", pat, r.Role)
		}
		if seen[pat] {
			return fmt.Errorf("branch rule %q: duplicate pattern", pat)
		}
		seen[pat] = true
		if r.RunWorkflows && !dispatch {
			return fmt.Errorf("branch rule %q: run_workflows requires %s", pat, runCap)
		}
	}
	if len(p.Tags.Create) > MaxGitHubTagPatterns {
		return fmt.Errorf("branch_policy allows at most %d tag patterns", MaxGitHubTagPatterns)
	}
	seenTags := map[string]bool{}
	for i, t := range p.Tags.Create {
		pat, err := normalizeGitHubPattern(t, "refs/tags/", false)
		if err != nil {
			return fmt.Errorf("tag pattern %q: %w", truncateRunes(t, 40), err)
		}
		if seenTags[pat] {
			return fmt.Errorf("tag pattern %q: duplicate pattern", pat)
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
	if len(name) > MaxGitHubMatchNameBytes || len(pattern) > MaxGitHubPatternLength*2 {
		return false
	}
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
	if defaultBranch != "" && strings.EqualFold(branch, defaultBranch) {
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

var policyCache struct {
	mu sync.Mutex
	m  map[string]policyCacheEntry
}

type policyCacheEntry struct {
	pols map[string]GitHubRepoBranchPolicy
	err  error
}

const policyCacheMax = 256

// ParseGitHubBranchPolicies is GitHubBranchPoliciesFromConfig with the result
// memoised per config string, for the proxy's per-request use. The returned
// map and its slices are shared: callers must not modify them. It decodes the
// config with the same case-insensitive field matching as every other reader,
// so a policy that validation accepted is never missed.
func ParseGitHubBranchPolicies(configJSON string) (map[string]GitHubRepoBranchPolicy, error) {
	if strings.TrimSpace(configJSON) == "" {
		return nil, nil
	}
	policyCache.mu.Lock()
	if e, ok := policyCache.m[configJSON]; ok {
		policyCache.mu.Unlock()
		return e.pols, e.err
	}
	policyCache.mu.Unlock()
	pols, err := GitHubBranchPoliciesFromConfig(configJSON)
	policyCache.mu.Lock()
	if policyCache.m == nil || len(policyCache.m) >= policyCacheMax {
		policyCache.m = map[string]policyCacheEntry{}
	}
	policyCache.m[configJSON] = policyCacheEntry{pols: pols, err: err}
	policyCache.mu.Unlock()
	return pols, err
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
			bp, err := rp.BranchPolicy.cloneAndValidate(rp.Capabilities.WorkflowDispatch, "the workflow_dispatch capability")
			if err != nil {
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
				actionsWrite := githubCapRowLevel(rule.GitHubCapabilityRows, repo, "actions") == GitHubCapWrite
				bp, err := entry.BranchPolicy.cloneAndValidate(actionsWrite, "Actions write")
				if err != nil {
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

// NormalizePermissionConfigPolicies returns configJSON with every branch
// policy stored in normalised form (patterns without refs/heads/ and
// refs/tags/ prefixes). Configs without a branch policy, and configs that
// need no rewriting, are returned byte for byte. The config is assumed to
// have passed ValidatePermissionConfig.
func NormalizePermissionConfigPolicies(configJSON string) (string, error) {
	if strings.TrimSpace(configJSON) == "" {
		return configJSON, nil
	}
	dec := json.NewDecoder(strings.NewReader(configJSON))
	dec.UseNumber()
	var root map[string]interface{}
	if err := dec.Decode(&root); err != nil {
		return "", fmt.Errorf("invalid permission config: %w", err)
	}
	changed := false
	normPolicy := func(v interface{}) {
		pol, ok := v.(map[string]interface{})
		if !ok {
			return
		}
		fix := func(s, prefix string) string {
			if t := strings.TrimPrefix(s, prefix); t != s {
				changed = true
				return t
			}
			return s
		}
		for _, rulesV := range ciValues(pol, "rules") {
			rules, _ := rulesV.([]interface{})
			for _, rv := range rules {
				r, ok := rv.(map[string]interface{})
				if !ok {
					continue
				}
				for k, v := range r {
					if b, ok := v.(string); ok && strings.EqualFold(k, "branches") {
						r[k] = fix(b, "refs/heads/")
					}
				}
			}
		}
		for _, tagsV := range ciValues(pol, "tags") {
			tags, _ := tagsV.(map[string]interface{})
			for k, v := range tags {
				cr, ok := v.([]interface{})
				if !ok || !strings.EqualFold(k, "create") {
					continue
				}
				for i, tv := range cr {
					if t, ok := tv.(string); ok {
						cr[i] = fix(t, "refs/tags/")
					}
				}
			}
		}
	}
	// Go's decoder matches field names case-insensitively, so the walk must too.
	for _, rv := range ciValues(root, "repositories") {
		for _, rp := range ciObjects(rv) {
			for _, pol := range ciValues(rp, "branch_policy") {
				normPolicy(pol)
			}
		}
	}
	for _, rulesV := range ciValues(root, "rules") {
		rules, _ := rulesV.([]interface{})
		for _, rv := range rules {
			rule, ok := rv.(map[string]interface{})
			if !ok {
				continue
			}
			for _, pv := range ciValues(rule, "github_branch_policies") {
				for _, ev := range ciObjects(pv) {
					for _, pol := range ciValues(ev, "branch_policy") {
						normPolicy(pol)
					}
				}
			}
		}
	}
	if !changed {
		return configJSON, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ciValues returns the values of every key of m equal to name ignoring case.
func ciValues(m map[string]interface{}, name string) []interface{} {
	var out []interface{}
	for k, v := range m {
		if strings.EqualFold(k, name) {
			out = append(out, v)
		}
	}
	return out
}

// ciObjects returns the object values of a map of objects.
func ciObjects(v interface{}) []map[string]interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	var out []map[string]interface{}
	for _, e := range m {
		if o, ok := e.(map[string]interface{}); ok {
			out = append(out, o)
		}
	}
	return out
}
