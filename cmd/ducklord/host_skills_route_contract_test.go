package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostSkillsRouteDocumentationContract(t *testing.T) {
	root := routingRepoRoot(t)
	pane, err := os.ReadFile(filepath.Join(root, "docs", "pane-routing.md"))
	if err != nil {
		t.Fatal(err)
	}
	ui, err := os.ReadFile(filepath.Join(root, "docs", "ui-routing-verification.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(pane) + "\n" + string(ui)
	if !strings.Contains(string(pane), "route.host-skills") {
		t.Error("pane routing omits route.host-skills")
	}
	if !strings.Contains(string(ui), "route.host-skills") {
		t.Error("UI routing manifest omits route.host-skills")
	}
	for _, branch := range []string{"route.host-skills", "Host list", "Host settings", "dual-pane", "Ducklord repository", "agent tree", "remote delete", "managed rename", "agent", "none", "push", "pull", "h", "Skills", "source", "target", "preview", "result", "Esc", "Ctrl-C", "restore", "clean", "public HTTPS", "INSECURE TLS", "diff", "confirmation"} {
		if !strings.Contains(text, branch) {
			t.Errorf("Host Skills route documentation omits %q", branch)
		}
	}
}

func routingRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "docs", "pane-routing.md")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found")
		}
		dir = parent
	}
}
