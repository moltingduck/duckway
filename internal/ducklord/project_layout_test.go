package ducklord

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testLayoutIdentity(name string) SessionIdentity {
	return SessionIdentity{InstanceID: uuid.MustParse("11111111-1111-4111-8111-111111111111").String(), SessionID: name}
}

func TestLegacyActivityStateGetsDefaultProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"notifications":{},"organization":{"mode":"custom"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := (ActivityStateStore{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ProjectLayout.Projects) != 1 || state.ProjectLayout.Projects[0].ID != DefaultProjectID {
		t.Fatalf("legacy state lost implicit default project: %+v", state.ProjectLayout)
	}
}

func TestProjectLayoutMembershipAndDetach(t *testing.T) {
	layout := NewProjectLayout()
	a := testLayoutIdentity("ABC123")
	b := testLayoutIdentity("DEF456")
	for _, session := range []SessionIdentity{a, b} {
		if err := layout.Discover(session); err != nil {
			t.Fatal(err)
		}
	}
	if got := layout.ProjectsFor(a); len(got) != 1 || got[0] != DefaultProjectID {
		t.Fatalf("new session should have a Default pane: %v", got)
	}
	first, err := layout.AddProject("Work")
	if err != nil {
		t.Fatal(err)
	}
	second, err := layout.AddProject("Review")
	if err != nil {
		t.Fatal(err)
	}
	pane, err := layout.Place(first, a, PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Place(first, a, PlaceNewTab, ""); err == nil {
		t.Fatal("duplicate pane in same Project accepted")
	}
	if _, err := layout.Place(second, a, PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	if got := layout.ProjectsFor(a); len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("cross-Project membership = %v", got)
	}
	if _, err := layout.Place(first, b, PlaceHorizontal, pane); err != nil {
		t.Fatal(err)
	}
	if err := layout.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := layout.Detach(first, a); err != nil {
		t.Fatal(err)
	}
	if got := layout.ProjectsFor(a); len(got) != 1 || got[0] != second {
		t.Fatalf("detach should preserve other Project: %v", got)
	}
	if err := layout.Detach(second, a); err != nil {
		t.Fatal(err)
	}
	if got := layout.ProjectsFor(a); len(got) != 1 || got[0] != DefaultProjectID {
		t.Fatalf("last explicit detach should return to Default: %v", got)
	}
	if err := layout.Detach(DefaultProjectID, a); err != nil {
		t.Fatal(err)
	}
	if got := layout.ProjectsFor(a); len(got) != 1 || got[0] != DefaultProjectID {
		t.Fatalf("Default detach should retain navigable pane: %v", got)
	}
	layout.Destroy(a)
	if got := layout.ProjectsFor(a); len(got) != 0 {
		t.Fatalf("destroy should remove every view: %v", got)
	}
	if err := layout.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProjectLayoutPersistedInActivityState(t *testing.T) {
	store := ActivityStateStore{Path: filepath.Join(t.TempDir(), "state.json")}
	state := NewActivityState()
	session := testLayoutIdentity("ABC123")
	projectID, err := state.ProjectLayout.AddProject("Same name")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ProjectLayout.AddProject("Same name"); err != nil {
		t.Fatal("duplicate display names should be allowed:", err)
	}
	if _, err := state.ProjectLayout.Place(projectID, session, PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ProjectLayout.ProjectsFor(session); len(got) != 1 || got[0] != projectID {
		t.Fatalf("project layout lost on save/load: %v", got)
	}
	copy := loaded.Clone()
	copy.ProjectLayout.Project(projectID).Name = "Changed"
	if loaded.ProjectLayout.Project(projectID).Name != "Same name" {
		t.Fatal("activity clone aliases project layout")
	}
}

func TestProjectLayoutNavigationAndProjectRemoval(t *testing.T) {
	layout := NewProjectLayout()
	session := testLayoutIdentity("ABC123")
	first, _ := layout.AddProject("One")
	second, _ := layout.AddProject("Two")
	if _, err := layout.Place(first, session, PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Place(second, session, PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	if got := layout.NavigateProject(session, second, first); got != second {
		t.Fatalf("current project should win: %q", got)
	}
	if got := layout.NavigateProject(session, "missing", second); got != second {
		t.Fatalf("last project should win: %q", got)
	}
	if got := layout.NavigateProject(session, "missing", "missing"); got != first {
		t.Fatalf("project order fallback = %q", got)
	}
	if err := layout.RemoveProject(first); err != nil {
		t.Fatal(err)
	}
	if got := layout.ProjectsFor(session); len(got) != 1 || got[0] != second {
		t.Fatalf("removal should leave other project reference: %v", got)
	}
	if err := layout.RemoveProject(second); err != nil {
		t.Fatal(err)
	}
	if got := layout.ProjectsFor(session); len(got) != 1 || got[0] != DefaultProjectID {
		t.Fatalf("last project removal should rehome session: %v", got)
	}
	if err := layout.RemoveProject(DefaultProjectID); err == nil {
		t.Fatal("Default Project removal accepted")
	}
}

func TestProjectLayoutRejectsUnsafeNamesAndMalformedTree(t *testing.T) {
	layout := NewProjectLayout()
	for _, name := range []string{"", " bad", "bad\x1b[2J", strings.Repeat("a", 65)} {
		if _, err := layout.AddProject(name); err == nil {
			t.Fatalf("unsafe project name accepted: %q", name)
		}
	}
	session := testLayoutIdentity("ABC123")
	if _, err := layout.Place(DefaultProjectID, session, PlaceNewTab, ""); err != nil {
		t.Fatal(err)
	}
	layout.Projects[0].Tabs[0].Root.ID = "not-a-uuid"
	if err := layout.Validate(); err == nil {
		t.Fatal("malformed pane ID accepted")
	}
}

func TestProjectLayoutDetachPaneRejectsStaleViewID(t *testing.T) {
	layout := NewProjectLayout()
	projectID, _ := layout.AddProject("Work")
	session := testLayoutIdentity("ABC123")
	firstPane, err := layout.Place(projectID, session, PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := layout.DetachPane(projectID, firstPane); err != nil {
		t.Fatal(err)
	}
	secondPane, err := layout.Place(projectID, session, PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	if firstPane == secondPane {
		t.Fatal("new view reused a stale pane ID")
	}
	if _, err := layout.DetachPane(projectID, firstPane); err == nil {
		t.Fatal("stale pane ID detached a newly placed view")
	}
	if got := layout.ProjectsFor(session); len(got) != 1 || got[0] != projectID {
		t.Fatalf("new view was removed: %v", got)
	}
}

func TestProjectLayoutMovePaneIsAtomicAndKeepsOneProjectReference(t *testing.T) {
	layout := NewProjectLayout()
	projectID, _ := layout.AddProject("Work")
	a, b := testLayoutIdentity("ABC123"), testLayoutIdentity("DEF456")
	first, _ := layout.Place(projectID, a, PlaceNewTab, "")
	second, _ := layout.Place(projectID, b, PlaceVertical, first)
	before := layout.Clone()
	if _, err := layout.MovePane(projectID, second, PlaceVertical, second); err == nil {
		t.Fatal("self split accepted")
	}
	if _, err := layout.MovePane(projectID, second, PlaceVertical, "missing"); err == nil {
		t.Fatal("invalid target accepted")
	}
	if got := layout.ProjectsFor(b); len(got) != 1 || got[0] != projectID || len(layout.Project(projectID).Tabs) != len(before.Project(projectID).Tabs) {
		t.Fatalf("failed move changed membership: %v", got)
	}
	moved, err := layout.MovePane(projectID, second, PlaceNewTab, "")
	if err != nil || moved == second {
		t.Fatalf("move failed or reused pane ID: %s %v", moved, err)
	}
	if got := layout.ProjectsFor(b); len(got) != 1 || got[0] != projectID {
		t.Fatalf("move duplicated or rehomed Session: %v", got)
	}
	if _, err := layout.DetachPane(projectID, second); err == nil {
		t.Fatal("stale source pane ID remained live")
	}
	if err := layout.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptPersistedProjectLayoutIsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	data := `{"version":1,"notifications":{},"organization":{"mode":"custom"},"project_layout":{"projects":[{"id":"default","name":"Default Project","tabs":[{"id":"not-a-uuid","root":{"id":"also-bad","session":{"instance_id":"11111111-1111-4111-8111-111111111111","session_id":"ABC123"}}}]}]}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := (ActivityStateStore{Path: path}).Load()
	if state == nil {
		t.Fatal("corrupt state should recover a usable default state")
	}
	var recovered *CorruptStateRecoveredError
	if !errors.As(err, &recovered) {
		t.Fatalf("wanted preserved corrupt state warning, got %v", err)
	}
	if _, err := os.Stat(recovered.PreservedPath); err != nil {
		t.Fatalf("corrupt state not preserved: %v", err)
	}
}
