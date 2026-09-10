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

func TestRunProjectsRejectsAmbiguousOrEmptyModes(t *testing.T) {
	tests := [][]string{
		{"--suggest", ""},
		{"--add", ""},
		{"--name", "name"},
		{"--suggest", "/", "--add", "/tmp"},
		{"--suggest", "/", "--suggest", "/tmp"},
		{"--add", "/tmp", "--add", "/var"},
		{"--add", "/tmp", "--name", ""},
	}
	for _, args := range tests {
		if err := runProjects(args, &bytes.Buffer{}); err == nil || strings.TrimSpace(err.Error()) == "" {
			t.Fatalf("args=%q error=%v", args, err)
		}
	}
}
