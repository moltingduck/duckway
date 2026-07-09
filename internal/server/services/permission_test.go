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

func TestRequestBodyRequiredForPermission(t *testing.T) {
	noConstraint := `{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"POST","path":"/OWNER/REPO.git/git-receive-pack","allow":true}],"deny_all_other":true}]}`
	if RequestBodyRequiredForPermission(noConstraint, "POST", "/OWNER/REPO.git/git-receive-pack") {
		t.Fatal("body should not be required when matched endpoint has no body constraints")
	}

	withConstraint := `{"version":"1","provider":"openai","rules":[{"endpoints":[{"method":"POST","path":"/v1/chat/completions","allow":true,"constraints":{"body":{"model":{"oneOf":["gpt-4o-mini"]}}}}],"deny_all_other":true}]}`
	if !RequestBodyRequiredForPermission(withConstraint, "POST", "/v1/chat/completions") {
		t.Fatal("body should be required when matched endpoint has body constraints")
	}
	if RequestBodyRequiredForPermission(withConstraint, "GET", "/v1/models") {
		t.Fatal("body should not be required for non-matching endpoint")
	}
	if !RequestBodyRequiredForPermission(`{`, "POST", "/v1/chat/completions") {
		t.Fatal("invalid config should require body/fallback handling")
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

func TestPermissionChecker_SegmentWildcardDoesNotOvermatch(t *testing.T) {
	config := PermissionConfig{
		Version: "1",
		Rules: []PermissionRule{{
			Endpoints: []EndpointRule{
				{Method: "POST", Path: "/repos/*/*/issues", Allow: true},
			},
			DenyAllOther: true,
		}},
	}

	configJSON, _ := json.Marshal(config)
	pc := NewPermissionChecker()

	r := pc.Check(string(configJSON), "ph-gh", "POST", "/repos/owner/repo/issues", nil)
	if !r.Allowed {
		t.Fatalf("expected issue creation allowed: %s", r.Reason)
	}

	for _, path := range []string{
		"/repos/owner/repo/git/refs",
		"/repos/owner/repo/releases",
		"/repos/owner/repo.git/git-receive-pack",
	} {
		r = pc.Check(string(configJSON), "ph-gh", "POST", path, nil)
		if r.Allowed {
			t.Fatalf("expected %s denied by segment wildcard", path)
		}
	}
}

func TestPermissionChecker_EmptyConfig(t *testing.T) {
	pc := NewPermissionChecker()
	r := pc.Check("", "ph4", "GET", "/anything", nil)
	if !r.Allowed {
		t.Error("empty config should allow everything")
	}
}

func TestValidatePermissionConfigRejectsStructurallyEmptyConfig(t *testing.T) {
	for _, config := range []string{
		`{}`,
		`{"version":"1","provider":"github","rules":[]}`,
		`{"version":"1","provider":"github","rules":[{"name":"empty"}]}`,
		`{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"GET"}]}]}`,
	} {
		if err := ValidatePermissionConfig(config); err == nil {
			t.Fatalf("ValidatePermissionConfig(%s) succeeded, want error", config)
		}
	}
	if err := ValidatePermissionConfig(""); err != nil {
		t.Fatalf("empty config should be valid allow-all: %v", err)
	}
}

func TestValidateGitHubRepoScopePermissionConfig(t *testing.T) {
	valid := `{
		"version":"1",
		"provider":"github",
		"rules":[{
			"name":"deploy-read-only",
			"endpoints":[
				{"method":"GET","path":"/OWNER/REPO.git/info/refs","allow":true},
				{"method":"POST","path":"/OWNER/REPO.git/git-upload-pack","allow":true},
				{"method":"GET","path":"/repos/OWNER/REPO","allow":true},
				{"method":"GET","path":"/repos/OWNER/REPO/*","allow":true}
			],
			"deny_all_other":true
		}]
	}`
	if err := ValidateGitHubRepoScopePermissionConfig(valid); err != nil {
		t.Fatalf("valid GitHub repo scope rejected: %v", err)
	}

	for _, config := range []string{
		``,
		`{"version":"1","provider":"openai","rules":[{"endpoints":[{"method":"GET","path":"/OWNER/REPO.git/info/refs","allow":true}],"deny_all_other":true}]}`,
		`{"version":"1","provider":"github","rules":[]}`,
		`{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"GET","path":"/OWNER/REPO.git/info/refs","allow":true}]}]}`,
		`{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"GET","path":"/*","allow":true}],"deny_all_other":true}]}`,
		`{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"GET","path":"/repos/*","allow":true}],"deny_all_other":true}]}`,
		`{"version":"1","provider":"github","rules":[{"endpoints":[{"method":"POST","path":"/OWNER/REPO.git/git-receive-pack","allow":false}],"deny_all_other":true}]}`,
	} {
		if err := ValidateGitHubRepoScopePermissionConfig(config); err == nil {
			t.Fatalf("ValidateGitHubRepoScopePermissionConfig(%s) succeeded, want error", config)
		}
	}
}
