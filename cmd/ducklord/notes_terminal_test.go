package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNotesTerminalTransitionSequences(t *testing.T) {
	var b bytes.Buffer
	notesTerminalTransition(&b, false)
	if got, want := b.String(), "\033[?1000l\033[?1002l\033[?1003l\033[?1006l\033[?25h\033[?1049l"; got != want {
		t.Fatalf("leave sequence %q, want %q", got, want)
	}
	b.Reset()
	notesTerminalTransition(&b, true)
	if got, want := b.String(), "\033[?1049h\033[?25l\033[?1002h\033[?1006h"; got != want {
		t.Fatalf("enter sequence %q, want %q", got, want)
	}
}

func TestRunNotesEditorRestoresSavedCookedStateBeforeEditor(t *testing.T) {
	root := t.TempDir()
	editor := filepath.Join(root, "editor")
	marker := filepath.Join(root, "marker")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf editor-visible > \""+marker+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	oldMakeRaw, oldRestore := notesMakeRaw, notesRestore
	defer func() { notesMakeRaw, notesRestore = oldMakeRaw, oldRestore }()
	var events []string
	saved := &termState{}
	s := &tuiState{terminalCookedState: saved, copyMode: true, frameOutput: frameOutput{frame: "stale", width: 1, height: 1}}
	notesMakeRaw = func() (*termState, error) {
		events = append(events, "raw")
		return &termState{}, nil
	}
	notesRestore = func(state *termState) {
		if state != saved {
			t.Errorf("restored state %p, want saved cooked state %p", state, saved)
		}
		events = append(events, "restore")
	}
	t.Setenv("EDITOR", editor)
	s.runNotesEditor(filepath.Join(root, "note.md"), nil)
	if !reflect.DeepEqual(events, []string{"restore", "raw"}) {
		t.Fatalf("terminal transition order %v, want restore then raw", events)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "editor-visible" {
		t.Fatalf("editor did not run visibly: content=%q err=%v", content, err)
	}
	if s.frameOutput.frame != "" || s.frameOutput.width != 0 || s.frameOutput.height != 0 {
		t.Fatalf("frame cache was not cleared: %+v", s.frameOutput)
	}
}

func TestRunNotesEditorUsesVimWhenEditorIsUnset(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker")
	vim := filepath.Join(root, "vim")
	if err := os.WriteFile(vim, []byte("#!/bin/sh\nprintf vim-default > \""+marker+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	oldMakeRaw, oldRestore := notesMakeRaw, notesRestore
	defer func() { notesMakeRaw, notesRestore = oldMakeRaw, oldRestore }()
	notesMakeRaw = func() (*termState, error) { return &termState{}, nil }
	notesRestore = func(*termState) {}
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", root)
	s := &tuiState{terminalCookedState: &termState{}}
	s.runNotesEditor(filepath.Join(root, "note.md"), nil)
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "vim-default" {
		t.Fatalf("default vim did not run: content=%q err=%v output=%q", content, err, s.outputErr)
	}
}

func TestRunNotesEditorReportsMissingDefaultEditor(t *testing.T) {
	oldMakeRaw, oldRestore := notesMakeRaw, notesRestore
	defer func() { notesMakeRaw, notesRestore = oldMakeRaw, oldRestore }()
	notesMakeRaw = func() (*termState, error) { return &termState{}, nil }
	notesRestore = func(*termState) {}
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", t.TempDir())
	s := &tuiState{terminalCookedState: &termState{}}
	s.runNotesEditor(filepath.Join(t.TempDir(), "note.md"), nil)
	if s.outputErr == "" || !strings.Contains(s.outputErr, "editor:") {
		t.Fatalf("missing editor was not reported: %q", s.outputErr)
	}
}

func TestRunNotesEditorNilStateIsSafe(t *testing.T) {
	var s *tuiState
	s.runNotesEditor("", nil)
}
