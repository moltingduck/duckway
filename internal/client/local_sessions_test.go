package client

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFakeSession drops a JSONL file at <root>/<projectDir>/<sid>.jsonl
// with the given lines. Returns the absolute file path.
func writeFakeSession(t *testing.T, root, projectDir, sid string, lines []string) string {
	t.Helper()
	dir := filepath.Join(root, projectDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, sid+".jsonl")
	content := ""
	for _, ln := range lines {
		content += ln + "\n"
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListLocalSessions_ParsesAndSortsNewestFirst(t *testing.T) {
	root := t.TempDir()

	// Older session.
	oldPath := writeFakeSession(t, root, "-home-me-projects-old", "11111111-aaaa-bbbb-cccc-000000000001", []string{
		`{"type":"user","cwd":"/home/me/projects/old","message":{"role":"user","content":"first old prompt"}}`,
		`{"type":"assistant","cwd":"/home/me/projects/old","message":{"role":"assistant","content":"ok"}}`,
	})
	_ = os.Chtimes(oldPath, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))

	// Newer session — content-array form.
	newPath := writeFakeSession(t, root, "-home-me-projects-shiny", "22222222-aaaa-bbbb-cccc-000000000002", []string{
		`{"type":"user","cwd":"/home/me/projects/shiny","message":{"role":"user","content":[{"type":"text","text":"build me a thing"}]}}`,
		`{"type":"assistant","cwd":"/home/me/projects/shiny","message":{"role":"assistant","content":"sure"}}`,
		`{"type":"user","cwd":"/home/me/projects/shiny","message":{"role":"user","content":"more please"}}`,
	})
	_ = os.Chtimes(newPath, time.Now(), time.Now())

	got, err := ListLocalSessions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(got))
	}
	if got[0].SessionID != "22222222-aaaa-bbbb-cccc-000000000002" {
		t.Errorf("newest should be first, got %s", got[0].SessionID)
	}
	if got[0].FirstMessage != "build me a thing" {
		t.Errorf("preview mismatch (array form): %q", got[0].FirstMessage)
	}
	if got[0].MessageCount != 3 {
		t.Errorf("count = %d, want 3", got[0].MessageCount)
	}
	if got[0].Cwd != "/home/me/projects/shiny" {
		t.Errorf("cwd = %q", got[0].Cwd)
	}
	if got[1].FirstMessage != "first old prompt" {
		t.Errorf("string-content preview: %q", got[1].FirstMessage)
	}
}

func TestListLocalSessions_MarksBoundSessions(t *testing.T) {
	root := t.TempDir()
	writeFakeSession(t, root, "-home-me-app", "33333333-1111-2222-3333-000000000001", []string{
		`{"type":"user","cwd":"/home/me/app","message":{"role":"user","content":"hello"}}`,
	})
	writeFakeSession(t, root, "-home-me-app", "33333333-1111-2222-3333-000000000002", []string{
		`{"type":"user","cwd":"/home/me/app","message":{"role":"user","content":"two"}}`,
	})

	bound := map[string]string{
		"dwch_already": "33333333-1111-2222-3333-000000000001",
	}
	got, err := ListLocalSessions(root, bound)
	if err != nil {
		t.Fatal(err)
	}

	var marked, unmarked int
	for _, s := range got {
		if s.BoundTo == "dwch_already" {
			marked++
		}
		if s.BoundTo == "" {
			unmarked++
		}
	}
	if marked != 1 || unmarked != 1 {
		t.Errorf("expected exactly 1 bound + 1 unbound, got marked=%d unmarked=%d", marked, unmarked)
	}
}

func TestListLocalSessions_MissingRootIsEmpty(t *testing.T) {
	got, err := ListLocalSessions(filepath.Join(t.TempDir(), "does-not-exist"), nil)
	if err != nil {
		t.Fatalf("missing root should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 sessions, got %d", len(got))
	}
}

func TestListLocalSessions_FallsBackToDirNameForCwd(t *testing.T) {
	root := t.TempDir()
	// Session file with no cwd in events — cwd must be reconstructed from
	// the directory name (Claude encodes /foo/bar as -foo-bar).
	writeFakeSession(t, root, "-tmp-anon", "44444444-aaaa-aaaa-aaaa-000000000001", []string{
		`{"type":"queue-operation","operation":"enqueue"}`,
	})
	got, err := ListLocalSessions(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].Cwd != "/tmp/anon" {
		t.Errorf("fallback cwd = %q, want /tmp/anon", got[0].Cwd)
	}
}
