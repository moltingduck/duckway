package main

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestImportedProjectNameIsDeterministic(t *testing.T) {
	existing := map[string]bool{"Work": true, "Work (imported)": true}
	if got := importedProjectName("Work", existing); got != "Work (imported 2)" {
		t.Fatalf("collision name = %q", got)
	}
	if got := importedProjectName("New", existing); got != "New" {
		t.Fatalf("unique name = %q", got)
	}
}

func TestProjectImportMutatesOnlyAfterConfirmation(t *testing.T) {
	root := t.TempDir()
	state := &tuiState{cfgPath: filepath.Join(root, "ducklord.json"), activityStore: ducklord.ActivityStateStore{Path: filepath.Join(root, "activity.json")}, activityState: ducklord.NewActivityState()}
	state.projectTransferAction = "import"
	state.projectTransfer = ducklord.ProjectTransfer{Project: ducklord.LocalProject{ID: uuid.NewString(), Name: "Imported"}}
	state.projectTransferPreview = ducklord.ProjectImportPreview{Project: state.projectTransfer.Project}
	state.workspacePaneMode, state.workspacePaneStep = true, "project-transfer-preview"
	if len(state.activity().ProjectLayout.Projects) != 1 {
		t.Fatal("unexpected initial project state")
	}
	state.handleProjectTransferInput([]byte("x"))
	if len(state.activity().ProjectLayout.Projects) != 1 {
		t.Fatal("non-confirmation mutated project state")
	}
	state.handleProjectTransferInput([]byte("\r"))
	if len(state.activity().ProjectLayout.Projects) != 2 || state.activity().ProjectLayout.Projects[1].Name != "Imported" {
		t.Fatal("confirmation did not persist imported project")
	}
}

func TestProjectImportPreviewCountsExistingSessionNotes(t *testing.T) {
	root := t.TempDir()
	state := &tuiState{cfgPath: filepath.Join(root, "ducklord.json")}
	existing := ducklord.SessionIdentity{InstanceID: "00000000-0000-0000-0000-000000000001", SessionID: "ABC123"}
	fresh := ducklord.SessionIdentity{InstanceID: "00000000-0000-0000-0000-000000000003", SessionID: "DEF456"}
	key, err := ducklord.SessionNotesIdentity(existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := ducklord.SaveScopedNotes(root, ducklord.NotesSession, key, "## Existing\nkeep"); err != nil {
		t.Fatal(err)
	}
	state.projectTransfer = ducklord.ProjectTransfer{SessionNotes: []ducklord.ProjectSessionNotes{
		{Session: existing, Entries: []ducklord.NoteEntry{{Title: "Imported"}}},
		{Session: fresh, Entries: []ducklord.NoteEntry{{Title: "New"}}},
	}}
	if got := state.projectImportSessionNoteConflicts(); got != 1 {
		t.Fatalf("session note conflicts = %d, want 1", got)
	}
}
