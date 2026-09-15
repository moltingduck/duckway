package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestDetailedUnreadFocusSurvivesInventoryUntilUnfocus(t *testing.T) {
	for _, twoUnread := range []bool{false, true} {
		name := "one unread"
		if twoUnread {
			name = "two unread"
		}
		t.Run(name, func(t *testing.T) {
			state, _, first, second := workspacePaneTestState(t)
			state.hostSync = make(map[string]ducklord.SessionUpdate)
			first.LastLine = "focused session output"
			inventory := []ducklord.RemoteSession{first, second}
			applyInventory := func(revision uint64) {
				t.Helper()
				if !state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", InstanceID: first.InstanceID,
					Generation: 1, Revision: revision, State: "live", Sessions: inventory}) {
					t.Fatal("inventory rejected")
				}
			}
			applyInventory(1)
			inventory[0].ActivitySequences = map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}
			if twoUnread {
				inventory[1].ActivitySequences = map[model.NotificationCategory]uint64{model.NotificationTaskCompleted: 1}
			}
			applyInventory(2)
			state.handleDetailedInput([]byte("l"))
			state.handleDetailedInput([]byte("f"))
			identity, _ := ducklord.IdentityFromSession(first)
			if state.detailFilter != ducklord.DetailUnread || state.detailSelected != identity {
				t.Fatalf("unread selection = %+v, filter = %s", state.detailSelected, state.detailFilter)
			}
			if action := state.handleDetailedInput([]byte("\r")); action != "focus" {
				t.Fatalf("Enter action = %q", action)
			}
			// Model the successful control-acceptance path, which acknowledges
			// activity immediately after granting input focus.
			state.focused = true
			state.activeAttachKey = sessionKey(first)
			state.markActivitySeen(state.activePTYSession())
			assertFocused := func() {
				t.Helper()
				if !state.focused || state.detailSelected != identity || state.workspaceNav.DetailSelection() != identity {
					t.Fatal("focused detail selection changed")
				}
				if _, err := state.workspacePaneRectAt(120, 24); err != nil {
					t.Fatalf("PTY input visibility guard rejected focus: %v", err)
				}
				var output bytes.Buffer
				state.renderWorkspacePreviewAt(&output, 120, 24)
				if !strings.Contains(output.String(), first.LastLine) {
					t.Fatalf("focused preview disappeared: %q", output.String())
				}
				for _, item := range state.detailedResults() {
					if item.Identity == identity && item.Unread {
						t.Fatal("focused row retained its acknowledged unread badge")
					}
				}
			}
			assertFocused()
			applyInventory(3)
			assertFocused()
			// Ctrl-] releases control and reapplies the filter before selecting
			// preview output, without requiring another inventory update.
			state.focused = false
			state.clearAttachIdentity()
			state.syncDetailSelection()
			want := ducklord.SessionIdentity{}
			wantCount := 0
			if twoUnread {
				want, _ = ducklord.IdentityFromSession(second)
				wantCount = 1
			}
			if state.detailSelected != want || state.workspaceNav.DetailSelection() != want || len(state.detailedResults()) != wantCount {
				t.Fatalf("filter not restored after unfocus: selected=%+v results=%+v", state.detailSelected, state.detailedResults())
			}
		})
	}
}

func TestDetailedFocusedFilterDoesNotRetainUnavailableSession(t *testing.T) {
	for _, status := range []string{"removed", "stopped"} {
		t.Run(status, func(t *testing.T) {
			state, _, first, second := workspacePaneTestState(t)
			state.hostSync = make(map[string]ducklord.SessionUpdate)
			state.enterDetailedMode()
			identity, _ := ducklord.IdentityFromSession(first)
			state.focused = true
			state.activeAttachKey = sessionKey(first)
			state.detailFilter = ducklord.DetailUnread
			if results := state.detailedResults(); len(results) != 1 || results[0].Identity != identity {
				t.Fatal("focused Session was not retained before inventory change")
			}
			inventory := []ducklord.RemoteSession{second}
			if status == "stopped" {
				first.Status = status
				inventory = append(inventory, first)
			}
			if !state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", InstanceID: first.InstanceID,
				Generation: 1, Revision: 1, State: "live", Sessions: inventory}) {
				t.Fatal("inventory rejected")
			}
			if len(state.detailedResults()) != 0 || state.detailSelected.Key() != "" || state.workspaceNav.DetailSelection().Key() != "" {
				t.Fatal("focused filter retained an unavailable Session")
			}
			if _, err := state.workspacePaneRectAt(120, 24); state.focused && err == nil {
				t.Fatal("unavailable Session still passes the focused PTY input guard")
			}
		})
	}
}

