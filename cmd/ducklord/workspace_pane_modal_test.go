package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func workspacePaneTestState(t *testing.T) (*tuiState, string, ducklord.RemoteSession, ducklord.RemoteSession) {
	t.Helper()
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	a := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "AAA111", Name: "A", Kind: "shell", Status: "running", RuntimeGeneration: 1}
	b := ducklord.RemoteSession{Client: "host", InstanceID: instance, SessionID: "BBB222", Name: "B", Kind: "shell", Status: "running", RuntimeGeneration: 1}
	activity := ducklord.NewActivityState()
	projectID, err := activity.ProjectLayout.AddProject("Work")
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := ducklord.IdentityFromSession(a)
	if _, err := activity.ProjectLayout.Place(projectID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	other, _ := ducklord.IdentityFromSession(b)
	if err := activity.ProjectLayout.Discover(other); err != nil {
		t.Fatal(err)
	}
	store := ducklord.ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")}
	if err := store.Save(activity); err != nil {
		t.Fatal(err)
	}
	state := &tuiState{workspacePreview: true, workspaceProjectFocus: true, cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}},
		activityState: activity, activityStore: store, sessions: []ducklord.RemoteSession{a, b}, selected: 0}
	return state, projectID, a, b
}

func TestWorkspaceProjectDeleteIsLocalAndRehomesLastReference(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	nav, err := state.workspaceNavigation()
	if err != nil || nav.SelectProject(projectID) != nil {
		t.Fatal("select Project: ", err)
	}
	nav.ToggleNotificationFocus()
	state.beginWorkspaceProjectDelete()
	if !state.workspacePaneMode || state.workspacePaneStep != "project-delete-confirm" || state.workspacePaneIndex != 1 || state.workspacePaneIntent.projectID != projectID {
		t.Fatal("delete modal did not pin selected Project and default to Cancel")
	}
	state.handleWorkspacePaneInput([]byte("\r")) // Default Cancel must be a no-op.
	if state.activity().ProjectLayout.Project(projectID) == nil {
		t.Fatal("Cancel deleted Project")
	}
	state.beginWorkspaceProjectDelete()
	state.handleWorkspacePaneInput([]byte("\x1b[A"))
	state.handleWorkspacePaneInput([]byte("\r"))
	identity, _ := ducklord.IdentityFromSession(a)
	if state.workspacePaneMode || state.activity().ProjectLayout.Project(projectID) != nil || state.sessions[0].SessionID != a.SessionID || !state.workspacePaneChanged {
		t.Fatal("local Project deletion removed remote Session or left modal open")
	}
	if projects := state.activity().ProjectLayout.ProjectsFor(identity); len(projects) != 1 || projects[0] != ducklord.DefaultProjectID {
		t.Fatalf("last Session reference not rehomed: %v", projects)
	}
	if nav.CurrentProjectID() != ducklord.DefaultProjectID || nav.NotificationFocusProjectID() != "" {
		t.Fatal("deleted Project remained selected or notification-focused")
	}
	loaded, err := state.activityStore.Load()
	if err != nil || loaded.ProjectLayout.Project(projectID) != nil {
		t.Fatalf("deletion was not persisted: %v", err)
	}
}

