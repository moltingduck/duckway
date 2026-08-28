package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PermissionConfig is the JSON structure stored in placeholder_keys.permission_config.
type PermissionConfig struct {
	Version  string           `json:"version"`
	Provider string           `json:"provider"`
	Rules    []PermissionRule `json:"rules"`
}

type PermissionRule struct {
	Name         string           `json:"name"`
	Endpoints    []EndpointRule   `json:"endpoints"`
	RateLimit    *RateLimitConfig `json:"rate_limit,omitempty"`
	DenyAllOther bool             `json:"deny_all_other"`
}

type EndpointRule struct {
	Method      string               `json:"method"`
	Path        string               `json:"path"`
	Allow       bool                 `json:"allow"`
	Constraints *EndpointConstraints `json:"constraints,omitempty"`
}

type EndpointConstraints struct {
	Body    map[string]FieldConstraint `json:"body,omitempty"`
	Headers map[string]FieldConstraint `json:"headers,omitempty"`
}

type FieldConstraint struct {
	OneOf     []string `json:"oneOf,omitempty"`
	Matches   []string `json:"matches,omitempty"`
	Max       *float64 `json:"max,omitempty"`
	Min       *float64 `json:"min,omitempty"`
	Forbidden bool     `json:"forbidden,omitempty"`
	Required  bool     `json:"required,omitempty"`
}

type GitHubCapabilityPermissionConfig struct {
	Version      string                            `json:"version"`
	Provider     string                            `json:"provider"`
	Repositories map[string]GitHubRepositoryPolicy `json:"repositories"`
}

type GitHubRepositoryPolicy struct {
	Capabilities         GitHubCapabilities `json:"capabilities"`
	WorkflowAllowlist    []string           `json:"workflow_allowlist,omitempty"`
	RefAllowlist         []string           `json:"ref_allowlist,omitempty"`
	EnvironmentAllowlist []string           `json:"environment_allowlist,omitempty"`
}

type GitHubCapabilities struct {
	Clone             bool `json:"clone"`
	Push              bool `json:"push"`
	IssuesRead        bool `json:"issues_read"`
	IssuesWrite       bool `json:"issues_write"`
	PullRequestsRead  bool `json:"pull_requests_read"`
	PullRequestsWrite bool `json:"pull_requests_write"`
	ReleasesRead      bool `json:"releases_read"`
	ReleasesWrite     bool `json:"releases_write"`
	ActionsRead       bool `json:"actions_read"`
	WorkflowDispatch  bool `json:"workflow_dispatch"`
	WorkflowRerun     bool `json:"workflow_rerun"`
	WorkflowCancel    bool `json:"workflow_cancel"`
}

