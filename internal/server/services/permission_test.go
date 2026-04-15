package services

import (
	"encoding/json"
	"testing"
)

func TestPermissionChecker_AllowedEndpoint(t *testing.T) {
	config := PermissionConfig{
		Version:  "1",
		Provider: "openai",
		Rules: []PermissionRule{{
			Name: "chat-only",
			Endpoints: []EndpointRule{
				{Method: "POST", Path: "/v1/chat/completions", Allow: true},
				{Method: "GET", Path: "/v1/models", Allow: true},
			},
			DenyAllOther: true,
		}},
	}

	configJSON, _ := json.Marshal(config)
	pc := NewPermissionChecker()

	// Allowed
	r := pc.Check(string(configJSON), "ph1", "POST", "/v1/chat/completions", nil)
	if !r.Allowed {
		t.Errorf("expected allowed, got denied: %s", r.Reason)
	}

	r = pc.Check(string(configJSON), "ph1", "GET", "/v1/models", nil)
	if !r.Allowed {
		t.Errorf("expected allowed, got denied: %s", r.Reason)
	}

	// Denied
	r = pc.Check(string(configJSON), "ph1", "POST", "/v1/images/generations", nil)
	if r.Allowed {
		t.Error("expected denied for unlisted endpoint")
	}
}

func TestPermissionChecker_ModelConstraint(t *testing.T) {
	max := 1024.0
	config := PermissionConfig{
		Version:  "1",
		Provider: "openai",
		Rules: []PermissionRule{{
			Endpoints: []EndpointRule{{
				Method: "POST",
				Path:   "/v1/chat/completions",
				Allow:  true,
				Constraints: &EndpointConstraints{
					Body: map[string]FieldConstraint{
						"model":      {OneOf: []string{"gpt-4o-mini"}},
						"max_tokens": {Max: &max},
					},
				},
			}},
			DenyAllOther: true,
		}},
	}

	configJSON, _ := json.Marshal(config)
	pc := NewPermissionChecker()

	// Allowed model
	body := `{"model":"gpt-4o-mini","max_tokens":512}`
	r := pc.Check(string(configJSON), "ph2", "POST", "/v1/chat/completions", []byte(body))
	if !r.Allowed {
		t.Errorf("expected allowed: %s", r.Reason)
	}

	// Denied model
	body = `{"model":"gpt-4o","max_tokens":512}`
	r = pc.Check(string(configJSON), "ph2", "POST", "/v1/chat/completions", []byte(body))
	if r.Allowed {
		t.Error("expected denied for wrong model")
	}

	// Denied max_tokens
	body = `{"model":"gpt-4o-mini","max_tokens":2048}`
	r = pc.Check(string(configJSON), "ph2", "POST", "/v1/chat/completions", []byte(body))
	if r.Allowed {
		t.Error("expected denied for max_tokens exceeding limit")
	}
}

func TestPermissionChecker_WildcardPath(t *testing.T) {
	config := PermissionConfig{
		Version: "1",
		Rules: []PermissionRule{{
			Endpoints: []EndpointRule{
				{Method: "GET", Path: "/repos/*", Allow: true},
			},
			DenyAllOther: true,
		}},
	}

	configJSON, _ := json.Marshal(config)
	pc := NewPermissionChecker()

	r := pc.Check(string(configJSON), "ph3", "GET", "/repos/owner/name", nil)
	if !r.Allowed {
		t.Errorf("expected wildcard match: %s", r.Reason)
	}

	r = pc.Check(string(configJSON), "ph3", "GET", "/users/me", nil)
	if r.Allowed {
		t.Error("expected denied for non-matching path")
	}
}

func TestPermissionChecker_EmptyConfig(t *testing.T) {
	pc := NewPermissionChecker()
	r := pc.Check("", "ph4", "GET", "/anything", nil)
	if !r.Allowed {
		t.Error("empty config should allow everything")
	}
}
