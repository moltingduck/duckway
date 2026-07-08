package services

import (
	"strings"
	"testing"
)

func TestGeneratePlaceholder(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		totalLen   int
		wantPrefix string
	}{
		{"OpenAI", "sk-proj-", 56, "sk-proj-dw_"},
		{"GitHub fine-grained", "github_pat_", 93, "github_pat_dw_"},
		{"GitHub classic", "ghp_", 40, "ghp_dw_"},
		{"Anthropic", "sk-ant-", 108, "sk-ant-dw_"},
		{"No prefix", "", 32, "dw_"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := GeneratePlaceholder(tt.prefix, tt.totalLen)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !strings.HasPrefix(key, tt.wantPrefix) {
				t.Errorf("key %q does not start with %q", key, tt.wantPrefix)
			}

			if len(key) != tt.totalLen {
				t.Errorf("key length %d, want %d", len(key), tt.totalLen)
			}

			if !IsPlaceholder(key) {
				t.Errorf("IsPlaceholder(%q) = false, want true", key)
			}
		})
	}
}

func TestIsPlaceholder(t *testing.T) {
	if IsPlaceholder("sk-proj-real-key-12345") {
		t.Error("real key detected as placeholder")
	}
	if !IsPlaceholder("sk-proj-dw_abc123") {
		t.Error("placeholder key not detected")
	}
}

func TestGeneratePlaceholderForRealKey_JWT(t *testing.T) {
	source := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJodHRwczovL2F1dGgub3BlbmFpLmNvbSIsImV4cCI6MTg5MzQ1NjAwMH0.sig"
	got, err := GeneratePlaceholderForRealKey(source, "sk-", 64)
	if err != nil {
		t.Fatalf("GeneratePlaceholderForRealKey: %v", err)
	}
	if parts := strings.Split(got, "."); len(parts) != 3 {
		t.Fatalf("JWT source should produce JWT-shaped phantom, got %q", got)
	}
	if !IsPlaceholder(got) {
		t.Fatalf("JWT phantom not detected as placeholder: %q", got)
	}
	if strings.HasPrefix(got, "sk-") {
		t.Fatalf("JWT source should not fall back to API key shape: %q", got)
	}
}

func TestGeneratePlaceholderForRealKey_GitHubFineGrainedPAT(t *testing.T) {
	real := "github_pat_" + strings.Repeat("a", 82)
	got, err := GeneratePlaceholderForRealKey(real, "ghp_", 40)
	if err != nil {
		t.Fatalf("GeneratePlaceholderForRealKey: %v", err)
	}
	if !strings.HasPrefix(got, "github_pat_") {
		t.Fatalf("placeholder prefix = %q, want github_pat_: %q", got[:min(len(got), 20)], got)
	}
	if len(got) != 93 {
		t.Fatalf("placeholder length = %d, want 93: %q", len(got), got)
	}
	if !strings.Contains(got, "dw_") {
		t.Fatalf("placeholder missing duckway marker: %q", got)
	}
	if !IsPlaceholder(got) {
		t.Fatalf("IsPlaceholder(%q) = false", got)
	}
}

func TestGeneratePlaceholderForRealKey_GitHubClassicPAT(t *testing.T) {
	real := "ghp_" + strings.Repeat("a", 36)
	got, err := GeneratePlaceholderForRealKey(real, "github_pat_", 93)
	if err != nil {
		t.Fatalf("GeneratePlaceholderForRealKey: %v", err)
	}
	if !strings.HasPrefix(got, "ghp_") {
		t.Fatalf("placeholder prefix = %q, want ghp_: %q", got[:min(len(got), 8)], got)
	}
	if len(got) != 40 {
		t.Fatalf("placeholder length = %d, want 40: %q", len(got), got)
	}
	if !IsPlaceholder(got) {
		t.Fatalf("IsPlaceholder(%q) = false", got)
	}
}

func TestGeneratePlaceholderForRealKey_GitHubAppCredentialUsesStatelessInstallationLength(t *testing.T) {
	real := `{"type":"github_app","app_id":99,"installation_id":123,"private_key":"-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----"}`
	got, err := GeneratePlaceholderForRealKey(real, "github_pat_", 93)
	if err != nil {
		t.Fatalf("GeneratePlaceholderForRealKey: %v", err)
	}
	if !strings.HasPrefix(got, "ghs_") {
		t.Fatalf("placeholder prefix = %q, want ghs_: %q", got[:min(len(got), 8)], got)
	}
	if len(got) != 520 {
		t.Fatalf("placeholder length = %d, want 520: %q", len(got), got)
	}
	if !strings.Contains(got, "dw_") {
		t.Fatalf("placeholder missing duckway marker: %q", got)
	}
	if !IsPlaceholder(got) {
		t.Fatalf("IsPlaceholder(%q) = false", got)
	}
}

func TestGeneratePlaceholderForRealKey_GitHubSupportedTokenFormats(t *testing.T) {
	tests := []struct {
		name string
		real string
		want string
	}{
		{"oauth access token", "gho_" + strings.Repeat("a", 36), "gho_"},
		{"app user token", "ghu_" + strings.Repeat("b", 36), "ghu_"},
		{"app installation token", "ghs_" + strings.Repeat("c", 36), "ghs_"},
		{"app installation stateless token", "ghs_123456_" + strings.Repeat("d", 120), "ghs_"},
		{"app refresh token", "ghr_" + strings.Repeat("e", 36), "ghr_"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GeneratePlaceholderForRealKey(tt.real, "github_pat_", 93)
			if err != nil {
				t.Fatalf("GeneratePlaceholderForRealKey: %v", err)
			}
			if !strings.HasPrefix(got, tt.want) {
				t.Fatalf("placeholder prefix = %q, want %q: %q", got[:min(len(got), len(tt.want))], tt.want, got)
			}
			if len(got) != len(tt.real) {
				t.Fatalf("placeholder length = %d, want %d: %q", len(got), len(tt.real), got)
			}
			if !strings.Contains(got, "dw_") {
				t.Fatalf("placeholder missing duckway marker: %q", got)
			}
			if !IsPlaceholder(got) {
				t.Fatalf("IsPlaceholder(%q) = false", got)
			}
		})
	}
}

func TestGeneratePassword(t *testing.T) {
	pw, err := GeneratePassword(16)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pw) != 16 {
		t.Errorf("password length %d, want 16", len(pw))
	}

	// Verify no ambiguous characters
	for _, c := range pw {
		if c == '0' || c == 'O' || c == 'l' || c == 'I' || c == '1' {
			// 1 is in the charset, but 0, O, l, I are excluded
			if c == '0' || c == 'O' || c == 'l' || c == 'I' {
				t.Errorf("password contains ambiguous character: %c", c)
			}
		}
	}
}
