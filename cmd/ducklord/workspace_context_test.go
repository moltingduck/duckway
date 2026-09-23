package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestWorkspaceContextConfigTargets(t *testing.T) {
	for _, target := range []string{"project", "tab", "quick", "pane", "blank", "release"} {
		t.Run(target, func(t *testing.T) {
			s, projectID, a, b := workspacePaneTestState(t)
			nav, _ := s.workspaceNavigation()
			_ = nav.SelectProject(projectID)
			width, height := terminalSize()
			g := ducklord.CalculateWorkspaceGeometry(width, height, 4)
			x, y := g.Quick.X+1, g.Quick.Y+2
			switch target {
			case "project":
				for i, p := range s.activity().ProjectLayout.Projects {
					if p.ID == projectID {
						x, y = g.Projects.X+1, g.Projects.Y+1+i
					}
				}
			case "tab":
				x, y = g.Terminal.X+modalCellWidth(" Work  ")+1, g.Terminal.Y
			case "pane":
				pane := ducklord.WorkspaceVisiblePaneRects(&s.activity().ProjectLayout, nav, g)[0]
				x, y = pane.Rect.X+1, pane.Rect.Y+1
				s.selected = 1 // Clicked pane A must not accidentally configure selected B.
			case "blank":
				x, y = g.Quick.X, g.Quick.Y
			}
			before := s.selected
			handled, _ := s.handleWorkspaceMouse(workspaceMouse(2, x, y, target == "release"))
			if !handled || s.focused || s.workspaceMouseFocus || s.selected != before {
				t.Fatal("context click changed PTY ownership or quick selection")
			}
			switch target {
			case "project":
				if !s.workspacePaneMode || s.workspacePaneStep != "context-config" || s.workspacePaneIntent.projectID != projectID {
					t.Fatalf("wrong project config: %+v", s.workspacePaneIntent)
				}
			case "tab":
				if !s.workspacePaneMode || s.workspacePaneStep != "tab-rename" || s.workspacePaneIntent.tabID != nav.CurrentTabID() {
					t.Fatal("tab rename not opened")
				}
			case "quick", "pane":
				want := b
				if target == "pane" {
					want = a
				}
				if !s.actionMenu || s.actionTarget.SessionID != want.SessionID || s.actionTarget.InstanceID != want.InstanceID {
					t.Fatalf("wrong session target: %+v", s.actionTarget)
				}
			case "blank":
				if !s.workspacePaneMode || s.workspacePaneName != "session-list" {
					t.Fatal("list header did not open settings")
				}
			default:
				if s.actionMenu || s.workspacePaneMode {
					t.Fatal("non-target click opened a form")
				}
			}
		})
	}
}

func TestWorkspaceProjectHelpAndContextHighlight(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	input := []byte(shortcutInput(s.cfg.Shortcut("help")))
	if handled, _ := s.handleWorkspaceProjectInput(input); handled {
		t.Fatal("Project intercepted help")
	}
	if action := s.handleInput(input); action != "help" {
		t.Fatalf("help action = %q", action)
	}
	s.helpMode = true
	s.helpSearchQuery = "project_create"
	var out bytes.Buffer
	s.renderHelpModal(&out, 100, 25)
	if !strings.Contains(out.String(), modalSelected) {
		t.Fatal("Project action missing background")
	}
	s.workspaceProjectFocus = false
	out.Reset()
	s.renderHelpModal(&out, 100, 25)
	if strings.Contains(out.String(), modalSelected) {
		t.Fatal("Project action highlighted from quick list")
	}
	if !s.helpActionAvailable("session_actions") || s.helpActionAvailable("detail_filter") {
		t.Fatal("quick list availability incorrect")
	}
	s.focused = true
	if s.helpActionAvailable("session_actions") || !s.helpActionAvailable("pty_unfocus") {
		t.Fatal("PTY availability incorrect")
	}
}

