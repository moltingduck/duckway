package ducklioncli

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func archiveWith(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

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

func TestFilesWriteCancellationNeverCommits(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "src")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "data"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := runFiles([]string{"read", src}, nil, &archive); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runFilesContext(ctx, []string{"write", dst, "cancelled"}, bytes.NewReader(archive.Bytes()), &bytes.Buffer{}); err == nil {
		t.Fatal("accepted canceled transfer")
	}
	if _, err := os.Stat(filepath.Join(dst, "cancelled")); !os.IsNotExist(err) {
		t.Fatalf("destination committed: %v", err)
	}
}

func TestFilesWriteRejectsTraversalAndDuplicateFooter(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	for _, archive := range [][]byte{
		archiveWith(t, &tar.Header{Name: exchangePayloadRoot, Typeflag: tar.TypeDir}, &tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}, &tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}),
		archiveWith(t, &tar.Header{Name: exchangePayloadRoot, Typeflag: tar.TypeDir}, &tar.Header{Name: exchangePayloadRoot + "/../escape", Typeflag: tar.TypeReg}, &tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}),
	} {
		if err := runFiles([]string{"write", dst, "result"}, bytes.NewReader(archive), &bytes.Buffer{}); err == nil {
			t.Fatal("accepted malformed archive")
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "result")); !os.IsNotExist(err) {
		t.Fatalf("committed malformed archive: %v", err)
	}
}
