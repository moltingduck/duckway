package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestProjectFilesInputSelectionAndConflictPreview(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", active: 0, left: projectFilesPane{endpoint: ducklord.FileEndpoint{Path: dir}, entries: []ducklord.FileEntry{{Name: "a.txt"}}, marked: map[string]bool{}}, right: projectFilesPane{endpoint: ducklord.FileEndpoint{Path: dir}, marked: map[string]bool{}}}
	s.handleProjectFilesInput([]byte(" "))
	if !s.projectFiles.left.marked["a.txt"] {
		t.Fatal("Space should select the current file")
	}
	s.handleProjectFilesInput([]byte("c"))
	if s.projectFiles.step != "preview" || s.projectFiles.conflict != "skip" {
		t.Fatalf("preview = %#v", s.projectFiles)
	}
	s.handleProjectFilesInput([]byte("r"))
	if s.projectFiles.conflict != "rename" {
		t.Fatal("r should select rename")
	}
}

func TestProjectFilesCloseCancelsAndRestoresOrigin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &tuiState{focused: true, activeAttachKey: "session-key", workspaceProjectFocus: false}
	s.projectFiles = projectFilesState{open: true, cancel: cancel, originFocused: true, originAttachKey: "session-key", originProjectFocus: false}
	s.closeProjectFiles()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("close should cancel the request")
	}
	if s.projectFiles.open || !s.focused || s.activeAttachKey != "session-key" {
		t.Fatalf("origin not restored: %#v", s.projectFiles)
	}
}

func TestProjectFilesPathUnicodeClearsSelectionAndCancelsPreviousLoad(t *testing.T) {
	dir := t.TempDir()
	previous, previousCancel := context.WithCancel(context.Background())
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{
		endpoint: ducklord.FileEndpoint{Path: dir}, marked: map[string]bool{"old.txt": true}, selected: 2,
	}, cancels: [2]context.CancelFunc{previousCancel}}
	s.handleProjectFilesInput([]byte("g"))
	s.handleProjectFilesInput([]byte("文"))
	s.handleProjectFilesInput([]byte("\b"))
	if s.projectFiles.left.endpoint.Path != dir {
		t.Fatalf("unicode backspace split the path: %q", s.projectFiles.left.endpoint.Path)
	}
	s.handleProjectFilesInput([]byte("\r"))
	select {
	case <-previous.Done():
	default:
		t.Fatal("new pane load did not cancel the superseded request")
	}
	if s.projectFiles.left.selected != 0 || len(s.projectFiles.left.marked) != 0 {
		t.Fatalf("path change retained selection: %#v", s.projectFiles.left)
	}
}

func TestProjectFilesEndpointPickerResetsAndShowsConfiguredName(t *testing.T) {
	dir := t.TempDir()
	s := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a"}, {Name: "client-b"}}}}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{label: "LOCAL", endpoint: ducklord.FileEndpoint{Path: dir}, marked: map[string]bool{}}, done: make(chan projectFilesEvent, 1)}
	s.projectFiles.endpointIndex = 2
	s.handleProjectFilesInput([]byte("h"))
	if s.projectFiles.endpointIndex != 0 {
		t.Fatalf("picker started at index %d, want Local", s.projectFiles.endpointIndex)
	}
	s.handleProjectFilesInput([]byte("j"))
	s.handleProjectFilesInput([]byte("\r"))
	if got := s.projectFiles.left.label; got != "client-a" {
		t.Fatalf("endpoint label = %q, want configured host name", got)
	}
	if s.projectFiles.left.endpoint.Client == nil || s.projectFiles.left.endpoint.Client.Name != "client-a" {
		t.Fatalf("wrong endpoint: %#v", s.projectFiles.left.endpoint)
	}
	s.closeProjectFiles()
}

func TestProjectFilesRejectsStalePaneEvents(t *testing.T) {
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, generation: 7, panegen: [2]uint64{3, 4}, left: projectFilesPane{entries: []ducklord.FileEntry{{Name: "old"}}}}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 7, panegen: 2, side: 0, entries: []ducklord.FileEntry{{Name: "stale"}}})
	if got := s.projectFiles.left.entries[0].Name; got != "old" {
		t.Fatalf("stale pane result replaced entries: %q", got)
	}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 7, panegen: 3, side: 0, entries: []ducklord.FileEntry{{Name: "fresh"}}})
	if got := s.projectFiles.left.entries[0].Name; got != "fresh" {
		t.Fatalf("current pane result was not applied: %q", got)
	}
}

func TestProjectFilesCtrlCCancelsCopyAndFencesCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "busy", generation: 9, copyCancel: cancel}
	s.handleProjectFilesInput([]byte("\x03"))
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Ctrl+C did not cancel the copy context")
	}
	if s.projectFiles.step != "browse" || s.projectFiles.generation != 10 || s.projectFiles.status != "Cancelled" {
		t.Fatalf("cancel state = %#v", s.projectFiles)
	}
	// A late completion for the cancelled generation must not overwrite status.
	s.applyProjectFilesEvent(projectFilesEvent{generation: 9, copy: true})
	if s.projectFiles.status != "Cancelled" {
		t.Fatalf("stale copy completion replaced cancellation status: %q", s.projectFiles.status)
	}
}

func TestProjectFilesMouseDragUsesDirectoryTarget(t *testing.T) {
	sourceDir, targetDir := t.TempDir(), t.TempDir()
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{
		endpoint: ducklord.FileEndpoint{Path: sourceDir}, entries: []ducklord.FileEntry{{Name: "file.txt"}, {Name: "nested", IsDir: true}}, marked: map[string]bool{},
	}, right: projectFilesPane{
		endpoint: ducklord.FileEndpoint{Path: targetDir}, entries: []ducklord.FileEntry{{Name: "drop", IsDir: true}}, marked: map[string]bool{},
	}}
	var rendered bytes.Buffer
	s.renderProjectFilesModal(&rendered, 150, 32)
	var source, target modalMouseRegion
	for _, region := range s.modalMouseRegions {
		if region.action.selection == &s.projectFiles.left.selected && region.action.index == 1 {
			source = region
		}
		if region.action.selection == &s.projectFiles.right.selected && region.action.index == 0 {
			target = region
		}
	}
	if source.row == 0 || target.row == 0 {
		t.Fatalf("missing drag regions: %#v", s.modalMouseRegions)
	}
	s.modalMouseInput(source.left, source.row)
	s.projectFilesMouseRelease(target.left, target.row)
	if s.projectFiles.step != "preview" || !s.projectFiles.left.marked["nested"] {
		t.Fatalf("drag did not select source and open preview: %#v", s.projectFiles)
	}
	if got := s.projectFiles.right.endpoint.Path; got != filepath.Join(targetDir, "drop") {
		t.Fatalf("drag target path = %q", got)
	}
}
