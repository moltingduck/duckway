package main

import (
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestSessionHandleRenameModalValidatesAndEscapes(t *testing.T) {
	target := ducklord.RemoteSession{Client: "host", InstanceID: "instance", SessionID: "session", Name: "old"}
	state := &tuiState{}
	state.beginSessionHandleRename(target)
	state.sessionRenameLine = ""
	if action := state.handleSessionHandleRenameInput([]byte("\r")); action != "" || state.sessionRenameErr == "" {
		t.Fatalf("invalid handle accepted: action=%q error=%q", action, state.sessionRenameErr)
	}
	state.sessionRenameLine = "new-handle"
	if action := state.handleSessionHandleRenameInput([]byte("\r")); action != "rename-submit" {
		t.Fatalf("valid handle action=%q", action)
	}
	state.handleSessionHandleRenameInput([]byte("\x1b"))
	if state.sessionRenameMode || state.sessionRenameTarget.SessionID != "" {
		t.Fatal("escape did not close and clear rename modal")
	}
}
