package main

import (
	"context"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestQuickShellPrefixAndPlacement(t *testing.T) {
	for suffix, want := range map[string]ducklord.PanePlacement{"-": ducklord.PlaceHorizontal, "\\": ducklord.PlaceVertical, "t": ducklord.PlaceNewTab} {
		s := &tuiState{workspacePreview: true, focused: true, cfg: &ducklord.Config{}}
		s.panePrefixPending, s.panePrefixSuffix = true, suffix
		s.panePrefixDeadline = time.Now().Add(time.Second)
		if _, command := s.handlePanePrefix([]byte(suffix)); command != "quick-shell:"+suffix {
			t.Fatalf("command=%q", command)
		}
		session := ducklord.RemoteSession{Client: "host", Cwd: "/work/app", SessionID: "s", InstanceID: "i"}
		s.sessions, s.activeAttachKey = []ducklord.RemoteSession{session}, "host/i/s"
		s.cfg.Clients = []ducklord.Client{{Name: "host", Host: "host"}}
		s.runner = fakeRunner{agents: []ducklord.RemoteAgent{{Type: "shell", Command: []string{"/bin/sh"}}}}
		s.beginQuickShell(context.Background(), make(chan createDiscoveryEvent, 1), suffix)
		if s.workspaceNewSessionIntent == nil || s.workspaceNewSessionIntent.placement != want || s.workspacePaneMode {
			t.Fatalf("placement=%v modal=%v", s.workspaceNewSessionIntent, s.workspacePaneMode)
		}
		if !s.focused || s.activeAttachKey != "host/i/s" {
			t.Fatalf("quick route changed focus/attach: focused=%v attach=%q", s.focused, s.activeAttachKey)
		}
		if s.newSessionKind != model.KindShell || s.newSessionClient != "host" || s.newSessionCWD != session.Cwd {
			t.Fatalf("stale origin")
		}
	}
}

func TestQuickShellPrefixAtomicRepeatedSuffix(t *testing.T) {
	for _, input := range []string{"\x02--", "\x02tt"} {
		s := &tuiState{workspacePreview: true, focused: true, cfg: &ducklord.Config{}, sessions: []ducklord.RemoteSession{{Client: "host", Cwd: "/work/app"}}}
		consumed, command := s.handlePanePrefix([]byte(input))
		want := "quick-shell:" + input[1:2]
		if !consumed || command != want {
			t.Fatalf("atomic %q: consumed=%v command=%q want=%q", input, consumed, command, want)
		}
		if s.panePrefixPending || s.panePrefixSuffix != "" || !s.panePrefixDeadline.IsZero() {
			t.Fatalf("atomic %q retained prefix state: pending=%v suffix=%q deadline=%v", input, s.panePrefixPending, s.panePrefixSuffix, s.panePrefixDeadline)
		}
		if consumed, command := s.handlePanePrefix([]byte("x")); consumed || command != "" {
			t.Fatalf("atomic %q consumed following ordinary key: consumed=%v command=%q", input, consumed, command)
		}
	}
}

func TestQuickShellPrefixRepeatedSuffixAfterFocusHandoff(t *testing.T) {
	for _, suffix := range []string{"-", "t"} {
		s := &tuiState{workspacePreview: true, focused: true, cfg: &ducklord.Config{}, sessions: []ducklord.RemoteSession{{Client: "host", Cwd: "/work/app"}}}
		if consumed, command := s.handlePanePrefix([]byte("\x02")); !consumed || command != "" {
			t.Fatalf("%s prefix capture: consumed=%v command=%q", suffix, consumed, command)
		}
		s.quickShellOrigin = &workspacePaneIntent{projectID: "project", targetID: "pane"}
		s.focused = false
		if consumed, command := s.handlePanePrefix([]byte(suffix)); !consumed || command != "" {
			t.Fatalf("%s first suffix: consumed=%v command=%q", suffix, consumed, command)
		}
		if consumed, command := s.handlePanePrefix([]byte(suffix)); !consumed || command != "quick-shell:"+suffix {
			t.Fatalf("%s repeated suffix: consumed=%v command=%q", suffix, consumed, command)
		}
	}
}

func TestQuickShellPrefixDelayedRepeatInsideWindow(t *testing.T) {
	s := &tuiState{workspacePreview: true, focused: true, cfg: &ducklord.Config{}, sessions: []ducklord.RemoteSession{{Client: "host", Cwd: "/work/app"}}}
	if consumed, command := s.handlePanePrefix([]byte("\x02")); !consumed || command != "" {
		t.Fatalf("prefix capture: consumed=%v command=%q", consumed, command)
	}
	if consumed, command := s.handlePanePrefix([]byte("t")); !consumed || command != "" {
		t.Fatalf("first suffix: consumed=%v command=%q", consumed, command)
	}
	time.Sleep(300 * time.Millisecond)
	if consumed, command := s.handlePanePrefix([]byte("t")); !consumed || command != "quick-shell:t" {
		t.Fatalf("delayed repeat: consumed=%v command=%q", consumed, command)
	}
}

func TestQuickShellPrefixExpiryDispatchesOrdinarySuffix(t *testing.T) {
	s := &tuiState{workspacePreview: true, focused: true, cfg: &ducklord.Config{}, sessions: []ducklord.RemoteSession{{Client: "host", Cwd: "/work/app"}}}
	s.panePrefixPending, s.panePrefixSuffix = true, "t"
	s.panePrefixDeadline = time.Now().Add(-time.Millisecond)
	if consumed, command := s.handlePanePrefix([]byte("t")); !consumed || command != "t" {
		t.Fatalf("expired repeat: consumed=%v command=%q", consumed, command)
	}
	if s.panePrefixPending || s.panePrefixSuffix != "" {
		t.Fatalf("expired repeat retained prefix state: pending=%v suffix=%q", s.panePrefixPending, s.panePrefixSuffix)
	}
}

func TestQuickShellMismatchReplaysInput(t *testing.T) {
	s := &tuiState{workspacePreview: true, focused: true, cfg: &ducklord.Config{}}
	s.panePrefixPending, s.panePrefixSuffix = true, "-"
	s.panePrefixDeadline = time.Now().Add(time.Second)
	if _, command := s.handlePanePrefix([]byte("t")); command != "-" || string(s.panePrefixReplay) != "t" {
		t.Fatalf("command=%q replay=%q", command, s.panePrefixReplay)
	}
	if got := string(s.takePanePrefixReplay()); got != "t" || len(s.panePrefixReplay) != 0 {
		t.Fatalf("replay was not consumed in order: got=%q remaining=%q", got, s.panePrefixReplay)
	}
}

func TestQuickShellDiscoveryFailureClearsCreationIntent(t *testing.T) {
	s := &tuiState{
		newSessionMode: true, newSessionDiscovering: true, newSessionRequestID: 7,
		workspaceNewSessionIntent: &workspacePaneIntent{placement: ducklord.PlaceNewTab},
	}
	s.hostSync = map[string]ducklord.SessionUpdate{"host": {Client: "host", Generation: 1, InstanceID: "instance", State: "live"}}
	s.hostConnectionEpoch = map[string]uint64{"host": 1}
	s.newSessionClient = "host"
	_, _, _, ready := s.applyCreateDiscovery(createDiscoveryEvent{
		id: 7, kind: "quick-shell", client: "host", generation: 1, instance: "instance",
		err: context.Canceled,
	})
	if ready || s.workspaceNewSessionIntent != nil || s.newSessionMode || s.newSessionDiscovering {
		t.Fatalf("quick discovery failure leaked state: ready=%v intent=%v mode=%v discovering=%v", ready, s.workspaceNewSessionIntent, s.newSessionMode, s.newSessionDiscovering)
	}
	if s.outputErr != context.Canceled.Error() || s.newSessionErr != context.Canceled.Error() {
		t.Fatalf("error was not preserved: output=%q modal=%q", s.outputErr, s.newSessionErr)
	}
}

func TestQuickShellStartFailureClearsCreationIntent(t *testing.T) {
	s := &tuiState{
		newSessionMode: true, newSessionStarting: true,
		workspaceNewSessionIntent: &workspacePaneIntent{placement: ducklord.PlaceHorizontal},
	}
	s.completeNewSessionStart(context.Background(), "host", "", context.Canceled)
	if s.workspaceNewSessionIntent != nil || s.newSessionMode || s.newSessionStarting {
		t.Fatalf("quick start failure leaked state: intent=%v mode=%v starting=%v", s.workspaceNewSessionIntent, s.newSessionMode, s.newSessionStarting)
	}
	if s.outputErr != context.Canceled.Error() || s.newSessionErr != context.Canceled.Error() {
		t.Fatalf("error was not preserved: output=%q modal=%q", s.outputErr, s.newSessionErr)
	}
}
func TestQuickShellOriginPinsFocusedSecondTab(t *testing.T) {
	activity := ducklord.NewActivityState()
	projectID, err := activity.ProjectLayout.AddProject("A")
	if err != nil {
		t.Fatal(err)
	}
	sessionA := ducklord.RemoteSession{Client: "host-a", InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "01ARZ3", Cwd: "/work/a"}
	sessionB := ducklord.RemoteSession{Client: "host-b", InstanceID: "8df68174-9e13-4dc9-b44d-8532c87f5972", SessionID: "01ARZ4", Cwd: "/work/b"}
	identityA, _ := ducklord.IdentityFromSession(sessionA)
	identityB, _ := ducklord.IdentityFromSession(sessionB)
	if _, err := activity.ProjectLayout.Place(projectID, identityA, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	paneB, err := activity.ProjectLayout.Place(projectID, identityB, ducklord.PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	project := activity.ProjectLayout.Project(projectID)
	tabB := project.Tabs[len(project.Tabs)-1].ID
	nav, err := ducklord.NewWorkspaceState(&activity.ProjectLayout)
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectPane(projectID, paneB); err != nil {
		t.Fatal(err)
	}
	s := &tuiState{workspacePreview: true, focused: true, cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host-a", Host: "host-a"}, {Name: "host-b", Host: "host-b"}}}, activityState: activity, workspaceNav: nav, sessions: []ducklord.RemoteSession{sessionA, sessionB}, runner: fakeRunner{agents: []ducklord.RemoteAgent{{Type: "shell", Command: []string{"/bin/sh"}}}}}
	s.quickShellOrigin = s.captureQuickShellOrigin()
	s.focused = false // focus can be cleared during asynchronous control handoff
	s.beginQuickShell(context.Background(), make(chan createDiscoveryEvent, 1), "\\")
	got := s.workspaceNewSessionIntent
	if got == nil || got.projectID != projectID || got.tabID != tabB || got.targetID != paneB || got.originIdentity != identityB {
		t.Fatalf("intent = %#v, want focused second tab project/tab/pane/session", got)
	}
}

func TestQuickShellPrefixClearsStaleOriginAndBeginRequiresFocus(t *testing.T) {
	origin := &workspacePaneIntent{projectID: "old", targetID: "pane"}
	s := &tuiState{workspacePreview: true, focused: false, cfg: &ducklord.Config{}, quickShellOrigin: origin}
	if consumed, _ := s.handlePanePrefix([]byte{2}); !consumed {
		t.Fatal("unfocused prefix was not consumed")
	}
	if s.quickShellOrigin != nil {
		t.Fatal("unfocused prefix retained stale origin")
	}
	s.quickShellOrigin = origin
	s.beginQuickShell(context.Background(), make(chan createDiscoveryEvent, 1), "-")
	if s.workspaceNewSessionIntent != nil || s.newSessionMode || s.quickShellOrigin != nil {
		t.Fatalf("unfocused begin created shell or retained origin: intent=%#v mode=%v origin=%#v", s.workspaceNewSessionIntent, s.newSessionMode, s.quickShellOrigin)
	}
}

func TestQuickShellPlacementRejectsStaleOrigin(t *testing.T) {
	s, project, session, _ := workspacePaneTestState(t)
	nav, _ := s.workspaceNavigation()
	s.focused = true
	if err := nav.SelectPane(project, s.activity().ProjectLayout.Project(project).Tabs[0].Root.ID); err != nil {
		_ = err
	}
	origin := s.captureQuickShellOrigin()
	if origin == nil {
		t.Fatal("missing origin")
	}
	if _, err := s.activity().ProjectLayout.MovePane(project, origin.targetID, ducklord.PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.placeCreatedWorkspacePane(*origin, session); err == nil {
		t.Fatal("moved origin was placed")
	}
}

func TestQuickShellAsyncTeardownDoesNotRestoreProjectFocus(t *testing.T) {
	s, project, session, _ := workspacePaneTestState(t)
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectPane(project, s.activity().ProjectLayout.Project(project).Tabs[0].Root.ID); err != nil {
		t.Fatal(err)
	}
	s.workspaceProjectFocus = false
	s.workspaceFocusFromProject = true
	s.activeAttachKey = sessionKey(session)
	s.workspaceNewSessionIntent = &workspacePaneIntent{projectID: project, targetID: s.activeAttachKey, placement: ducklord.PlaceNewTab}
	s.clearAttachIdentity()
	if s.workspaceProjectFocus {
		t.Fatal("async quick-shell teardown restored Project focus")
	}
	if s.workspaceFocusFromProject || s.activeAttachKey != "" {
		t.Fatalf("quick-shell teardown leaked focus state: fromProject=%v attach=%q", s.workspaceFocusFromProject, s.activeAttachKey)
	}
}

func TestQuickShellPlacementKeepsCreatedSessionFocus(t *testing.T) {
	s, _, _, created := workspacePaneTestState(t)
	identity, ok := ducklord.IdentityFromSession(created)
	if !ok {
		t.Fatal("created Session has no identity")
	}
	paneID := s.projectPaneID(ducklord.DefaultProjectID, identity)
	if paneID == "" {
		t.Fatal("created Session has no Default Project pane")
	}
	s.workspaceProjectFocus = true
	s.workspaceFocusFromProject = true
	intent := workspacePaneIntent{projectID: ducklord.DefaultProjectID, targetID: paneID, placement: ducklord.PlaceNewTab}
	if err := s.placeCreatedWorkspacePane(intent, created); err != nil {
		t.Fatal(err)
	}
	if s.workspaceProjectFocus || s.workspaceFocusFromProject {
		t.Fatalf("created Session placement left Project focus flags set: project=%v fromProject=%v", s.workspaceProjectFocus, s.workspaceFocusFromProject)
	}
	if !s.workspaceAttachFromProject || !s.workspacePlacementFocusPending {
		t.Fatalf("created Session placement did not request PTY focus: attachFromProject=%v pending=%v", s.workspaceAttachFromProject, s.workspacePlacementFocusPending)
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := s.activity().ProjectLayout.PaneSession(nav.CurrentProjectID(), nav.CurrentPaneID())
	if !ok || selected != identity {
		t.Fatalf("created Session was not selected after placement: project=%q pane=%q selected=%+v", nav.CurrentProjectID(), nav.CurrentPaneID(), selected)
	}
	// Inventory placement happens before the old pooled control is rejected.
	// Its teardown must clear old output focus without erasing the new route.
	wantAttachKey := sessionKey(created)
	if s.activeAttachKey != wantAttachKey {
		t.Fatalf("created Session route missing before stale teardown: got=%q want=%q", s.activeAttachKey, wantAttachKey)
	}
	s.clearAttachIdentity()
	if s.workspaceProjectFocus || s.workspaceFocusFromProject {
		t.Fatalf("late teardown stole created Session focus: project=%v fromProject=%v", s.workspaceProjectFocus, s.workspaceFocusFromProject)
	}
	if s.activeAttachKey != wantAttachKey || !s.workspacePlacementFocusPending {
		t.Fatalf("late teardown erased pending created Session route: attach=%q pending=%v", s.activeAttachKey, s.workspacePlacementFocusPending)
	}
	active, ok := ducklord.IdentityFromSession(s.activePTYSession())
	createdIdentity, _ := ducklord.IdentityFromSession(created)
	if !ok || active != createdIdentity {
		t.Fatalf("synthetic Enter would target the wrong Session after stale teardown: active=%+v want=%+v", active, createdIdentity)
	}
}
