package main

import (
	"context"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestPanePrefixAcrossPendingControlCompletion(t *testing.T) {
	for _, suffix := range []string{"-", "\\", "t", ","} {
		t.Run(suffix, func(t *testing.T) {
			s := &tuiState{cfg: &ducklord.Config{}, workspacePreview: true, activeAttachKey: "target"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			openCancel := cancel
			var control *ducklord.ControlSession
			controlID := 7
			prefix := []byte{2}
			if !handlePendingPTYInput(s, prefix, true, &control, &openCancel, &controlID) {
				t.Fatal("opening control did not gate input")
			}
			// Mirror the input loop's pending-focus branch, then deliver a
			// successful completion between the prefix and its command key.
			s.handlePanePrefix(prefix)
			openCancel = nil
			s.focused = true
			if handlePendingPTYInput(s, []byte(suffix), true, &control, &openCancel, &controlID) {
				t.Fatal("completed control still gated input")
			}
			consumed, command := s.handlePanePrefix([]byte(suffix))
			if !consumed || command != suffix || s.panePrefixPending {
				t.Fatalf("command could leak to PTY after completion: consumed=%t command=%q", consumed, command)
			}
			if ctx.Err() != nil || controlID != 7 || s.activeAttachKey != "target" {
				t.Fatal("prefix altered control ownership")
			}
		})
	}
}

func TestPanePrefixPendingControlCancellationClearsCommand(t *testing.T) {
	s := &tuiState{cfg: &ducklord.Config{}, workspacePreview: true, activeAttachKey: "target"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	openCancel := cancel
	var control *ducklord.ControlSession
	controlID := 7
	s.handlePanePrefix([]byte{2})
	if !handlePendingPTYInput(s, []byte{27}, true, &control, &openCancel, &controlID) {
		t.Fatal("pending focus cancellation was not consumed")
	}
	s.handlePanePrefix([]byte{27})
	if s.panePrefixPending || ctx.Err() != context.Canceled || controlID == 7 {
		t.Fatal("cancellation left prefix armed or control unfenced")
	}
	if consumed, _ := s.handlePanePrefix([]byte("t")); consumed {
		t.Fatal("canceled prefix activated a command on a later target")
	}
}
