package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestWorkspaceClosedDefaultPaneOnlyReopensOnExplicitNavigation(t *testing.T) {
	for _, route := range []string{"quick", "detail"} {
		t.Run(route, func(t *testing.T) {
			state, _, _, session := workspacePaneTestState(t)
			identity, _ := ducklord.IdentityFromSession(session)
			if err := state.activity().ProjectLayout.Detach(ducklord.DefaultProjectID, identity); err != nil {
				t.Fatal(err)
			}
			if err := state.activityStore.Save(state.activity()); err != nil {
				t.Fatal(err)
			}
			loaded, err := state.activityStore.Load()
			if err != nil {
				t.Fatal(err)
			}
			state.activityState = loaded
			state.selected, state.workspaceProjectFocus = 1, false
			nav, err := state.workspaceNavigation()
			if err != nil {
				t.Fatal(err)
			}
			if len(state.activity().ProjectLayout.Project(ducklord.DefaultProjectID).Tabs) != 0 {
				t.Fatal("startup reopened closed pane")
			}
			if route == "detail" {
				nav.EnterDetail()
				state.detailSelected = identity
				if err := nav.PreviewDetail(identity); err != nil {
					t.Fatal(err)
				}
				if len(state.activity().ProjectLayout.Project(ducklord.DefaultProjectID).Tabs) != 0 {
					t.Fatal("preview reopened closed pane")
				}
				if got := state.handleDetailedInput([]byte(state.cfg.Shortcut("detail_jump"))); got != "jump" {
					t.Fatalf("jump = %q: %s", got, state.outputErr)
				}
			} else {
				state.workspaceFollowQuickSelection()
			}
			loaded, err = state.activityStore.Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.ProjectLayout.Project(ducklord.DefaultProjectID).Tabs) != 1 || len(loaded.ProjectLayout.SuppressedDefaultSessions) != 0 {
				t.Fatal("explicit reopening was not persisted")
			}
			if nav.CurrentProjectID() != ducklord.DefaultProjectID || nav.CurrentPaneID() == "" {
				t.Fatal("reopened pane was not selected")
			}
		})
	}
}

