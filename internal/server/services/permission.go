package services

import (
	"encoding/json"
	"fmt"
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
	Max       *float64 `json:"max,omitempty"`
	Min       *float64 `json:"min,omitempty"`
	Forbidden bool     `json:"forbidden,omitempty"`
}

type RateLimitConfig struct {
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`
	RequestsPerHour   int `json:"requests_per_hour,omitempty"`
	RequestsPerDay    int `json:"requests_per_day,omitempty"`
}

var githubRepoPathPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

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
	var config PermissionConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return config, fmt.Errorf("invalid permission config: %w", err)
	}
	return config, nil
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
		result := pc.checkRule(rule, placeholderID, method, path, bodyBytes)
		if result.Allowed || result.Reason != "" {
			return result
		}
	}

	return PermissionResult{Allowed: true} // No rules matched = allow by default
}

func (pc *PermissionChecker) checkRule(rule PermissionRule, placeholderID, method, path string, bodyBytes []byte) PermissionResult {
	matched := false

	for _, ep := range rule.Endpoints {
		if matchEndpoint(ep, method, path) {
			matched = true
			if !ep.Allow {
				return PermissionResult{Allowed: false, Reason: fmt.Sprintf("endpoint %s %s denied by rule '%s'", method, path, rule.Name)}
			}

			// Check constraints
			if ep.Constraints != nil && len(bodyBytes) > 0 {
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

func matchEndpoint(ep EndpointRule, method, path string) bool {
	if ep.Method != "*" && ep.Method != method {
		return false
	}
	return matchPath(ep.Path, path)
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
		return "" // Can't parse body, skip constraint checking
	}

	for field, constraint := range constraints.Body {
		val, exists := body[field]

		if constraint.Forbidden {
			if exists {
				return fmt.Sprintf("field '%s' is forbidden", field)
			}
			continue
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