func TestWorkspaceProjectDeleteRejectsDefaultStaleAndSaveFailure(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(ducklord.DefaultProjectID)
	state.beginWorkspaceProjectDelete()
	if state.workspacePaneMode || state.activity().ProjectLayout.Project(ducklord.DefaultProjectID) == nil {
		t.Fatal("Default Project deletion was allowed")
	}
	_ = nav.SelectProject(projectID)
	state.beginWorkspaceProjectDelete()
	state.activityStore.Path = t.TempDir() // Saving over directory must fail.
	state.handleWorkspacePaneInput([]byte("\x1b[A"))
	state.handleWorkspacePaneInput([]byte("\r"))
	if !state.workspacePaneMode || !strings.Contains(state.workspacePaneErr, "save Project deletion") || state.activity().ProjectLayout.Project(projectID) == nil || nav.CurrentProjectID() != projectID {
		t.Fatal("save failure mutated Project or navigation")
	}
	state.closeWorkspacePane()
	state.workspacePaneIntent.projectID = "missing-project"
	if err := state.commitWorkspaceProjectDelete(); err == nil || state.activity().ProjectLayout.Project(projectID) == nil {
		t.Fatal("stale Project identity deleted another Project")
	}
	identity, _ := ducklord.IdentityFromSession(a)
	if projects := state.activity().ProjectLayout.ProjectsFor(identity); len(projects) != 1 || projects[0] != projectID {
		t.Fatalf("failed deletion changed membership: %v", projects)
	}
}