func TestWorkspaceDefaultPaneReopenSaveFailurePreservesClosedView(t *testing.T) {
	state, _, _, session := workspacePaneTestState(t)
	identity, _ := ducklord.IdentityFromSession(session)
	if err := state.activity().ProjectLayout.Detach(ducklord.DefaultProjectID, identity); err != nil {
		t.Fatal(err)
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	beforeProject, beforePane := nav.CurrentProjectID(), nav.CurrentPaneID()
	// The existing state file cannot serve as a directory.
	state.activityStore.Path = filepath.Join(state.activityStore.Path, "state.json")
	state.selected, state.workspaceProjectFocus = 1, false
	state.outputForKey, state.outputText = "previous-preview", "previous output"
	state.ptyScrollOffset = 3
	var control *ducklord.ControlSession
	var controlDone <-chan error
	previewRequested := false
	state.handleQuickSelection(&control, &controlDone, func(bool) { previewRequested = true })
	if !strings.Contains(state.outputErr, "reopen Session pane:") || len(state.activity().ProjectLayout.SuppressedDefaultSessions) != 1 || len(state.activity().ProjectLayout.Project(ducklord.DefaultProjectID).Tabs) != 0 {
		t.Fatal("failed persistence changed closed view")
	}
	if previewRequested || state.outputForKey != "previous-preview" || state.outputText != "previous output" || state.ptyScrollOffset != 3 {
		t.Fatal("failed reopen reset or requested preview")
	}
	if nav.CurrentProjectID() != beforeProject || nav.CurrentPaneID() != beforePane {
		t.Fatal("failed persistence changed navigation")
	}
}

func TestWorkspaceQuickSelectionReopensBeforeRequestingPreview(t *testing.T) {
	state, _, _, session := workspacePaneTestState(t)
	identity, _ := ducklord.IdentityFromSession(session)
	if err := state.activity().ProjectLayout.Detach(ducklord.DefaultProjectID, identity); err != nil {
		t.Fatal(err)
	}
	state.selected, state.workspaceProjectFocus = 1, false
	var control *ducklord.ControlSession
	var controlDone <-chan error
	requests := 0
	state.handleQuickSelection(&control, &controlDone, func(fence bool) {
		requests++
		if !fence {
			t.Fatal("selection did not fence previous preview")
		}
		loaded, err := state.activityStore.Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(loaded.ProjectLayout.SuppressedDefaultSessions) != 0 || len(loaded.ProjectLayout.Project(ducklord.DefaultProjectID).Tabs) != 1 {
			t.Fatal("preview requested before reopen was persisted")
		}
		nav, err := state.workspaceNavigation()
		if err != nil || nav.CurrentProjectID() != ducklord.DefaultProjectID || nav.CurrentPaneID() == "" {
			t.Fatal("preview requested before reopened pane was selected")
		}
	})
	if requests != 1 || state.outputErr != "loading live preview..." {
		t.Fatalf("requests = %d, status = %q", requests, state.outputErr)
	}
}

func TestWorkspaceMouseAndSearchStopAfterReopenSaveFailure(t *testing.T) {
	for _, route := range []string{"mouse", "search-activation", "search-focus"} {
		t.Run(route, func(t *testing.T) {
			state, _, _, session := workspacePaneTestState(t)
			identity, _ := ducklord.IdentityFromSession(session)
			if err := state.activity().ProjectLayout.Detach(ducklord.DefaultProjectID, identity); err != nil {
				t.Fatal(err)
			}
			nav, err := state.workspaceNavigation()
			if err != nil {
				t.Fatal(err)
			}
			beforeProject, beforePane := nav.CurrentProjectID(), nav.CurrentPaneID()
			state.activityStore.Path = filepath.Join(state.activityStore.Path, "state.json")
			state.workspaceProjectFocus = false
			state.outputForKey, state.outputText = "previous-preview", "previous output"
			if route == "mouse" {
				width, height := terminalSize()
				geometry := ducklord.CalculateWorkspaceGeometry(width, height, 4)
				x, y := geometry.Quick.X+1, geometry.Quick.Y+2
				state.handleWorkspaceMouse(workspaceMouse(0, x, y, false))
				handled, changed := state.handleWorkspaceMouse(workspaceMouse(0, x, y, true))
				if !handled || changed {
					t.Fatal("failed mouse reopen requested output selection")
				}
			} else {
				state.searchMode = true
				if route == "search-activation" {
					state.searchPendingRequestID = 42
				}
				if state.prepareSearchWorkspaceSelection(sessionKey(session)) {
					t.Fatal("failed search reopen allowed activation/focus")
				}
				if !state.searchMode || state.searchPendingRequestID != 0 || !strings.Contains(state.searchErr, "reopen Session pane:") {
					t.Fatal("failed search reopen did not retain modal and report retryable failure")
				}
			}
			if !strings.Contains(state.outputErr, "reopen Session pane:") || state.outputForKey != "previous-preview" || state.outputText != "previous output" {
				t.Fatal("failed reopen masked error or replaced preview")
			}
			if nav.CurrentProjectID() != beforeProject || nav.CurrentPaneID() != beforePane || len(state.activity().ProjectLayout.SuppressedDefaultSessions) != 1 {
				t.Fatal("failed reopen changed layout/navigation")
			}
		})
	}
}

func TestWorkspacePreviewRendersLiveSelectedSessionWithoutGrantingFocus(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "ABC123"}
	activity := ducklord.NewActivityState()
	if err := activity.ProjectLayout.Discover(identity); err != nil {
		t.Fatal(err)
	}
	session := ducklord.RemoteSession{Client: "host-a", InstanceID: identity.InstanceID, SessionID: identity.SessionID,
		Name: "codex-shell", Kind: "shell", Status: "running", RuntimeGeneration: 1}
	terminal := ducklord.NewTerminal(15, 60, 16)
	terminal.Write([]byte("\033[31mRED_FRAME\033[0m"))
	state := &tuiState{ownerName: "local", activityState: activity, sessions: []ducklord.RemoteSession{session}, terminal: terminal,
		outputForKey: sessionKey(session), outputFresh: true}
	var out bytes.Buffer
	state.renderWorkspacePreviewAt(&out, 120, 20)
	got := out.String()
	for _, want := range []string{"PROJECTS", "SESSIONS", "Default Project", "codex-shell", "\033[0;31mRED_FRAME"} {
		if !strings.Contains(got, want) {
			t.Fatalf("workspace missing %q: %q", want, got)
		}
	}
	if !strings.Contains(got, "◇ host-a/codex-shell") || strings.Contains(got, "▣ host-a/codex-shell") {
		t.Fatalf("preview was presented as focused: %q", got)
	}
	if state.focused || state.activeAttachKey != "" {
		t.Fatal("rendering granted PTY focus")
	}
	state.workspacePreview = true
	out.Reset()
	state.render(&out)
	if !strings.Contains(out.String(), "PROJECTS") || !strings.Contains(out.String(), "SESSIONS") {
		t.Fatalf("live TUI render path did not select workspace: %q", out.String())
	}
	state.focused = true
	state.activeAttachKey = sessionKey(session)
	out.Reset()
	state.renderWorkspacePreviewAt(&out, 120, 20)
	if !strings.Contains(out.String(), "▣ host-a/codex-shell") {
		t.Fatalf("focused session was not marked: %q", out.String())
	}
}

