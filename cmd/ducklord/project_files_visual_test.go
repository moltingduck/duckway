package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestProjectFilesHistoryIsBoundedAndProjectScoped(t *testing.T) {
	s := &tuiState{projectFilesHistory: map[string][]projectFilesBatch{}}
	s.projectFiles.historyProject = "project-a"
	for i := 0; i < 21; i++ {
		s.projectFilesRememberBatch(projectFilesBatch{at: time.Unix(int64(i), 0), names: []string{fmt.Sprint(i)}})
	}
	a := s.projectFilesHistory["project-a"]
	if len(a) != 20 || a[0].names[0] != "20" || a[19].names[0] != "1" {
		t.Fatalf("bounded history = %#v", a)
	}
	s.projectFiles.historyProject = "project-b"
	s.projectFilesRememberBatch(projectFilesBatch{names: []string{"other"}})
	if len(s.projectFilesHistory["project-b"]) != 1 || len(s.projectFilesHistory["project-a"]) != 20 {
		t.Fatal("history leaked across projects")
	}
	// Opening a new modal state for the same project reads the preserved map.
	reopened := &tuiState{projectFilesHistory: s.projectFilesHistory}
	reopened.projectFiles.historyProject = "project-a"
	reopened.projectFiles.history = reopened.projectFilesHistory[reopened.projectFiles.historyProject]
	if reopened.projectFiles.history[0].names[0] != "20" {
		t.Fatal("same-project history did not survive reopen")
	}
}

func TestProjectFilesGeometryWideStackedAndUnicode(t *testing.T) {
	if projectFilesVisibleOffset(11, 5) != 7 || projectFilesVisibleOffset(2, 5) != 0 {
		t.Fatal("visible offset is not shared scroll logic")
	}
	_, _, wide := projectFilesGeometry(150, 32, 1)
	if wide[0].rows <= 0 || wide[1].entriesStart != wide[0].entriesStart || wide[1].left <= wide[0].right {
		t.Fatalf("wide geometry: %#v", wide)
	}
	width, _, narrow := projectFilesGeometry(42, 22, 1)
	if width >= 58 || narrow[1].start <= narrow[0].start || narrow[0].rows <= 0 {
		t.Fatalf("stacked geometry: width=%d %#v", width, narrow)
	}
	got := projectClip("資料📁長い名前.txt", 8)
	if modalCellWidth(got) > 8 {
		t.Fatalf("unicode clip width %d: %q", modalCellWidth(got), got)
	}
}

func TestProjectFilesLatestMarksOnlyCopiedAtExactEndpoint(t *testing.T) {
	source, destination := ducklord.FileEndpoint{Path: "/src"}, ducklord.FileEndpoint{Path: "/dst"}
	s := &tuiState{}
	s.projectFiles.history = []projectFilesBatch{{source: source, destination: destination, items: []projectFilesItem{{name: "renamed.txt", destination: "/dst/renamed.txt", state: "copied"}, {name: "skip.txt", destination: "/dst/skip.txt", state: "skipped"}}}}
	if got := s.projectFilesLatestMark(projectFilesPane{endpoint: destination}, ducklord.FileEntry{Name: "renamed.txt"}); !strings.Contains(got, "Received") {
		t.Fatalf("received mark = %q", got)
	}
	if got := s.projectFilesLatestMark(projectFilesPane{endpoint: destination}, ducklord.FileEntry{Name: "skip.txt"}); got != "" {
		t.Fatalf("skipped file got success mark %q", got)
	}
	if got := s.projectFilesLatestMark(projectFilesPane{endpoint: ducklord.FileEndpoint{Path: "/elsewhere"}}, ducklord.FileEntry{Name: "renamed.txt"}); got != "" {
		t.Fatalf("unrelated path got mark %q", got)
	}
	changed := &ducklord.Client{Name: "remote", Host: "new.example"}
	s.projectFiles.history[0].source = ducklord.FileEndpoint{Client: &ducklord.Client{Name: "remote", Host: "old.example"}, Path: "/src"}
	if got := s.projectFilesLatestMark(projectFilesPane{endpoint: ducklord.FileEndpoint{Client: changed, Path: "/src"}}, ducklord.FileEntry{Name: "renamed.txt"}); got != "" {
		t.Fatalf("replacement endpoint got stale mark %q", got)
	}
}

func TestProjectFilesPartialFailureAndStaleProgress(t *testing.T) {
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, generation: 5, step: "busy", historyProject: "p", items: []projectFilesItem{{name: "active", state: "copying"}, {name: "later", state: "queued"}}}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 4, copy: true, progress: &ducklord.FileCopyProgress{Name: "active", State: "copied"}})
	if s.projectFiles.items[0].state != "copying" {
		t.Fatal("stale progress mutated current batch")
	}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 5, copy: true, err: fmt.Errorf("network stopped")})
	if s.projectFiles.items[0].state != "failed" || s.projectFiles.items[1].state != "not started" {
		t.Fatalf("partial outcomes: %#v", s.projectFiles.items)
	}
}

