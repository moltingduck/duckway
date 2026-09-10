package projectregistry

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSuggestDirectoriesReturnsOnlyMatchingDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"Alpha", "alpine", "beta"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "also-file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := SuggestDirectories(filepath.Join(root, "al"), 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "Alpha"), filepath.Join(root, "alpine")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suggestions = %#v, want %#v", got, want)
	}
}

func TestAddResolvedPathTreatsGlobCharactersLiterally(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "project[one]")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(root, "config"))
	project, err := store.AddResolvedPath(directory, "literal")
	if err != nil {
		t.Fatal(err)
	}
	if project.Path != directory || project.Name != "literal" {
		t.Fatalf("project = %#v", project)
	}
}

func TestConcurrentAddsDoNotLoseProjects(t *testing.T) {
	root := t.TempDir()
	store := NewStore(filepath.Join(root, "config"))
	const count = 12
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		directory := filepath.Join(root, "project-"+string(rune('a'+i)))
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			_, err := store.AddResolvedPath(path, "")
			errs <- err
		}(directory)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	projects, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != count {
		t.Fatalf("saved %d projects, want %d", len(projects), count)
	}
}

func TestAddResolvedPathRejectsDuplicateExplicitName(t *testing.T) {
	root := t.TempDir()
	store := NewStore(filepath.Join(root, "config"))
	for _, name := range []string{"one", "two"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AddResolvedPath(filepath.Join(root, "one"), "same"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddResolvedPath(filepath.Join(root, "two"), "same"); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("duplicate-name error = %v", err)
	}
}