func TestWorkspaceHelpInterceptionAvailability(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.workspaceProjectFocus = false
	s.helpMode = true
	if s.helpActionAvailable("list_search") {
		t.Fatal("slash searches pinned help, not sessions")
	}
	s.helpSearchActive = true
	if s.helpActionAvailable("session_actions") || s.helpActionAvailable("pane_prefix") || !s.helpActionAvailable("help") {
		t.Fatal("help search availability incorrect")
	}
	s.helpSearchActive = false
	s.focused = true
	if s.helpActionAvailable("pty_copy") {
		t.Fatal("copy key is forwarded while focused")
	}
	s.focused = false
	if !s.enterDetailedMode() {
		t.Fatal("detail mode unavailable")
	}
	s.detailSearchFocused = true
	if !s.helpActionAvailable("pane_prefix") || s.helpActionAvailable("detail_filter") {
		t.Fatal("detail search routing incorrect")
	}
}

func TestWorkspaceContextDetailedTargets(t *testing.T) {
	for _, pane := range []bool{false, true} {
		s, _, _, _ := workspacePaneTestState(t)
		s.workspaceProjectFocus = false
		if !s.enterDetailedMode() {
			t.Fatal("detail mode unavailable")
		}
		results := s.detailedResults()
		if len(results) < 2 {
			t.Fatal("missing fixture sessions")
		}
		width, height := terminalSize()
		g := ducklord.CalculateDetailGeometry(width, height, 4)
		x, y := g.Pane.X+1, g.Pane.Y+1
		want := s.detailSelected
		if !pane {
			selected := 0
			for i, r := range results {
				if r.Identity == s.detailSelected {
					selected = i
				}
			}
			found := false
			for row := g.List.Y; row < g.List.Y+g.List.Height; row++ {
				if index := ducklord.DetailSessionIndexAt(g.List, selected, len(results), g.List.X+1, row); index == 1 {
					x, y, want, found = g.List.X+1, row, results[index].Identity, true
					break
				}
			}
			if !found {
				t.Fatal("second detail card not visible")
			}
		}
		s.handleWorkspaceMouse(workspaceMouse(2, x, y, false))
		actual, valid := ducklord.IdentityFromSession(s.actionTarget)
		if !s.actionMenu || !valid || actual != want || s.workspaceMouseFocus || s.focused {
			t.Fatalf("wrong detailed context: %+v", s.actionTarget)
		}
	}
}

func TestWorkspaceAreaMouseAndPrefix(t *testing.T) {
	for _, area := range []string{"project-pane", "session-list", "terminal", "tab"} {
		for _, right := range []bool{false, true} {
			t.Run(area+fmt.Sprint(right), func(t *testing.T) {
				s, projectID, _, _ := workspacePaneTestState(t)
				nav, _ := s.workspaceNavigation()
				_ = nav.SelectProject(projectID)
				w, h := terminalSize()
				g := ducklord.CalculateWorkspaceGeometry(w, h, 4)
				x, y := g.Projects.X, g.Projects.Y
				switch area {
				case "session-list":
					x, y = g.Quick.X, g.Quick.Y
				case "terminal":
					x, y = g.Terminal.X, g.Terminal.Y
				case "tab":
					x, y = g.Terminal.X+modalCellWidth(" Work  ")+1, g.Terminal.Y
				}
				button := 0
				if right {
					button = 2
				}
				for _, release := range []bool{false, true} {
					input := workspaceMouse(button, x, y, release)
					s.handlePanePrefix(input)
					s.handleWorkspaceMouse(input)
				}
				if !right {
					handled, _ := s.handlePanePrefix([]byte(shortcutInput(s.cfg.Shortcut("pane_prefix"))))
					if !handled {
						t.Fatal("prefix not handled")
					}
					handled, command := s.handlePanePrefix([]byte("c"))
					if !handled || command != "c" {
						t.Fatal("config suffix forwarded")
					}
					s.openPrefixPane(command)
				}
				if area == "tab" {
					if s.workspacePaneStep != "tab-rename" {
						t.Fatal("wrong tab config", s.workspacePaneStep)
					}
					return
				}
				if s.workspacePaneStep != "context-config" || s.workspacePaneName != area {
					t.Fatalf("wrong area: %s %s", s.workspacePaneStep, s.workspacePaneName)
				}
			})
		}
	}
}