func TestDetailedModeConfigurablePreviewAndFocus(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	state := &tuiState{workspacePreview: true, cfg: &ducklord.Config{Shortcuts: map[string]string{
		"detail_next": "J", "detail_previous": "K", "detail_focus": "!",
	}}, activityState: ducklord.NewActivityState(), sessions: []ducklord.RemoteSession{
		{Client: "host", InstanceID: instance, SessionID: "AAA111", Name: "Alpha", Status: "running", Kind: "shell"},
		{Client: "host", InstanceID: instance, SessionID: "BBB222", Name: "Beta", Status: "running", Kind: "shell"},
	}}
	project, err := state.activityState.ProjectLayout.AddProject("Work")
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range state.sessions {
		identity, _ := ducklord.IdentityFromSession(session)
		if _, err := state.activityState.ProjectLayout.Place(project, identity, ducklord.PlaceNewTab, ""); err != nil {
			t.Fatal(err)
		}
	}
	if action := state.handleDetailedInput([]byte("l")); action != "changed" {
		t.Fatalf("enter detailed mode: %q", action)
	}
	first := state.detailSelected
	for _, key := range []string{"j", "\x1b[B", "\r"} {
		if action := state.handleDetailedInput([]byte(key)); action != "changed" || state.detailSelected != first {
			t.Fatalf("replaced default %q still active: %q, %+v", key, action, state.detailSelected)
		}
	}
	state.handleDetailedInput([]byte("J"))
	second := state.detailSelected
	if second == first || second.Key() == "" {
		t.Fatal("configured next did not change preview")
	}
	for _, key := range []string{"k", "\x1b[A"} {
		state.handleDetailedInput([]byte(key))
		if state.detailSelected != second {
			t.Fatalf("replaced previous binding %q still active", key)
		}
	}
	if action := state.handleDetailedInput([]byte("!")); action != "focus" {
		t.Fatalf("configured focus returned %q", action)
	}
	state.handleDetailedInput([]byte("K"))
	if state.detailSelected != first || state.focused || state.activeAttachKey != "" {
		t.Fatal("configured previous failed or preview granted PTY control")
	}
	if line := state.detailStatusLine(); !strings.Contains(line, "K/J preview · ! focus") {
		t.Fatalf("status omits current bindings: %s", line)
	}
	state.handleDetailedInput([]byte("/"))
	state.handleDetailedInput([]byte("J"))
	state.handleDetailedInput([]byte("!"))
	if state.detailQuery != "J!" {
		t.Fatalf("action bindings consumed search text: %q", state.detailQuery)
	}
	if action := state.handleDetailedInput([]byte("\r")); action != "changed" || state.detailSearchFocused {
		t.Fatal("Enter did not finish search editing")
	}
}

func TestDetailedSearchAcceptsPrintableHelpBinding(t *testing.T) {
	for _, helpKey := range []string{"?", "x", "專"} {
		t.Run(helpKey, func(t *testing.T) {
			state := &tuiState{workspacePreview: true, cfg: &ducklord.Config{Shortcuts: map[string]string{"help": helpKey}}, activityState: ducklord.NewActivityState()}
			if !state.enterDetailedMode() {
				t.Fatal("could not enter detailed mode")
			}
			if action := state.handleDetailedInput([]byte(helpKey)); action != "" {
				t.Fatalf("unfocused search intercepted help: %q", action)
			}
			state.handleDetailedInput([]byte("/"))
			if action := state.handleDetailedInput([]byte(helpKey)); action != "changed" || state.detailQuery != helpKey || !state.detailSearchFocused {
				t.Fatalf("help binding intercepted search text: action=%q query=%q focused=%v", action, state.detailQuery, state.detailSearchFocused)
			}
		})
	}
}

