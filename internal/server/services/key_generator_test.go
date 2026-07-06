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
		{"GitHub", "ghp_", 40, "ghp_dw_"},
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
