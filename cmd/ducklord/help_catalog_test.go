package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestHelpCatalogCoversFeatureAreasAndQuickShellSequences(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		entries := helpCatalog(workspace)
		categories := map[string]bool{}
		actions := map[string]bool{}
		entryByLabel := map[string]helpEntry{}
		for _, entry := range entries {
			entryByLabel[entry.label] = entry
			if entry.category != "" {
				categories[entry.category] = true
			}
			if entry.action != "" {
				actions[entry.action] = true
			}
		}
		for action := range ducklord.DefaultShortcuts {
			if !actions[action] && helpShortcutAppliesInContext(action, workspace) {
				t.Errorf("default shortcut %q is not discoverable in the catalog", action)
			}
		}
		if workspace && (!actions["list_sort"] || !actions["list_sort_direction"] || actions["list_organize"]) {
			t.Errorf("workspace catalog has inaccurate sort routes: %#v", actions)
		}
		if !workspace && (!actions["list_organize"] || !actions["list_groups"] || actions["list_sort"] || actions["list_sort_direction"]) {
			t.Errorf("legacy catalog has inaccurate organization routes: %#v", actions)
		}
		for _, category := range []string{"SESSION LIST & GROUPS", "PROJECT PANE", "TERMINAL AREA", "NOTES", "PROJECT FILES", "PROJECT TRANSFER", "HOST SKILLS", "HOST RESOURCES"} {
			if !categories[category] {
				t.Errorf("help catalog is missing category %q", category)
			}
		}
		if sessionEntry := entryByLabel["Open selected Session"]; sessionEntry.action != "" || !strings.Contains(sessionEntry.detail, "Enter attaches the selected Session") || !strings.Contains(sessionEntry.detail, "group expands or collapses") {
			t.Errorf("selected Session Enter route is missing or inaccurate: %#v", sessionEntry)
		}
		hostEntry := entryByLabel["Open host list and actions"]
		if !strings.Contains(hostEntry.detail, "Enter opens actions") || !strings.Contains(hostEntry.detail, "Connections stages connect/disconnect with Space, then Enter applies") || strings.Contains(hostEntry.detail, "host list stages connect/disconnect") {
			t.Errorf("Host list and Connections routes are conflated or inaccurate: %q", hostEntry.detail)
		}
		if workspace {
			projectEntry := entryByLabel["Activate selected Project pane"]
			if projectEntry.action != "" || !strings.Contains(projectEntry.detail, "Enter attaches the selected Session pane") || !strings.Contains(projectEntry.detail, "empty Project opens the Terminal tab creation flow") {
				t.Errorf("Project-focus Enter route is missing or inaccurate: %#v", projectEntry)
			}
		} else if _, found := entryByLabel["Activate selected Project pane"]; found {
			t.Error("legacy catalog should not advertise the workspace-only Project pane Enter route")
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
			{"Host Skills dashboard", "public HTTPS"},
			{"Create quick-shell Terminal tab", "Host and CWD"},
			{"Browse Project files", "chooses a bookmark"},
			{"Import Project", "adding a suffix for name collisions"},
			{"Detach focused Session pane", "Default Project does not support"},
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
}

func helpShortcutAppliesInContext(action string, workspace bool) bool {
	switch action {
	case "list_organize", "list_groups", "list_reorder_up", "list_reorder_down":
		return !workspace
	case "list_sort", "list_sort_direction", "project_focus", "project_create", "project_delete", "project_notification_focus", "project_add_pane", "project_move_pane", "project_detach_pane", "project_prev_tab", "project_next_tab", "project_prev_pane", "project_next_pane", "project_hosts", "detail_list", "detail_search", "detail_filter", "detail_previous", "detail_next", "detail_focus", "detail_jump", "pty_copy", "pty_unfocus", "pane_prefix":
		return workspace
	default:
		return true
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
	screen := ducklord.NewTerminal(24, 80, 0)
	screen.Write([]byte(strings.ReplaceAll(out.String(), "\n", "\r\n")))
	visible := strings.Join(screen.RenderLines(24, 80), "\n")
	if !strings.Contains(visible, "F1 close") {
		t.Fatalf("80x24 help footer clipped configured close shortcut:\n%s", visible)
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
