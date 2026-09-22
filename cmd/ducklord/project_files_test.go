package main

import (
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