func TestWorkspaceAreaPersistencePreservesDiskChanges(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	disk := s.cfg.Clone()
	disk.QuickSort = "host"
	if err := ducklord.SaveConfig(s.cfgPath, disk); err != nil {
		t.Fatal(err)
	}
	if err := s.saveWorkspaceAreaSetting(func(c *ducklord.Config) { c.WorkspaceTheme = ducklord.DefaultWorkspaceTheme() }); err != nil {
		t.Fatal(err)
	}
	loaded, err := ducklord.LoadConfig(s.cfgPath)
	if err != nil || loaded.QuickSort != "host" || loaded.WorkspaceTheme.Background != "#111c2b" {
		t.Fatal("settings overwritten", err)
	}
	before := s.cfg
	s.cfgPath = filepath.Join(t.TempDir(), "missing", "config.yaml")
	if err := s.saveWorkspaceAreaSetting(func(c *ducklord.Config) { c.QuickSort = "type" }); err == nil || s.cfg != before {
		t.Fatal("failed save changed live config")
	}
}

func TestWorkspaceConfigFocusedSessionCombinedPrefix(t *testing.T) {
	for _, combined := range []bool{false, true} {
		s, _, _, b := workspacePaneTestState(t)
		s.focused = true
		s.workspaceProjectFocus = false
		s.activeAttachKey = sessionKey(b)
		prefix := shortcutInput(s.cfg.Shortcut("pane_prefix"))
		if !combined {
			s.handlePanePrefix([]byte(prefix))
			prefix = ""
		}
		handled, command := s.handlePanePrefix([]byte(prefix + "c"))
		if !handled || command != "c" {
			t.Fatal("not local")
		}
		s.focused = false
		s.activeAttachKey = ""
		s.openPrefixPane(command)
		if !s.actionMenu || s.actionTarget.SessionID != b.SessionID {
			t.Fatal("lost attached identity")
		}
	}
}

func TestWorkspaceDetailAreaPrefix(t *testing.T) {
	for _, area := range []string{"session-list", "terminal"} {
		s, _, _, _ := workspacePaneTestState(t)
		s.workspaceProjectFocus = false
		if !s.enterDetailedMode() {
			t.Fatal("detail mode")
		}
		w, h := terminalSize()
		g := ducklord.CalculateDetailGeometry(w, h, 4)
		rect := g.List
		if area == "terminal" {
			rect = g.Pane
		}
		for _, release := range []bool{false, true} {
			input := workspaceMouse(0, rect.X, rect.Y, release)
			s.handlePanePrefix(input)
			s.handleWorkspaceMouse(input)
		}
		s.handlePanePrefix([]byte(shortcutInput(s.cfg.Shortcut("pane_prefix"))))
		_, command := s.handlePanePrefix([]byte("c"))
		s.openPrefixPane(command)
		if s.workspacePaneName != area {
			t.Fatal("wrong detail area", s.workspacePaneName)
		}
	}
}

func TestWorkspaceAreaCreateProjectFromSessionFocus(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.workspaceProjectFocus = false
	s.beginWorkspaceAreaConfig("project-pane")
	s.handleWorkspaceAreaConfig([]byte("\r"))
	if !s.workspacePaneMode || s.workspacePaneStep != "project-create" {
		t.Fatal("project action failed")
	}
}

func TestWorkspaceProjectCopyShortcut(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.workspaceProjectFocus = true
	if handled, _ := s.handleWorkspaceProjectInput([]byte("v")); handled {
		t.Fatal("Project swallowed copy shortcut")
	}
	if action := s.handleInput([]byte("v")); action != "copy-mode" {
		t.Fatalf("copy action = %q", action)
	}
	if !s.helpActionAvailable("pty_copy") {
		t.Fatal("Project copy help is not highlighted")
	}
}

func TestWorkspaceAreaConfigFooterDescribesOpenAndApplyAt80x24(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.beginWorkspaceAreaConfig("project-pane")
	var out bytes.Buffer
	s.renderWorkspaceAreaConfig(&out, 80, 24)
	screen := renderedModalScreen(out.Bytes(), 80, 24)
	if !strings.Contains(screen, "↑/↓ choose · Enter open/apply · Esc close") {
		t.Fatalf("workspace config footer missing open/apply instruction: %q", screen)
	}
	s.handleWorkspaceAreaConfig([]byte("\r"))
	if !s.workspacePaneMode || s.workspacePaneStep != "project-create" {
		t.Fatalf("Create project did not open its child flow: mode=%v step=%q", s.workspacePaneMode, s.workspacePaneStep)
	}
}