func TestWorkspaceReadOnlyHintMatchesOfflineAndOwnerPreflight(t *testing.T) {
	state := &tuiState{ownerName: "local", disconnectedHosts: map[string]bool{}}
	shell := ducklord.RemoteSession{Client: "host", Name: "shell", Status: "running", Kind: "shell", RuntimeGeneration: 1}
	if state.workspaceSessionReadOnly(shell) {
		t.Fatal("connected Shell should be presented as writable")
	}
	state.disconnectedHosts["host"] = true
	if !state.workspaceSessionReadOnly(shell) {
		t.Fatal("explicitly disconnected Shell appeared writable")
	}
	state.disconnectedHosts["host"] = false
	shell.Status, shell.Error = "disconnected", "SSH unavailable"
	if !state.workspaceSessionReadOnly(shell) {
		t.Fatal("stale Shell preview appeared writable")
	}
	agent := ducklord.RemoteSession{Client: "host", Name: "agent", Status: "running", Kind: "agent", RuntimeGeneration: 1,
		WriterKind: "terminal", WriterID: "another"}
	if !state.workspaceSessionReadOnly(agent) {
		t.Fatal("non-owner Agent appeared writable")
	}
	agent.WriterID = "local"
	if state.workspaceSessionReadOnly(agent) {
		t.Fatal("owned Agent appeared read-only")
	}
}

