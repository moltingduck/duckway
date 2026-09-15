package ducklioncli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

func TestBookmarksCommandAndLegacyAliasInspectDirectory(t *testing.T) {
	path := t.TempDir()
	for _, command := range []string{"bookmarks", "projects"} {
		t.Run(command, func(t *testing.T) {
			var out bytes.Buffer
			if err := Run(nil, []string{command, "--inspect-dir", path, "--json"}, &out); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Path   string `json:"path"`
				Exists bool   `json:"exists"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Path != path || !result.Exists {
				t.Fatalf("inspection=%q error=%v", out.String(), err)
			}
		})
	}
}

func TestBookmarksCommandRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--unknown"},
		{"--add"},
		{"--name"},
		{"--suggest"},
		{"--inspect-dir"},
		{"--create-dir"},
		{"--add", " "},
		{"--create-dir", "relative"},
		{"--inspect-dir", "/tmp", "--inspect-dir", "/tmp"},
		{"--create-dir", "/tmp", "--create-dir", "/tmp"},
		{"--add", "/tmp", "--name", "a", "--name", "b"},
		{"--inspect-dir", "/tmp", "--add", "/tmp"},
	} {
		var out bytes.Buffer
		err := Run(nil, append([]string{"bookmarks"}, args...), &out)
		if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "project") {
			t.Errorf("args=%q error=%v output=%q", args, err, out.String())
		}
	}
}

func TestBookmarksAndProjectsShareSavedRegistry(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("DUCKWAY_CONFIG_DIR", configDir)
	path := t.TempDir()
	var added bytes.Buffer
	if err := Run(nil, []string{"bookmarks", "--add", path, "--name", "Saved directory", "--json"}, &added); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configDir, "cc-projects.json")); err != nil {
		t.Fatalf("existing registry format was not preserved: %v", err)
	}
	var bookmarkList, legacyList bytes.Buffer
	if err := Run(nil, []string{"bookmarks", "--json"}, &bookmarkList); err != nil {
		t.Fatal(err)
	}
	if err := Run(nil, []string{"projects", "--json"}, &legacyList); err != nil {
		t.Fatal(err)
	}
	if bookmarkList.String() != legacyList.String() {
		t.Fatalf("commands returned different registries: %s / %s", bookmarkList.String(), legacyList.String())
	}
	var bookmarks []ProjectOutput
	if err := json.Unmarshal(bookmarkList.Bytes(), &bookmarks); err != nil {
		t.Fatal(err)
	}
	for _, bookmark := range bookmarks {
		if bookmark.Path == path && bookmark.Name == "Saved directory" && bookmark.Source == "duckway-client" {
			return
		}
	}
	t.Fatalf("saved bookmark missing: %s", bookmarkList.String())
}

func TestBookmarkRegistryErrorPreservesCauseAndUserText(t *testing.T) {
	for _, tt := range []struct{ original, want string }{
		{`project name "project name" is already used by /tmp/project registry`, `bookmark name "project name" is already used by /tmp/project registry`},
		{"project registry is not a regular file", "bookmark registry is not a regular file"},
		{"stat /tmp/project name: permission denied", "stat /tmp/project name: permission denied"},
		{"parse cc-projects.json: invalid JSON", "parse cc-projects.json: invalid JSON"},
	} {
		cause := errors.New(tt.original)
		err := bookmarkRegistryError(cause)
		if err.Error() != tt.want || !errors.Is(err, cause) {
			t.Fatalf("error=%q want=%q cause preserved=%t", err, tt.want, errors.Is(err, cause))
		}
	}
}

func TestBookmarksRegistryErrorsUseBookmarkTerminology(t *testing.T) {
	t.Setenv("DUCKWAY_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "project registry")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"line\nbreak", "hidden\u202ename", strings.Repeat("x", 257)} {
		var out bytes.Buffer
		err := Run(nil, []string{"bookmarks", "--add", path, "--name", name, "--json"}, &out)
		if err == nil || !strings.HasPrefix(err.Error(), "bookmark name ") || out.Len() != 0 {
			t.Fatalf("name=%q error=%v output=%q", name, err, out.String())
		}
	}
	if err := Run(nil, []string{"bookmarks", "--add", path, "--name", "project name"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	err := Run(nil, []string{"bookmarks", "--add", root, "--name", "project name"}, &bytes.Buffer{})
	want := fmt.Sprintf("bookmark name %q is already used by %s", "project name", path)
	if err == nil || err.Error() != want {
		t.Fatalf("duplicate name error=%v want=%q", err, want)
	}
}

func TestBookmarksRejectsNonRegularRegistryWithBookmarkError(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("DUCKWAY_CONFIG_DIR", configDir)
	if err := os.Mkdir(filepath.Join(configDir, "cc-projects.json"), 0700); err != nil {
		t.Fatal(err)
	}
	err := Run(nil, []string{"bookmarks", "--json"}, &bytes.Buffer{})
	if err == nil || err.Error() != "bookmark registry is not a regular file" {
		t.Fatalf("registry error=%v", err)
	}
}
