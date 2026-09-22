package ducklioncli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestFilesReadWriteRegularFile(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dst := filepath.Join(root, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := runFiles([]string{"read", src}, nil, &archive); err != nil {
		t.Fatal(err)
	}
	if err := runFiles([]string{"write", dst, "result"}, &archive, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "result"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("received %q: %v", got, err)
	}
}

func TestFilesWriteLateCollisionPreservesDestination(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dst := filepath.Join(root, "dst")
	if err := os.WriteFile(src, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := runFiles([]string{"read", src}, nil, &archive); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "result"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runFiles([]string{"write", dst, "result"}, &archive, io.Discard); err == nil {
		t.Fatal("accepted destination collision")
	}
	got, err := os.ReadFile(filepath.Join(dst, "result"))
	if err != nil || string(got) != "old" {
		t.Fatalf("collision replaced destination with %q: %v", got, err)
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

func archiveEntries(t *testing.T, flag byte, count int) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: exchangePayloadRoot, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("%s/e%05d", exchangePayloadRoot, i), Typeflag: flag}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestFilesWriteRejectsArchiveEntryLimitWithoutCommit(t *testing.T) {
	for _, flag := range []byte{tar.TypeReg, tar.TypeDir} {
		t.Run(fmt.Sprintf("type-%d", flag), func(t *testing.T) {
			dst := t.TempDir()
			archive := archiveEntries(t, flag, exchangeMaxEntries)
			if err := runFiles([]string{"write", dst, "result"}, bytes.NewReader(archive), io.Discard); err == nil {
				t.Fatal("accepted archive with too many zero-size entries")
			}
			if _, err := os.Lstat(filepath.Join(dst, "result")); !os.IsNotExist(err) {
				t.Fatalf("committed rejected archive: %v", err)
			}
			if stages, err := filepath.Glob(filepath.Join(dst, ".exchange-*")); err != nil || len(stages) != 0 {
				t.Fatalf("staging cleanup: %v %v", stages, err)
			}
		})
	}
}

func TestFilesReadRejectsArchivePathDepth(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < exchangeMaxPathComponents; i++ {
		src = filepath.Join(src, "d")
		if err := os.Mkdir(src, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeTar(context.Background(), filepath.Join(root, "source"), io.Discard); err == nil {
		t.Fatal("produced archive with excessive path depth")
	}
}

func TestFilesWriteRejectsArchivePathDepthWithoutCommit(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: exchangePayloadRoot, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	name := exchangePayloadRoot
	for i := 0; i < exchangeMaxPathComponents; i++ {
		name += "/d"
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := runFiles([]string{"write", dst, "result"}, bytes.NewReader(b.Bytes()), io.Discard); err == nil {
		t.Fatal("accepted archive with excessive path depth")
	}
	if _, err := os.Lstat(filepath.Join(dst, "result")); !os.IsNotExist(err) {
		t.Fatalf("committed rejected archive: %v", err)
	}
}

func TestFilesWriteOverwriteRejectsDestinationTypeMismatch(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "new"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := runFiles([]string{"read", src}, nil, &archive); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "result"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runFiles([]string{"write", dst, "result", "--overwrite"}, &archive, io.Discard); err == nil {
		t.Fatal("accepted directory overwrite of regular destination")
	}
	got, err := os.ReadFile(filepath.Join(dst, "result"))
	if err != nil || string(got) != "old" {
		t.Fatalf("destination changed: %q %v", got, err)
	}
}

func TestFilesReadWritePreservesOwnerExecutableBit(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "source"), filepath.Join(root, "dst")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := runFiles([]string{"read", src}, nil, &archive); err != nil {
		t.Fatal(err)
	}
	if err := runFiles([]string{"write", dst, "result"}, &archive, io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dst, "result"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode %o, want 0700", info.Mode().Perm())
	}
}

func TestFilesListReportsSymlinkAsNonTransferableOccupant(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runFiles([]string{"list", root}, nil, &out); err != nil {
		t.Fatal(err)
	}
	var entries []fileCLIEntry
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name == "link" {
			if !entry.NonTransferable {
				t.Fatal("symlink reported as transferable")
			}
			return
		}
	}
	t.Fatal("symlink omitted from occupied entries")
}

func TestFilesReadDirectoryWritesZeroSizedDirectoryHeader(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(src, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "child", "nested.txt"), []byte("nested\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := runFiles([]string{"read", src}, nil, &archive); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(archive.Bytes()))
	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != exchangePayloadRoot || h.Typeflag != tar.TypeDir || h.Size != 0 {
		t.Fatalf("root header = name %q type %q size %d", h.Name, h.Typeflag, h.Size)
	}
}
