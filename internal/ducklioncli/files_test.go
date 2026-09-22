package ducklioncli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesTarWriteStagesAndValidatesRoot(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "data"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := runFiles([]string{"read", src}, bytes.NewReader(nil), &archive); err != nil {
		t.Fatal(err)
	}
	if err := runFiles([]string{"write", dst, "renamed"}, &archive, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	archive.Reset()
	if err := runFiles([]string{"read", src}, nil, &archive); err != nil {
		t.Fatal(err)
	}
	fullArchive := append([]byte(nil), archive.Bytes()...)
	if err := runFiles([]string{"write", dst, "src"}, &archive, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "src", "data"))
	if err != nil || string(b) != "payload" {
		t.Fatalf("received %q: %v", b, err)
	}
	bad := fullArchive[:len(fullArchive)-20]
	if err := runFiles([]string{"write", dst, "broken"}, bytes.NewReader(bad), &bytes.Buffer{}); err == nil {
		t.Fatal("accepted truncated transfer")
	}
	if _, err := os.Stat(filepath.Join(dst, "broken")); !os.IsNotExist(err) {
		t.Fatalf("partial destination exists: %v", err)
	}
}