func TestWorkspaceOutputPriorityFollowsSelectedProjectPaneNotQuickList(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	bIdentity, _ := ducklord.IdentityFromSession(b)
	bPaneID, err := state.activity().ProjectLayout.Place(projectID, bIdentity, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectPane(projectID, bPaneID); err != nil {
		t.Fatal(err)
	}
	visible := []ducklord.TerminalSelection{
		{Client: ducklord.Client{Name: a.Client}, InstanceID: a.InstanceID, SessionID: a.SessionID, RuntimeGeneration: a.RuntimeGeneration},
		{Client: ducklord.Client{Name: b.Client}, InstanceID: b.InstanceID, SessionID: b.SessionID, RuntimeGeneration: b.RuntimeGeneration},
	}
	priority := state.workspacePreferredSelection(visible, a)
	if priority.SessionID != b.SessionID || state.currentSession().SessionID != a.SessionID {
		t.Fatalf("output priority changed quick selection or ignored B: %+v", priority)
	}
	other, _, quick, defaultSession := workspacePaneTestState(t)
	otherNav, _ := other.workspaceNavigation()
	if err := otherNav.SelectProject(ducklord.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	priority = other.workspacePreferredSelection([]ducklord.TerminalSelection{
		{Client: ducklord.Client{Name: quick.Client}, InstanceID: quick.InstanceID, SessionID: quick.SessionID, RuntimeGeneration: 1},
		{Client: ducklord.Client{Name: defaultSession.Client}, InstanceID: defaultSession.InstanceID, SessionID: defaultSession.SessionID, RuntimeGeneration: 1},
	}, quick)
	if priority.SessionID != defaultSession.SessionID || other.currentSession().SessionID != quick.SessionID {
		t.Fatalf("Project-only B did not win output priority: %+v", priority)
	}
}

func TestWorkspacePreviewDoesNotMarkUnreadSeenBeforePaneFocus(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "ABC123"}
	session := ducklord.RemoteSession{Client: "host-a", InstanceID: identity.InstanceID, SessionID: identity.SessionID,
		Name: "attention", Kind: "shell", Status: "running", RuntimeGeneration: 1}
	state := &tuiState{workspacePreview: true, sessions: []ducklord.RemoteSession{session}, selected: 0,
		terminal: ducklord.NewTerminal(12, 40, 0), outputForKey: sessionKey(session), outputFresh: true, terminalGeneration: 1}
	if state.sessionFreshlyDisplayed(session) {
		t.Fatal("read-only Session list preview would mark unread seen")
	}
	state.focused = true
	state.activeAttachKey = sessionKey(session)
	if !state.sessionFreshlyDisplayed(session) {
		t.Fatal("focused live Session pane was not eligible to clear unread")
	}
	state.activeAttachKey = "another Session"
	if state.sessionFreshlyDisplayed(session) {
		t.Fatal("another pane's focus cleared this Session's unread")
	}
}

func TestWorkspaceProjectNavigationDoesNotStealQuickSelectionOrPTYControl(t *testing.T) {
	a := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: a.InstanceID, SessionID: "BBB222"}
	activity := ducklord.NewActivityState()
	first, err := activity.ProjectLayout.AddProject("Work A")
	if err != nil {
		t.Fatal(err)
	}
	second, err := activity.ProjectLayout.AddProject("Work B")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(first, a, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(second, b, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{workspacePreview: true, activityState: activity, sessions: []ducklord.RemoteSession{
		{Client: "host", InstanceID: a.InstanceID, SessionID: a.SessionID, Name: "alpha", Kind: "shell"},
		{Client: "host", InstanceID: b.InstanceID, SessionID: b.SessionID, Name: "beta", Kind: "shell"},
	}}
	nav, err := state.workspaceNavigation()
	if err != nil || nav.CurrentProjectID() != first {
		t.Fatalf("quick Session did not select its Project: project=%q err=%v", nav.CurrentProjectID(), err)
	}
	if handled, _ := state.handleWorkspaceProjectInput([]byte("P")); !handled || !state.workspaceProjectFocus {
		t.Fatal("Project pane did not gain read-only focus")
	}
	if handled, changed := state.handleWorkspaceProjectInput([]byte("j")); !handled || !changed {
		t.Fatal("Project pane did not move to next Project")
	}
	var screen bytes.Buffer
	state.renderWorkspacePreviewAt(&screen, 120, 20)
	if nav.CurrentProjectID() != second || state.selected != 0 || state.focused || state.activeAttachKey != "" {
		t.Fatalf("Project navigation changed Session selection/control: project=%q selected=%d focused=%t attach=%q",
			nav.CurrentProjectID(), state.selected, state.focused, state.activeAttachKey)
	}
	if !strings.Contains(screen.String(), "› Work B") || !strings.Contains(screen.String(), "host/beta") {
		t.Fatalf("Terminal area did not follow Project pane: %q", screen.String())
	}
	state.activityState = state.activity().Clone()
	state.renderWorkspacePreviewAt(&screen, 120, 20)
	if nav.CurrentProjectID() != second {
		t.Fatal("atomic activity-state clone reset Project navigation")
	}
}

func TestWorkspaceAsyncQuickCursorChangeDoesNotStealFocusedProjectPane(t *testing.T) {
	a := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: a.InstanceID, SessionID: "BBB222"}
	activity := ducklord.NewActivityState()
	projectA, _ := activity.ProjectLayout.AddProject("A")
	projectB, _ := activity.ProjectLayout.AddProject("B")
	_, _ = activity.ProjectLayout.Place(projectA, a, ducklord.PlaceNewTab, "")
	_, _ = activity.ProjectLayout.Place(projectB, b, ducklord.PlaceNewTab, "")
	state := &tuiState{workspacePreview: true, activityState: activity, sessions: []ducklord.RemoteSession{
		{Client: "host", InstanceID: a.InstanceID, SessionID: a.SessionID, Kind: "shell"},
		{Client: "host", InstanceID: b.InstanceID, SessionID: b.SessionID, Kind: "shell"},
	}, selected: 1}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectA); err != nil {
		t.Fatal(err)
	}
	state.activeAttachKey = sessionKey(state.sessions[0])
	state.focused = true
	state.selected = 0 // an async inventory update chose a fallback quick row
	if _, err := state.workspaceNavigation(); err != nil {
		t.Fatal(err)
	}
	if nav.CurrentProjectID() != projectA || !state.focused || state.activeAttachKey != sessionKey(state.sessions[0]) {
		t.Fatal("async quick-list selection stole focused Project pane")
	}
	state.focused = false
	state.selected = 1
	state.clearAttachIdentity() // failed control cleanup is not quick-list navigation
	if nav.CurrentProjectID() != projectA {
		t.Fatal("control cleanup stole Project navigation")
	}
	state.selected = 1
	state.workspaceFollowQuickSelection()
	if nav.CurrentProjectID() != projectB {
		t.Fatal("explicit quick-list navigation failed")
	}
}

