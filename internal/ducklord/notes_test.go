package ducklord

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseNotesEntriesAndPreview(t *testing.T) {
	got := ParseNotes("intro\n## First\nline one\nline two\n### nested\n## Second\nbody")
	if len(got) != 2 || got[0].Title != "First" || got[0].Body != "line one\nline two\n### nested" || got[1].Body != "body" {
		t.Fatalf("entries=%+v", got)
	}
	if got[0].Preview != "line one line two ### nested" {
		t.Fatalf("preview=%q", got[0].Preview)
	}
}

func TestParseNotesSkipsEmptyTitles(t *testing.T) {
	got := ParseNotes("## \ninvalid body\n## Valid\nbody")
	if len(got) != 1 || got[0].Title != "Valid" || got[0].Body != "body" {
		t.Fatalf("empty title was retained: %+v", got)
	}
}

func TestReplaceNotePreservesOtherEntries(t *testing.T) {
	root := t.TempDir()
	if err := SaveNotes(root, "p", "## First\none\n## Second\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	entries, err := ReplaceNote(root, "p", 1, NoteEntry{Title: "Changed", Body: "new body"})
	if err != nil || len(entries) != 2 || entries[0].Title != "First" || entries[0].Body != "one" || entries[1].Title != "Changed" || entries[1].Body != "new body" {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
}

func TestRevisionGuardsReplaceAndAppendAgainstStaleEditors(t *testing.T) {
	root := t.TempDir()
	if err := SaveNotes(root, "p", "## First\none\n## Second\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	rev, err := NotesRevision(root, "p")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveNotes(root, "p", "## First\nchanged elsewhere\n## Second\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceNoteIfRevision(root, "p", 0, &rev, NoteEntry{Title: "First", Body: "stale"}); err == nil || !strings.Contains(err.Error(), "changed while editing") {
		t.Fatalf("stale replacement err=%v", err)
	}
	if _, err := AppendNoteIfRevision(root, "p", rev, NoteEntry{Title: "Stale", Body: "stale"}); err == nil || !strings.Contains(err.Error(), "changed while editing") {
		t.Fatalf("stale append err=%v", err)
	}
	got, err := LoadNotes(root, "p")
	if err != nil || len(got) != 2 || got[0].Body != "changed elsewhere" {
		t.Fatalf("stale edit changed notebook: entries=%+v err=%v", got, err)
	}
}

func TestAppendNoteIfRevisionReloadsCurrentNotebook(t *testing.T) {
	root := t.TempDir()
	if err := SaveNotes(root, "p", "## First\none\n"); err != nil {
		t.Fatal(err)
	}
	rev, err := NotesRevision(root, "p")
	if err != nil {
		t.Fatal(err)
	}
	got, err := AppendNoteIfRevision(root, "p", rev, NoteEntry{Title: "Second", Body: "two"})
	if err != nil || len(got) != 2 || got[0].Body != "one" || got[1].Title != "Second" {
		t.Fatalf("append entries=%+v err=%v", got, err)
	}
}

func TestRevisionAllowsOnlyOneConcurrentReplacement(t *testing.T) {
	root := t.TempDir()
	if err := SaveNotes(root, "p", "## First\none\n"); err != nil {
		t.Fatal(err)
	}
	rev, err := NotesRevision(root, "p")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, body := range []string{"left", "right"} {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			<-start
			_, callErr := ReplaceNoteIfRevision(root, "p", 0, &rev, NoteEntry{Title: "First", Body: body})
			results <- callErr
		}(body)
	}
	close(start)
	wg.Wait()
	close(results)
	var successes, conflicts int
	for callErr := range results {
		if callErr == nil {
			successes++
		} else if strings.Contains(callErr.Error(), "changed while editing") {
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestNotesSnapshotBindsEntriesToRevision(t *testing.T) {
	root := t.TempDir()
	if err := SaveNotes(root, "p", "## Old\nold body\n"); err != nil {
		t.Fatal(err)
	}
	if err := SaveNotes(root, "p", "## Current\ncurrent body\n## Another\nbody\n"); err != nil {
		t.Fatal(err)
	}
	entries, revision, err := NotesSnapshot(root, "p")
	if err != nil || len(entries) != 2 || entries[0].Title != "Current" || entries[0].Body != "current body" {
		t.Fatalf("snapshot entries=%+v revision=%x err=%v", entries, revision, err)
	}
	want, err := NotesRevision(root, "p")
	if err != nil || revision != want {
		t.Fatalf("snapshot revision=%x current=%x err=%v", revision, want, err)
	}
}

func TestFormatNotesRoundTrip(t *testing.T) {
	want := []NoteEntry{{Title: "A", Body: "one\ntwo"}, {Title: "B", Body: "body"}}
	got := ParseNotes(FormatNotes(want))
	if len(got) != len(want) || got[0].Title != "A" || got[0].Body != "one\ntwo" || got[1].Title != "B" {
		t.Fatalf("round trip=%+v", got)
	}
}

func TestNotesRejectsSymlinkedRootAndValidatesTitles(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unsupported")
	}
	if err := SaveNotes(link, "p", "## x\ny"); err == nil {
		t.Fatal("symlinked root accepted")
	}
	root := t.TempDir()
	if err := SaveNotes(root, "p", "## x\ny"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceNote(root, "p", 0, NoteEntry{Title: "bad\ntitle"}); err == nil {
		t.Fatal("multiline title accepted")
	}
	got := ParseNotes("## title\n" + strings.Repeat("界", 121))
	if len([]rune(got[0].Preview)) != 121 || !strings.HasSuffix(got[0].Preview, "…") {
		t.Fatalf("preview=%q", got[0].Preview)
	}
}
func TestNotesStorageProjectScopedAndSafe(t *testing.T) {
	root := t.TempDir()
	if err := SaveNotes(root, "stable-id", "## A\nbody"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "stable-id", "notes.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := NotesPath(root, "../escape"); err == nil {
		t.Fatal("traversal accepted")
	}
	got, err := LoadNotes(root, "stable-id")
	if err != nil || len(got) != 1 || got[0].Body != "body" {
		t.Fatalf("%+v %v", got, err)
	}
	if mode := mustMode(t, filepath.Join(root, "projects", "stable-id")); mode != 0700 {
		t.Fatalf("project mode=%#o", mode)
	}
	if mode := mustMode(t, filepath.Join(root, "projects", "stable-id", "notes.md")); mode != 0600 {
		t.Fatalf("notes mode=%#o", mode)
	}
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestNotesStorageRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "projects")); err != nil {
		t.Skip("symlinks unsupported")
	}
	if _, err := NotesPath(root, "p"); err == nil {
		t.Fatal("symlinked project root accepted")
	}
	if err := os.Remove(filepath.Join(root, "projects")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "projects", "p"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside.md"), filepath.Join(root, "projects", "p", "notes.md")); err != nil {
		t.Skip("symlinks unsupported")
	}
	for name, fn := range map[string]func() error{"path": func() error { _, e := NotesPath(root, "p"); return e }, "load": func() error { _, e := LoadNotes(root, "p"); return e }, "save": func() error { return SaveNotes(root, "p", "x") }} {
		if err := fn(); err == nil {
			t.Fatalf("%s accepted symlink", name)
		}
	}
}

func TestRestoreScopedNotesDoesNotFollowSwappedSymlink(t *testing.T) {
	root := t.TempDir()
	if err := SaveScopedNotes(root, NotesProject, "p", "old"); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "outside.md")
	if err := os.WriteFile(external, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(root, "projects", "p", "notes.md")
	if err := os.Remove(notes); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, notes); err != nil {
		t.Skip("symlinks unsupported")
	}
	if err := RestoreScopedNotes(root, NotesProject, "p", []byte("restored"), true); err == nil {
		t.Fatal("accepted swapped symlink")
	}
	got, err := os.ReadFile(external)
	if err != nil || string(got) != "outside" {
		t.Fatalf("external target changed: %q, %v", got, err)
	}
	if target, err := os.Readlink(notes); err != nil || target != external {
		t.Fatalf("swapped symlink changed: %q, %v", target, err)
	}
}

func TestReadScopedNotesRawRejectsOversizedNotebook(t *testing.T) {
	root := t.TempDir()
	if err := SaveScopedNotes(root, NotesGlobal, "", strings.Repeat("x", maxScopedNotesBytes+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadScopedNotesRaw(root, NotesGlobal, ""); err == nil || !strings.Contains(err.Error(), "scoped notes exceed size limit") {
		t.Fatalf("oversized notebook error = %v", err)
	}
}

func TestOSC52CopyContainsOnlyBody(t *testing.T) {
	seq := OSC52Copy("body\ntext")
	parts := strings.Split(seq, ";")
	if len(parts) != 3 {
		t.Fatal(seq)
	}
	b, e := base64.StdEncoding.DecodeString(strings.TrimSuffix(parts[2], "\x07"))
	if e != nil || string(b) != "body\ntext" {
		t.Fatalf("%q %v", b, e)
	}
}

func TestScopedNotesIsolationAndLegacyProjectCompatibility(t *testing.T) {
	root := t.TempDir()
	if err := SaveNotes(root, "p", "## Project\nlegacy"); err != nil {
		t.Fatal(err)
	}
	if err := SaveScopedNotes(root, NotesGlobal, "", "## Global\nworld"); err != nil {
		t.Fatal(err)
	}
	if err := SaveScopedNotes(root, NotesSession, "sess-1", "## Session\nlocal"); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		scope           NotesScope
		id, title, body string
	}{{NotesProject, "p", "Project", "legacy"}, {NotesGlobal, "", "Global", "world"}, {NotesSession, "sess-1", "Session", "local"}}
	for _, c := range checks {
		got, err := LoadScopedNotes(root, c.scope, c.id)
		if err != nil || len(got) != 1 || got[0].Title != c.title || got[0].Body != c.body {
			t.Fatalf("%v/%q: %+v %v", c.scope, c.id, got, err)
		}
	}
	if _, err := ScopedNotesPath(root, NotesSession, "../escape"); err == nil {
		t.Fatal("session traversal accepted")
	}
	if _, err := ScopedNotesPath(root, NotesGlobal, "identity"); err == nil {
		t.Fatal("global identity accepted")
	}
}

func TestScopedNotesRevisionRejectsStaleEdit(t *testing.T) {
	root := t.TempDir()
	if err := SaveScopedNotes(root, NotesGlobal, "", "## One\nold"); err != nil {
		t.Fatal(err)
	}
	_, rev, err := ScopedNotesSnapshot(root, NotesGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveScopedNotes(root, NotesGlobal, "", "## One\nnew"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceScopedNoteIfRevision(root, NotesGlobal, "", 0, &rev, NoteEntry{Title: "One", Body: "stale"}); err == nil {
		t.Fatal("stale scoped edit accepted")
	}
}

func TestSessionNotesIdentitySeparatesHostsWithSameSessionID(t *testing.T) {
	a := SessionIdentity{InstanceID: "9df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "ABC123"}
	b := SessionIdentity{InstanceID: "6df68174-9e13-4dc9-b44d-8532c87f5971", SessionID: "ABC123"}
	nameA, err := SessionNotesIdentity(a)
	if err != nil {
		t.Fatal(err)
	}
	nameB, err := SessionNotesIdentity(b)
	if err != nil {
		t.Fatal(err)
	}
	if nameA == nameB || strings.ContainsAny(nameA+nameB, `/\\`) {
		t.Fatalf("session identities collide or escape paths: %q %q", nameA, nameB)
	}
	root := t.TempDir()
	if err := SaveScopedNotes(root, NotesSession, nameA, "## A\nhost A"); err != nil {
		t.Fatal(err)
	}
	if err := SaveScopedNotes(root, NotesSession, nameB, "## B\nhost B"); err != nil {
		t.Fatal(err)
	}
	gotA, err := LoadScopedNotes(root, NotesSession, nameA)
	if err != nil || len(gotA) != 1 || gotA[0].Body != "host A" {
		t.Fatalf("host A notebook: %+v %v", gotA, err)
	}
	gotB, err := LoadScopedNotes(root, NotesSession, nameB)
	if err != nil || len(gotB) != 1 || gotB[0].Body != "host B" {
		t.Fatalf("host B notebook: %+v %v", gotB, err)
	}
}