func TestWorkspaceProjectDeleteKeepsSharedSessionInOtherProject(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	identity, _ := ducklord.IdentityFromSession(a)
	otherID, err := state.activity().ProjectLayout.AddProject("Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.activity().ProjectLayout.Place(otherID, identity, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	state.beginWorkspaceProjectDelete()
	state.handleWorkspacePaneInput([]byte("\x1b[A"))
	state.handleWorkspacePaneInput([]byte("\r"))
	if projects := state.activity().ProjectLayout.ProjectsFor(identity); len(projects) != 1 || projects[0] != otherID {
		t.Fatalf("shared Session was duplicated in Default or lost: %v", projects)
	}
}

func TestWorkspaceProjectDeleteEmptyAndHostScoped(t *testing.T) {
	state, _, _, _ := workspacePaneTestState(t)
	emptyID, err := state.activity().ProjectLayout.AddProject("Empty")
	if err != nil {
		t.Fatal(err)
	}
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(emptyID)
	state.hostScoped = true
	state.beginWorkspaceProjectDelete()
	if state.workspacePaneMode {
		t.Fatal("host-scoped TUI opened local Project deletion")
	}
	state.hostScoped = false
	state.beginWorkspaceProjectDelete()
	state.handleWorkspacePaneInput([]byte("\x1b[A"))
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.activity().ProjectLayout.Project(emptyID) != nil || nav.CurrentProjectID() == emptyID {
		t.Fatal("empty Project deletion did not reconcile navigation")
	}
}

func TestWorkspacePaneModalPlacesExistingAtomicallyAndKeepsQuickSelection(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	state.beginWorkspacePane()
	if !state.workspacePaneMode || state.workspacePaneStep != "placement" {
		t.Fatal("Project pane modal did not open")
	}
	state.handleWorkspacePaneInput([]byte("j")) // split vertically
	state.handleWorkspacePaneInput([]byte("\r"))
	state.handleWorkspacePaneInput([]byte("j")) // add existing
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneStep != "existing" || len(state.workspacePaneCandidates()) != 2 {
		t.Fatalf("existing picker did not offer current Project pane for move: %v", state.workspacePaneChoices())
	}
	state.handleWorkspacePaneInput([]byte("\x1b[B")) // choose B, not A already in Project
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneMode || state.selected != 0 || state.focused || state.currentSession().SessionID != a.SessionID {
		t.Fatal("local placement changed quick-list selection or PTY focus")
	}
	identity, _ := ducklord.IdentityFromSession(b)
	if got := state.activity().ProjectLayout.ProjectsFor(identity); len(got) != 1 || got[0] != projectID {
		t.Fatalf("placed B membership=%v", got)
	}
	if nav.CurrentProjectID() != projectID {
		t.Fatal("new Session pane not selected in Project")
	}
	loaded, err := state.activityStore.Load()
	if err != nil || len(loaded.ProjectLayout.ProjectsFor(identity)) != 1 || loaded.ProjectLayout.ProjectsFor(identity)[0] != projectID {
		t.Fatalf("placement was not persisted: %v %v", loaded, err)
	}
}

func TestWorkspacePaneSaveFailureDoesNotMutateLiveLayout(t *testing.T) {
	state, projectID, _, b := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	intent := workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	before := state.activity().ProjectLayout.Clone()
	state.activityStore.Path = t.TempDir() // rename over a directory fails
	if err := state.placeWorkspacePane(intent, b); err == nil || !strings.Contains(err.Error(), "save Session pane") {
		t.Fatalf("save failure was not reported: %v", err)
	}
	identity, _ := ducklord.IdentityFromSession(b)
	if got := state.activity().ProjectLayout.ProjectsFor(identity); len(got) != 1 || got[0] != ducklord.DefaultProjectID {
		t.Fatalf("failed save changed live Project membership: %v", got)
	}
	if len(before.Projects) != len(state.activity().ProjectLayout.Projects) || nav.CurrentProjectID() != projectID {
		t.Fatal("failed save changed Project navigation")
	}
}

func TestWorkspacePanePickerExcludesUnavailableAndPrefersLiveAlias(t *testing.T) {
	state, projectID, _, b := workspacePaneTestState(t)
	state.workspacePaneIntent = workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	offline := b
	offline.Client = "offline-alias"
	state.sessions = []ducklord.RemoteSession{offline, b}
	state.hostSync = map[string]ducklord.SessionUpdate{"offline-alias": {State: "disconnected"}, "host": {State: "live"}}
	if got := state.workspacePaneCandidates(); len(got) != 1 || got[0].Client != "host" {
		t.Fatalf("picker did not prefer live alias: %+v", got)
	}
	state.sessions[1].Status = "stopped"
	if got := state.workspacePaneCandidates(); len(got) != 0 {
		t.Fatalf("picker offered stopped Session: %+v", got)
	}
	if err := state.placeWorkspacePane(state.workspacePaneIntent, b); err == nil || !strings.Contains(err.Error(), "changed or host disconnected") {
		t.Fatalf("stale candidate was placed: %v", err)
	}
}

func TestWorkspaceProjectCreateAndNewShellIntent(t *testing.T) {
	state, projectID, _, _ := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	state.beginWorkspaceProject()
	for _, key := range []string{"新", "專", "案"} {
		state.handleWorkspacePaneInput([]byte(key))
	}
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneMode || nav.CurrentProjectID() == projectID || state.activity().ProjectLayout.Project(nav.CurrentProjectID()).Name != "新專案" {
		t.Fatalf("Project creation failed: %+v", nav)
	}
	state.beginWorkspacePane()
	state.handleWorkspacePaneInput([]byte("\r")) // new tab
	if !state.handleWorkspacePaneInput([]byte("\r")) || state.workspaceNewSessionIntent == nil {
		t.Fatal("New shell source lost its fixed Project placement intent")
	}
	if state.workspaceNewSessionIntent.projectID != nav.CurrentProjectID() || state.workspaceNewSessionIntent.placement != ducklord.PlaceNewTab {
		t.Fatalf("wrong new-shell intent: %+v", state.workspaceNewSessionIntent)
	}
}

func TestWorkspaceNewShellCompletionPlacesExactSessionOrLeavesDefault(t *testing.T) {
	for _, changedHost := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable host", true: "changed host"}[changedHost], func(t *testing.T) {
			state, projectID, a, b := workspacePaneTestState(t)
			state.runner = fakeRunner{sessions: []ducklord.RemoteSession{a, b}}
			state.pooledOutput = true
			state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
			state.newSessionMode, state.newSessionStarting = true, true
			state.newSessionStartGeneration, state.newSessionStartInstance = 2, a.InstanceID
			generation := uint64(2)
			if changedHost {
				generation++
			}
			state.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", Generation: generation, InstanceID: a.InstanceID}}
			state.completeNewSessionStart(context.Background(), "host", b.SessionID, nil)
			identity, _ := ducklord.IdentityFromSession(b)
			projects := state.activity().ProjectLayout.ProjectsFor(identity)
			if changedHost {
				if len(projects) != 1 || projects[0] != ducklord.DefaultProjectID || !strings.Contains(state.outputErr, "host changed") {
					t.Fatalf("changed host placed shell into stale Project: %v %q", projects, state.outputErr)
				}
			} else if len(projects) != 1 || projects[0] != projectID || state.outputErr != "" {
				t.Fatalf("stable host did not place new shell: %v %q", projects, state.outputErr)
			}
			if state.workspaceNewSessionIntent != nil {
				t.Fatal("completed shell kept stale pane intent")
			}
		})
	}
}