func TestWorkspaceVisibleSelectionsKeepsLiveBackgroundWhenQuickSessionOffline(t *testing.T) {
	a := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: a.InstanceID, SessionID: "BBB222"}
	activity := ducklord.NewActivityState()
	first, err := activity.ProjectLayout.Place(ducklord.DefaultProjectID, a, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(ducklord.DefaultProjectID, b, ducklord.PlaceVertical, first); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{workspacePreview: true, cfg: &ducklord.Config{Clients: []ducklord.Client{
		{Name: "offline", Host: "offline"}, {Name: "online", Host: "online"},
	}}, activityState: activity, hostSync: map[string]ducklord.SessionUpdate{
		"offline": {State: "reconnecting"}, "online": {State: "live"},
	}, sessions: []ducklord.RemoteSession{
		{Client: "offline", InstanceID: a.InstanceID, SessionID: a.SessionID, Name: "alpha", Status: "running", RuntimeGeneration: 1},
		{Client: "online", InstanceID: b.InstanceID, SessionID: b.SessionID, Name: "beta", Status: "running", RuntimeGeneration: 1},
	}}
	visible := state.workspaceVisibleSelections(ducklord.TerminalSelection{})
	if len(visible) != 1 || visible[0].Client.Name != "online" || visible[0].SessionID != b.SessionID {
		t.Fatalf("offline priority hid the live background pane: %+v", visible)
	}
}

