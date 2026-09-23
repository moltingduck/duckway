package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if !strings.Contains(state.outputErr, "notification focus turned off") {
		t.Fatalf("deleted focused Project gave no focus-off notice: %q", state.outputErr)
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
	if got := state.workspacePaneCandidates(); len(got) != 1 || got[0].Client != "offline-alias" {
		t.Fatalf("picker failed to preserve known offline Session: %+v", got)
	}
	if err := state.placeWorkspacePane(state.workspacePaneIntent, b); err == nil || !strings.Contains(err.Error(), "changed or is unavailable") {
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
	for _, key := range []string{"\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D", "\x1b[H", "\x1b[F", "\x1b[3~", "\x1bOP", "\x1bx"} {
		state.handleWorkspacePaneInput([]byte(key))
		if state.workspacePaneName != "新專案" || !state.workspacePaneMode || state.workspacePaneStep != "project-create" {
			t.Fatalf("key %q corrupted Project creation: name=%q mode=%v step=%q", key, state.workspacePaneName, state.workspacePaneMode, state.workspacePaneStep)
		}
	}
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneStep != "project-hosts-create" {
		t.Fatal("missing Project Host selection")
	}
	state.handleWorkspacePaneInput([]byte("\r")) // select host
	state.handleWorkspacePaneInput([]byte("\x1b[B"))
	state.handleWorkspacePaneInput([]byte("\r")) // save project
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

func TestWorkspaceExistingSessionSearchIgnoresEscapeSequences(t *testing.T) {
	state, _, _, _ := workspacePaneTestState(t)
	state.workspacePaneMode, state.workspacePaneStep = true, "existing"
	state.workspacePaneQuery, state.workspacePaneIndex = "專案", 1
	for _, key := range []string{"\x1b[C", "\x1b[D", "\x1b[H", "\x1b[F", "\x1b[3~", "\x1bOP", "\x1bx"} {
		state.handleWorkspacePaneInput([]byte(key))
		if state.workspacePaneQuery != "專案" || state.workspacePaneIndex != 1 || state.workspacePaneStep != "existing" {
			t.Fatalf("key %q corrupted existing Session search: query=%q index=%d step=%q", key, state.workspacePaneQuery, state.workspacePaneIndex, state.workspacePaneStep)
		}
	}
	state.handleWorkspacePaneInput([]byte("\x7f"))
	if state.workspacePaneQuery != "專" {
		t.Fatalf("Unicode backspace: query=%q", state.workspacePaneQuery)
	}
	state.handleWorkspacePaneInput([]byte("\x1b"))
	if state.workspacePaneStep != "source" || state.workspacePaneQuery != "" {
		t.Fatal("Escape did not return to source selection and clear search")
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
			} else if len(projects) != 1 || projects[0] != projectID || state.outputErr != "Session pane created" {
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

func TestWorkspaceNewShellDiscoveryIgnoresTransientNonLiveUpdate(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	state.newSessionMode = true
	state.newSessionDiscovering = true
	state.newSessionStep = "host"
	state.newSessionClient = "host"
	state.hostSync = map[string]ducklord.SessionUpdate{"host": {
		Client: "host", State: "live", Generation: 2, InstanceID: a.InstanceID,
	}}

	if !state.applySessionUpdate(ducklord.SessionUpdate{
		Client: "host", State: "reconnecting", Generation: 2, InstanceID: a.InstanceID,
	}) {
		t.Fatal("transient non-live update was rejected")
	}
	if !state.newSessionDiscovering || state.newSessionStep != "host" || state.workspaceNewSessionIntent == nil {
		t.Fatalf("workspace shell discovery was cancelled: discovering=%v step=%q intent=%+v", state.newSessionDiscovering, state.newSessionStep, state.workspaceNewSessionIntent)
	}
	_, _, _, ready := state.applyCreateDiscovery(createDiscoveryEvent{
		kind: "projects", client: "host", generation: 2, instance: a.InstanceID,
		projects: []ducklord.RemoteProject{{Name: "Work", Path: "/work"}},
	})
	if ready || state.newSessionDiscovering || state.newSessionStep != "project" || state.workspaceNewSessionIntent == nil {
		t.Fatalf("workspace shell discovery result was discarded: ready=%v discovering=%v step=%q intent=%+v", ready, state.newSessionDiscovering, state.newSessionStep, state.workspaceNewSessionIntent)
	}
}

func TestWorkspaceNewShellDiscoveryRetriesAfterHostReconnect(t *testing.T) {
	state, projectID, _, _ := workspacePaneTestState(t)
	state.runner = fakeRunner{}
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	state.newSessionMode = true
	state.newSessionClient = "host"
	state.newSessionKind = "shell"
	state.newSessionStep = "host"
	state.newSessionErr = "host connection changed; choose it again"
	done := make(chan createDiscoveryEvent, 1)
	previous := ducklord.SessionUpdate{Client: "host", State: "offline", Generation: 1, InstanceID: "old"}
	update := ducklord.SessionUpdate{Client: "host", State: "live", Generation: 2, InstanceID: "new"}
	state.hostSync = map[string]ducklord.SessionUpdate{"host": update}
	if !state.retryWorkspaceCreateDiscovery(context.Background(), done, previous, update) {
		t.Fatal("workspace discovery was not retried after reconnect")
	}
	if state.workspaceNewSessionIntent == nil || state.newSessionClient != "host" || state.newSessionStep != "host" {
		t.Fatalf("workspace selection was not retained: intent=%+v client=%q step=%q", state.workspaceNewSessionIntent, state.newSessionClient, state.newSessionStep)
	}
	select {
	case event := <-done:
		if event.kind != "projects" || event.client != "host" {
			t.Fatalf("unexpected retry event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("workspace discovery retry did not complete")
	}
}

func TestWorkspaceNewShellDiscoveryRetriesWhenInFlightRequestIsCanceled(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	state.runner = fakeRunner{}
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceVertical}
	state.newSessionMode = true
	state.newSessionDiscovering = true
	state.newSessionClient = "host"
	state.newSessionKind = "shell"
	state.newSessionStep = "host"
	previous := ducklord.SessionUpdate{Client: "host", State: "live", Generation: 1, InstanceID: "old"}
	update := ducklord.SessionUpdate{Client: "host", State: "live", Generation: 2, InstanceID: a.InstanceID}
	state.hostSync = map[string]ducklord.SessionUpdate{"host": update}
	done := make(chan createDiscoveryEvent, 1)
	if !state.retryWorkspaceCreateDiscovery(context.Background(), done, previous, update) {
		t.Fatal("workspace discovery was not retried after the in-flight request was canceled")
	}
	if !state.newSessionDiscovering || state.workspaceNewSessionIntent == nil {
		t.Fatalf("workspace selection was not retained: discovering=%v intent=%+v", state.newSessionDiscovering, state.workspaceNewSessionIntent)
	}
	select {
	case event := <-done:
		if event.kind != "projects" || event.client != "host" {
			t.Fatalf("unexpected retry event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("workspace discovery retry did not complete")
	}
}

func TestWorkspaceNewShellDiscoveryDoesNotRestartActiveRequestOnInventoryUpdate(t *testing.T) {
	state, projectID, _, _ := workspacePaneTestState(t)
	state.runner = fakeRunner{}
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceHorizontal}
	state.newSessionMode = true
	state.newSessionDiscovering = true
	state.newSessionClient = "host"
	state.newSessionKind = "shell"
	state.newSessionStep = "host"
	state.newSessionErr = "loading bookmarks..."
	state.newSessionCancel = func() {}
	previous := ducklord.SessionUpdate{Client: "host", State: "live", Generation: 1, InstanceID: "old"}
	update := ducklord.SessionUpdate{Client: "host", State: "live", Generation: 2, InstanceID: "new"}
	state.hostSync = map[string]ducklord.SessionUpdate{"host": update}
	done := make(chan createDiscoveryEvent, 1)
	if state.retryWorkspaceCreateDiscovery(context.Background(), done, previous, update) {
		t.Fatal("active host discovery was restarted after an inventory update")
	}
	select {
	case event := <-done:
		t.Fatalf("unexpected replacement discovery event: %+v", event)
	default:
	}
}

func TestWorkspaceNewShellDefaultPlacementAfterDiscovery(t *testing.T) {
	for _, delivery := range []string{"immediate", "delayed poll", "delayed update"} {
		for _, placement := range []ducklord.PanePlacement{ducklord.PlaceNewTab, ducklord.PlaceVertical, ducklord.PlaceHorizontal} {
			t.Run(delivery+"/"+string(placement), func(t *testing.T) {
				state, _, a, b := workspacePaneTestState(t)
				created := b
				created.SessionID, created.Name = "CCC333", "Created"
				identity, _ := ducklord.IdentityFromSession(created)
				targetIdentity, _ := ducklord.IdentityFromSession(b)
				targetID := state.projectPaneID(ducklord.DefaultProjectID, targetIdentity)
				state.workspaceNewSessionIntent = &workspacePaneIntent{
					projectID: ducklord.DefaultProjectID, targetID: targetID, placement: placement,
				}
				state.pooledOutput = true
				state.newSessionMode, state.newSessionStarting = true, true
				state.newSessionStartGeneration, state.newSessionStartInstance = 2, a.InstanceID
				state.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", Generation: 2, InstanceID: a.InstanceID}}
				inventory := []ducklord.RemoteSession{a, b, created}
				state.runner = fakeRunner{sessions: inventory}
				if delivery != "immediate" {
					state.runner = fakeRunner{sessions: inventory[:2]}
				}
				state.completeNewSessionStart(context.Background(), "host", created.SessionID, nil)
				if delivery != "immediate" {
					if len(state.pendingWorkspacePlacements) != 1 {
						t.Fatal("creation did not retain pending placement")
					}
					if delivery == "delayed poll" {
						state.runner = fakeRunner{sessions: inventory}
						state.refreshSessions(context.Background())
					} else if !state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", State: "live", Generation: 2,
						InstanceID: a.InstanceID, Revision: 1, Sessions: inventory}) {
						t.Fatal("delayed inventory was rejected")
					}
				}
				if len(state.pendingWorkspacePlacements) != 0 || state.outputErr != "Session pane created" {
					t.Fatalf("creation failed: pending=%d message=%q", len(state.pendingWorkspacePlacements), state.outputErr)
				}
				saved, err := state.activityStore.Load()
				if err != nil {
					t.Fatal(err)
				}
				for _, layout := range []*ducklord.ProjectLayout{&state.activity().ProjectLayout, &saved.ProjectLayout} {
					project := layout.Project(ducklord.DefaultProjectID)
					if placement == ducklord.PlaceNewTab {
						if len(project.Tabs) != 2 || project.Tabs[1].Root.Session == nil || *project.Tabs[1].Root.Session != identity {
							t.Fatalf("new tab was not preserved: %+v", project.Tabs)
						}
					} else {
						if len(project.Tabs) != 1 {
							t.Fatalf("split left extra discovered tab: %d", len(project.Tabs))
						}
						root := project.Tabs[0].Root
						if root.Direction != ducklord.SplitDirection(placement) || root.First == nil || root.Second == nil ||
							root.First.ID != targetID || root.First.Session == nil || *root.First.Session != targetIdentity ||
							root.Second.Session == nil || *root.Second.Session != identity {
							t.Fatalf("created session did not split chosen target: %+v", root)
						}
					}
				}
				nav, err := state.workspaceNavigation()
				if err != nil {
					t.Fatal(err)
				}
				selected, ok := state.activity().ProjectLayout.PaneSession(nav.CurrentProjectID(), nav.CurrentPaneID())
				if !ok || selected != identity {
					t.Fatal("new pane was not selected")
				}
			})
		}
	}
}

func TestWorkspaceNewShellWaitsForExactSessionSynchronization(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	state.runner = fakeRunner{sessions: []ducklord.RemoteSession{a}}
	state.pooledOutput = true
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	state.newSessionMode, state.newSessionStarting = true, true
	state.newSessionStartGeneration, state.newSessionStartInstance = 2, a.InstanceID
	state.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", Generation: 2, InstanceID: a.InstanceID}}
	state.completeNewSessionStart(context.Background(), "host", b.SessionID, nil)
	if len(state.pendingWorkspacePlacements) != 1 || !strings.Contains(state.outputErr, "waiting for session synchronization") {
		t.Fatalf("start discarded pending pane placement: %+v %q", state.pendingWorkspacePlacements, state.outputErr)
	}
	identity, _ := ducklord.IdentityFromSession(b)
	if got := state.activity().ProjectLayout.ProjectsFor(identity); len(got) != 0 {
		t.Fatalf("placed shell before inventory listed it: %v", got)
	}
	state.runner = fakeRunner{sessions: []ducklord.RemoteSession{a, b}}
	state.refreshSessions(context.Background())
	if len(state.pendingWorkspacePlacements) != 0 {
		t.Fatal("synchronized shell retained pending placement")
	}
	if got := state.activity().ProjectLayout.ProjectsFor(identity); len(got) != 1 || got[0] != projectID {
		t.Fatalf("synchronized shell was not placed: %v", got)
	}
}

func TestWorkspacePendingPlacementRejectsHostConnectionEpochChange(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	state.runner = fakeRunner{sessions: []ducklord.RemoteSession{a}}
	state.pooledOutput = true
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	state.newSessionMode, state.newSessionStarting = true, true
	state.newSessionStartGeneration, state.newSessionStartInstance = 2, a.InstanceID
	state.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", Generation: 2, InstanceID: a.InstanceID}}
	state.completeNewSessionStart(context.Background(), "host", b.SessionID, nil)
	state.bumpHostConnectionEpoch("host")
	state.runner = fakeRunner{sessions: []ducklord.RemoteSession{a, b}}
	state.refreshSessions(context.Background())
	identity, _ := ducklord.IdentityFromSession(b)
	if got := state.activity().ProjectLayout.ProjectsFor(identity); len(got) != 1 || got[0] != ducklord.DefaultProjectID || len(state.pendingWorkspacePlacements) != 0 {
		t.Fatalf("stale connection placed pane: %v pending=%d", got, len(state.pendingWorkspacePlacements))
	}
}

func TestWorkspacePendingPlacementClearsOnHostDisconnect(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	state.runner = fakeRunner{sessions: []ducklord.RemoteSession{a}}
	state.pooledOutput = true
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	state.newSessionMode, state.newSessionStarting = true, true
	state.newSessionStartGeneration, state.newSessionStartInstance = 2, a.InstanceID
	state.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", Generation: 2, InstanceID: a.InstanceID}}
	state.completeNewSessionStart(context.Background(), "host", b.SessionID, nil)
	if !state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", State: "disconnected", Generation: 3, InstanceID: a.InstanceID}) {
		t.Fatal("disconnect update was rejected")
	}
	if len(state.pendingWorkspacePlacements) != 0 {
		t.Fatal("Host disconnect retained stale pending placement")
	}
}

func TestWorkspacePendingPlacementUsesDelayedHostUpdate(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	state.runner = fakeRunner{sessions: []ducklord.RemoteSession{a}}
	state.pooledOutput = true
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	state.newSessionMode, state.newSessionStarting = true, true
	state.newSessionStartGeneration, state.newSessionStartInstance = 2, a.InstanceID
	state.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", Generation: 2, InstanceID: a.InstanceID}}
	state.completeNewSessionStart(context.Background(), "host", b.SessionID, nil)
	if !state.applySessionUpdate(ducklord.SessionUpdate{Client: "host", State: "live", Generation: 2, InstanceID: a.InstanceID,
		Revision: 1, Sessions: []ducklord.RemoteSession{a, b}}) {
		t.Fatal("delayed Host inventory was rejected")
	}
	identity, _ := ducklord.IdentityFromSession(b)
	if got := state.activity().ProjectLayout.ProjectsFor(identity); len(got) != 1 || got[0] != projectID || len(state.pendingWorkspacePlacements) != 0 {
		t.Fatalf("delayed Host update did not place pane: %v pending=%d", got, len(state.pendingWorkspacePlacements))
	}
}

func TestWorkspacePendingPlacementExpiresWithoutSession(t *testing.T) {
	state, projectID, a, b := workspacePaneTestState(t)
	state.runner = fakeRunner{sessions: []ducklord.RemoteSession{a}}
	state.pooledOutput = true
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	state.newSessionMode, state.newSessionStarting = true, true
	state.newSessionStartGeneration, state.newSessionStartInstance = 2, a.InstanceID
	state.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", State: "live", Generation: 2, InstanceID: a.InstanceID}}
	state.completeNewSessionStart(context.Background(), "host", b.SessionID, nil)
	state.pendingWorkspacePlacements[0].createdAt = time.Now().Add(-31 * time.Second)
	state.refreshSessions(context.Background()) // same bounded poll used by the TUI ticker
	if len(state.pendingWorkspacePlacements) != 0 || !strings.Contains(state.outputErr, "not synchronized") {
		t.Fatalf("missing Session retained pending pane forever: %+v %q", state.pendingWorkspacePlacements, state.outputErr)
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

func TestWorkspaceDefaultPaneDetachPersistsClosedViewAndMembership(t *testing.T) {
	state, _, _, session := workspacePaneTestState(t)
	identity, _ := ducklord.IdentityFromSession(session)
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectQuickSession(identity); err != nil {
		t.Fatal(err)
	}
	paneID := nav.CurrentPaneID()
	state.beginWorkspaceDetach()
	state.handleWorkspacePaneInput([]byte("\r")) // Cancel is the default.
	if _, ok := state.activity().ProjectLayout.PaneSession(ducklord.DefaultProjectID, paneID); !ok {
		t.Fatal("default Cancel removed the pane")
	}
	state.beginWorkspaceDetach()
	state.handleWorkspacePaneInput([]byte("\x1b[A"))
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneMode || !state.workspacePaneChanged {
		t.Fatal("Default pane closure did not commit")
	}
	loaded, err := state.activityStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.ProjectLayout.PaneSession(ducklord.DefaultProjectID, paneID); ok {
		t.Fatal("closed Default pane remained in persisted layout")
	}
	if projects := loaded.ProjectLayout.ProjectsFor(identity); len(projects) != 1 || projects[0] != ducklord.DefaultProjectID {
		t.Fatalf("closure lost implicit Default membership: %v", projects)
	}
	if suppressed := loaded.ProjectLayout.SuppressedDefaultSessions; len(suppressed) != 1 || suppressed[0] != identity {
		t.Fatalf("closure did not persist Default suppression: %v", suppressed)
	}
	if err := loaded.ProjectLayout.Discover(identity); err != nil {
		t.Fatal(err)
	}
	if len(loaded.ProjectLayout.Project(ducklord.DefaultProjectID).Tabs) != 0 {
		t.Fatal("rediscovery reopened the closed Default pane")
	}
	if len(state.sessions) != 2 || !reflect.DeepEqual(state.sessions[1], session) {
		t.Fatal("local closure changed remote inventory")
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

func TestWorkspaceExistingPickerMoveConfirmationEscapeReturnsToPicker(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	nav, _ := state.workspaceNavigation()
	_ = nav.SelectProject(projectID)
	oldID := nav.CurrentPaneID()
	state.beginWorkspacePane()
	state.handleWorkspacePaneInput([]byte("\r"))     // new tab
	state.handleWorkspacePaneInput([]byte("\x1b[B")) // existing Session
	state.handleWorkspacePaneInput([]byte("\r"))
	state.handleWorkspacePaneInput([]byte(a.SessionID)) // retain search when going back
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneStep != "existing-move-confirm" || state.workspacePaneCandidate.Client == "" {
		t.Fatal("picker did not open move confirmation with a pinned candidate")
	}
	state.handleWorkspacePaneInput([]byte("\x1b"))
	if !state.workspacePaneMode || state.workspacePaneStep != "existing" || state.workspacePaneQuery != a.SessionID {
		t.Fatalf("Escape did not return to filtered picker: mode=%t step=%q query=%q", state.workspacePaneMode, state.workspacePaneStep, state.workspacePaneQuery)
	}
	if _, ok := state.activity().ProjectLayout.PaneSession(projectID, oldID); !ok || state.workspacePaneChanged {
		t.Fatal("Escape changed the existing pane")
	}
	state.handleWorkspacePaneInput([]byte("\x1b"))
	if state.workspacePaneStep != "source" || state.workspacePaneQuery != "" {
		t.Fatal("second Escape did not return to source selection")
	}
}

func TestWorkspaceDroppedPaneMoveConfirmationEscapeReturnsToPlacement(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	identity, _ := ducklord.IdentityFromSession(a)
	oldID := state.projectPaneID(projectID, identity)
	state.beginWorkspaceDrop(a, identity, projectID, oldID)
	state.handleWorkspacePaneInput([]byte("\r")) // new tab
	if state.workspacePaneStep != "existing-move-confirm" {
		t.Fatalf("drop did not open move confirmation: %q", state.workspacePaneStep)
	}
	state.handleWorkspacePaneInput([]byte("\x1b"))
	if !state.workspacePaneMode || state.workspacePaneStep != "drop-placement" || state.workspacePaneCandidate.SessionID != a.SessionID || state.workspacePaneCandidate.RuntimeGeneration != a.RuntimeGeneration || len(state.workspacePaneChoices()) != 3 {
		t.Fatalf("Escape did not restore drag placement: mode=%t step=%q", state.workspacePaneMode, state.workspacePaneStep)
	}
	if _, ok := state.activity().ProjectLayout.PaneSession(projectID, oldID); !ok || state.workspacePaneChanged {
		t.Fatal("Escape moved the dragged pane")
	}
	state.handleWorkspacePaneInput([]byte("\r"))
	state.handleWorkspacePaneInput([]byte("\x1b[A")) // confirm Move after returning
	state.handleWorkspacePaneInput([]byte("\r"))
	if state.workspacePaneMode || !state.workspacePaneChanged {
		t.Fatalf("move after Escape failed: %q", state.workspacePaneErr)
	}
}

func TestWorkspaceExistingPickerRejectsSessionChangeBeforeMoveConfirmation(t *testing.T) {
	for _, change := range []string{"runtime restarted", "session removed"} {
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
			case "runtime restarted":
				state.sessions[0].RuntimeGeneration++
			case "session removed":
				state.sessions = state.sessions[1:]
			}
			state.handleWorkspacePaneInput([]byte("\x1b[A")) // explicitly Move
			state.handleWorkspacePaneInput([]byte("\r"))
			if !state.workspacePaneMode || !strings.Contains(state.workspacePaneErr, "changed or is unavailable") || state.workspacePaneChanged {
				t.Fatalf("stale Session move was not rejected: mode=%t error=%q changed=%t", state.workspacePaneMode, state.workspacePaneErr, state.workspacePaneChanged)
			}
			if _, ok := state.activity().ProjectLayout.PaneSession(projectID, oldID); !ok {
				t.Fatal("stale Session move modified the Project layout")
			}
		})
	}
}

func TestWorkspaceExistingPickerMovesKnownOfflinePaneLocally(t *testing.T) {
	state, projectID, a, _ := workspacePaneTestState(t)
	nav, err := state.workspaceNavigation()
	if err != nil || nav.SelectProject(projectID) != nil {
		t.Fatal("select Project: ", err)
	}
	oldID := nav.CurrentPaneID()
	state.sessions[0].Status = "disconnected"
	state.hostSync = map[string]ducklord.SessionUpdate{"host": {State: "disconnected"}}
	state.disconnectedHosts = map[string]bool{"host": true}
	state.beginWorkspacePane()
	state.handleWorkspacePaneInput([]byte("\r"))     // new tab
	state.handleWorkspacePaneInput([]byte("\x1b[B")) // add existing
	state.handleWorkspacePaneInput([]byte("\r"))
	if choices := state.workspacePaneChoices(); len(choices) == 0 || !strings.Contains(choices[0], "(offline)") {
		t.Fatalf("offline Session missing from picker: %v", choices)
	}
	state.handleWorkspacePaneInput([]byte("\r"))     // choose A
	state.handleWorkspacePaneInput([]byte("\x1b[A")) // select Move
	state.handleWorkspacePaneInput([]byte("\r"))
	identity, _ := ducklord.IdentityFromSession(a)
	if state.workspacePaneMode || !state.workspacePaneChanged || len(state.sessions) != 2 {
		t.Fatalf("offline local move did not commit: mode=%t err=%q", state.workspacePaneMode, state.workspacePaneErr)
	}
	if _, ok := state.activity().ProjectLayout.PaneSession(projectID, oldID); ok {
		t.Fatal("old offline pane survived move")
	}
	if got := state.activity().ProjectLayout.ProjectsFor(identity); len(got) != 1 || got[0] != projectID {
		t.Fatalf("offline move changed Project membership: %v", got)
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

func TestWorkspaceExistingPickerCanPlaceKnownOfflineSession(t *testing.T) {
	state, projectID, _, offline := workspacePaneTestState(t)
	for i := range state.sessions {
		if state.sessions[i].SessionID == offline.SessionID {
			state.sessions[i].Status = "disconnected"
			state.sessions[i].Error = "SSH unavailable"
		}
	}
	state.disconnectedHosts = map[string]bool{offline.Client: true}
	identity, ok := ducklord.IdentityFromSession(offline)
	if !ok {
		t.Fatal("offline fixture has no identity")
	}
	intent := workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}
	if err := state.placeWorkspacePane(intent, state.sessions[1]); err != nil {
		t.Fatalf("place known offline Session: %v", err)
	}
	if got := state.activity().ProjectLayout.ProjectsFor(identity); len(got) != 1 || got[0] != projectID {
		t.Fatalf("offline Session placement=%v", got)
	}
}

func TestNotesEmptyEnterIsSafe(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.notesEntries = nil
	s.handleNotesInput([]byte("\r"))
	if s.outputErr == "" {
		t.Fatal("empty Notes Enter produced no feedback")
	}
}

func TestNotesExitNeverFallsThroughToCreate(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	priorPane := nav.CurrentPaneID()
	s.notesPreviousProjectID, s.notesPreviousPaneID = projectID, priorPane
	s.notesPreviousProjectFocus = true
	s.workspacePaneMode, s.workspacePaneStep = true, "notes"
	if !s.handleCentralModalCancel([]byte("\x1b")) {
		t.Fatal("Notes Esc was not consumed")
	}
	if s.workspacePaneMode || s.newSessionMode {
		t.Fatalf("Notes Esc leaked into Create: pane=%v create=%v", s.workspacePaneMode, s.newSessionMode)
	}
	if nav.CurrentProjectID() != projectID || nav.CurrentPaneID() != priorPane {
		t.Fatalf("Notes Esc changed navigation: %s/%s", nav.CurrentProjectID(), nav.CurrentPaneID())
	}

	s.workspacePaneMode, s.workspacePaneStep, s.notesFormActive = true, "notes", true
	s.notesPreviousProjectID, s.notesPreviousPaneID = projectID, priorPane
	s.openPrefixPane("o")
	if s.workspacePaneMode || s.newSessionMode || s.notesFormActive {
		t.Fatalf("Notes prefix+o leaked state: pane=%v create=%v form=%v", s.workspacePaneMode, s.newSessionMode, s.notesFormActive)
	}

	// Ctrl-C follows the same modal cancellation path and must not be routed
	// into Project actions while the built-in form is active.
	s.workspacePaneMode, s.workspacePaneStep, s.notesFormActive = true, "notes", true
	if !s.handleCentralModalCancel([]byte("\x03")) {
		t.Fatal("Notes Ctrl-C was not consumed")
	}
	if !s.workspacePaneMode || s.newSessionMode || s.notesFormActive {
		t.Fatalf("Notes Ctrl-C did not clear form: pane=%v create=%v form=%v", s.workspacePaneMode, s.newSessionMode, s.notesFormActive)
	}
	if !s.handleCentralModalCancel([]byte("\x03")) || s.workspacePaneMode {
		t.Fatal("second Notes Ctrl-C did not close modal")
	}
}

func TestNotesFormCursorEditsAtInsertionPoint(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.notesFormActive, s.notesFormField, s.notesFormTitle = true, 0, "abc"
	s.notesFormCursor = 1
	s.handleNotesInput([]byte("X"))
	if s.notesFormTitle != "aXbc" || s.notesFormCursor != 2 {
		t.Fatalf("insert at cursor: value=%q cursor=%d", s.notesFormTitle, s.notesFormCursor)
	}
	s.handleNotesInput([]byte("\x1b[C"))
	s.handleNotesInput([]byte("\x1b[D"))
	s.handleNotesInput([]byte("\x7f"))
	if s.notesFormTitle != "abc" || s.notesFormCursor != 1 {
		t.Fatalf("delete before cursor: value=%q cursor=%d", s.notesFormTitle, s.notesFormCursor)
	}
	s.handleNotesInput([]byte("\x1b[3~"))
	if s.notesFormTitle != "ac" || s.notesFormCursor != 1 {
		t.Fatalf("forward delete at cursor: value=%q cursor=%d", s.notesFormTitle, s.notesFormCursor)
	}
	s.handleNotesInput([]byte("\x1b[C"))
	s.handleNotesInput([]byte("\x1b[3~"))
	if s.notesFormTitle != "ac" || s.notesFormCursor != 2 {
		t.Fatalf("forward delete at end changed value: value=%q cursor=%d", s.notesFormTitle, s.notesFormCursor)
	}
	s.handleNotesInput([]byte("\x1b[D"))
	var out strings.Builder
	s.renderNotesModal(&out, 80, 20)
	if !strings.Contains(out.String(), "Editing Title") || !strings.Contains(out.String(), "cursor 1/2") || !strings.Contains(out.String(), "a│c") {
		t.Fatalf("cursor state not visible: %q", out.String())
	}
}

func TestNotesFormArrowsSurviveModalInputRouting(t *testing.T) {
	s := &tuiState{
		workspacePaneMode: true,
		workspacePaneStep: "notes",
		notesFormActive:   true,
		notesFormField:    0,
		notesFormTitle:    "abc",
		notesFormCursor:   1,
	}
	for _, tc := range []struct {
		input  []byte
		cursor int
	}{
		{[]byte("\x1b[C"), 2},
		{[]byte("\x1b[D"), 1},
	} {
		if s.shouldDiscardModalArrow(tc.input) {
			t.Fatalf("Notes form arrow %q was discarded by modal routing", tc.input)
		}
		if s.handleCentralModalCancel(tc.input) {
			t.Fatalf("Notes form arrow %q was treated as modal cancellation", tc.input)
		}
		if !s.handleWorkspacePaneInput(tc.input) || s.notesFormCursor != tc.cursor {
			t.Fatalf("Notes form arrow %q did not reach cursor handler: cursor=%d", tc.input, s.notesFormCursor)
		}
	}
}

func TestDetachConfirmationArrowThenEnterKeepsModalSelection(t *testing.T) {
	s := &tuiState{
		workspacePaneMode:  true,
		workspacePaneStep:  "detach-confirm",
		workspacePaneIndex: 1, // Cancel is the safe default.
	}
	s.handleWorkspacePaneInput([]byte("\x1b[A"))
	if !s.workspacePaneMode || s.workspacePaneIndex != 0 {
		t.Fatalf("up arrow changed detach modal incorrectly: open=%v index=%d", s.workspacePaneMode, s.workspacePaneIndex)
	}
}

func TestNotesModalRendersManuscriptExcerptWithinHeight(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.workspacePaneMode = true
	s.workspacePaneStep = "notes"
	s.notesScope = ducklord.NotesProject
	s.notesEntries = []ducklord.NoteEntry{{Title: "Chronicle", Body: "first line\nwith a very long body that should be shortened before it reaches the modal display"}}

	var out strings.Builder
	s.renderWorkspacePaneModal(&out, 100, 8)
	rendered := out.String()
	for _, want := range []string{"Notes Codex", "Chronicle", "first line with a very long body", "Enter copies selected CONTENT"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("Notes modal missing %q in %q", want, rendered)
		}
	}
	if strings.Contains(rendered, "before it reaches the modal display") {
		t.Fatalf("Notes modal rendered an unbounded body excerpt: %q", rendered)
	}
}

func TestOpenPrefixNotesCreatesLoadsAndIsIdempotent(t *testing.T) {
	s, projectID, a, _ := workspacePaneTestState(t)
	s.cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	s.notesProjectID = projectID
	priorNav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	priorProject, priorPane := priorNav.CurrentProjectID(), priorNav.CurrentPaneID()
	if err := ducklord.SaveNotes(filepath.Dir(s.cfgPath), projectID, "## Seed\nbody\n"); err != nil {
		t.Fatal(err)
	}
	s.openPrefixPane("o")
	if !s.workspacePaneMode || s.workspacePaneStep != "notes" || len(s.notesEntries) != 1 || s.notesEntries[0].Body != "body" {
		t.Fatalf("Notes open failed: mode=%v entries=%v err=%s", s.workspacePaneMode, s.notesEntries, s.outputErr)
	}
	wantIdentity, ok := ducklord.IdentityFromSession(a)
	if !ok || s.notesSessionIdentity != wantIdentity {
		t.Fatalf("Notes open lost selected Session identity: got=%+v want=%+v", s.notesSessionIdentity, wantIdentity)
	}
	if !s.syncCurrentNotes() || s.notesSessionIdentity != wantIdentity {
		t.Fatalf("Notes sync lost selected Session identity: got=%+v want=%+v", s.notesSessionIdentity, wantIdentity)
	}
	s.handleNotesInput([]byte("s"))
	if strings.Contains(s.outputErr, "unavailable") {
		t.Fatalf("Session scope became unavailable after opening Notes: %q", s.outputErr)
	}
	p := s.activity().ProjectLayout.Project(projectID)
	count := len(p.Tabs)
	s.openPrefixPane("o")
	if len(p.Tabs) != count {
		t.Fatalf("repeated Notes open added tab: %d -> %d", count, len(p.Tabs))
	}
	if s.workspacePaneMode {
		t.Fatal("prefix+o did not toggle Notes modal closed")
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if nav.CurrentProjectID() != priorProject || nav.CurrentPaneID() != priorPane {
		t.Fatalf("Esc after repeated Notes open returned to %s/%s, want %s/%s", nav.CurrentProjectID(), nav.CurrentPaneID(), priorProject, priorPane)
	}
}

func TestProjectFocusPrefixNotesRouteReturnsToProjectPane(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	s.workspaceProjectFocus = true
	priorPane := nav.CurrentPaneID()

	if consumed, command := s.handlePanePrefix([]byte("\x02")); !consumed || command != "" {
		t.Fatalf("prefix start consumed=%v command=%q", consumed, command)
	}
	consumed, command := s.handlePanePrefix([]byte("o"))
	if !consumed || command != "o" {
		t.Fatalf("prefix+o consumed=%v command=%q", consumed, command)
	}
	s.openPrefixPane(command)
	if !s.workspacePaneMode || s.workspacePaneStep != "notes" {
		t.Fatalf("prefix+o did not open Notes modal: project=%q pane=%q err=%q", nav.CurrentProjectID(), nav.CurrentPaneID(), s.outputErr)
	}
	if !s.handleCentralModalCancel([]byte("\x1b")) {
		t.Fatal("Esc was not consumed by Notes modal")
	}
	if nav.CurrentProjectID() != projectID || nav.CurrentPaneID() != priorPane {
		t.Fatalf("Esc returned to %s/%s, want %s/%s", nav.CurrentProjectID(), nav.CurrentPaneID(), projectID, priorPane)
	}
	if !s.workspaceProjectFocus {
		t.Fatal("Esc did not restore Project focus")
	}
}

func TestNotesModalDoesNotMutateLayoutAndFormPersists(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	s.cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	before := s.activity().ProjectLayout.Clone()
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	project, pane := nav.CurrentProjectID(), nav.CurrentPaneID()
	s.openPrefixPane("o")
	if !s.workspacePaneMode || s.workspacePaneStep != "notes" {
		t.Fatalf("Notes modal did not open: %q", s.outputErr)
	}
	if !reflect.DeepEqual(before, s.activity().ProjectLayout) || nav.CurrentProjectID() != project || nav.CurrentPaneID() != pane {
		t.Fatal("opening Notes changed the workspace layout or selection")
	}
	s.handleNotesInput([]byte("a"))
	for _, b := range [][]byte{[]byte("New note"), []byte("\t"), []byte("body"), []byte("\x13")} {
		s.handleNotesInput(b)
	}
	entries, err := ducklord.LoadNotes(filepath.Dir(s.cfgPath), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Title != "New note" || entries[0].Body != "body" {
		t.Fatalf("form did not persist note: %+v", entries)
	}
	s.handleNotesInput([]byte("e"))
	s.handleNotesInput([]byte("\x13"))
}

func TestEditNotesRestoreFailureLeavesError(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	s.cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	s.notesProjectID = projectID
	if err := ducklord.SaveNotes(filepath.Dir(s.cfgPath), projectID, "## Seed\nbody\n"); err != nil {
		t.Fatal(err)
	}
	oldMakeRaw, oldRestore := notesMakeRaw, notesRestore
	defer func() { notesMakeRaw, notesRestore = oldMakeRaw, oldRestore }()
	calls := 0
	notesMakeRaw = func() (*termState, error) {
		calls++
		if calls == 2 {
			return nil, fmt.Errorf("injected restore failure")
		}
		return &termState{}, nil
	}
	notesRestore = func(*termState) {}
	t.Setenv("EDITOR", "true")
	s.editNotesNotebook()
	if s.outputErr != "restore editor terminal: injected restore failure" {
		t.Fatalf("restore failure outputErr=%q", s.outputErr)
	}
}

func TestSyncCurrentNotesLoadsSelectedProject(t *testing.T) {
	s, first, _, _ := workspacePaneTestState(t)
	root := t.TempDir()
	s.cfgPath = filepath.Join(root, "config.yaml")
	second, err := s.activity().ProjectLayout.AddProject("Second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.activity().ProjectLayout.PlaceNote(second); err != nil {
		t.Fatal(err)
	}
	if err = ducklord.SaveNotes(root, first, "## First\nfirst body\n"); err != nil {
		t.Fatal(err)
	}
	if err = ducklord.SaveNotes(root, second, "## Second\nsecond body\n"); err != nil {
		t.Fatal(err)
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err = nav.SelectProject(second); err != nil {
		t.Fatal(err)
	}
	_, pane, ok := s.activity().ProjectLayout.NotePane(second)
	if !ok {
		t.Fatal("missing second Notes pane")
	}
	if err = nav.SelectPane(second, pane); err != nil {
		t.Fatal(err)
	}
	s.notesSessionIdentity = ducklord.SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "ABC123"}
	s.notesProjectID = first
	if !s.syncCurrentNotes() || len(s.notesEntries) != 1 || s.notesEntries[0].Body != "second body" {
		t.Fatalf("sync did not replace entries: %+v", s.notesEntries)
	}
	if s.notesSessionIdentity.Key() != "" || s.notesSessionID != "" {
		t.Fatalf("stale session identity retained on Notes pane: %+v/%q", s.notesSessionIdentity, s.notesSessionID)
	}
	s.notesScope = ducklord.NotesSession
	s.handleNotesInput([]byte("s"))
	if !strings.Contains(s.outputErr, "unavailable") {
		t.Fatalf("expected unavailable Session book, got %q", s.outputErr)
	}
}

func TestNotesEntryEditorsPersistAddEditAndWholeNotebook(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	root := t.TempDir()
	s.cfgPath = filepath.Join(root, "config.yaml")
	s.notesProjectID = projectID
	if err := ducklord.SaveNotes(root, projectID, "## First\none\n"); err != nil {
		t.Fatal(err)
	}
	editor := filepath.Join(root, "editor")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s\\n' '## Edited' 'changed' > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.notesEntries, _ = ducklord.LoadNotes(root, projectID)
	s.workspacePaneIndex = 0
	oldMakeRaw, oldRestore := notesMakeRaw, notesRestore
	defer func() { notesMakeRaw, notesRestore = oldMakeRaw, oldRestore }()
	notesMakeRaw = func() (*termState, error) { return &termState{}, nil }
	notesRestore = func(*termState) {}
	t.Setenv("EDITOR", editor)
	s.editNotesEntry(0)
	entries, err := ducklord.LoadNotes(root, projectID)
	if err != nil || len(entries) != 1 || entries[0].Title != "Edited" || entries[0].Body != "changed" {
		t.Fatalf("selected edit entries=%+v err=%v", entries, err)
	}
	s.editNotesEntry(-1)
	entries, err = ducklord.LoadNotes(root, projectID)
	if err != nil || len(entries) != 2 || entries[1].Title != "Edited" {
		t.Fatalf("add entries=%+v err=%v", entries, err)
	}
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s\\n' '## Whole' 'notebook body' > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.editNotesNotebook()
	entries, err = ducklord.LoadNotes(root, projectID)
	if err != nil || len(entries) != 1 || entries[0].Title != "Whole" || entries[0].Body != "notebook body" {
		t.Fatalf("whole edit entries=%+v err=%v output=%q", entries, err, s.outputErr)
	}
}

func TestNotesEntryEditRejectsReorderedSelectionBeforeEditor(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	root := t.TempDir()
	s.cfgPath = filepath.Join(root, "config.yaml")
	s.notesProjectID = projectID
	s.notesEntries = []ducklord.NoteEntry{{Title: "Old", Body: "old body"}}
	if err := ducklord.SaveNotes(root, projectID, "## Current\ncurrent body\n## Other\nbody\n"); err != nil {
		t.Fatal(err)
	}
	s.editNotesEntry(0)
	if !strings.Contains(s.outputErr, "note changed while editing") {
		t.Fatalf("missing stale selection error: %q", s.outputErr)
	}
	if len(s.notesEntries) != 2 || s.notesEntries[0].Title != "Current" {
		t.Fatalf("stale selection was not refreshed: %+v", s.notesEntries)
	}
	entries, err := ducklord.LoadNotes(root, projectID)
	if err != nil || len(entries) != 2 || entries[0].Title != "Current" {
		t.Fatalf("disk notes changed after rejected edit: %+v err=%v", entries, err)
	}
}

func TestNotesInputDoesNotConsumeHelpKeys(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	s.notesProjectID = projectID
	s.notesEntries = []ducklord.NoteEntry{{Title: "One"}, {Title: "Two"}}
	s.workspacePaneIndex = 0
	s.helpMode = true
	for _, key := range []string{"j", "a", "e", "E", "\r", "\x1b"} {
		if s.handleNotesInput([]byte(key)) {
			t.Fatalf("help key %q was consumed", key)
		}
	}
	if s.workspacePaneIndex != 0 || len(s.notesEntries) != 2 {
		t.Fatalf("help input changed Notes state: index=%d entries=%d", s.workspacePaneIndex, len(s.notesEntries))
	}
}

func TestScopedNotesInputRoutesSearchAndPages(t *testing.T) {
	s, projectID, a, b := workspacePaneTestState(t)
	root := t.TempDir()
	s.cfgPath = filepath.Join(root, "config.yaml")
	if _, err := s.activity().ProjectLayout.Place(projectID, ducklord.SessionIdentity{InstanceID: b.InstanceID, SessionID: b.SessionID}, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	s.notesProjectID = projectID
	s.notesSessionIdentity, _ = ducklord.IdentityFromSession(a)
	var sessionBook string
	var err error
	sessionBook, err = ducklord.SessionNotesIdentity(s.notesSessionIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := ducklord.SaveScopedNotes(root, ducklord.NotesGlobal, "", "## Global\nalpha\n"); err != nil {
		t.Fatal(err)
	}
	if err := ducklord.SaveScopedNotes(root, ducklord.NotesProject, projectID, "## Project\nbeta\n## Other\ngamma\n"); err != nil {
		t.Fatal(err)
	}
	if err := ducklord.SaveScopedNotes(root, ducklord.NotesSession, sessionBook, "## Session\ndelta\n"); err != nil {
		t.Fatal(err)
	}
	s.notesScope = ducklord.NotesProject
	s.loadNotesScope()
	s.handleNotesInput([]byte("g"))
	if s.notesScope != ducklord.NotesGlobal || len(s.notesEntries) != 1 || s.notesEntries[0].Title != "Global" {
		t.Fatalf("global route: scope=%q entries=%+v", s.notesScope, s.notesEntries)
	}
	s.handleNotesInput([]byte("s"))
	if s.notesScope != ducklord.NotesSession || len(s.notesEntries) != 1 || s.notesEntries[0].Title != "Session" {
		t.Fatalf("session route: scope=%q entries=%+v err=%q", s.notesScope, s.notesEntries, s.outputErr)
	}
	s.handleNotesInput([]byte("p"))
	s.handleNotesInput([]byte("/"))
	s.handleNotesInput([]byte("g"))
	s.handleNotesInput([]byte("\r"))
	if len(s.notesEntries) != 1 || s.notesEntries[0].Title != "Other" {
		t.Fatalf("project search: query=%q entries=%+v", s.notesQuery, s.notesEntries)
	}
	if len(s.notesEntryIndexes) != 1 || s.notesEntryIndexes[0] != 1 {
		t.Fatalf("search lost raw entry identity: indexes=%v", s.notesEntryIndexes)
	}
	s.notesQuery = ""
	s.loadNotesScope()
	s.workspacePaneIndex = 0
	firstIdentity, _ := ducklord.IdentityFromSession(a)
	s.notesSessionIdentity = firstIdentity
	s.notesSessionID = firstIdentity.SessionID
	s.handleNotesInput([]byte("\x1b[C"))
	// Project Notes opens the Session picker when the Project has multiple
	// attached Sessions; select the second child explicitly.
	s.handleNotesInput([]byte("\x1b[B"))
	s.handleNotesInput([]byte("\r"))
	if s.notesScope != ducklord.NotesSession || s.notesSessionID != b.SessionID {
		t.Fatalf("right did not navigate project to child session: scope=%q session=%q", s.notesScope, s.notesSessionID)
	}
}

func TestProjectNotesRightArrowOpensItsOnlySession(t *testing.T) {
	s, projectID, a, _ := workspacePaneTestState(t)
	s.notesProjectID = projectID
	s.notesScope = ducklord.NotesProject
	s.notesSessionIdentity, _ = ducklord.IdentityFromSession(a)
	s.notesSessionID = a.SessionID
	if !s.handleNotesInput([]byte("\x1b[C")) {
		t.Fatal("right arrow was not consumed")
	}
	if s.notesScope != ducklord.NotesSession || s.notesSessionName() != a.Name {
		t.Fatalf("Project Notes did not enter named Session scope: scope=%q name=%q id=%q", s.notesScope, s.notesSessionName(), s.notesSessionID)
	}
}

func TestProjectNotesRightArrowListsSessionsAddedAfterOpening(t *testing.T) {
	s, projectID, a, b := workspacePaneTestState(t)
	first, _ := ducklord.IdentityFromSession(a)
	second, _ := ducklord.IdentityFromSession(b)
	s.notesProjectID = projectID
	s.notesScope = ducklord.NotesProject
	s.notesSessionIdentity = first
	s.notesSessionID = first.SessionID
	s.loadNotesScope()
	if _, err := s.activity().ProjectLayout.Place(projectID, second, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}

	if !s.handleNotesInput([]byte("\x1b[C")) {
		t.Fatal("right arrow was not consumed")
	}
	if s.notesPickerMode != "session" || !reflect.DeepEqual(s.notesPickerChoices, []string{first.Key(), second.Key()}) {
		t.Fatalf("Project Notes picker choices=%v mode=%q", s.notesPickerChoices, s.notesPickerMode)
	}
	var out strings.Builder
	s.renderNotesModal(&out, 100, 30)
	if !strings.Contains(out.String(), a.Name) || !strings.Contains(out.String(), b.Name) {
		t.Fatalf("Project Notes picker omitted attached Session: %q", out.String())
	}

	s.handleNotesInput([]byte("j"))
	s.handleNotesInput([]byte("\r"))
	if s.notesScope != ducklord.NotesSession || s.notesSessionIdentity != second {
		t.Fatalf("Project Notes picker did not select added Session: scope=%q identity=%+v", s.notesScope, s.notesSessionIdentity)
	}
}

func TestSessionNotesPickerUsesSessionNames(t *testing.T) {
	s, projectID, a, b := workspacePaneTestState(t)
	first, _ := ducklord.IdentityFromSession(a)
	second, _ := ducklord.IdentityFromSession(b)
	if _, err := s.activity().ProjectLayout.Place(projectID, second, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	s.openNotesPicker("session", []string{first.Key(), second.Key()})
	var out strings.Builder
	s.renderNotesModal(&out, 100, 30)
	if !strings.Contains(out.String(), a.Name) || !strings.Contains(out.String(), b.Name) {
		t.Fatalf("Session picker omitted names: %q", out.String())
	}
	if strings.Contains(out.String(), first.Key()) || strings.Contains(out.String(), second.Key()) {
		t.Fatalf("Session picker exposed stable IDs: %q", out.String())
	}
}

func TestCloseNotesModalClearsPickerState(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.workspacePaneMode, s.workspacePaneStep = true, "notes"
	s.notesPickerMode = "session"
	s.notesPickerChoices = []string{"one"}
	s.notesPickerIndex = 1
	s.closeNotesModal()
	if s.notesPickerMode != "" || s.notesPickerChoices != nil || s.notesPickerIndex != 0 {
		t.Fatalf("closing Notes retained picker state: mode=%q choices=%v index=%d", s.notesPickerMode, s.notesPickerChoices, s.notesPickerIndex)
	}
}

func TestCloseNotesModalFallsBackWhenOriginSessionDisappears(t *testing.T) {
	s, projectID, origin, _ := workspacePaneTestState(t)
	originKey := sessionKey(origin)
	s.workspacePaneMode = true
	s.workspacePaneStep = "notes"
	s.notesPreviousProjectID = projectID
	s.notesPreviousPaneID = "removed-origin"
	s.notesPreviousFocused = true
	s.notesPreviousAttachKey = originKey
	s.notesPreviousAttachValid = true
	s.activeAttachKey = originKey
	s.notesFocusRestorePending = true
	s.notesPendingInputKey = originKey
	s.notesPendingInput = []byte("stale")
	s.sessions = s.sessions[1:]

	s.closeNotesModal()

	if s.activeAttachKey != "" || s.focused || s.notesFocusRestorePending || s.notesFocusRestoreReselect {
		t.Fatalf("stale origin retained focus state: active=%q focused=%v pending=%v reselect=%v", s.activeAttachKey, s.focused, s.notesFocusRestorePending, s.notesFocusRestoreReselect)
	}
	if s.notesPendingInputKey != "" || len(s.notesPendingInput) != 0 {
		t.Fatalf("stale origin retained queued input: key=%q input=%q", s.notesPendingInputKey, s.notesPendingInput)
	}
}

func TestNotesSessionPickerResolvesSelectedIdentity(t *testing.T) {
	s, projectID, a, b := workspacePaneTestState(t)
	root := t.TempDir()
	s.cfgPath = filepath.Join(root, "config.yaml")
	first, _ := ducklord.IdentityFromSession(a)
	second, _ := ducklord.IdentityFromSession(b)
	if _, err := s.activity().ProjectLayout.Place(projectID, second, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	firstBook, err := ducklord.SessionNotesIdentity(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBook, err := ducklord.SessionNotesIdentity(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := ducklord.SaveScopedNotes(root, ducklord.NotesSession, firstBook, "## First\nfirst body\n"); err != nil {
		t.Fatal(err)
	}
	if err := ducklord.SaveScopedNotes(root, ducklord.NotesSession, secondBook, "## Second\nsecond body\n"); err != nil {
		t.Fatal(err)
	}
	s.notesProjectID = projectID
	s.notesScope = ducklord.NotesSession
	s.notesSessionIdentity = first
	s.notesSessionID = first.SessionID
	choices := []string{first.Key(), second.Key()}
	if !s.openNotesPicker("session", choices) || !s.handleNotesPicker([]byte("\x1b[B")) || !s.handleNotesPicker([]byte("\r")) {
		t.Fatal("session picker did not accept second choice")
	}
	if s.notesSessionIdentity != second || len(s.notesEntries) != 1 || s.notesEntries[0].Body != "second body" {
		t.Fatalf("selected session was not loaded: identity=%+v entries=%+v err=%q", s.notesSessionIdentity, s.notesEntries, s.outputErr)
	}
}

func TestNotesHelpHighlightsNotebookActions(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	s.workspacePaneMode = true
	s.workspacePaneStep = "notes"
	s.notesProjectID = projectID
	s.helpMode = true
	if nav.CurrentProjectID() != projectID {
		t.Fatalf("Notes project not selected: project=%q want=%q", nav.CurrentProjectID(), projectID)
	}
	// Help documents Notes as searchable guidance; it does not dispatch notebook
	// shortcuts while the pinned help overlay owns input.
	s.workspacePaneMode = false
	for _, entry := range helpCatalog(false) {
		if entry.label == "Browse scoped Notes" && (!strings.Contains(entry.detail, "g/p/s selects scope directly") || !strings.Contains(entry.detail, "Left/Right or h/l changes scope")) {
			t.Fatalf("Notes scope guidance missing or inaccurate: %q", entry.detail)
		}
		if entry.label == "Copy selected note content" && !strings.Contains(entry.detail, "system clipboard") {
			t.Fatalf("Notes copy guidance missing: %q", entry.detail)
		}
	}
	s.helpSearchQuery = "Browse scoped Notes"
	var out strings.Builder
	s.renderHelpModal(&out, 100, 300)
	rendered := out.String()
	if !strings.Contains(rendered, "Browse scoped Notes") {
		t.Fatalf("searchable Notes guidance missing: %q", rendered)
	}
	for _, entry := range helpCatalog(false) {
		if entry.label == "Add note" && entry.action != "" {
			t.Fatal("Notes informational guidance must not masquerade as a directly dispatchable key")
		}
	}
}

func TestSplitEditorArgsSupportsQuotesWithoutShell(t *testing.T) {
	args, err := splitEditorArgs(`"/tmp/my editor" --wait 'draft file.md'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/tmp/my editor", "--wait", "draft file.md"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%q want=%q", args, want)
	}
	if _, err := splitEditorArgs(`vim "unterminated`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestWorkspaceNewShellHostEnterDoesNotRestartDiscovery(t *testing.T) {
	state, projectID, _, _ := workspacePaneTestState(t)
	state.runner = fakeRunner{}
	state.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceVertical}
	state.newSessionMode = true
	state.newSessionKind = "shell"
	state.newSessionClient = "host"
	state.newSessionStep = "host"
	state.newSessionLine = "host"
	state.newSessionDiscovering = true
	state.newSessionRequestID = 7
	state.newSessionCancel = func() { t.Fatal("duplicate Enter canceled active discovery") }
	done := make(chan createDiscoveryEvent, 1)
	if _, _, _, ready, err := state.submitCreateStep(context.Background(), done); err != nil || ready {
		t.Fatalf("duplicate host Enter changed wizard state: ready=%v err=%v", ready, err)
	}
	if !state.newSessionDiscovering || state.newSessionRequestID != 7 || state.newSessionStep != "host" {
		t.Fatalf("active discovery was restarted or advanced: discovering=%v request=%d step=%q", state.newSessionDiscovering, state.newSessionRequestID, state.newSessionStep)
	}
}

func TestNotesLongListKeepsFootersVisibleAt80x24(t *testing.T) {
	s := &tuiState{}
	for i := 0; i < 24; i++ {
		s.notesEntries = append(s.notesEntries, ducklord.NoteEntry{Title: fmt.Sprintf("note-%02d", i), Body: "body"})
	}
	s.workspacePaneIndex = len(s.notesEntries) - 1
	var out strings.Builder
	s.renderNotesModal(&out, 80, 24)
	screen := renderedModalScreen([]byte(out.String()), 80, 24)
	for _, want := range []string{
		"› note-23",
		"Enter copy content · ↑/↓ j/k select · ←/→ h/l scope",
		"g/p/s scope · / search · a add · e edit · E notebook · Esc close",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("80x24 Notes render missing %q in %q", want, screen)
		}
	}
}

func TestNotesFootersDescribeHandledScopeSearchAndFormKeys(t *testing.T) {
	s := &tuiState{}
	var out strings.Builder
	s.renderNotesModal(&out, 80, 24)
	screen := renderedModalScreen([]byte(out.String()), 80, 24)
	for _, want := range []string{"Enter copy content · ↑/↓ j/k select · ←/→ h/l scope", "g/p/s scope · / search · a add · e edit · E notebook · Esc close"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("notes footer missing %q in %q", want, screen)
		}
	}
	s.notesFormActive = true
	out.Reset()
	s.renderNotesModal(&out, 80, 24)
	screen = renderedModalScreen([]byte(out.String()), 80, 24)
	if !strings.Contains(screen, "Enter next/save · Tab field · ←/→ cursor") {
		t.Fatalf("form footer missing Enter behavior: %q", screen)
	}
}
