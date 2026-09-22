package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestPendingPTYInputRejectsNavigationBeforeControlOpens(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		s := &tuiState{cfg: &ducklord.Config{}, activeAttachKey: "target", selected: 2}
		ctx, cancel := context.WithCancel(context.Background())
		openCancel := cancel
		var control *ducklord.ControlSession
		id := 7
		for _, key := range []string{"b", "j", "/", "\r", "q", "\x1b[<0;10;10M"} {
			if !handlePendingPTYInput(s, []byte(key), workspace, &control, &openCancel, &id) {
				t.Fatalf("workspace=%t pending-open input %q escaped to navigation", workspace, key)
			}
			if !strings.Contains(s.outputErr, "input was not sent") || s.activeAttachKey != "target" || s.selected != 2 || s.workspaceProjectFocus || id != 7 || ctx.Err() != nil {
				t.Fatalf("pending-open input changed target or canceled request: key=%q error=%q", key, s.outputErr)
			}
		}
		cancel()
	}
}

func TestPendingPTYInputCancellationFencesLateCompletion(t *testing.T) {
	for _, key := range []string{"\x1d", "\x1b", "\x03"} {
		for _, opened := range []bool{false, true} {
			s := &tuiState{cfg: &ducklord.Config{}, activeAttachKey: "target"}
			ctx, cancel := context.WithCancel(context.Background())
			openCancel := cancel
			var control *ducklord.ControlSession
			reader, writer := io.Pipe()
			if opened {
				openCancel = nil
				control = &ducklord.ControlSession{Stdin: writer}
			}
			id := 7
			late := controlOpenEvent{id: id, key: s.activeAttachKey}
			if !handlePendingPTYInput(s, []byte(key), true, &control, &openCancel, &id) {
				t.Fatalf("opened=%t cancellation %q escaped pending focus", opened, key)
			}
			if control != nil || openCancel != nil || s.activeAttachKey != "" || s.focused || late.id == id || late.key == s.activeAttachKey {
				t.Fatalf("opened=%t canceled request can still accept late completion", opened)
			}
			if !opened && ctx.Err() != context.Canceled {
				t.Fatal("pending open context was not canceled")
			}
			if opened {
				if _, err := writer.Write([]byte("unexpected")); err != io.ErrClosedPipe {
					t.Fatalf("canceled control writer remains open: %v", err)
				}
			}
			if handlePendingPTYInput(s, []byte("b"), true, &control, &openCancel, &id) {
				t.Fatal("canceled focus still blocks navigation")
			}
			cancel()
			reader.Close()
			writer.Close()
		}
	}
}

func TestPendingPTYInputRejectsUntilOutputReady(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}, activeAttachKey: "target"}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	control := &ducklord.ControlSession{Stdin: writer}
	var openCancel context.CancelFunc
	id := 7
	if !handlePendingPTYInput(s, []byte("j"), true, &control, &openCancel, &id) || !strings.Contains(s.outputErr, "input was not sent") {
		t.Fatal("pending output did not reject input")
	}
	s.focused = true
	if handlePendingPTYInput(s, []byte("j"), true, &control, &openCancel, &id) {
		t.Fatal("ready control did not resume ordinary focused input")
	}
}

func TestPendingPTYInputYieldsToNewSessionWizard(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}, newSessionMode: true}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	control := &ducklord.ControlSession{Stdin: writer}
	var openCancel context.CancelFunc
	id := 7

	if handlePendingPTYInput(s, []byte("\r"), true, &control, &openCancel, &id) {
		t.Fatal("new-session wizard input was consumed by the stale PTY lease")
	}
	if control == nil || id != 7 {
		t.Fatal("new-session wizard input changed the stale PTY lease")
	}
}

func TestPendingPTYInputYieldsToProjectFilesModal(t *testing.T) {
	s := &tuiState{
		cfg:             &ducklord.Config{},
		activeAttachKey: "target",
		projectFiles:    projectFilesState{open: true},
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	control := &ducklord.ControlSession{Stdin: writer}
	var openCancel context.CancelFunc
	id := 7

	if handlePendingPTYInput(s, []byte("\x03"), true, &control, &openCancel, &id) {
		t.Fatal("project-files cancel was consumed by the stale PTY lease")
	}
	if control == nil || s.activeAttachKey != "target" || id != 7 {
		t.Fatal("project-files cancel changed the originating PTY lease")
	}
}

func TestPendingPTYSessionRemovalBeforeControlCompletionRestoresNavigation(t *testing.T) {
	s, _, removed, remaining := workspacePaneTestState(t)
	s.hostSync = make(map[string]ducklord.SessionUpdate)
	s.activeAttachKey = sessionKey(removed)
	previousActive := s.effectiveAttachKey()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	openCancel := cancel
	var control *ducklord.ControlSession
	id := 7
	late := controlOpenEvent{id: id, key: previousActive}
	if !s.applySessionUpdate(ducklord.SessionUpdate{Client: removed.Client, InstanceID: removed.InstanceID,
		State: "live", Generation: 1, Revision: 1, Sessions: []ducklord.RemoteSession{remaining}}) {
		t.Fatal("inventory removal was rejected")
	}
	if !cancelRemovedPTYControl(s, previousActive, &control, &openCancel, &id) {
		t.Fatal("removed Session did not invalidate its pending control")
	}
	if ctx.Err() != context.Canceled || openCancel != nil || late.id == id || s.effectiveAttachKey() != "" {
		t.Fatal("removed Session left pending control or allowed its late completion")
	}
	// The stale completion is discarded by ID, so it cannot release the
	// pending-input gate: removal itself must already have released it.
	for _, key := range []string{"j", "b", "q"} {
		if handlePendingPTYInput(s, []byte(key), true, &control, &openCancel, &id) {
			t.Fatalf("navigation %q remained blocked after Session removal", key)
		}
	}
}
