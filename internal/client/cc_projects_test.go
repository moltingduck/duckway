package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCCProjectStoreAddResolvesRelativePaths(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	app := filepath.Join(work, "app")
	if err := os.MkdirAll(app, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	store := NewCCProjectStore(filepath.Join(root, ".duckway"))
	added, err := store.Add([]string{"app"}, "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(added) != 1 {
		t.Fatalf("added len = %d", len(added))
	}
	if added[0].Path != app {
		t.Fatalf("path = %q, want %q", added[0].Path, app)
	}
	if added[0].Name != "app" {
		t.Fatalf("name = %q, want app", added[0].Name)
	}
}

func TestCCProjectStoreAddExpandsGlob(t *testing.T) {
	root := t.TempDir()
	alpha := filepath.Join(root, "projects", "alpha")
	beta := filepath.Join(root, "projects", "beta")
	if err := os.MkdirAll(alpha, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(beta, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "projects", "notes.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	store := NewCCProjectStore(filepath.Join(root, ".duckway"))
	added, err := store.Add([]string{filepath.Join(root, "projects", "*")}, "")
	if err != nil {
		t.Fatalf("Add glob: %v", err)
	}
	if len(added) != 2 {
		t.Fatalf("added len = %d, want 2: %+v", len(added), added)
	}
	projects, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("projects len = %d, want 2", len(projects))
	}
	if projects[0].Name != "alpha" || projects[0].Path != alpha {
		t.Fatalf("project[0] = %+v, want alpha %s", projects[0], alpha)
	}
	if projects[1].Name != "beta" || projects[1].Path != beta {
		t.Fatalf("project[1] = %+v, want beta %s", projects[1], beta)
	}
}

func TestCCProjectStoreResolveByNumberNameAndPath(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	if err := os.MkdirAll(app, 0700); err != nil {
		t.Fatal(err)
	}
	store := NewCCProjectStore(filepath.Join(root, ".duckway"))
	if _, err := store.Add([]string{app}, "duckway"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, ref := range []string{"1", "duckway", app} {
		got, err := store.Resolve(ref)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", ref, err)
		}
		if got.Path != app {
			t.Fatalf("Resolve(%q).Path = %q, want %q", ref, got.Path, app)
		}
	}
}

func TestCCProjectStoreClearRemovesRegistryOnly(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	if err := os.MkdirAll(app, 0700); err != nil {
		t.Fatal(err)
	}
	store := NewCCProjectStore(filepath.Join(root, ".duckway"))
	if _, err := store.Add([]string{app}, "duckway"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	n, err := store.Clear()
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared = %d, want 1", n)
	}
	if st, err := os.Stat(app); err != nil || !st.IsDir() {
		t.Fatalf("project folder should remain: stat=%v err=%v", st, err)
	}
	projects, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects after clear = %+v, want empty", projects)
	}
}
