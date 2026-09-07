package supervisor

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetainedOutputKeepsExactBoundedSuffixAndOffsets(t *testing.T) {
	dir := t.TempDir()
	prefix := bytes.Repeat([]byte("a"), RetainedOutputCapacity-17)
	suffix := bytes.Repeat([]byte("b"), 73)
	output, err := OpenRetainedOutput(dir, "ABC123", 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Write(prefix); err != nil {
		t.Fatal(err)
	}
	if err := output.Write(suffix); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := ReadRetainedOutput(dir, "ABC123", 4)
	if err != nil {
		t.Fatal(err)
	}
	written := append(prefix, suffix...)
	want := written[len(written)-RetainedOutputCapacity:]
	if !bytes.Equal(snapshot.Data, want) {
		t.Fatalf("retained suffix mismatch: got %d bytes", len(snapshot.Data))
	}
	if snapshot.StartOffset != uint64(len(written)-len(want)) || snapshot.EndOffset != uint64(len(written)) {
		t.Fatalf("offsets = %d..%d, want %d..%d", snapshot.StartOffset, snapshot.EndOffset, len(written)-len(want), len(written))
	}
	for _, path := range []string{RetainedOutputPath(dir, 4), RetainedOutputPath(dir, 4) + ".json"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode = %o", filepath.Base(path), info.Mode().Perm())
		}
	}
}

func TestRetainedOutputReopenAppendsWithoutResettingLogicalOffset(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenRetainedOutput(dir, "ABC123", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenRetainedOutput(dir, "ABC123", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Write([]byte("-second")); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadRetainedOutput(dir, "ABC123", 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Data) != "first-second" || snapshot.StartOffset != 0 || snapshot.EndOffset != 12 {
		t.Fatalf("snapshot = %#v data=%q", snapshot, snapshot.Data)
	}
}

func TestRetainedOutputExpiryAnchorIsCloseTime(t *testing.T) {
	dir := t.TempDir()
	output, err := OpenRetainedOutput(dir, "ABC123", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Write([]byte("quiet-runtime")); err != nil {
		t.Fatal(err)
	}
	output.metadata.UpdatedAtMS = time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	closedAfter := time.Now()
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := RetainedOutputInfo(dir, "ABC123", 1)
	if err != nil {
		t.Fatal(err)
	}
	if info.UpdatedAt.UnixMilli() < closedAfter.UnixMilli() {
		t.Fatalf("expiry anchor=%v before close=%v", info.UpdatedAt, closedAfter)
	}
}

func TestRetainedOutputRejectsSymlinkAndWrongGenerationMetadata(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, RetainedOutputPath(dir, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRetainedOutput(dir, "ABC123", 1); err == nil {
		t.Fatal("symlink output path accepted")
	}

	output, err := OpenRetainedOutput(dir, "ABC123", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRetainedOutput(dir, "ABC123", 3); err == nil {
		t.Fatal("different generation accepted")
	}
}

func TestParseRetainedOutputGeneration(t *testing.T) {
	for name, want := range map[string]uint64{"output.1.log": 1, "output.42.log": 42, "output.0.log": 0, "output.x.log": 0, "output.1.json": 0} {
		got, ok := ParseRetainedOutputGeneration(name)
		if got != want || ok != (want != 0) {
			t.Fatalf("ParseRetainedOutputGeneration(%q) = %d, %v", name, got, ok)
		}
	}
}

func TestRetainedOutputPrunesOldGenerationsPerSession(t *testing.T) {
	dir := t.TempDir()
	for generation := uint64(1); generation <= RetainedOutputGenerationLimit+2; generation++ {
		output, err := OpenRetainedOutput(dir, "ABC123", generation)
		if err != nil {
			t.Fatal(err)
		}
		if err := output.Write([]byte{byte(generation)}); err != nil {
			t.Fatal(err)
		}
		if err := output.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(RetainedOutputPath(dir, 1)); !os.IsNotExist(err) {
		t.Fatalf("oldest generation remains: %v", err)
	}
	if _, err := os.Stat(RetainedOutputPath(dir, RetainedOutputGenerationLimit+2)); err != nil {
		t.Fatalf("current generation missing: %v", err)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "output.*.log"))
	if err != nil || len(entries) != RetainedOutputGenerationLimit {
		t.Fatalf("retained generations=%d err=%v", len(entries), err)
	}
}