func TestWorkspacePreviewPreflightUsesVisibleLeafNotWholeTerminal(t *testing.T) {
	a := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: a.InstanceID, SessionID: "BBB222"}
	activity := ducklord.NewActivityState()
	first, err := activity.ProjectLayout.Place(ducklord.DefaultProjectID, a, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(ducklord.DefaultProjectID, b, ducklord.PlaceVertical, first); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{activityState: activity, selected: 1, sessions: []ducklord.RemoteSession{
		{Client: "host", InstanceID: a.InstanceID, SessionID: a.SessionID, Kind: "shell"},
		{Client: "host", InstanceID: b.InstanceID, SessionID: b.SessionID, Kind: "shell"},
	}}
	rect, err := state.workspacePaneRectAt(120, 20)
	if err != nil || rect.X != 88 || rect.Width != 33 || rect.Height != 16 {
		t.Fatalf("selected right pane rect=%+v err=%v", rect, err)
	}
	if state.workspaceNav.Region() != ducklord.RegionQuickList {
		t.Fatalf("geometry lookup stole keyboard region: %s", state.workspaceNav.Region())
	}
	state.focused = true
	state.terminal = ducklord.NewTerminal(15, 33, 0)
	var rendered bytes.Buffer
	state.renderWorkspacePreviewAt(&rendered, 120, 20)
	if !strings.Contains(rendered.String(), "\033[6;88H\033[?25h") {
		t.Fatalf("focused cursor was not placed inside right pane: %q", rendered.String())
	}
	activity.ProjectLayout = ducklord.NewProjectLayout()
	first, err = activity.ProjectLayout.Place(ducklord.DefaultProjectID, a, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(ducklord.DefaultProjectID, b, ducklord.PlaceHorizontal, first); err != nil {
		t.Fatal(err)
	}
	if _, err := state.workspacePaneRectAt(120, 5); err == nil {
		t.Fatal("hidden lower pane passed control preflight")
	}
}

func TestWorkspaceFocusedReadOnlyStatusSurvivesClearedError(t *testing.T) {
	for _, tc := range []struct {
		name, kind, writer string
		readOnly           bool
	}{
		{"foreign agent", "agent", "other", true},
		{"owned agent", "agent", "viewer", false},
		{"shared shell", "shell", "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := ducklord.RemoteSession{Client: "host", SessionID: "AAA111", Kind: tc.kind, WriterKind: "terminal", WriterID: tc.writer}
			state := &tuiState{cfg: &ducklord.Config{}, ownerName: "viewer", focused: true,
				sessions: []ducklord.RemoteSession{session}, activeAttachKey: sessionKey(session)}
			var output bytes.Buffer
			state.renderWorkspacePreviewAt(&output, 130, 28)
			got := strings.Contains(output.String(), "Session focus: read-only")
			if got != tc.readOnly {
				t.Fatalf("read-only status = %v, want %v", got, tc.readOnly)
			}
		})
	}
}

func TestWorkspaceProjectPaneTargetsBDuringQuickSelectionA(t *testing.T) {
	a := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: a.InstanceID, SessionID: "BBB222"}
	activity := ducklord.NewActivityState()
	projectA, err := activity.ProjectLayout.AddProject("A")
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := activity.ProjectLayout.AddProject("B")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(projectA, a, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := activity.ProjectLayout.Place(projectB, b, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	sessionA := ducklord.RemoteSession{Client: "host", InstanceID: a.InstanceID, SessionID: a.SessionID, Kind: "shell", Status: "running", RuntimeGeneration: 1}
	sessionB := ducklord.RemoteSession{Client: "host", InstanceID: b.InstanceID, SessionID: b.SessionID, Kind: "shell", Status: "running", RuntimeGeneration: 1}
	state := &tuiState{workspacePreview: true, cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}},
		activityState: activity, sessions: []ducklord.RemoteSession{sessionA, sessionB}, selected: 0}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectB); err != nil {
		t.Fatal(err)
	}
	target, err := state.workspaceSelectedPaneSession()
	if err != nil || sessionKey(target) != sessionKey(sessionB) {
		t.Fatalf("Project B target=%+v err=%v", target, err)
	}
	if state.selected != 0 || sessionKey(state.currentSession()) != sessionKey(sessionA) {
		t.Fatal("Project pane targeting changed quick-list selection")
	}
	state.activeAttachKey = sessionKey(target)
	if sessionKey(state.activePTYSession()) != sessionKey(sessionB) || !state.canResizeCurrentSession() {
		t.Fatal("active PTY target did not follow Project B's pane")
	}
	if _, err := state.workspacePaneRectAt(120, 20); err != nil {
		t.Fatalf("Project B pane geometry: %v", err)
	}
	if nav.Region() != ducklord.RegionProjects {
		t.Fatalf("geometry query stole keyboard focus: %s", nav.Region())
	}
}

