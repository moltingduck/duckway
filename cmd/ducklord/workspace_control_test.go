package main

import "testing"

func TestWorkspaceControlMayAcceptRejectsNewSessionWizard(t *testing.T) {
	state := &tuiState{}
	if !state.workspaceControlMayAccept() {
		t.Fatal("baseline workspace control gate should accept an unfocused state")
	}
	state.newSessionMode = true
	if state.workspaceControlMayAccept() {
		t.Fatal("new session wizard must block workspace control acceptance")
	}
}

func TestWorkspaceControlCannotReacquireFocusWhileNotesOpen(t *testing.T) {
	state := &tuiState{workspacePaneMode: true, workspacePaneStep: "notes"}
	if state.workspaceControlMayAccept() {
		t.Fatal("workspace control must not reacquire focus while Notes is open")
	}

	state.workspacePaneMode = false
	state.workspacePaneStep = ""
	if !state.workspaceControlMayAccept() {
		t.Fatal("workspace control should be eligible after Notes closes")
	}
}