func TestWorkspaceNewShellIntentClearsOnHostChangeAndCancel(t *testing.T) {
	state, projectID, _, _ := workspacePaneTestState(t)
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	state.newSessionMode, state.newSessionStarting = true, true
	state.newSessionClient, state.newSessionStartGeneration, state.newSessionStartInstance = "host", 2, "old"
	if !state.invalidateCreateStart(ducklord.SessionUpdate{Client: "host", State: "live", Generation: 3, InstanceID: "new"}) || state.workspaceNewSessionIntent != nil {
		t.Fatal("host replacement retained stale Project placement intent")
	}
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	state.cancelCreate()
	if state.workspaceNewSessionIntent != nil {
		t.Fatal("cancel retained stale Project placement intent")
	}
}

func TestWorkspaceMoveTargetEmptyNavigationKeepsValidIndex(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	state.workspacePaneIntent = workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceVertical}
	identity, _ := ducklord.IdentityFromSession(a)
	paneID := state.projectPaneForSession(identity)
	if paneID == "" {
		t.Fatal("fixture has no Session pane")
	}
	state.workspacePaneMode = true
	state.workspacePaneStep = "move-target"
	state.workspacePaneIntent = workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceVertical}
	state.workspacePaneSourceID = paneID
	if choices := state.workspacePaneChoices(); len(choices) != 0 {
		t.Fatalf("expected no move target, got %v", choices)
	}
	state.handleWorkspacePaneInput([]byte("j"))
	state.handleWorkspacePaneInput([]byte("\x1b[B"))
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneIndex != 0 || state.workspacePaneStep != "move-target" {
		t.Fatalf("empty choices changed modal state: index=%d step=%s", state.workspacePaneIndex, state.workspacePaneStep)
	}
}

func TestHostDisconnectTargetsControlledPaneNotQuickListCursor(t *testing.T) {
	state, _, a, b := workspacePaneTestState(t)
	a.Client = "quick-host"
	b.Client = "focused-host"
	state.sessions = []ducklord.RemoteSession{a, b}
	state.selected = 0
	state.activeAttachKey = sessionKey(b)
	if !state.hostOwnsActivePTY("focused-host", nil) {
		t.Fatal("focused pane Host was not detected")
	}
	if state.hostOwnsActivePTY("quick-host", nil) {
		t.Fatal("quick-list cursor Host incorrectly owns controlled PTY")
	}
	state.activeAttachKey = ""
	state.pendingAttachKey = sessionKey(b)
	if !state.hostOwnsActivePTY("focused-host", nil) {
		t.Fatal("pending attach Host was not detected")
	}
}

