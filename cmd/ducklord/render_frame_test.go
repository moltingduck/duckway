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

func TestFrameOutputUpdatesRowsWithoutErasingScreen(t *testing.T) {
	var output frameOutput
	var out strings.Builder
	first := "\033[?25l\033[H\033[2Jhello\n\033[31m世界\033[0m\nlast"
	output.write(&out, first, 12, 4)
	screen := ducklord.NewTerminal(4, 12, 0)
	screen.Write([]byte(strings.ReplaceAll(out.String(), "\n", "\r\n")))
	out.Reset()
	output.write(&out, first, 12, 4)
	if out.Len() != 0 {
		t.Fatal("unchanged frame redrawn")
	}
	second := "\033[?25l\033[H\033[2Jhi\n\033[32m界\033[0m\nlast"
	output.write(&out, second, 12, 4)
	if strings.Contains(out.String(), "\033[2J") || strings.Contains(out.String(), "last") {
		t.Fatal("unchanged rows or full screen redrawn")
	}
	screen.Write([]byte(out.String()))
	want := ducklord.NewTerminal(4, 12, 0)
	want.Write([]byte(strings.ReplaceAll(second, "\n", "\r\n")))
	if strings.Join(screen.RenderLines(4, 12), "\n") != strings.Join(want.RenderLines(4, 12), "\n") {
		t.Fatalf("incremental output differs: %q", screen.RenderLines(4, 12))
	}
	out.Reset()
	output.write(&out, second, 14, 5)
	if !strings.Contains(out.String(), "\033[2J") {
		t.Fatal("resize did not reset screen")
	}
}

type shortFrameWriter struct{}

func (shortFrameWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestFrameOutputRetriesAfterShortWrite(t *testing.T) {
	var output frameOutput
	output.write(shortFrameWriter{}, "\033[H\033[2Jhello", 12, 4)
	var out strings.Builder
	output.write(&out, "\033[H\033[2Jhello", 12, 4)
	if !strings.Contains(out.String(), "\033[2J") {
		t.Fatal("short write cached incomplete screen")
	}
}
