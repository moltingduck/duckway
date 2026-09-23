package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestHelpCatalogCoversFeatureAreasAndQuickShellSequences(t *testing.T) {
	entries := helpCatalog(false)
	categories := map[string]bool{}
	actions := map[string]bool{}
	for _, entry := range entries {
		if entry.category != "" {
			categories[entry.category] = true
		}
		if entry.action != "" {
			actions[entry.action] = true
		}
	}
	for action := range ducklord.DefaultShortcuts {
		if !actions[action] && action != "list_sort" && action != "list_sort_direction" {
			t.Errorf("default shortcut %q is not discoverable in the catalog", action)
		}
	}
	for _, category := range []string{"SESSION LIST & GROUPS", "PROJECT PANE", "TERMINAL AREA", "NOTES", "PROJECT FILES", "PROJECT TRANSFER", "HOST SKILLS", "HOST RESOURCES"} {
		if !categories[category] {
			t.Errorf("help catalog is missing category %q", category)
		}
	}
	for _, action := range []string{"prefix+-", "prefix+\\", "prefix+--", "prefix+\\\\", "prefix+t", "prefix+tt", "prefix+space", "prefix+slash"} {
		if !actions[action] {
			t.Errorf("help catalog is missing supported route %q", action)
		}
	}
	var verticalQuick *helpEntry
	for i := range entries {
		if entries[i].action == "prefix+\\\\" {
			verticalQuick = &entries[i]
		}
	}
	if verticalQuick == nil || !strings.Contains(verticalQuick.detail, "\\ twice within 500 ms") {
		t.Fatalf("vertical quick-shell sequence is not accurately explained: %#v", verticalQuick)
	}
	for _, entry := range entries {
		if entry.label == "Browse scoped Notes" && (!strings.Contains(entry.detail, "Left/Right or h/l changes scope") || !strings.Contains(entry.detail, "j/k or Up/Down selects entries")) {
			t.Errorf("Notes scope/navigation keys are inaccurate: %q", entry.detail)
		}
	}
	for _, check := range []struct{ label, text string }{
		{"Browse Project files", "Backspace/Delete goes to parent"},
		{"Resolve Project file conflicts", "s skips, r renames, o overwrites"},
		{"Host Skills legacy agent list", "not Host Skills dashboard actions"},
	} {
		found := false
		for _, entry := range entries {
			if entry.label == check.label && strings.Contains(entry.detail, check.text) {
				found = true
			}
		}
		if !found {
			t.Errorf("catalog lacks accurate %s guidance containing %q", check.label, check.text)
		}
	}
}

func TestHelpCatalogSearchesGuidanceAndUsesConfiguredShortcuts(t *testing.T) {
	s := &tuiState{
		helpMode:         true,
		workspacePreview: true,
		cfg:              &ducklord.Config{Shortcuts: map[string]string{"pane_prefix": "ctrl+g", "help": "F1"}},
	}
	s.helpSearchQuery = "Open local help"
	var out bytes.Buffer
	s.renderHelpModal(&out, 100, 80)
	if !strings.Contains(out.String(), "ctrl+g ?") {
		t.Fatal("configured pane prefix was not rendered")
	}
	if !strings.Contains(out.String(), "F1 close") {
		t.Fatal("configured Help shortcut was not rendered")
	}
	s.helpSearchQuery = "bookmarks"
	out.Reset()
	s.renderHelpModal(&out, 100, 80)
	if !strings.Contains(strings.ToLower(out.String()), "terminal output bookmarks") {
		t.Fatal("feature guidance is not searchable by its feature term")
	}
	s.helpSearchQuery = "host skills"
	out.Reset()
	s.renderHelpModal(&out, 100, 80)
	if !strings.Contains(out.String(), "push/pull/deploy/import") && !strings.Contains(out.String(), "push") {
		t.Fatal("Host Skills subactions are not discoverable")
	}
}

func TestHelpHighlightsTerminalOriginWithoutMakingUnavailableRowsClickable(t *testing.T) {
	s := &tuiState{workspacePreview: true, focused: true, activeAttachKey: "host/session", cfg: &ducklord.Config{Shortcuts: map[string]string{"pane_prefix": "ctrl+g", "help": "F1"}}}
	s.sessions = []ducklord.RemoteSession{{Client: "host", Cwd: "/work"}}
	s.selected = 0
	s.toggleHelp()
	if s.focused || !s.helpOriginFocused || !s.helpFocusRestorePending {
		t.Fatalf("opening help from a focused Terminal must capture origin and restore focus asynchronously: focused=%v origin=%v pending=%v", s.focused, s.helpOriginFocused, s.helpFocusRestorePending)
	}
	if !s.helpActionAvailableFromTerminalOrigin("prefix+O") {
		t.Fatal("a terminal-only route should remain highlighted from its captured origin")
	}
	if s.helpActionAvailable("prefix+O") {
		t.Fatal("origin highlighting must not make the route dispatchable while help owns focus")
	}
	s.helpSearchQuery = "Search sessions"
	var out bytes.Buffer
	s.renderHelpModal(&out, 80, 24)
	if strings.Contains(out.String(), modalSelected+"  list_search") {
		t.Fatal("navigation-only action should not inherit Terminal-origin highlight")
	}
}

func TestHelpFooterFitsTypicalTerminalWidth(t *testing.T) {
	s := &tuiState{helpMode: true, cfg: &ducklord.Config{Shortcuts: map[string]string{"help": "F1"}}}
	var out bytes.Buffer
	s.renderHelpModal(&out, 80, 24)
	if !strings.Contains(out.String(), "F1 close") {
		t.Fatalf("80x24 help footer clipped configured close shortcut:\n%s", out.String())
	}
}

func TestHelpAvailabilityDimsGuidanceAndContextualRoutes(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}, workspacePreview: false}
	if s.helpActionAvailable("") || s.helpActionAvailable("prefix+tt") {
		t.Fatal("informational entries and workspace routes must not be marked available in the session list")
	}
	s.workspacePreview = true
	if !s.helpActionAvailable("prefix+o") || s.helpActionAvailable("prefix+tt") {
		t.Fatal("Notes route should be available in workspace; quick shell requires live focused Session")
	}
	s.focused = true
	s.sessions = []ducklord.RemoteSession{{Client: "host", Cwd: "/work"}}
	s.selected = 0
	if !s.helpActionAvailable("prefix+tt") {
		t.Fatal("quick-shell sequence should be available for a focused live Session")
	}
	s.sessions[0].Cwd = ""
	if s.helpActionAvailable("prefix+tt") {
		t.Fatal("quick-shell sequence must be dim without a live Session")
	}
}
