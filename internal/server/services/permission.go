package services

import (
	"encoding/json"
	"fmt"
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
	Name         string              `json:"name"`
	Endpoints    []EndpointRule      `json:"endpoints"`
	RateLimit    *RateLimitConfig    `json:"rate_limit,omitempty"`
	DenyAllOther bool                `json:"deny_all_other"`
}

type EndpointRule struct {
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Allow       bool                `json:"allow"`
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

	var config PermissionConfig
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
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
	// Simple wildcard: /v1/files/* matches /v1/files/abc and /v1/files/abc/def
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(path, prefix+"/") || path == prefix
	}
	// Exact prefix with trailing wildcard
	if strings.Contains(pattern, "*") {
		parts := strings.SplitN(pattern, "*", 2)
		return strings.HasPrefix(path, parts[0])
	}
	return false
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