func TestProjectFilesHistorySelectionScrollsAndRendererScopesANSI(t *testing.T) {
	s := &tuiState{}
	items := make([]projectFilesItem, 12)
	for i := range items {
		items[i] = projectFilesItem{name: fmt.Sprintf("item-%02d", i), state: "copied"}
	}
	s.projectFiles.history = []projectFilesBatch{{sourceLabel: "LOCAL", destinationLabel: "REMOTE", items: items}}
	s.projectFiles.historyItem = 11
	lines := s.projectFilesHistoryLines(70, 11)
	joined := ""
	for _, line := range lines {
		joined += line.text + "\n"
	}
	if !strings.Contains(joined, "item-11") || strings.Contains(joined, "item-00") {
		t.Fatalf("history selection did not scroll into view: %s", joined)
	}

	var generic, exchange strings.Builder
	renderModalBoxWidth(&generic, 80, 20, 40, []modalRenderLine{{text: "raw\x1b[8mhidden"}})
	if strings.Contains(generic.String(), "\x1b[8m") {
		t.Fatal("generic modal renderer passed through injected SGR")
	}
	renderModalBoxWidthANSI(&exchange, 80, 20, 40, []modalRenderLine{{text: projectFilesPanelColor(0, true) + "safe" + modalReset}})
	if !strings.Contains(exchange.String(), projectFilesPanelColor(0, true)) {
		t.Fatal("trusted exchange renderer stripped panel color")
	}
}

func TestProjectFilesCompletionAckWaitsForConsumerAfterCopyCancel(t *testing.T) {
	lifecycle, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan projectFilesEvent, 1)
	done <- projectFilesEvent{progress: &ducklord.FileCopyProgress{State: "copied"}}
	ack := projectFilesEvent{copy: true, generation: 9, err: context.Canceled}
	sent := make(chan struct{})
	go func() { sendProjectFilesCompletion(lifecycle, done, ack); close(sent) }()
	select {
	case <-sent:
		t.Fatal("ack unexpectedly dropped or bypassed full event channel")
	case <-time.After(20 * time.Millisecond):
	}
	<-done
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("completion acknowledgement remained blocked")
	}
	if got := <-done; got.generation != 9 || !got.copy || got.err != context.Canceled {
		t.Fatalf("completion acknowledgement = %#v", got)
	}
}

func TestProjectFilesCompletionAckStopsWhenAppLifecycleEnds(t *testing.T) {
	lifecycle, stop := context.WithCancel(context.Background())
	done := make(chan projectFilesEvent)
	stop()
	finished := make(chan struct{})
	go func() { sendProjectFilesCompletion(lifecycle, done, projectFilesEvent{copy: true}); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("sender did not exit after application lifecycle ended")
	}
}

func TestProjectFilesNarrowRenderKeepsSelectedRowsAndPanelColors(t *testing.T) {
	entries := make([]ducklord.FileEntry, 12)
	for i := range entries {
		entries[i] = ducklord.FileEntry{Name: fmt.Sprintf("file-%02d", i)}
	}
	s := &tuiState{}
	s.projectFiles.open, s.projectFiles.step = true, "browse"
	s.projectFiles.left = projectFilesPane{label: "LOCAL", endpoint: ducklord.FileEndpoint{Path: "/tmp/source"}, entries: entries, selected: 11, marked: map[string]bool{}}
	s.projectFiles.right = projectFilesPane{label: "DEST", endpoint: ducklord.FileEndpoint{Path: "/tmp/destination"}, entries: entries, selected: 11, marked: map[string]bool{}}
	var out strings.Builder
	s.renderProjectFilesModal(&out, 50, 32)
	view := out.String()
	if !strings.Contains(view, "LEFT") || !strings.Contains(view, "RIGHT") || !strings.Contains(view, "file-11") {
		t.Fatalf("narrow viewport omitted panel/selected entry: %q", view)
	}
	if strings.Count(view, "│") < 4 || !strings.Contains(view, projectFilesPanelColor(0, true)) || !strings.Contains(view, projectFilesPanelColor(1, false)) {
		t.Fatalf("independent panel frames/colors missing: %q", view)
	}
}

func TestProjectFilesScrolledMouseRegionsMatchVisibleRowsWideAndStacked(t *testing.T) {
	for _, cols := range []int{50, 150} {
		t.Run(fmt.Sprint(cols), func(t *testing.T) {
			entries := make([]ducklord.FileEntry, 20)
			for i := range entries {
				entries[i] = ducklord.FileEntry{Name: fmt.Sprintf("item-%02d", i)}
			}
			s := &tuiState{}
			s.projectFiles.open, s.projectFiles.step = true, "browse"
			s.projectFiles.left = projectFilesPane{label: "LOCAL", endpoint: ducklord.FileEndpoint{Path: "/src"}, entries: entries, selected: 19, marked: map[string]bool{}}
			s.projectFiles.right = projectFilesPane{label: "REMOTE", endpoint: ducklord.FileEndpoint{Path: "/dst"}, entries: entries, selected: 17, marked: map[string]bool{}}
			var out strings.Builder
			s.renderProjectFilesModal(&out, cols, 32)
			for side, selected := range []int{19, 17} {
				foundSelected := false
				for _, region := range s.modalMouseRegions {
					if (side == 0 && region.action.selection == &s.projectFiles.left.selected) || (side == 1 && region.action.selection == &s.projectFiles.right.selected) {
						if region.action.index == selected {
							foundSelected = true
						}
					}
				}
				if !foundSelected {
					t.Fatalf("side %d selected item %d has no mouse region", side, selected)
				}
			}
		})
	}
}