func TestWorkspaceProjectPaneRejectsStaleControlIdentity(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	target := ducklord.RemoteSession{Client: "host-b", InstanceID: instance, SessionID: "BBB222", Kind: "agent",
		WriterKind: "terminal", WriterID: "desk", OwnershipEpoch: 8, RuntimeGeneration: 3}
	base := ducklord.ControlSession{ClientKey: target.Client, InstanceID: target.InstanceID, SessionID: target.SessionID,
		OwnershipEpoch: target.OwnershipEpoch, RuntimeGeneration: target.RuntimeGeneration}
	if !controlMatchesSession(&base, target, "desk") {
		t.Fatal("current Project pane control was rejected")
	}
	for name, mutate := range map[string]func(*ducklord.ControlSession){
		"old quick-list pane": func(c *ducklord.ControlSession) { c.SessionID = "AAA111" },
		"old host instance":   func(c *ducklord.ControlSession) { c.InstanceID = "4af68174-9e13-4dc9-b44d-8532c87f5971" },
		"old writer epoch":    func(c *ducklord.ControlSession) { c.OwnershipEpoch-- },
		"old runtime":         func(c *ducklord.ControlSession) { c.RuntimeGeneration-- },
	} {
		t.Run(name, func(t *testing.T) {
			stale := base
			mutate(&stale)
			if controlMatchesSession(&stale, target, "desk") {
				t.Fatalf("stale Project pane control accepted: %+v", stale)
			}
		})
	}
}

func TestWorkspaceProjectShortcutsBrowseTabsAndSplitPanesWithoutQuickSelection(t *testing.T) {
	a := ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: a.InstanceID, SessionID: "BBB222"}
	c := ducklord.SessionIdentity{InstanceID: a.InstanceID, SessionID: "CCC333"}
	activity := ducklord.NewActivityState()
	projectID, err := activity.ProjectLayout.AddProject("Work")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := activity.ProjectLayout.Place(projectID, a, ducklord.PlaceNewTab, "")
	second, _ := activity.ProjectLayout.Place(projectID, b, ducklord.PlaceVertical, first)
	third, _ := activity.ProjectLayout.Place(projectID, c, ducklord.PlaceNewTab, "")
	state := &tuiState{workspacePreview: true, workspaceProjectFocus: true, cfg: &ducklord.Config{}, activityState: activity,
		sessions: []ducklord.RemoteSession{{InstanceID: a.InstanceID, SessionID: a.SessionID}, {InstanceID: b.InstanceID, SessionID: b.SessionID}, {InstanceID: c.InstanceID, SessionID: c.SessionID}}, selected: 0}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		key, want string
	}{
		{"L", second}, {"H", first}, {"]", third}, {"[", first},
	} {
		handled, changed := state.handleWorkspaceProjectInput([]byte(step.key))
		if !handled || !changed || nav.CurrentPaneID() != step.want || nav.Region() != ducklord.RegionProjects {
			t.Fatalf("key %q: handled=%v changed=%v location=%+v", step.key, handled, changed, nav)
		}
		if state.selected != 0 || state.focused || state.workspaceAttachFromProject {
			t.Fatalf("key %q changed quick selection or PTY focus", step.key)
		}
	}
}
