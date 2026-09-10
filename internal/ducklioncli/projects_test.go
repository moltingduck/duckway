package ducklioncli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunProjectsSuggestJSON(t *testing.T) {
	root := t.TempDir()
	match := filepath.Join(root, "project one")
	if err := os.Mkdir(match, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "project file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runProjects([]string{"--suggest", filepath.Join(root, "pro"), "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var paths []string
	if err := json.Unmarshal(out.Bytes(), &paths); err != nil || len(paths) != 1 || paths[0] != match {
		t.Fatalf("paths=%#v jsonErr=%v output=%q", paths, err, out.String())
	}
}

func TestRunProjectsInspectsAndRecursivelyCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one", "two", "three")
	var out bytes.Buffer
	if err := runProjects([]string{"--inspect-dir", path, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var inspected struct {
		Exists bool `json:"exists"`
	}
	if json.Unmarshal(out.Bytes(), &inspected) != nil || inspected.Exists {
		t.Fatalf("inspect=%q", out.String())
	}
	out.Reset()
	if err := runProjects([]string{"--create-dir", path, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var created struct{ Exists, Created bool }
	if json.Unmarshal(out.Bytes(), &created) != nil || !created.Exists || !created.Created {
		t.Fatalf("create=%q", out.String())
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("recursive directory missing: %v", err)
	}
}

func TestRunProjectsRejectsFilesAndSymbolicLinks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runProjects([]string{"--create-dir", file, "--json"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected existing file to be rejected")
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "child")} {
		if err := runProjects([]string{"--create-dir", path, "--json"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("path=%q error=%v", path, err)
		}
	}
}

func TestRunProjectsRejectsAmbiguousOrEmptyModes(t *testing.T) {
	tests := [][]string{
		{"--suggest", ""},
		{"--add", ""},
		{"--name", "name"},
		{"--suggest", "/", "--add", "/tmp"},
		{"--suggest", "/", "--suggest", "/tmp"},
		{"--add", "/tmp", "--add", "/var"},
		{"--add", "/tmp", "--name", ""},
		{"--inspect-dir", "relative"},
		{"--inspect-dir", "/tmp", "--create-dir", "/tmp"},
	}
	for _, args := range tests {
		if err := runProjects(args, &bytes.Buffer{}); err == nil || strings.TrimSpace(err.Error()) == "" {
			t.Fatalf("args=%q error=%v", args, err)
		}
	}
}