func TestDetailedModeRestoresKeyboardFocus(t *testing.T) {
	for _, exit := range []struct{ name, key string }{{"toggle", "l"}, {"escape", "\x1b"}} {
		t.Run(exit.name, func(t *testing.T) {
			instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
			activity := ducklord.NewActivityState()
			state := &tuiState{workspacePreview: true, cfg: &ducklord.Config{}, activityState: activity}
			var projects []string
			for i, name := range []string{"Alpha", "Beta"} {
				project, err := activity.ProjectLayout.AddProject(name)
				if err != nil {
					t.Fatal(err)
				}
				identity := ducklord.SessionIdentity{InstanceID: instance, SessionID: []string{"AAA111", "BBB222"}[i]}
				if _, err := activity.ProjectLayout.Place(project, identity, ducklord.PlaceNewTab, ""); err != nil {
					t.Fatal(err)
				}
				projects = append(projects, project)
				state.sessions = append(state.sessions, ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: identity.SessionID, Name: name, Status: "running", Kind: "shell"})
			}
			nav, err := state.workspaceNavigation()
			if err != nil {
				t.Fatal(err)
			}
			// Successive visits must each save their own focus, including a visit
			// from the quick list after returning to the Project list.
			for _, projectFocus := range []bool{true, false, true} {
				if err := nav.SelectQuickSession(ducklord.SessionIdentity{InstanceID: instance, SessionID: "AAA111"}); err != nil {
					t.Fatal(err)
				}
				state.selected = 0
				state.workspaceProjectFocus = projectFocus
				if projectFocus {
					if err := nav.SelectProject(projects[0]); err != nil {
						t.Fatal(err)
					}
				}
				region, tab, pane := nav.Region(), nav.CurrentTabID(), nav.CurrentPaneID()
				if action := state.handleDetailedInput([]byte("l")); action != "changed" || state.workspaceProjectFocus {
					t.Fatalf("enter with Project focus %v: action=%q focus=%v", projectFocus, action, state.workspaceProjectFocus)
				}
				if state.enterDetailedMode() {
					t.Fatal("repeated entry must not overwrite saved focus")
				}
				state.handleDetailedInput([]byte("j"))
				if action := state.handleDetailedInput([]byte(exit.key)); action != "changed" || nav.InDetailMode() || state.workspaceProjectFocus != projectFocus {
					t.Fatalf("exit with Project focus %v: action=%q focus=%v", projectFocus, action, state.workspaceProjectFocus)
				}
				if nav.CurrentProjectID() != projects[0] || nav.CurrentTabID() != tab || nav.CurrentPaneID() != pane || nav.Region() != region {
					t.Fatal("exit did not restore the saved workspace location")
				}
				state.exitDetailedMode() // A redundant exit must not clear restored focus.
				handled, changed := state.handleWorkspaceProjectInput([]byte("j"))
				if projectFocus {
					if !handled || !changed || nav.CurrentProjectID() != projects[1] || state.selected != 0 {
						t.Fatal("j after exit did not move only the Project selection")
					}
				} else {
					if handled || changed {
						t.Fatal("Project list consumed j after exiting to quick-list focus")
					}
					state.handleInput([]byte("j"))
					if state.selected != 1 {
						t.Fatal("j after exit did not move the quick-list selection")
					}
				}
				if state.focused || state.workspaceAttachFromProject || state.activeAttachKey != "" {
					t.Fatal("returning keyboard focus granted PTY control")
				}
			}
		})
	}
}

func TestDetailedSessionModeSearchPreviewAndJumpPreserveNormalLocation(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	a := ducklord.SessionIdentity{InstanceID: instance, SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: instance, SessionID: "BBB222"}
	activity := ducklord.NewActivityState()
	projectA, err := activity.ProjectLayout.AddProject("Alpha")
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := activity.ProjectLayout.AddProject("專案乙")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = activity.ProjectLayout.Place(projectA, a, ducklord.PlaceNewTab, "")
	_, _ = activity.ProjectLayout.Place(projectB, b, ducklord.PlaceNewTab, "")
	state := &tuiState{workspacePreview: true, workspaceProjectFocus: true, cfg: &ducklord.Config{}, activityState: activity,
		sessions: []ducklord.RemoteSession{
			{Client: "host-a", InstanceID: instance, SessionID: a.SessionID, Name: "Codex", Status: "running", Kind: "shell"},
			{Client: "host-b", InstanceID: instance, SessionID: b.SessionID, Name: "Claude", Status: "running", Kind: "shell"},
		}}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if nav.CurrentProjectID() != projectA {
		t.Fatal("normal workspace did not begin in selected Session's Project")
	}
	if action := state.handleDetailedInput([]byte("l")); action != "changed" || !nav.InDetailMode() {
		t.Fatalf("enter detailed mode: %q", action)
	}
	if action := state.handleDetailedInput([]byte("/")); action != "changed" || !state.detailSearchFocused {
		t.Fatalf("focus search: %q", action)
	}
	for _, key := range []string{"專", "案"} {
		state.handleDetailedInput([]byte(key))
	}
	if results := state.detailedResults(); len(results) != 1 || results[0].Identity != b || state.detailSelected != b {
		t.Fatalf("Unicode Project search results=%+v selected=%+v", results, state.detailSelected)
	}
	for _, key := range []string{"\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D", "\x1b[H", "\x1b[F", "\x1b[3~", "\x1bOP", "\x1bx"} {
		state.handleDetailedInput([]byte(key))
		if state.detailQuery != "專案" || !state.detailSearchFocused || state.detailSelected != b {
			t.Fatalf("key %q corrupted detailed search: query=%q focused=%v selected=%+v", key, state.detailQuery, state.detailSearchFocused, state.detailSelected)
		}
	}
	if nav.CurrentProjectID() != projectA || state.focused || state.activeAttachKey != "" {
		t.Fatal("search preview changed normal Project or granted PTY control")
	}
	var output bytes.Buffer
	state.renderWorkspacePreviewAt(&output, 120, 24)
	if !strings.Contains(output.String(), "SESSIONS") || !strings.Contains(output.String(), "Claude") || strings.Contains(output.String(), "TERMINAL TAB") {
		t.Fatal("detailed mode did not render the single-Session preview")
	}
	state.handleDetailedInput([]byte("\x1b")) // clear query first
	if state.detailQuery != "" || !state.detailSearchFocused {
		t.Fatal("Escape did not clear the query first")
	}
	state.handleDetailedInput([]byte("\x1b"))
	if state.detailSearchFocused {
		t.Fatal("second Escape did not return to detailed list")
	}
	state.handleDetailedInput([]byte("l"))
	if nav.InDetailMode() || nav.CurrentProjectID() != projectA || state.detailQuery != "" {
		t.Fatal("leaving detailed mode did not restore the normal workspace")
	}
	state.handleDetailedInput([]byte("l"))
	state.handleDetailedInput([]byte("j"))
	if state.detailSelected != b {
		t.Fatal("moving detailed selection did not preview the next Session")
	}
	if action := state.handleDetailedInput([]byte("g")); action != "jump" || nav.InDetailMode() || nav.CurrentProjectID() != projectB {
		t.Fatalf("g did not jump to selected Session's Project: %q project=%q", action, nav.CurrentProjectID())
	}
	if state.workspaceProjectFocus || state.detailReturnProjectFocus {
		t.Fatal("jump retained the original Project-list focus instead of allowing Session pane focus")
	}
}

