package ducklord

import (
	"bytes"
	"strings"
	"testing"
)

func TestWorkspaceTabHeadingShowsNameAndAddButton(t *testing.T) {
	layout := NewProjectLayout()
	projectID := layout.Projects[0].ID
	_, err := layout.Place(projectID, testLayoutIdentity("ABC123"), PlaceNewTab, "")
	if err != nil {
		t.Fatal(err)
	}
	layout.Projects[0].Tabs[0].Name = "工作"
	nav, err := NewWorkspaceState(&layout)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	RenderWorkspaceBodyWithOptions(&out, CalculateWorkspaceGeometry(120, 30, 4), &layout, nav, nil, nil, nil, WorkspaceRenderOptions{})
	if !strings.Contains(out.String(), "●工作  [+]") {
		t.Fatalf("tab name/add button missing: %q", out.String())
	}
}
