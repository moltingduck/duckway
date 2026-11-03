package web

import (
	"os"
	"strings"
	"testing"
)

func TestCCTemplateShowsCodexSandboxOutsideEdit(t *testing.T) {
	template, err := os.ReadFile("templates/cc.html")
	if err != nil {
		t.Fatalf("read CC template: %v", err)
	}
	source := string(template)

	checks := []string{
		"function sandboxBadge(sandbox)",
		"${agentLabel(c.agent_type)}${sandboxBadge(sb)}",
	}
	for _, check := range checks {
		if !strings.Contains(source, check) {
			t.Errorf("CC template does not render Codex sandbox outside Edit; missing %q", check)
		}
	}
}

func TestCCTemplateCodexSandboxBadgeHighlightsUnsafeModes(t *testing.T) {
	template, err := os.ReadFile("templates/cc.html")
	if err != nil {
		t.Fatalf("read CC template: %v", err)
	}
	source := string(template)

	checks := []string{
		"sandbox === 'danger-full-access' ? 'badge-red'",
		"sandbox === 'none' ? 'badge-yellow'",
		"configured sandbox: ${label}",
		"none (use client Codex config)",
		"duckway cc watch restart",
	}
	for _, check := range checks {
		if !strings.Contains(source, check) {
			t.Errorf("CC template is missing sandbox badge behavior %q", check)
		}
	}
}