func TestWorkspacePaneMoveAndDetachLeaveRemoteInventoryUntouched(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	bID, _ := ducklord.IdentityFromSession(b)
	paneID, err := state.activity().ProjectLayout.Place(projectID, bID, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectPane(projectID, paneID); err != nil {
		t.Fatal(err)
	}
	state.beginWorkspaceMove()
	state.handleWorkspacePaneInput([]byte("\r"))     // new tab
	state.handleWorkspacePaneInput([]byte("\x1b[A")) // select Move
	state.handleWorkspacePaneInput([]byte("\r"))     // confirm
	if state.workspacePaneMode || !state.workspacePaneChanged || len(state.sessions) != 2 || state.sessions[0].SessionID != a.SessionID || state.sessions[1].SessionID != b.SessionID {
		t.Fatal("move affected remote inventory or did not commit")
	}
	if _, ok := state.activity().ProjectLayout.PaneSession(projectID, paneID); ok {
		t.Fatal("old pane survived move")
	}
	state.workspacePaneChanged = false
	state.beginWorkspaceDetach()
	state.handleWorkspacePaneInput([]byte("\x1b[A")) // select Detach
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneMode || !state.workspacePaneChanged || len(state.sessions) != 2 {
		t.Fatal("detach affected remote inventory or did not commit")
	}
	if got := state.activity().ProjectLayout.ProjectsFor(bID); len(got) != 1 || got[0] != ducklord.DefaultProjectID {
		t.Fatalf("detached session not in Default: %v", got)
	}
}

func TestWorkspacePaneActionStaleSourceAndSaveFailureAreAtomic(t *testing.T) {
	state, projectID, _, _ := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	state.beginWorkspaceDetach()
	state.workspacePaneSourceID = "stale"
	if err := state.commitWorkspacePaneAction(true); err == nil {
		t.Fatal("stale source accepted")
	}
	state.closeWorkspacePane()
	state.beginWorkspaceDetach()
	state.activityStore.Path = t.TempDir()
	if err := state.commitWorkspacePaneAction(true); err == nil || !strings.Contains(err.Error(), "save Session pane") {
		t.Fatalf("save failure not reported: %v", err)
	}
	if _, ok := state.activity().ProjectLayout.PaneSession(projectID, state.workspacePaneSourceID); !ok || state.workspacePaneChanged {
		t.Fatal("failed save mutated live layout")
	}
}

func TestWorkspaceExistingPickerMovesSameProjectPaneOnlyAfterConfirmation(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	oldID := nav.CurrentPaneID()
	state.beginWorkspacePane()
	state.handleWorkspacePaneInput([]byte("\r"))     // new tab
	state.handleWorkspacePaneInput([]byte("\x1b[B")) // existing
	state.handleWorkspacePaneInput([]byte("\r"))
	if choices := state.workspacePaneChoices(); len(choices) == 0 || !strings.Contains(choices[0], "move in Project") {
		t.Fatalf("same-Project pane missing from picker: %v", choices)
	}
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneStep != "existing-move-confirm" || state.workspacePaneIndex != 1 {
		t.Fatalf("move confirmation missing or unsafe default: %s %d", state.workspacePaneStep, state.workspacePaneIndex)
	}
	state.handleWorkspacePaneInput([]byte("\r")) // default Cancel
	if _, ok := state.activity().ProjectLayout.PaneSession(projectID, oldID); !ok || state.workspacePaneChanged {
		t.Fatal("Cancel moved pane")
	}
	state.beginWorkspacePane()
	state.handleWorkspacePaneInput([]byte("\r"))
	state.handleWorkspacePaneInput([]byte("\x1b[B"))
	state.handleWorkspacePaneInput([]byte("\r"))
	state.handleWorkspacePaneInput([]byte("\r"))
	state.handleWorkspacePaneInput([]byte("\x1b[A")) // explicitly Move
	state.handleWorkspacePaneInput([]byte("\r"))
	identity, _ := ducklord.IdentityFromSession(a)
	if state.workspacePaneMode || !state.workspacePaneChanged {
		t.Fatal("confirmed move did not commit")
	}
	if _, ok := state.activity().ProjectLayout.PaneSession(projectID, oldID); ok {
		t.Fatal("old pane survived")
	}
	if got := state.activity().ProjectLayout.ProjectsFor(identity); len(got) != 1 || got[0] != projectID {
		t.Fatalf("move duplicated membership: %v", got)
	}
	if len(state.sessions) != 2 || state.sessions[0].SessionID != a.SessionID {
		t.Fatal("local move touched remote inventory")
	}
}

func TestWorkspaceExistingPickerRejectsSessionChangeBeforeMoveConfirmation(t *testing.T) {
	for _, change := range []string{"host disconnected", "runtime restarted"} {
		t.Run(change, func(t *testing.T) {
			state, projectID, a, _ := workspacePaneTestState(t)
			nav, _ := state.workspaceNavigation()
			_ = nav.SelectProject(projectID)
			oldID := nav.CurrentPaneID()
			state.beginWorkspacePane()
			state.handleWorkspacePaneInput([]byte("\r"))     // new tab
			state.handleWorkspacePaneInput([]byte("\x1b[B")) // existing Session
			state.handleWorkspacePaneInput([]byte("\r"))
			state.handleWorkspacePaneInput([]byte("\r")) // A -> move confirmation
			if state.workspacePaneStep != "existing-move-confirm" || state.workspacePaneCandidate.SessionID != a.SessionID {
				t.Fatal("picker did not retain the selected Session generation for confirmation")
			}
			switch change {
			case "host disconnected":
				state.hostSync = map[string]ducklord.SessionUpdate{"host": {State: "disconnected"}}
			case "runtime restarted":
				state.sessions[0].RuntimeGeneration++
			}
			state.handleWorkspacePaneInput([]byte("\x1b[A")) // explicitly Move
			state.handleWorkspacePaneInput([]byte("\r"))
			if !state.workspacePaneMode || !strings.Contains(state.workspacePaneErr, "changed or Host disconnected") || state.workspacePaneChanged {
				t.Fatalf("stale Session move was not rejected: mode=%t error=%q changed=%t", state.workspacePaneMode, state.workspacePaneErr, state.workspacePaneChanged)
			}
			if _, ok := state.activity().ProjectLayout.PaneSession(projectID, oldID); !ok {
				t.Fatal("stale Session move modified the Project layout")
			}
		})
	}
}

func TestWorkspaceExistingPickerRejectsStaleTargetAndSaveFailure(t *testing.T) {
	for _, failure := range []string{"stale target", "save failure"} {
		t.Run(failure, func(t *testing.T) {
			state, projectID, a, b := workspacePaneTestState(t)
			nav, _ := state.workspaceNavigation()
			_ = nav.SelectProject(projectID)
			aID, _ := ducklord.IdentityFromSession(a)
			bID, _ := ducklord.IdentityFromSession(b)
			bPaneID, err := state.activity().ProjectLayout.Place(projectID, bID, ducklord.PlaceNewTab, "")
			if err != nil {
				t.Fatal(err)
			}
			_ = nav.SelectPane(projectID, bPaneID)
			state.beginWorkspacePane()
			state.handleWorkspacePaneInput([]byte("j")) // split beside B
			state.handleWorkspacePaneInput([]byte("\r"))
			state.handleWorkspacePaneInput([]byte("\x1b[B"))
			state.handleWorkspacePaneInput([]byte("\r"))
			state.handleWorkspacePaneInput([]byte("\r")) // A -> confirmation
			if state.workspacePaneStep != "existing-move-confirm" {
				t.Fatalf("no confirmation: %s", state.workspacePaneStep)
			}
			if failure == "stale target" {
				state.workspacePaneIntent.targetID = "missing"
			} else {
				state.activityStore.Path = t.TempDir()
			}
			state.handleWorkspacePaneInput([]byte("\x1b[A"))
			state.handleWorkspacePaneInput([]byte("\r"))
			if !state.workspacePaneMode || state.workspacePaneErr == "" || state.workspacePaneChanged {
				t.Fatalf("failed move did not remain in modal: %q", state.workspacePaneErr)
			}
			if _, ok := state.activity().ProjectLayout.PaneSession(projectID, bPaneID); !ok {
				t.Fatal("failed move changed target pane")
			}
			if got := state.activity().ProjectLayout.ProjectsFor(aID); len(got) != 1 || got[0] != projectID {
				t.Fatalf("failed move changed A: %v", got)
			}
		})
	}
}
