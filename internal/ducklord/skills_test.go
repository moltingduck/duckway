package ducklord

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSkillFilePinnedRootRejectsSwappedNestedSymlink(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "value"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "value"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	old := filepath.Join(root, "old")
	if err := os.Rename(nested, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, nested); err != nil {
		t.Fatal(err)
	}
	data, err := readSkillFile(fd, "nested/value", 100)
	if err == nil || len(data) != 0 {
		t.Fatalf("read swapped tree: data=%q err=%v", data, err)
	}
}

func TestSkillConfigValidation(t *testing.T) {
	for _, source := range []string{"http://example.test/skill", "https://", "https://user@example.test/x#frag"} {
		if err := (&SkillTrackingSource{URL: source}).Normalize(); err == nil {
			t.Errorf("accepted source %q", source)
		}
	}
	if err := (&SkillTrackingSource{URL: "https://example.test/skill"}).Normalize(); err == nil {
		t.Fatal("accepted source without skill ID")
	}
	if err := (&SkillTrackingSource{SkillID: "demo", URL: "https://example.test/skill"}).Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := (&SkillTrackingSource{SkillID: "../demo", URL: "https://example.test/skill"}).Normalize(); err == nil {
		t.Fatal("accepted unsafe source skill ID")
	}
	if err := (&Config{SkillSources: []SkillTrackingSource{{SkillID: "demo", URL: "https://example.test/a"}, {SkillID: "demo", URL: "https://example.test/b"}}}).normalize(); err == nil || !strings.Contains(err.Error(), "duplicate skill source") {
		t.Fatalf("duplicate source err=%v", err)
	}
	for _, target := range []SkillInstallTarget{{ID: "x", Path: "relative"}, {ID: "../x", Path: "/tmp/x"}, {ID: "codex", Path: "/tmp/skills", Skills: []ManagedHostSkill{{ID: "demo", Management: "invalid"}}}, {ID: "codex", Path: "/tmp/skills", Skills: []ManagedHostSkill{{ID: "demo", Management: "push"}, {ID: "demo", Management: "pull"}}}} {
		if err := target.Normalize(); err == nil {
			t.Errorf("accepted target %+v", target)
		}
	}
	validTarget := SkillInstallTarget{ID: "codex", Path: "/tmp/skills", Skills: []ManagedHostSkill{{ID: "demo", Management: "push"}, {ID: "remote", Management: "pull"}, {ID: "manual", Management: "none"}}}
	if err := validTarget.Normalize(); err != nil {
		t.Fatalf("valid per-agent management rejected: %v", err)
	}
	client := Client{Name: "host", Host: "host", SkillTargets: []SkillInstallTarget{{ID: "same", Path: "/one"}, {ID: "same", Path: "/two"}}}
	if err := client.Normalize(); err == nil || !strings.Contains(err.Error(), "duplicate skill target") {
		t.Fatalf("duplicate target err=%v", err)
	}
	client = Client{Name: "host", Host: "host", SelectedSkills: []string{"one", "one"}}
	if err := client.Normalize(); err == nil || !strings.Contains(err.Error(), "duplicate selected skill") {
		t.Fatalf("duplicate selection err=%v", err)
	}
}

func TestInspectSkillRejectsSymlinksAndBounds(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "skill")
	if err := os.Mkdir(skill, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := InspectSkill(skill, "demo")
	if err != nil || info.Files != 1 || info.Digest == "" {
		t.Fatalf("inspect=%+v err=%v", info, err)
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(skill, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectSkill(skill, "demo"); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := InspectSkill(skill, "../escape"); err == nil {
		t.Fatal("unsafe identifier accepted")
	}
}

func TestImportSkillPinsRootAcrossTransferSwap(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "value"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external")
	if err := os.Mkdir(external, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "value"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "old")
	skillTransferTestHook = func(rel string) {
		if rel != "nested/value" {
			return
		}
		skillTransferTestHook = nil
		if err := os.Rename(filepath.Join(src, "nested"), old); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(src, "nested")); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { skillTransferTestHook = nil }()
	if _, err := ImportSkill(repo, "demo", src); err == nil {
		t.Fatal("import accepted swapped symlink")
	}
	if _, err := os.Stat(filepath.Join(repo, "demo", "nested", "value")); !os.IsNotExist(err) {
		t.Fatalf("unexpected imported content: %v", err)
	}
}

func TestImportSkillAtomicallyReplacesOnlyAfterValidation(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	src := filepath.Join(root, "src")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportSkill(repo, "demo", src); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "demo", "SKILL.md")); string(got) != "new" {
		t.Fatalf("import=%q", got)
	}
	if err := os.Remove(filepath.Join(src, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportSkill(repo, "demo", src); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "demo", "SKILL.md")); string(got) != "new" {
		t.Fatalf("old skill lost after failed replacement: %q", got)
	}
}

func TestCopySkillRejectsSymlinksDuringInstall(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	src := filepath.Join(root, "src")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(repo, "demo")
	if err := copySkill(src, dst); err == nil || !strings.Contains(err.Error(), "skill contains symlink") {
		t.Fatalf("symlinked source accepted: %v", err)
	}
}

func TestSkillPreviewDoesNotOverwriteBeforeCommit(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(repo, "demo"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "demo", "SKILL.md"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := PrepareSkillPreview(repo, "demo", src)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "demo", "SKILL.md")); string(got) != "old" {
		t.Fatalf("skill overwritten during preview: %q", got)
	}
	if preview.Diff == "" || !strings.Contains(preview.Diff, "--- a/demo/SKILL.md") {
		t.Fatalf("diff=%q", preview.Diff)
	}
	if strings.Contains(preview.Diff, "@@ -0 +1 @@\n-\n+") {
		t.Fatalf("new-file diff contains an empty deletion: %q", preview.Diff)
	}
	if _, err := os.Stat(preview.StagedPath); err != nil {
		t.Fatalf("staged preview missing: %v", err)
	}
	staged := filepath.Dir(preview.StagedPath)
	if _, err := CommitSkillPreview(&preview); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "demo", "SKILL.md")); string(got) != "new" {
		t.Fatalf("committed skill=%q", got)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("preview staging remained: %v", err)
	}
}

func TestSkillPreviewCleanupLeavesManagedSkill(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(repo, "demo"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "demo", "SKILL.md"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := PrepareSkillPreview(repo, "demo", src)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Dir(preview.StagedPath)
	CleanupSkillPreview(&preview)
	CleanupSkillPreview(&preview)
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("preview staging remained: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "demo", "SKILL.md")); string(got) != "old" {
		t.Fatalf("skill changed after cleanup: %q", got)
	}
}
