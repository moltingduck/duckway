package ducklord

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestListFilesAndCopyLocal(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(src, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "z.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dir", "a.txt"), []byte("nested"), 0600); err != nil {
		t.Fatal(err)
	}
	es, err := ListFiles(context.Background(), FileEndpoint{Path: src})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{es[0].Name, es[1].Name}; !reflect.DeepEqual(got, []string{"dir", "z.txt"}) {
		t.Fatalf("entries %v", got)
	}
	if _, err = CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"z.txt", "dir"}, Conflict: "skip"}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "dir", "a.txt")); err != nil || string(b) != "nested" {
		t.Fatalf("copied directory: %q %v", b, err)
	}
}

func TestCopyConflictPoliciesAndValidation(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.MkdirAll(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(src, "x"), "new")
	write(filepath.Join(dst, "x"), "old")
	res, err := CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"x"}})
	if err != nil || len(res) != 1 || !res[0].Skipped {
		t.Fatalf("skip: %#v %v", res, err)
	}
	res, err = CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"x"}, Conflict: "rename"})
	if err != nil || res[0].Destination != filepath.Join(dst, "x (1)") {
		t.Fatalf("rename: %#v %v", res, err)
	}
	if _, err = CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"x"}, Conflict: "overwrite"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "x")); string(b) != "new" {
		t.Fatalf("overwrite produced %q", b)
	}
	for _, names := range [][]string{{"../x"}, {"x", "x"}} {
		if _, err = CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: names}); err == nil {
			t.Fatalf("accepted names %v", names)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = CopyFiles(ctx, FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancel: %v", err)
	}
}

func TestProjectExchangePathPersistentPrivate(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	a, err := ProjectExchangePath(config, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ProjectExchangePath(config, "project-a")
	if err != nil || a != b {
		t.Fatalf("path persistence: %q %q %v", a, b, err)
	}
	c, err := ProjectExchangePath(config, "project-b")
	if err != nil || c == a {
		t.Fatalf("path collision: %q %q %v", a, c, err)
	}
	i, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if i.Mode().Perm() != 0700 {
		t.Fatalf("mode %o", i.Mode().Perm())
	}
}
