package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func testHostSkillsState() *tuiState {
	return &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{
		Name: "alpha", Host: "alpha",
		SkillTargets: []ducklord.SkillInstallTarget{{ID: "codex", Path: "/tmp/skills"}, {ID: "claude", Path: "/tmp/claude"}},
	}}}, hostMenuMode: true, hostMenuStep: "actions", hostMenuTarget: "alpha", hostMenuIndex: 4}
}

func TestHostSkillsRouteOpensDirectDashboard(t *testing.T) {
	s := testHostSkillsState()
	s.handleHostMenuInput([]byte("\r"))
	if s.hostMenuStep != "skills-dashboard" {
		t.Fatalf("open step=%q", s.hostMenuStep)
	}
	s.handleHostSkillsInput([]byte("\t"))
	if s.hostSkillsPane != 1 || s.hostSkillsTargetRemoteIndex != 0 {
		t.Fatalf("pane=%d remote=%d", s.hostSkillsPane, s.hostSkillsTargetRemoteIndex)
	}
	s.handleHostSkillsInput([]byte("\x1b"))
	if s.hostMenuStep != "actions" || s.hostMenuIndex != 4 {
		t.Fatalf("dashboard Esc step=%q index=%d", s.hostMenuStep, s.hostMenuIndex)
	}
	s.handleHostMenuInput([]byte("\x1b"))
	if s.hostMenuStep != "host-list" {
		t.Fatalf("action Esc step=%q", s.hostMenuStep)
	}
	s.handleHostMenuInput([]byte("\x1b"))
	if s.hostMenuMode {
		t.Fatal("host list Esc did not close route")
	}
}

