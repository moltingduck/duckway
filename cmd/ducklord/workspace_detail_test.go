package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestDetailedSessionModeSearchPreviewAndJumpPreserveNormalLocation(t *testing.T) {
	instance := "9df68174-9e13-4dc9-b44d-8532c87f5971"
	a := ducklord.SessionIdentity{InstanceID: instance, SessionID: "AAA111"}
	b := ducklord.SessionIdentity{InstanceID: instance, SessionID: "BBB222"}
	activity := ducklord.NewActivityState()
	projectA, err := activity.ProjectLayout.AddProject("Alpha")
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := activity.ProjectLayout.AddProject("專案乙")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = activity.ProjectLayout.Place(projectA, a, ducklord.PlaceNewTab, "")
	_, _ = activity.ProjectLayout.Place(projectB, b, ducklord.PlaceNewTab, "")
	state := &tuiState{workspacePreview: true, cfg: &ducklord.Config{}, activityState: activity,
		sessions: []ducklord.RemoteSession{
			{Client: "host-a", InstanceID: instance, SessionID: a.SessionID, Name: "Codex", Status: "running", Kind: "shell"},
			{Client: "host-b", InstanceID: instance, SessionID: b.SessionID, Name: "Claude", Status: "running", Kind: "shell"},
		}}
	nav, err := state.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if nav.CurrentProjectID() != projectA {
		t.Fatal("normal workspace did not begin in selected Session's Project")
	}
	if action := state.handleDetailedInput([]byte("D")); action != "changed" || !nav.InDetailMode() {
		t.Fatalf("enter detailed mode: %q", action)
	}
	if action := state.handleDetailedInput([]byte("/")); action != "changed" || !state.detailSearchFocused {
		t.Fatalf("focus search: %q", action)
	}
	for _, key := range []string{"專", "案"} {
		state.handleDetailedInput([]byte(key))
	}
	if results := state.detailedResults(); len(results) != 1 || results[0].Identity != b || state.detailSelected != b {
		t.Fatalf("Unicode Project search results=%+v selected=%+v", results, state.detailSelected)
	}
	if nav.CurrentProjectID() != projectA || state.focused || state.activeAttachKey != "" {
		t.Fatal("search preview changed normal Project or granted PTY control")
	}
	var output bytes.Buffer
	state.renderWorkspacePreviewAt(&output, 120, 24)
	if !strings.Contains(output.String(), "SESSIONS") || !strings.Contains(output.String(), "Claude") || strings.Contains(output.String(), "TERMINAL TAB") {
		t.Fatal("detailed mode did not render the single-Session preview")
	}
	state.handleDetailedInput([]byte("\x1b")) // clear query first
	if state.detailQuery != "" || !state.detailSearchFocused {
		t.Fatal("Escape did not clear the query first")
	}
	state.handleDetailedInput([]byte("\x1b"))
	if state.detailSearchFocused {
		t.Fatal("second Escape did not return to detailed list")
	}
	state.handleDetailedInput([]byte("D"))
	if nav.InDetailMode() || nav.CurrentProjectID() != projectA || state.detailQuery != "" {
		t.Fatal("leaving detailed mode did not restore the normal workspace")
	}
	state.handleDetailedInput([]byte("D"))
	state.handleDetailedInput([]byte("j"))
	if state.detailSelected != b {
		t.Fatal("moving detailed selection did not preview the next Session")
	}
	if action := state.handleDetailedInput([]byte("g")); action != "jump" || nav.InDetailMode() || nav.CurrentProjectID() != projectB {
		t.Fatalf("g did not jump to selected Session's Project: %q project=%q", action, nav.CurrentProjectID())
	}
}
