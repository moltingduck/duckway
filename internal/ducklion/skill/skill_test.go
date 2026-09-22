package skill

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsSymlinkTarget(t *testing.T) {
	r := t.TempDir()
	real := filepath.Join(r, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"list", link}, bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted symlink target")
	}
}

func makeSkill(t *testing.T, root, id string) {
	t.Helper()
	d := filepath.Join(root, id)
	if err := os.MkdirAll(d, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("skill"), 0600); err != nil {
		t.Fatal(err)
	}
}

func tarData(t *testing.T, entries int, size int64) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i := 0; i < entries; i++ {
		name := fmt.Sprintf("skill/file-%d", i)
		if i == 0 {
			name = "skill/SKILL.md"
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: size}); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := io.CopyN(tw, strings.NewReader(strings.Repeat("x", int(size))), size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func TestSkillRejectsNonSkillPathsAndSymlinkManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := deleteSkillRoot(root, "file"); err == nil {
		t.Fatal("delete accepted regular file")
	}
	if err := os.Mkdir(filepath.Join(root, "link-skill"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(root, "link-skill", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	var listed bytes.Buffer
	if err := Run([]string{"list", root}, nil, &listed, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listed.String(), "link-skill") {
		t.Fatal("listed symlinked SKILL.md")
	}
	if err := Run([]string{"download", root, "link-skill"}, nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("download accepted symlinked SKILL.md")
	}
}

func deleteSkillRoot(root, id string) error {
	return Run([]string{"delete", root, id}, nil, &bytes.Buffer{}, &bytes.Buffer{})
}

func TestSkillUploadDoesNotReplaceNonSkill(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "skill"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"upload", root, "skill"}, tarData(t, 1, 1), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("upload replaced non-skill path")
	}
	if got, _ := os.ReadFile(filepath.Join(root, "skill")); string(got) != "keep" {
		t.Fatalf("non-skill path changed: %q", got)
	}
}

func TestSkillUploadRejectsArchiveLimits(t *testing.T) {
	root := t.TempDir()
	if err := Run([]string{"upload", root, "skill"}, tarData(t, maxFiles+1, 0), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted archive over entry limit")
	}
	if err := Run([]string{"upload", root, "skill"}, tarData(t, 1, maxBytes+1), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted archive over byte limit")
	}
}