func TestHostSkillsDeleteConfirmationShowsRemoteContext(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep, s.hostSkillsPendingID = "skills-delete-confirm", "remote-skill"
	var out bytes.Buffer
	s.renderHostSkillsModal(&out, 110, 28)
	text := out.String()
	for _, want := range []string{"Host: alpha", "Agent: codex", "Target path: /tmp/skills", "Skill ID: remote-skill", "Enter confirm", "Esc cancel"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
}

func TestBeginHostSkillsActivatesAndListsFirstTarget(t *testing.T) {
	s := testHostSkillsState()
	s.hostSkillsDone = make(chan hostSkillsEvent, 1)
	s.beginHostSkills()
	if s.hostMenuStep != "skills-dashboard" || s.hostSkillsTargetIndex != 0 || !s.hostSkillsTargetExpanded["codex"] {
		t.Fatalf("step=%q target=%d expanded=%v", s.hostMenuStep, s.hostSkillsTargetIndex, s.hostSkillsTargetExpanded)
	}
	if s.hostSkillsListRequestID == 0 || s.hostSkillsBusy {
		t.Fatalf("list request=%d busy=%v", s.hostSkillsListRequestID, s.hostSkillsBusy)
	}
	s.cleanupHostSkills()
}

func TestHostSkillsManagementIsScopedToAgent(t *testing.T) {
	s := testHostSkillsState()
	repo := t.TempDir()
	s.hostSkillsRepository = repo
	if err := os.Mkdir(filepath.Join(repo, "ducklord-quack"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ducklord-quack", "SKILL.md"), []byte("# quack"), 0600); err != nil {
		t.Fatal(err)
	}
	s.hostMenuStep, s.hostSkillsTargetIndex, s.hostSkillsPane = "skills-dashboard", 0, 0
	s.handleHostSkillsInput([]byte("p"))
	if got := s.cfg.Clients[0].SkillTargets[0].Skills; len(got) != 1 || got[0].ID != "ducklord-quack" || got[0].Management != "push" {
		t.Fatalf("codex state=%+v", got)
	}
	s.hostSkillsTargetIndex = 1
	if got := s.hostSkillManagement("ducklord-quack"); got != "none" {
		t.Fatalf("other agent inherited state=%q", got)
	}
	s.handleHostSkillsInput([]byte("p"))
	s.handleHostSkillsInput([]byte("n"))
	if got := s.hostSkillManagement("ducklord-quack"); got != "none" {
		t.Fatalf("none state=%q", got)
	}
	s.hostSkillsTargetIndex = 0
	if got := s.hostSkillManagement("ducklord-quack"); got != "push" {
		t.Fatalf("first agent state=%q", got)
	}
	var out bytes.Buffer
	s.renderHostSkillsModal(&out, 110, 28)
	if !strings.Contains(out.String(), "[push] ducklord-quack") || !strings.Contains(out.String(), "Ducklord Managed Repository") {
		t.Fatalf("status rendering=%q", out.String())
	}
}

func TestHostSkillsExplicitNoneOverridesLegacySelectedSkill(t *testing.T) {
	s := testHostSkillsState()
	s.cfg.Clients[0].SelectedSkills = []string{"ducklord-quack"}
	s.hostSkillsTargetIndex = 0
	if got := s.hostSkillManagement("ducklord-quack"); got != "push" {
		t.Fatalf("legacy state=%q", got)
	}
	if err := s.setHostSkillManagement("ducklord-quack", "none"); err != nil {
		t.Fatal(err)
	}
	if got := s.hostSkillManagement("ducklord-quack"); got != "none" {
		t.Fatalf("explicit none state=%q", got)
	}
	if got := s.cfg.Clients[0].SkillTargets[0].Skills; len(got) != 1 || got[0].Management != "none" {
		t.Fatalf("stored state=%+v", got)
	}
}

func TestHostSkillsRemoteListEventKeepsSkillsInputOwner(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep, s.hostSkillsBusy, s.hostSkillsPane, s.hostSkillsTargetIndex, s.hostSkillsTargetRemoteIndex = "skills-dashboard", false, 1, 1, 0
	s.applyHostSkillsEvent(hostSkillsEvent{action: "list", targetID: "claude", names: []string{"client-a-meow"}})
	if s.hostMenuStep != "skills-dashboard" || s.hostSkillsBusy || s.hostSkillsPane != 1 || s.hostSkillsTargetIndex != 1 {
		t.Fatalf("step=%q busy=%v", s.hostMenuStep, s.hostSkillsBusy)
	}
	if got := s.remoteSkillsForTarget(s.cfg.Clients[0].SkillTargets[1]); len(got) != 1 || got[0] != "client-a-meow" {
		t.Fatalf("remote names=%v", got)
	}
}

func TestHostSkillsStaleListEventDoesNotChangeSelectedStatus(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep, s.hostSkillsPane, s.hostSkillsTargetIndex = "skills-dashboard", 1, 1
	s.hostSkillsBusy = true
	s.hostSkillsErr = "current status"
	s.hostSkillsRemote = []string{"current-remote"}
	s.applyHostSkillsEvent(hostSkillsEvent{action: "list", targetID: "codex", names: []string{"stale"}})
	if s.hostSkillsErr != "current status" || s.hostMenuStep != "skills-dashboard" || !s.hostSkillsBusy {
		t.Fatalf("stale list changed visible state: err=%q step=%q busy=%v", s.hostSkillsErr, s.hostMenuStep, s.hostSkillsBusy)
	}
	if len(s.hostSkillsRemote) != 1 || s.hostSkillsRemote[0] != "current-remote" {
		t.Fatalf("stale list changed visible remote data: %v", s.hostSkillsRemote)
	}
	if got := s.remoteSkillsForTarget(s.cfg.Clients[0].SkillTargets[0]); len(got) != 1 || got[0] != "stale" {
		t.Fatalf("stale list was not cached for its target: %v", got)
	}
}

func TestHostSkillsStaleListErrorDoesNotChangeSelectedStatus(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep, s.hostSkillsPane, s.hostSkillsTargetIndex = "skills-dashboard", 1, 1
	s.hostSkillsBusy = true
	s.hostSkillsErr = "current status"
	s.hostSkillsRemote = []string{"current-remote"}
	s.applyHostSkillsEvent(hostSkillsEvent{action: "list", targetID: "codex", err: errors.New("stale failure")})
	if s.hostSkillsErr != "current status" || s.hostMenuStep != "skills-dashboard" || !s.hostSkillsBusy {
		t.Fatalf("stale list error changed visible state: err=%q step=%q busy=%v", s.hostSkillsErr, s.hostMenuStep, s.hostSkillsBusy)
	}
	if got := s.hostSkillsRemoteByTarget["codex"]; got != nil {
		t.Fatalf("stale list error populated cache: %v", got)
	}
}

func TestHostSkillsEventFromPreviousHostCannotChangeSameTargetCache(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep, s.hostSkillsPane, s.hostSkillsTargetIndex = "skills-dashboard", 1, 0
	s.hostMenuTarget = "beta"
	s.hostSkillsBusy = true
	s.hostSkillsErr = "current status"
	s.hostSkillsRemote = []string{"current-remote"}
	s.hostSkillsRemoteByTarget = map[string][]string{"codex": {"current-cache"}}
	s.applyHostSkillsEvent(hostSkillsEvent{action: "list", host: "alpha", targetID: "codex", names: []string{"stale"}})
	if !s.hostSkillsBusy || s.hostSkillsErr != "current status" {
		t.Fatalf("previous-host event changed status: busy=%v err=%q", s.hostSkillsBusy, s.hostSkillsErr)
	}
	if got := s.hostSkillsRemoteByTarget["codex"]; len(got) != 1 || got[0] != "current-cache" {
		t.Fatalf("previous-host event changed same-target cache: %v", got)
	}
	if len(s.hostSkillsRemote) != 1 || s.hostSkillsRemote[0] != "current-remote" {
		t.Fatalf("previous-host event changed visible remote data: %v", s.hostSkillsRemote)
	}
}

func TestHostSkillsIdentitylessListErrorDoesNotChangeSelectedStatus(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep, s.hostSkillsPane, s.hostSkillsTargetIndex = "skills-dashboard", 1, 1
	s.hostSkillsBusy = true
	s.hostSkillsErr = "current status"
	s.hostSkillsRemote = []string{"current-remote"}
	s.applyHostSkillsEvent(hostSkillsEvent{action: "list", err: errors.New("unscoped failure")})
	if s.hostSkillsErr != "current status" || s.hostMenuStep != "skills-dashboard" || !s.hostSkillsBusy {
		t.Fatalf("identity-less list error changed visible state: err=%q step=%q busy=%v", s.hostSkillsErr, s.hostMenuStep, s.hostSkillsBusy)
	}
	if len(s.hostSkillsRemote) != 1 || s.hostSkillsRemote[0] != "current-remote" {
		t.Fatalf("identity-less list error changed visible remote data: %v", s.hostSkillsRemote)
	}
}

func TestHostSkillsDeleteEventUsesCapturedTargetAndSkill(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep, s.hostSkillsPane, s.hostSkillsTargetIndex = "skills-busy", 1, 1
	s.hostSkillsPendingID = "other-current-selection"
	s.hostSkillsRemoteByTarget = map[string][]string{
		"claude": {"captured-skill", "other-current-selection"},
	}
	s.applyHostSkillsEvent(hostSkillsEvent{action: "delete", targetID: "claude", skillID: "captured-skill"})
	if s.hostSkillsErr != "Deleted captured-skill" || s.hostMenuStep != "skills-dashboard" {
		t.Fatalf("delete status=%q step=%q", s.hostSkillsErr, s.hostMenuStep)
	}
	if got := s.hostSkillsRemoteByTarget["claude"]; len(got) != 1 || got[0] != "other-current-selection" {
		t.Fatalf("captured target cache=%v", got)
	}
	if s.hostSkillsPendingID != "other-current-selection" {
		t.Fatalf("pending skill changed: %q", s.hostSkillsPendingID)
	}
}

func TestHostSkillsDeleteEventDoesNotUseCurrentTargetCache(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep, s.hostSkillsPane, s.hostSkillsTargetIndex = "skills-busy", 1, 1
	s.hostSkillsRemoteByTarget = map[string][]string{
		"codex":  {"current-skill"},
		"claude": {"captured-skill", "keep-skill"},
	}
	s.applyHostSkillsEvent(hostSkillsEvent{action: "delete", targetID: "claude", skillID: "captured-skill"})
	if got := s.hostSkillsRemoteByTarget["codex"]; len(got) != 1 || got[0] != "current-skill" {
		t.Fatalf("current target cache changed: %v", got)
	}
	if got := s.hostSkillsRemoteByTarget["claude"]; len(got) != 1 || got[0] != "keep-skill" {
		t.Fatalf("captured target cache=%v", got)
	}
}

func TestHostSkillsDeployResultRefreshesSelectedTarget(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep = "skills-result"
	s.hostSkillsErr = "Deployed 1 selected skill(s)"
	s.hostSkillsDone = make(chan hostSkillsEvent, 1)
	s.hostSkillsPane = 1
	s.hostSkillsTargetIndex = 1
	s.hostSkillsTargetRemoteIndex = 2
	before := s.hostSkillsListRequestID

	s.handleHostSkillsInput([]byte("\r"))
	if s.hostMenuStep != "skills-dashboard" {
		t.Fatalf("step=%q", s.hostMenuStep)
	}
	if s.hostSkillsListRequestID != before+1 || s.hostSkillsBusy {
		t.Fatalf("refresh request=%d before=%d busy=%v", s.hostSkillsListRequestID, before, s.hostSkillsBusy)
	}
	if s.hostSkillsPane != 1 || s.hostSkillsTargetIndex != 1 || s.hostSkillsTargetRemoteIndex != 2 {
		t.Fatalf("selection changed pane=%d target=%d remote=%d", s.hostSkillsPane, s.hostSkillsTargetIndex, s.hostSkillsTargetRemoteIndex)
	}
	s.cleanupHostSkills()
}

func TestHostSkillsRepositoryAndTargetStateStaySeparate(t *testing.T) {
	s := testHostSkillsState()
	s.hostSkillsRepository = t.TempDir()
	if err := os.Mkdir(filepath.Join(s.hostSkillsRepository, "local-only"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.hostSkillsRepository, "local-only", "SKILL.md"), []byte("# local"), 0600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Clients[0].SkillTargets[1].Skills = []ducklord.ManagedHostSkill{{ID: "saved-remote", Management: "none"}}
	s.hostSkillsRemoteByTarget = map[string][]string{"claude": {"listed-remote"}}
	if got := s.hostSkillNames(); len(got) != 1 || got[0] != "local-only" {
		t.Fatalf("local names=%v", got)
	}
	got := s.remoteSkillsForTarget(s.cfg.Clients[0].SkillTargets[1])
	if len(got) != 2 || got[0] != "listed-remote" || got[1] != "saved-remote" {
		t.Fatalf("target names=%v", got)
	}
}

func TestHostSkillsRenameRequiresExplicitConfirmation(t *testing.T) {
	s := testHostSkillsState()
	s.hostSkillsRepository = t.TempDir()
	if err := os.Mkdir(filepath.Join(s.hostSkillsRepository, "old-name"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.hostSkillsRepository, "old-name", "SKILL.md"), []byte("# old"), 0600); err != nil {
		t.Fatal(err)
	}
	s.hostMenuStep, s.hostSkillsPane, s.hostSkillsIndex = "skills-dashboard", 0, 0
	s.handleHostSkillsInput([]byte("m"))
	if s.hostMenuStep != "skills-rename" {
		t.Fatalf("draft step=%q", s.hostMenuStep)
	}
	s.hostSkillsRenameDraft = "new-name"
	s.handleHostSkillsInput([]byte("\r"))
	if s.hostMenuStep != "skills-rename-confirm" {
		t.Fatalf("confirm step=%q", s.hostMenuStep)
	}
	if _, err := os.Stat(filepath.Join(s.hostSkillsRepository, "old-name")); err != nil {
		t.Fatalf("rename happened before confirmation: %v", err)
	}
	s.handleHostSkillsInput([]byte("y"))
	if _, err := os.Stat(filepath.Join(s.hostSkillsRepository, "new-name")); err != nil {
		t.Fatalf("confirmed rename missing: %v", err)
	}
}

func TestHostSkillsRenameSaveFailureRollsBackRepositoryAndConfig(t *testing.T) {
	s := testHostSkillsState()
	s.hostSkillsRepository = t.TempDir()
	if err := os.Mkdir(filepath.Join(s.hostSkillsRepository, "old-name"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.hostSkillsRepository, "old-name", "SKILL.md"), []byte("# old"), 0600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Clients[0].SelectedSkills = []string{"old-name"}
	s.cfgPath = t.TempDir() // SaveConfig cannot write over an existing directory.
	s.hostSkillsRenameOld, s.hostSkillsRenameDraft = "old-name", "new-name"

	s.applyHostSkillRename()

	if s.hostSkillsErr == "" {
		t.Fatal("expected save failure")
	}
	if _, err := os.Stat(filepath.Join(s.hostSkillsRepository, "old-name", "SKILL.md")); err != nil {
		t.Fatalf("old skill was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.hostSkillsRepository, "new-name")); !os.IsNotExist(err) {
		t.Fatalf("new skill remains after rollback: %v", err)
	}
	if got := s.cfg.Clients[0].SelectedSkills[0]; got != "old-name" {
		t.Fatalf("config was not restored: %q", got)
	}
}

func TestHostSkillsCtrlCCleansAndClosesWholeRoute(t *testing.T) {
	s := testHostSkillsState()
	s.hostMenuStep, s.hostSkillsBusy = "skills-preview", true
	called := false
	s.hostSkillsCancel = func() { called = true }
	s.closeHostMenuWithCtrlC()
	if s.hostMenuMode || s.hostSkillsBusy || !called {
		t.Fatalf("mode=%v busy=%v cancelled=%v", s.hostMenuMode, s.hostSkillsBusy, called)
	}
}
