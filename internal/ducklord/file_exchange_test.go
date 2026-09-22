package ducklord

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestCopyRejectsSymlinkAndSelfSubtreeButAllowsSibling(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(src, "escape")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"escape"}}); err == nil {
		t.Fatal("accepted symlink source")
	}
	if _, err := CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: filepath.Join(src, "child")}, Names: []string{"child"}}); err == nil {
		t.Fatal("accepted destination inside source")
	}
	sibling := filepath.Join(root, "sibling")
	if err := os.Mkdir(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: sibling}, Names: []string{"file"}}); err != nil {
		t.Fatalf("sibling copy: %v", err)
	}
}

func TestCopyDirectoryOverwriteRefusedAndLateCollisionPreservesDestination(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.MkdirAll(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dir", "x"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dst, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"dir"}, Conflict: "overwrite"}); err == nil {
		t.Fatal("overwrote existing directory")
	}
	if err := os.WriteFile(filepath.Join(dst, "x"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"x"}, Conflict: "skip"}); err == nil {
		// skip is expected to succeed and preserve the existing file.
	} else {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "x"))
	if err != nil || string(b) != "old" {
		t.Fatalf("destination changed: %q %v", b, err)
	}
}

func TestLimitedBufferReportsFullWriteLength(t *testing.T) {
	b := &limitedBuffer{limit: 3}
	n, err := b.Write([]byte("012345"))
	if err != nil || n != 6 || b.String() != "012" {
		t.Fatalf("write result n=%d err=%v contents=%q", n, err, b.String())
	}
}

func TestRemotePipeWithholdsFooterWhenSourceFails(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "archive.tar")
	received := filepath.Join(root, "received.tar")
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: exchangePayloadRoot, Typeflag: tar.TypeReg, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ssh := filepath.Join(root, "ssh")
	script := "#!/bin/sh\ncase \"$*\" in\n  *source*) cat \"$EXCHANGE_ARCHIVE\"; exit 1 ;;\n  *) cat > \"$EXCHANGE_RECEIVED\" ;;\nesac\n"
	if err := os.WriteFile(ssh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXCHANGE_ARCHIVE", archive)
	t.Setenv("EXCHANGE_RECEIVED", received)
	source := Client{Name: "source", Host: "source", SSH: ssh, Ducklion: "source"}
	destination := Client{Name: "destination", Host: "destination", SSH: ssh, Ducklion: "destination"}
	if err := remotePipe(context.Background(), source, "/source/item", destination, "/destination", "item", false); err == nil {
		t.Fatal("accepted source command failure")
	}
	got, err := os.ReadFile(received)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(exchangeFooter)) {
		t.Fatal("destination received completion footer after source failure")
	}
}

