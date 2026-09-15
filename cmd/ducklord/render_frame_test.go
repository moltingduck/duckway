package main

import (
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

type frameRecorder struct{ writes []string }

func (w *frameRecorder) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

func TestTUIRenderPublishesCompleteFrame(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		s := &tuiState{cfg: &ducklord.Config{}, workspacePreview: workspace}
		out := &frameRecorder{}
		s.render(out)
		if len(out.writes) != 1 {
			t.Fatalf("workspace=%v: got %d writes, exposing partial redraws", workspace, len(out.writes))
		}
		frame := out.writes[0]
		if !strings.HasPrefix(frame, "\x1b[?2026h") || !strings.HasSuffix(frame, "\x1b[?2026l") {
			t.Fatalf("workspace=%v: unbalanced synchronized frame", workspace)
		}
		if !strings.Contains(frame, "\x1b[2J") || !strings.Contains(frame, "ducklord") {
			t.Fatal("missing clear or content")
		}
	}
}

func TestTUIRenderCopyModeDoesNotStartFrame(t *testing.T) {
	s := &tuiState{copyMode: true}
	out := &frameRecorder{}
	s.render(out)
	if len(out.writes) != 0 {
		t.Fatal("copy mode emitted a frame")
	}
}
