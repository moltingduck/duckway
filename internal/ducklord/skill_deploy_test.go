package ducklord

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultSkillRepository(t *testing.T) {
	path := DefaultSkillRepository()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("home directory unavailable: %v", err)
	}
	want := filepath.Join(home, ".ducklord", "skills")
	if path != want {
		t.Fatalf("DefaultSkillRepository()=%q, want %q", path, want)
	}
}

func TestEnsureSkillRepositoryCreatesPrivateDirectory(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "nested", "skills")
	if err := EnsureSkillRepository(repository); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(repository)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("repository is not a real directory: %s", info.Mode())
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("repository mode=%o, want 700", info.Mode().Perm())
	}

	symlink := filepath.Join(t.TempDir(), "skills")
	if err := os.Symlink(repository, symlink); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSkillRepository(symlink); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink repository error=%v", err)
	}
}

func TestEnsureSkillRepositoryRejectsSymlinkedExistingParent(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(link, "nested", "skills")
	if err := EnsureSkillRepository(repository); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlinked parent accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(actual, "nested")); !os.IsNotExist(err) {
		t.Fatalf("created through symlinked parent: %v", err)
	}
}

func TestDeploySelectedSkillsFansOutAndReportsPartialFailure(t *testing.T) {
	original := uploadManagedSkill
	t.Cleanup(func() { uploadManagedSkill = original })
	var calls []string
	boom := errors.New("transfer failed")
	uploadManagedSkill = func(_ context.Context, client Client, repository, identifier, target string) (SkillInfo, error) {
		calls = append(calls, client.Name+":"+identifier+":"+target+":"+repository)
		if target == "/second" {
			return SkillInfo{}, boom
		}
		return SkillInfo{Identifier: identifier}, nil
	}
	repository := filepath.Join(t.TempDir(), "skills")
	if err := EnsureSkillRepository(repository); err != nil {
		t.Fatal(err)
	}
	for _, identifier := range []string{"one", "two"} {
		dir := filepath.Join(repository, identifier)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(identifier), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{Clients: []Client{{
		Name: "alpha", Host: "example.test",
		SkillTargets:   []SkillInstallTarget{{ID: "one", Path: "/first"}, {ID: "two", Path: "/second"}},
		SelectedSkills: []string{"one", "two"},
	}}}
	results, err := DeploySelectedSkills(context.Background(), cfg, "alpha", repository)
	if err == nil || !strings.Contains(err.Error(), `upload skill "one" to target "two"`) {
		t.Fatalf("error=%v", err)
	}
	if len(results) != 1 || results[0].Identifier != "one" {
		t.Fatalf("partial results=%+v", results)
	}
	if len(calls) != 2 || !strings.HasSuffix(calls[0], ":/first:"+repository) || !strings.HasSuffix(calls[1], ":/second:"+repository) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestDeploySelectedSkillsPreflightsBeforeUploading(t *testing.T) {
	original := uploadManagedSkill
	t.Cleanup(func() { uploadManagedSkill = original })
	calls := 0
	uploadManagedSkill = func(context.Context, Client, string, string, string) (SkillInfo, error) {
		calls++
		return SkillInfo{}, nil
	}
	repository := filepath.Join(t.TempDir(), "skills")
	if err := EnsureSkillRepository(repository); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, "present"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "present", "SKILL.md"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Clients: []Client{{Name: "alpha", Host: "example.test", SkillTargets: []SkillInstallTarget{{ID: "one", Path: "/first"}}, SelectedSkills: []string{"present", "missing"}}}}
	if _, err := DeploySelectedSkills(context.Background(), cfg, "alpha", repository); err == nil || !strings.Contains(err.Error(), `selected skill "missing"`) {
		t.Fatalf("preflight err=%v", err)
	}
	if calls != 0 {
		t.Fatalf("uploads started before preflight: %d", calls)
	}
}

func TestDeploySelectedSkillsUsesDefaultRepositoryWhenEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repository := DefaultSkillRepository()
	if err := EnsureSkillRepository(repository); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, "present"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "present", "SKILL.md"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	original := uploadManagedSkill
	t.Cleanup(func() { uploadManagedSkill = original })
	var gotRepository string
	uploadManagedSkill = func(_ context.Context, _ Client, actualRepository, _, _ string) (SkillInfo, error) {
		gotRepository = actualRepository
		return SkillInfo{Identifier: "present"}, nil
	}
	cfg := &Config{Clients: []Client{{
		Name: "alpha", Host: "example.test",
		SkillTargets:   []SkillInstallTarget{{ID: "default", Path: "/skills"}},
		SelectedSkills: []string{"present"},
	}}}
	if _, err := DeploySelectedSkills(context.Background(), cfg, "alpha", ""); err != nil {
		t.Fatal(err)
	}
	if gotRepository != repository {
		t.Fatalf("upload repository=%q, want %q", gotRepository, repository)
	}
}

func TestDeployTargetSkillsUsesOnlySelectedTargetPushState(t *testing.T) {
	original := uploadManagedSkill
	t.Cleanup(func() { uploadManagedSkill = original })
	var calls []string
	uploadManagedSkill = func(_ context.Context, client Client, repository, identifier, target string) (SkillInfo, error) {
		calls = append(calls, client.Name+":"+identifier+":"+target+":"+repository)
		return SkillInfo{Identifier: identifier}, nil
	}
	repository := filepath.Join(t.TempDir(), "skills")
	if err := EnsureSkillRepository(repository); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"push-here", "pull-here", "other-agent"} {
		if err := os.Mkdir(filepath.Join(repository, id), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository, id, "SKILL.md"), []byte(id), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{Clients: []Client{{
		Name: "alpha", Host: "example.test",
		SkillTargets: []SkillInstallTarget{
			{ID: "codex", Path: "/codex", Skills: []ManagedHostSkill{{ID: "push-here", Management: "push"}, {ID: "pull-here", Management: "pull"}}},
			{ID: "claude", Path: "/claude", Skills: []ManagedHostSkill{{ID: "other-agent", Management: "push"}}},
		},
	}}}
	results, err := DeployTargetSkills(context.Background(), cfg, "alpha", "codex", repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Identifier != "push-here" {
		t.Fatalf("results=%+v", results)
	}
	want := []string{"alpha:push-here:/codex:" + repository}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestDeployTargetSkillsExplicitNoneOverridesLegacySelection(t *testing.T) {
	original := uploadManagedSkill
	t.Cleanup(func() { uploadManagedSkill = original })
	calls := 0
	uploadManagedSkill = func(_ context.Context, _ Client, _, _, _ string) (SkillInfo, error) {
		calls++
		return SkillInfo{}, nil
	}
	repository := filepath.Join(t.TempDir(), "skills")
	if err := EnsureSkillRepository(repository); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Clients: []Client{{
		Name: "alpha", Host: "example.test", SelectedSkills: []string{"legacy-push"},
		SkillTargets: []SkillInstallTarget{{ID: "codex", Path: "/codex", Skills: []ManagedHostSkill{{ID: "legacy-push", Management: "none"}}}},
	}}}
	results, err := DeployTargetSkills(context.Background(), cfg, "alpha", "codex", repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 || calls != 0 {
		t.Fatalf("explicit none deployed results=%+v calls=%d", results, calls)
	}
}