type RateLimitConfig struct {
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`
	RequestsPerHour   int `json:"requests_per_hour,omitempty"`
	RequestsPerDay    int `json:"requests_per_day,omitempty"`
}

var githubRepoPathPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var githubWorkflowPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ValidatePermissionConfig checks the structural shape of a non-empty
// permission config before storing it. Empty config still means allow-all.
func ValidatePermissionConfig(configJSON string) error {
	if strings.TrimSpace(configJSON) == "" {
		return nil
	}
	config, err := ParsePermissionConfig(configJSON)
	if err != nil {
		return err
	}
	if len(config.Rules) == 0 {
		return fmt.Errorf("permission config must contain at least one rule")
	}
	for i, rule := range config.Rules {
		if len(rule.Endpoints) == 0 {
			return fmt.Errorf("rule %d must contain at least one endpoint", i)
		}
		for j, ep := range rule.Endpoints {
			if strings.TrimSpace(ep.Method) == "" {
				return fmt.Errorf("rule %d endpoint %d missing method", i, j)
			}
			if strings.TrimSpace(ep.Path) == "" {
				return fmt.Errorf("rule %d endpoint %d missing path", i, j)
			}
		}
	}
	return nil
}

// ValidateGitHubRepoScopePermissionConfig ensures a GitHub App minter
// placeholder can only mint for an explicit repository allow-list.
func ValidateGitHubRepoScopePermissionConfig(configJSON string) error {
	if strings.TrimSpace(configJSON) == "" {
		return fmt.Errorf("github app minter assignments require repository-scoped permission_config")
	}
	config, err := ParsePermissionConfig(configJSON)
	if err != nil {
		return err
	}
	if config.Provider != "github" {
		return fmt.Errorf("github app minter permission_config provider must be github")
	}
	if config.Version == "2" {
		return nil
	}
	if len(config.Rules) == 0 {
		return fmt.Errorf("github app minter permission_config must contain at least one rule")
	}
	for i, rule := range config.Rules {
		if !rule.DenyAllOther {
			return fmt.Errorf("github app minter rule %d must set deny_all_other", i)
		}
		if len(rule.Endpoints) == 0 {
			return fmt.Errorf("github app minter rule %d must contain endpoints", i)
		}
		for j, ep := range rule.Endpoints {
			repo, ok := githubRepoFromScopedEndpoint(ep)
			if !ok || !githubRepoPathPattern.MatchString(repo) {
				return fmt.Errorf("github app minter rule %d endpoint %d is not an allowed repository-scoped GitHub path", i, j)
			}
		}
	}
	return nil
}

func ParsePermissionConfig(configJSON string) (PermissionConfig, error) {
	var header struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(configJSON), &header); err != nil {
		return PermissionConfig{}, fmt.Errorf("invalid permission config: %w", err)
	}
	if header.Version == "2" {
		return parseGitHubCapabilityPermissionConfig(configJSON)
	}
	if header.Version != "1" {
		return PermissionConfig{}, fmt.Errorf("unsupported permission config version %q", header.Version)
	}
	var config PermissionConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return config, fmt.Errorf("invalid permission config: %w", err)
	}
	return config, nil
}

func parseGitHubCapabilityPermissionConfig(configJSON string) (PermissionConfig, error) {
	var policy GitHubCapabilityPermissionConfig
	dec := json.NewDecoder(bytes.NewBufferString(configJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&policy); err != nil {
		return PermissionConfig{}, fmt.Errorf("invalid github capability permission config: %w", err)
	}
	if policy.Version != "2" || policy.Provider != "github" {
		return PermissionConfig{}, fmt.Errorf("github capability permission config requires version 2 and provider github")
	}
	if len(policy.Repositories) == 0 {
		return PermissionConfig{}, fmt.Errorf("github capability permission config requires repositories")
	}
	endpoints := make([]EndpointRule, 0)
	seenRepos := make(map[string]struct{}, len(policy.Repositories))
	for repo, repoPolicy := range policy.Repositories {
		if !githubRepoPathPattern.MatchString(repo) {
			return PermissionConfig{}, fmt.Errorf("invalid github repository %q", repo)
		}
		canonicalRepo := strings.ToLower(repo)
		if _, exists := seenRepos[canonicalRepo]; exists {
			return PermissionConfig{}, fmt.Errorf("duplicate case-insensitive github repository %q", repo)
		}
		seenRepos[canonicalRepo] = struct{}{}
		var err error
		endpoints, err = appendGitHubCapabilityEndpoints(endpoints, canonicalRepo, repoPolicy)
		if err != nil {
			return PermissionConfig{}, fmt.Errorf("repository %s: %w", repo, err)
		}
	}
	return PermissionConfig{Version: "2", Provider: "github", Rules: []PermissionRule{{
		Name: "github capabilities", Endpoints: endpoints, DenyAllOther: true,
	}}}, nil
}

func appendGitHubCapabilityEndpoints(dst []EndpointRule, repo string, p GitHubRepositoryPolicy) ([]EndpointRule, error) {
	start := len(dst)
	c := p.Capabilities
	if c.Push && !c.Clone || c.IssuesWrite && !c.IssuesRead || c.PullRequestsWrite && !c.PullRequestsRead || c.ReleasesWrite && !c.ReleasesRead {
		return nil, fmt.Errorf("write capabilities require their corresponding read capability")
	}
	if (c.WorkflowDispatch || c.WorkflowRerun || c.WorkflowCancel) && !c.ActionsRead {
		return nil, fmt.Errorf("workflow mutation capabilities require actions_read")
	}
	if c.WorkflowDispatch && (len(p.WorkflowAllowlist) == 0 || len(p.RefAllowlist) == 0) {
		return nil, fmt.Errorf("workflow_dispatch requires workflow_allowlist and ref_allowlist")
	}
	add := func(method, suffix string, constraints *EndpointConstraints) {
		dst = append(dst, EndpointRule{Method: method, Path: suffix, Allow: true, Constraints: constraints})
	}
	if c.Clone {
		add("GET", "/"+repo+".git/info/refs", nil)
		add("POST", "/"+repo+".git/git-upload-pack", nil)
		add("GET", "/repos/"+repo, nil)
	}
	if c.Push {
		add("POST", "/"+repo+".git/git-receive-pack", nil)
	}
	addREST := func(read, write bool, resource string) {
		if read {
			add("GET", "/repos/"+repo+"/"+resource, nil)
			add("GET", "/repos/"+repo+"/"+resource+"/*", nil)
		}
		if write {
			add("POST", "/repos/"+repo+"/"+resource, nil)
			add("POST", "/repos/"+repo+"/"+resource+"/*", nil)
			add("PATCH", "/repos/"+repo+"/"+resource+"/*", nil)
			add("DELETE", "/repos/"+repo+"/"+resource+"/*", nil)
		}
	}
	addREST(c.IssuesRead, c.IssuesWrite, "issues")
	addREST(c.PullRequestsRead, c.PullRequestsWrite, "pulls")
	addREST(c.ReleasesRead, c.ReleasesWrite, "releases")
	if c.ActionsRead {
		add("GET", "/repos/"+repo+"/actions", nil)
		add("GET", "/repos/"+repo+"/actions/*", nil)
	}
	if c.WorkflowDispatch {
		for _, workflow := range p.WorkflowAllowlist {
			if !githubWorkflowPattern.MatchString(workflow) {
				return nil, fmt.Errorf("invalid workflow %q", workflow)
			}
			body := map[string]FieldConstraint{"ref": {Matches: p.RefAllowlist, Required: true}}
			if len(p.EnvironmentAllowlist) > 0 {
				body["inputs.environment"] = FieldConstraint{OneOf: p.EnvironmentAllowlist}
			}
			add("POST", "/repos/"+repo+"/actions/workflows/"+workflow+"/dispatches", &EndpointConstraints{Body: body})
		}
	}
	if c.WorkflowRerun {
		add("POST", "/repos/"+repo+"/actions/runs/*/rerun", nil)
	}
	if c.WorkflowCancel {
		add("POST", "/repos/"+repo+"/actions/runs/*/cancel", nil)
	}
	if len(dst) == start {
		return nil, fmt.Errorf("at least one capability must be enabled")
	}
	return dst, nil
}

func githubRepoFromScopedEndpoint(ep EndpointRule) (string, bool) {
	method := strings.ToUpper(ep.Method)
	path := strings.TrimSpace(ep.Path)
	if strings.Contains(path, "..") {
		return "", false
	}
	switch {
	case method == "GET" && strings.HasSuffix(path, ".git/info/refs"):
		return strings.TrimPrefix(strings.TrimSuffix(path, ".git/info/refs"), "/"), ep.Allow
	case method == "POST" && strings.HasSuffix(path, ".git/git-upload-pack"):
		return strings.TrimPrefix(strings.TrimSuffix(path, ".git/git-upload-pack"), "/"), ep.Allow
	case method == "POST" && strings.HasSuffix(path, ".git/git-receive-pack"):
		return strings.TrimPrefix(strings.TrimSuffix(path, ".git/git-receive-pack"), "/"), ep.Allow
	case method == "GET" && strings.HasPrefix(path, "/repos/") && !strings.HasSuffix(path, "/*"):
		return strings.TrimPrefix(path, "/repos/"), ep.Allow
	case method == "GET" && strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/*"):
		return strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/*"), ep.Allow
	default:
		return "", false
	}
}

// PermissionChecker evaluates requests against permission configs.
type PermissionChecker struct {
	rateLimits sync.Map // key: placeholderID+window -> *rateLimitState
}

type rateLimitState struct {
	count    int
	windowAt time.Time
}

func NewPermissionChecker() *PermissionChecker {
	return &PermissionChecker{}
}

type PermissionResult struct {
	Allowed bool
	Reason  string
}

// Check evaluates whether a request is permitted.
func (pc *PermissionChecker) Check(configJSON string, placeholderID, method, path string, bodyBytes []byte) PermissionResult {
	if configJSON == "" {
		return PermissionResult{Allowed: true}
	}

	config, err := ParsePermissionConfig(configJSON)
	if err != nil {
		return PermissionResult{Allowed: false, Reason: "invalid permission config: " + err.Error()}
	}

	for _, rule := range config.Rules {
		result := pc.checkRule(config.Provider, rule, placeholderID, method, path, bodyBytes)
		if result.Allowed || result.Reason != "" {
			return result
		}
	}

	return PermissionResult{Allowed: true} // No rules matched = allow by default
}

// RequestBodyRequiredForPermission reports whether evaluating configJSON for
// this request may require reading the request body. Callers can use this to
// keep large binary requests streaming when permission checks only depend on
// method/path/rate limits.
func RequestBodyRequiredForPermission(configJSON, method, path string) bool {
	if strings.TrimSpace(configJSON) == "" {
		return false
	}
	config, err := ParsePermissionConfig(configJSON)
	if err != nil {
		return true
	}
	for _, rule := range config.Rules {
		for _, ep := range rule.Endpoints {
			if !matchEndpoint(config.Provider, ep, method, path) || ep.Constraints == nil {
				continue
			}
			if len(ep.Constraints.Body) > 0 {
				return true
			}
		}
	}
	return false
}

func (pc *PermissionChecker) checkRule(provider string, rule PermissionRule, placeholderID, method, path string, bodyBytes []byte) PermissionResult {
	matched := false

	for _, ep := range rule.Endpoints {
		if matchEndpoint(provider, ep, method, path) {
			matched = true
			if !ep.Allow {
				return PermissionResult{Allowed: false, Reason: fmt.Sprintf("endpoint %s %s denied by rule '%s'", method, path, rule.Name)}
			}

			// Check constraints
			if ep.Constraints != nil && len(ep.Constraints.Body) > 0 {
				if reason := checkConstraints(ep.Constraints, bodyBytes); reason != "" {
					return PermissionResult{Allowed: false, Reason: reason}
				}
			}
		}
	}

	if !matched && rule.DenyAllOther {
		return PermissionResult{Allowed: false, Reason: fmt.Sprintf("endpoint %s %s not allowed by rule '%s'", method, path, rule.Name)}
	}

	if matched {
		// Check rate limit
		if rule.RateLimit != nil {
			if reason := pc.checkRateLimit(rule.RateLimit, placeholderID); reason != "" {
				return PermissionResult{Allowed: false, Reason: reason}
			}
		}
		return PermissionResult{Allowed: true}
	}

	return PermissionResult{} // Not matched, not denied
}

func matchEndpoint(provider string, ep EndpointRule, method, path string) bool {
	if ep.Method != "*" && ep.Method != method {
		return false
	}
	if provider == "github" && matchGitHubRepoPath(ep.Path, path) {
		return true
	}
	return matchPath(ep.Path, path)
}

func matchGitHubRepoPath(pattern, path string) bool {
	pRepo, pKind, pSuffix, ok := githubRepoPathParts(pattern)
	if !ok {
		return false
	}
	repo, kind, suffix, ok := githubRepoPathParts(path)
	if !ok || pKind != kind || !strings.EqualFold(pRepo, repo) {
		return false
	}
	return pSuffix == suffix || (pKind == "rest" && matchPath(pSuffix, suffix))
}

func githubRepoPathParts(raw string) (repo, kind, suffix string, ok bool) {
	path := strings.TrimSpace(raw)
	switch {
	case strings.HasSuffix(path, ".git/info/refs"):
		return strings.TrimPrefix(strings.TrimSuffix(path, ".git/info/refs"), "/"), "git", "info/refs", true
	case strings.HasSuffix(path, ".git/git-upload-pack"):
		return strings.TrimPrefix(strings.TrimSuffix(path, ".git/git-upload-pack"), "/"), "git", "git-upload-pack", true
	case strings.HasSuffix(path, ".git/git-receive-pack"):
		return strings.TrimPrefix(strings.TrimSuffix(path, ".git/git-receive-pack"), "/"), "git", "git-receive-pack", true
	case strings.HasPrefix(path, "/repos/"):
		rest := strings.TrimPrefix(path, "/repos/")
		parts := strings.Split(rest, "/")
		if len(parts) < 2 {
			return "", "", "", false
		}
		repo := parts[0] + "/" + parts[1]
		if len(parts) == 2 {
			return repo, "rest", "", true
		}
		return repo, "rest", strings.Join(parts[2:], "/"), true
	default:
		return "", "", "", false
	}
}

func matchPath(pattern, path string) bool {
	if pattern == path {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")
	if len(patternParts) == 1 && patternParts[0] == "*" {
		return true
	}
	if len(patternParts) > 0 && patternParts[len(patternParts)-1] == "*" {
		if len(pathParts) < len(patternParts)-1 {
			return false
		}
		for i := 0; i < len(patternParts)-1; i++ {
			if patternParts[i] == "*" {
				continue
			}
			if patternParts[i] != pathParts[i] {
				return false
			}
		}
		return true
	}
	if len(patternParts) != len(pathParts) {
		return false
	}
	for i := range patternParts {
		if patternParts[i] == "*" {
			continue
		}
		if patternParts[i] != pathParts[i] {
			return false
		}
	}
	return true
}

func checkConstraints(constraints *EndpointConstraints, bodyBytes []byte) string {
	if constraints.Body == nil {
		return ""
	}

	var body map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		return "request body must be valid JSON"
	}

	for field, constraint := range constraints.Body {
		val, exists := nestedField(body, field)

		if constraint.Forbidden {
			if exists {
				return fmt.Sprintf("field '%s' is forbidden", field)
			}
			continue
		}

		if !exists && constraint.Required {
			return fmt.Sprintf("field '%s' is required", field)
		}
		if !exists {
			continue // Field not present, skip
		}

		if len(constraint.OneOf) > 0 {
			strVal, ok := val.(string)
			if !ok {
				return fmt.Sprintf("field '%s' must be a string", field)
			}
			found := false
			for _, allowed := range constraint.OneOf {
				if strVal == allowed {
					found = true
					break
				}
			}
			if !found {
				return fmt.Sprintf("field '%s' value '%s' not in allowed list %v", field, strVal, constraint.OneOf)
			}
		}
		if len(constraint.Matches) > 0 {
			strVal, ok := val.(string)
			if !ok {
				return fmt.Sprintf("field '%s' must be a string", field)
			}
			matched := false
			for _, pattern := range constraint.Matches {
				if ok, _ := path.Match(pattern, strVal); ok {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Sprintf("field '%s' value '%s' does not match allowed patterns", field, strVal)
			}
		}

		if constraint.Max != nil {
			numVal, ok := toFloat64(val)
			if ok && numVal > *constraint.Max {
				return fmt.Sprintf("field '%s' value %v exceeds max %v", field, numVal, *constraint.Max)
			}
		}

		if constraint.Min != nil {
			numVal, ok := toFloat64(val)
			if ok && numVal < *constraint.Min {
				return fmt.Sprintf("field '%s' value %v below min %v", field, numVal, *constraint.Min)
			}
		}
	}

	return ""
}

func nestedField(body map[string]interface{}, field string) (interface{}, bool) {
	var current interface{} = body
	for _, part := range strings.Split(field, ".") {
		obj, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func (pc *PermissionChecker) checkRateLimit(config *RateLimitConfig, placeholderID string) string {
	now := time.Now()

	if config.RequestsPerMinute > 0 {
		if reason := pc.checkWindow(placeholderID, "minute", time.Minute, config.RequestsPerMinute, now); reason != "" {
			return reason
		}
	}
	if config.RequestsPerHour > 0 {
		if reason := pc.checkWindow(placeholderID, "hour", time.Hour, config.RequestsPerHour, now); reason != "" {
			return reason
		}
	}
	if config.RequestsPerDay > 0 {
		if reason := pc.checkWindow(placeholderID, "day", 24*time.Hour, config.RequestsPerDay, now); reason != "" {
			return reason
		}
	}

	return ""
}

func (pc *PermissionChecker) checkWindow(placeholderID, window string, duration time.Duration, limit int, now time.Time) string {
	key := placeholderID + ":" + window
	val, _ := pc.rateLimits.LoadOrStore(key, &rateLimitState{windowAt: now})
	state := val.(*rateLimitState)

	if now.Sub(state.windowAt) > duration {
		state.count = 0
		state.windowAt = now
	}

	state.count++
	if state.count > limit {
		return fmt.Sprintf("rate limit exceeded: %d requests per %s (limit: %d)", state.count, window, limit)
	}
	return ""
}