func TestRemotePipeCancellationJoinsBlockedSource(t *testing.T) {
	root := t.TempDir()
	ssh := filepath.Join(root, "ssh")
	script := "#!/bin/sh\ncase \"$*\" in\n  *source*) while :; do printf x; done ;;\n  *) cat >/dev/null ;;\nesac\n"
	if err := os.WriteFile(ssh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	source := Client{Name: "source", Host: "source", SSH: ssh, Ducklion: "source"}
	destination := Client{Name: "destination", Host: "destination", SSH: ssh, Ducklion: "destination"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- remotePipe(ctx, source, "/source/item", destination, "/destination", "item", false) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted canceled transfer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled transfer did not join source and destination")
	}
}

func TestExtractTarRootRejectsArchiveResourceLimits(t *testing.T) {
	archive := func(flag byte, count int) []byte {
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
	for _, flag := range []byte{tar.TypeReg, tar.TypeDir} {
		t.Run(fmt.Sprintf("type-%d", flag), func(t *testing.T) {
			dst := t.TempDir()
			dstRoot, err := os.OpenRoot(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer dstRoot.Close()
			stage, err := makeExchangeStage(dstRoot)
			if err != nil {
				t.Fatal(err)
			}
			if err := extractTarRoot(bytes.NewReader(archive(flag, exchangeMaxEntries)), dstRoot, stage, exchangePayloadRoot); err == nil {
				t.Fatal("accepted archive with too many zero-size entries")
			}
			if err := dstRoot.RemoveAll(stage); err != nil {
				t.Fatal(err)
			}
			if _, err := dstRoot.Lstat(stage); !os.IsNotExist(err) {
				t.Fatalf("staging directory remains: %v", err)
			}
		})
	}
}

func TestTarOneAndExtractTarRootRejectArchivePathDepth(t *testing.T) {
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
	if err := tarOne(context.Background(), filepath.Join(root, "source"), "ignored", io.Discard); err == nil {
		t.Fatal("produced archive with excessive path depth")
	}

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
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
	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer dstRoot.Close()
	stage, err := makeExchangeStage(dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer dstRoot.RemoveAll(stage)
	if err := extractTarRoot(bytes.NewReader(archive.Bytes()), dstRoot, stage, exchangePayloadRoot); err == nil {
		t.Fatal("accepted archive with excessive path depth")
	}
	if _, err := dstRoot.Lstat(filepath.Join(stage, name)); !os.IsNotExist(err) {
		t.Fatalf("created excessive path: %v", err)
	}
}

func TestLocalToRemoteCancellationJoinsTarProducer(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 2<<20), 0600); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(root, "started")
	ssh := filepath.Join(root, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\ntouch \"$EXCHANGE_STARTED\"\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXCHANGE_STARTED", started)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- localToRemote(ctx, src, Client{Name: "remote", Host: "remote", SSH: ssh, Ducklion: "ducklion"}, root, "result", false)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("remote command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("cancellation result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local-to-remote transfer did not join tar producer")
	}
	if _, err := os.Lstat(filepath.Join(root, "result")); !os.IsNotExist(err) {
		t.Fatalf("canceled transfer committed destination: %v", err)
	}
}

func TestCopyLocalOverwriteRejectsDestinationTypeMismatch(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(src, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dir", "new"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "dir"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"dir"}, Conflict: "overwrite"}); err == nil {
		t.Fatal("accepted directory overwrite of regular destination")
	}
	got, err := os.ReadFile(filepath.Join(dst, "dir"))
	if err != nil || string(got) != "old" {
		t.Fatalf("destination changed: %q %v", got, err)
	}
}

func TestCopyLocalPreservesOwnerExecutableBit(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "run"), []byte("#!/bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: FileEndpoint{Path: dst}, Names: []string{"run"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dst, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode %o, want 0700", info.Mode().Perm())
	}
}

func TestRemoteToLocalOverwriteRejectsDestinationTypeMismatch(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "source.tar")
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: exchangePayloadRoot, Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: exchangePayloadRoot + "/new", Typeflag: tar.TypeReg, Size: 3, Mode: 0600}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: exchangeFooter, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "result"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	ssh := filepath.Join(root, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\ncat \"$EXCHANGE_ARCHIVE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXCHANGE_ARCHIVE", archivePath)
	client := Client{Name: "source", Host: "source", SSH: ssh, Ducklion: "ducklion"}
	if err := remoteToLocal(context.Background(), client, "/source", dst, "result", true); err == nil {
		t.Fatal("accepted directory overwrite of regular destination")
	}
	got, err := os.ReadFile(filepath.Join(dst, "result"))
	if err != nil || string(got) != "old" {
		t.Fatalf("destination changed: %q %v", got, err)
	}
}

func TestCrossEndpointNonTransferableDestinationConflictPolicies(t *testing.T) {
	for _, kind := range []string{"symlink", "special"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
			if err := os.Mkdir(src, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dst, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "item"), []byte("payload"), 0600); err != nil {
				t.Fatal(err)
			}
			ssh := filepath.Join(root, "ssh")
			script := "#!/bin/sh\ncase \"$*\" in\n  *'files list'*) printf '[{\"name\":\"item\",\"is_dir\":false,\"size\":0,\"non_transferable\":true}]\\n' ;;\n  *) cat >/dev/null ;;\nesac\n"
			if err := os.WriteFile(ssh, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			destination := FileEndpoint{Client: &Client{Name: "remote", Host: "remote", SSH: ssh, Ducklion: "ducklion"}, Path: dst}
			for _, policy := range []string{"skip", "rename"} {
				results, err := CopyFiles(context.Background(), FileCopyRequest{Source: FileEndpoint{Path: src}, Destination: destination, Names: []string{"item"}, Conflict: policy})
				if err != nil {
					t.Fatalf("%s: %v", policy, err)
				}
				if policy == "skip" {
					if len(results) != 1 || !results[0].Skipped {
						t.Fatalf("skip results: %#v", results)
					}
				} else if len(results) != 1 || filepath.Base(results[0].Destination) != "item (1)" {
					t.Fatalf("rename results: %#v", results)
				}
			}
		})
	}
}

func TestChooseLocalDestinationRenameHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.WriteFile(filepath.Join(root, "item"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := chooseLocalDestination(ctx, r, "item", "rename"); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("rename cancellation: %v", err)
	}
}
