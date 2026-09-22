package ducklord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const sInstance = "11111111-1111-4111-8111-111111111111"

func transferProject() LocalProject {
	return LocalProject{ID: uuid.NewString(), Name: "demo", Tabs: []TerminalTab{{ID: uuid.NewString(), Root: &SessionPane{ID: uuid.NewString(), Session: &SessionIdentity{InstanceID: sInstance, SessionID: "ABC123"}}}}}
}

func TestProjectTransferOutputMetadataOnly(t *testing.T) {
	b, err := ExportProject(transferProject(), nil, nil, []ProjectOutputBookmark{{Session: SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}, OutputAnchor: "42"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "output_text") || strings.Contains(string(b), "terminal_output") {
		t.Fatal("raw output leaked")
	}
}

func TestProjectTransferRejectsTrailingData(t *testing.T) {
	b, err := ExportProject(transferProject(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProjectTransfer(append(b, []byte("{}")...)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func TestWriteProjectTransferUsesUniqueTemporaryName(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "export.json")
	if err := os.WriteFile(filepath.Join(d, ".project-transfer-fixed"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteProjectTransfer(path, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "{}" {
		t.Fatalf("write result: %q, %v", got, err)
	}
}

func TestWriteProjectTransferRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	parent := filepath.Join(root, "exports")
	if err := os.Symlink(outside, parent); err != nil {
		t.Skip("symlinks unsupported")
	}
	if err := WriteProjectTransfer(filepath.Join(parent, "export.json"), []byte("{}")); err == nil {
		t.Fatal("accepted symlink parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "export.json")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was modified: %v", err)
	}
}

func TestProjectTransferBookmarkDuplicatePolicy(t *testing.T) {
	p := transferProject()
	s := *p.Tabs[0].Root.Session
	if _, err := ExportProject(p, nil, nil, []ProjectOutputBookmark{{Session: s, Label: "a"}, {Session: s, Label: "b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportProject(p, nil, nil, []ProjectOutputBookmark{{Session: s, Label: "a"}, {Session: s, Label: "a"}}); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestProjectTransferRejectsNonmemberMetadataAndDefault(t *testing.T) {
	p := transferProject()
	other := SessionIdentity{InstanceID: sInstance, SessionID: "OTHER"}
	if _, err := ExportProject(p, nil, []ProjectSessionNotes{{Session: other}}, nil); err == nil {
		t.Fatal("nonmember notes accepted")
	}
	if _, err := ExportProject(p, nil, nil, []ProjectOutputBookmark{{Session: other}}); err == nil {
		t.Fatal("nonmember bookmark accepted")
	}
	p.ID = DefaultProjectID
	if _, err := ExportProject(p, nil, nil, nil); err == nil {
		t.Fatal("default export accepted")
	}
}

func TestProjectTransferDecodeSchemaAndPreview(t *testing.T) {
	b, _ := ExportProject(transferProject(), nil, nil, nil)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	raw["x"] = 1
	unknown, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProjectTransfer(unknown); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := DecodeProjectTransfer([]byte(`{"version":99}`)); err == nil {
		t.Fatal("future version accepted")
	}
	p := transferProject()
	a := *p.Tabs[0].Root.Session
	z := SessionIdentity{InstanceID: sInstance, SessionID: "ZZZ999"}
	y := SessionIdentity{InstanceID: sInstance, SessionID: "AAA111"}
	p.Tabs[0].Root.Session = &z
	p.Tabs = append(p.Tabs, TerminalTab{ID: uuid.NewString(), Root: &SessionPane{ID: uuid.NewString(), Session: &y}})
	b, err = ExportProject(p, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewProjectImport(b, map[SessionIdentity]bool{}, map[string]bool{"demo": true})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.NameCollision || len(preview.Unavailable) != 2 || preview.Unavailable[0] != y || preview.Unavailable[1] != z || a == z {
		t.Fatalf("bad preview: %+v", preview)
	}
}