func TestDetailedSelectionClearsAndRecoversAfterNoMatches(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "AAA111"}
	activity := ducklord.NewActivityState()
	if err := activity.ProjectLayout.Discover(identity); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{workspacePreview: true, cfg: &ducklord.Config{}, activityState: activity,
		sessions: []ducklord.RemoteSession{{Client: "host", InstanceID: identity.InstanceID, SessionID: identity.SessionID,
			Name: "Claude", Kind: "shell", Status: "running", RuntimeGeneration: 1}}}
	if !state.enterDetailedMode() {
		t.Fatal("could not enter detailed mode")
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	state.detailQuery = "no matching Session"
	if state.syncDetailSelection() || state.detailSelected.Key() != "" || nav.DetailSelection().Key() != "" {
		t.Fatal("empty result retained stale detailed selection")
	}
	state.detailQuery = ""
	if !state.syncDetailSelection() || state.detailSelected != identity || nav.DetailSelection() != identity {
		t.Fatal("selection did not recover after clearing the search")
	}
}

func TestDetailedSelectionFollowsAuthoritativeSessionRemoval(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	a := ducklord.SessionIdentity{InstanceID: instance, SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: instance, SessionID: "BBB222"}
	activity := ducklord.NewActivityState()
	for _, identity := range []ducklord.SessionIdentity{a, b} {
		if err := activity.ProjectLayout.Discover(identity); err != nil {
			t.Fatal(err)
		}
	}
	first := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: a.SessionID, Name: "A", Kind: "shell", Status: "running", RuntimeGeneration: 1}
	second := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: b.SessionID, Name: "B", Kind: "shell", Status: "running", RuntimeGeneration: 1}
	state := &tuiState{workspacePreview: true, cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, activityState: activity,
		hostSync: make(map[string]ducklord.SessionUpdate), sessions: []ducklord.RemoteSession{first, second}}
	if !state.enterDetailedMode() {
		t.Fatal("could not enter detailed mode")
	}
	state.handleDetailedInput([]byte("j"))
	if state.detailSelected != b {
		t.Fatal("failed to select B before removal")
	}
	if !state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", InstanceID: instance, Generation: 1, Revision: 1,
		State: "live", Sessions: []ducklord.RemoteSession{first}}) {
		t.Fatal("authoritative inventory was rejected")
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if state.detailSelected != a || nav.DetailSelection() != a || len(state.detailedResults()) != 1 || len(state.activity().ProjectLayout.ProjectsFor(b)) != 0 {
		t.Fatalf("detail preview did not follow surviving Session: selected=%+v nav=%+v results=%+v", state.detailSelected, nav.DetailSelection(), state.detailedResults())
	}
}
