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
	state.workspaceQuickKey = state.currentKey()
	return state, projectID, a, b
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
	if state.workspacePaneStep != "existing" || len(state.workspacePaneCandidates()) != 1 {
		t.Fatalf("existing picker did not filter current Project: %v", state.workspacePaneChoices())
	}
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
