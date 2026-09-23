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

func TestProjectFilesCancellationLeavesQueuedItemsNotStarted(t *testing.T) {
	s := &tuiState{}
	s.projectFiles = projectFilesState{
		open:            true,
		generation:      6,
		step:            "busy",
		copyCancelling:  true,
		copyDestination: 1,
		items: []projectFilesItem{
			{name: "in-flight", state: "copying"},
			{name: "queued", state: "queued"},
		},
	}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 6, copy: true, err: context.Canceled})
	if got := s.projectFiles.items[0].state; got != "cancelled" {
		t.Fatalf("in-flight item state = %q, want cancelled", got)
	}
	if got := s.projectFiles.items[1].state; got != "not started" {
		t.Fatalf("queued item state = %q, want not started", got)
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

func TestProjectFilesCopyWorkerContextStopsWithApplicationLifecycle(t *testing.T) {
	lifecycle, shutdown := context.WithCancel(context.Background())
	s := &tuiState{projectFiles: projectFilesState{lifecycle: lifecycle}}
	ctx, cancel := s.projectFilesWorkerContext()
	defer cancel()
	shutdown()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("copy worker context remained live after application shutdown")
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

func TestProjectFilesPanelsUseCompleteBordersAndSeparateIdentityFields(t *testing.T) {
	entries := []ducklord.FileEntry{{Name: "one.txt"}}
	for _, cols := range []int{50, 150} {
		t.Run(fmt.Sprintf("cols-%d", cols), func(t *testing.T) {
			s := &tuiState{}
			s.projectFiles.open, s.projectFiles.step = true, "browse"
			s.projectFiles.left = projectFilesPane{label: "client-a", endpoint: ducklord.FileEndpoint{Path: "/left"}, entries: entries, query: "go", marked: map[string]bool{}}
			s.projectFiles.right = projectFilesPane{label: "PROJECT SHELF", endpoint: ducklord.FileEndpoint{Path: "/right"}, entries: entries, marked: map[string]bool{}}
			var out strings.Builder
			s.renderProjectFilesModal(&out, cols, 32)
			view := out.String()
			if strings.Count(view, "┌") != 2 || strings.Count(view, "└") != 2 {
				t.Fatalf("panels need one complete frame each: %q", view)
			}
			for _, want := range []string{"LEFT", "RIGHT", "Host: client-a", "Host: PROJECT SHELF", "Path: /left", "Path: /right", "1 visible · 0 marked", "Filter: go"} {
				if !strings.Contains(view, want) {
					t.Fatalf("panel metadata %q missing: %q", want, view)
				}
			}
		})
	}
}

func TestProjectFilesPanelIdentitySurvivesTerminalRendering(t *testing.T) {
	path := "/home/duck/exchange-a-source-" + strings.Repeat("9", 19)
	for _, cols := range []int{50, 150} {
		t.Run(fmt.Sprintf("cols-%d", cols), func(t *testing.T) {
			s := &tuiState{}
			s.projectFiles.open, s.projectFiles.step = true, "browse"
			s.projectFiles.left = projectFilesPane{label: "client-a", endpoint: ducklord.FileEndpoint{Path: path}, entries: []ducklord.FileEntry{{Name: "source.txt"}}, marked: map[string]bool{}}
			s.projectFiles.right = projectFilesPane{label: "client-b", endpoint: ducklord.FileEndpoint{Path: "/home/duck/target"}, entries: []ducklord.FileEntry{{Name: "target.txt"}}, marked: map[string]bool{}}
			var frame strings.Builder
			s.renderProjectFilesModal(&frame, cols, 32)

			screen := ducklord.NewTerminal(32, cols, 0)
			screen.Write([]byte(strings.ReplaceAll(frame.String(), "\n", "\r\n")))
			view := strings.Join(screen.RenderLines(32, cols), "\n")
			for _, want := range []string{"LEFT", "RIGHT", "Host: client-a", "Host: client-b", "source.txt", "target.txt"} {
				if !strings.Contains(view, want) {
					t.Fatalf("terminal omitted %q: %q", want, view)
				}
			}

			_, panelWidth, _ := projectFilesGeometry(cols, 32, 1)
			wantPath := projectClip("Path: "+path, panelWidth-2)
			if !strings.Contains(view, wantPath) {
				t.Fatalf("terminal path clipping differs from panel geometry: want %q in %q", wantPath, view)
			}
		})
	}
}

func TestProjectFilesLongPanelMetadataDoesNotPushOutRightHeader(t *testing.T) {
	s := &tuiState{}
	s.projectFiles.open, s.projectFiles.step = true, "browse"
	s.projectFiles.left = projectFilesPane{
		label:    "client-a-" + strings.Repeat("very-long-name-", 12),
		endpoint: ducklord.FileEndpoint{Path: "/home/duck/source"},
		entries:  []ducklord.FileEntry{{Name: "source.txt"}},
		status:   "Loading " + strings.Repeat("slowly ", 20),
		marked:   map[string]bool{},
	}
	s.projectFiles.right = projectFilesPane{
		label:    "client-b-" + strings.Repeat("very-long-name-", 12),
		endpoint: ducklord.FileEndpoint{Path: "/home/duck/destination"},
		entries:  []ducklord.FileEntry{{Name: "destination.txt"}},
		status:   "Loading " + strings.Repeat("slowly ", 20),
		marked:   map[string]bool{},
	}
	var frame strings.Builder
	s.renderProjectFilesModal(&frame, 150, 32)

	screen := ducklord.NewTerminal(32, 150, 0)
	screen.Write([]byte(strings.ReplaceAll(frame.String(), "\n", "\r\n")))
	view := strings.Join(screen.RenderLines(32, 150), "\n")
	for _, want := range []string{"LEFT", "RIGHT", "Host: client-a-", "Host: client-b-", "Loading slowly"} {
		if !strings.Contains(view, want) {
			t.Fatalf("long panel metadata hid %q: %q", want, view)
		}
	}
}

func TestProjectFilesTransferResultSurvivesPaneRefresh(t *testing.T) {
	results := []string{
		"Cancelled: copied 1, skipped 0",
		"Copied 1, skipped 0; partial: disk full",
	}
	for _, cols := range []int{50, 150} {
		for _, result := range results {
			t.Run(fmt.Sprintf("cols-%d/%s", cols, result), func(t *testing.T) {
				s := &tuiState{}
				s.projectFiles.open, s.projectFiles.step = true, "browse"
				s.projectFiles.status = result
				s.projectFiles.left = projectFilesPane{
					label:    "LOCAL",
					endpoint: ducklord.FileEndpoint{Path: "/source"},
					entries:  []ducklord.FileEntry{{Name: "source.txt"}},
					status:   "1 items",
					marked:   map[string]bool{},
				}
				s.projectFiles.right = projectFilesPane{
					label:    "client-a",
					endpoint: ducklord.FileEndpoint{Path: "/destination"},
					entries:  []ducklord.FileEntry{{Name: "destination.txt"}},
					status:   "1 items",
					marked:   map[string]bool{},
				}
				var frame strings.Builder
				s.renderProjectFilesModal(&frame, cols, 32)

				screen := ducklord.NewTerminal(32, cols, 0)
				screen.Write([]byte(strings.ReplaceAll(frame.String(), "\n", "\r\n")))
				view := strings.Join(screen.RenderLines(32, cols), "\n")
				width, _, _ := projectFilesGeometry(cols, 32, 2)
				wantResult := projectClip("Transfer: "+result, width-2)
				for _, want := range []string{wantResult, "1 items", "LEFT", "RIGHT"} {
					if !strings.Contains(view, want) {
						t.Fatalf("terminal omitted %q after pane refresh: %q", want, view)
					}
				}
			})
		}
	}
}

func TestProjectFilesListingCompletionClearsInitialGlobalLoadingStatus(t *testing.T) {
	s := &tuiState{projectFiles: projectFilesState{
		open:       true,
		generation: 4,
		status:     "Loading…",
		left:       projectFilesPane{loading: true},
		right:      projectFilesPane{loading: true},
		panegen:    [2]uint64{2, 3},
	}}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 4, panegen: 2, side: 0, entries: []ducklord.FileEntry{{Name: "source.txt"}}})
	if got := s.projectFiles.status; got != "Loading…" {
		t.Fatalf("first listing changed initial status = %q", got)
	}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 4, panegen: 3, side: 1, entries: []ducklord.FileEntry{{Name: "destination.txt"}}})
	if got := s.projectFiles.status; got != "" {
		t.Fatalf("initial loading status remained after both listings = %q", got)
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
			s.projectFiles.status = "Cancelled: copied 1, skipped 0"
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

func TestProjectFilesHistoryShowsRenamedTargetAndWrappedSelectedPaths(t *testing.T) {
	sourceRoot := "/source-root/very-long-source-directory-0123456789"
	destinationRoot := "/destination-root/very-long-destination-directory-0123456789"
	item := projectFilesItem{
		name:        "source-original-name.txt",
		destination: destinationRoot + "/renamed-target-final-suffix.txt",
		state:       "copied",
	}
	sourcePath := sourceRoot + "/" + item.name
	for _, cols := range []int{50, 120} {
		t.Run(fmt.Sprintf("cols-%d", cols), func(t *testing.T) {
			s := &tuiState{projectFiles: projectFilesState{
				open: true, step: "history", history: []projectFilesBatch{{
					sourceLabel: "LOCAL", destinationLabel: "client-a",
					source: ducklord.FileEndpoint{Path: sourceRoot}, destination: ducklord.FileEndpoint{Path: destinationRoot},
					items: []projectFilesItem{item},
				}},
			}}
			var frame strings.Builder
			s.renderProjectFilesModal(&frame, cols, 32)
			screen := ducklord.NewTerminal(32, cols, 0)
			screen.Write([]byte(strings.ReplaceAll(frame.String(), "\n", "\r\n")))
			view := strings.Join(screen.RenderLines(32, cols), "\n")
			for _, want := range []string{"Copied", "source-original-name.txt", "renamed-target-final-suffix.txt", "Source:", "Destination:"} {
				if !strings.Contains(view, want) {
					t.Fatalf("terminal omitted %q: %q", want, view)
				}
			}
			if cols == 120 {
				for _, want := range []string{sourcePath, item.destination} {
					if !strings.Contains(view, want) {
						t.Fatalf("wide terminal omitted selected path %q: %q", want, view)
					}
				}
			} else {
				for _, want := range []string{"/source-root/very-long-source-direct", "/destination-root/very-long-des"} {
					if !strings.Contains(view, want) {
						t.Fatalf("narrow terminal omitted wrapped path component %q: %q", want, view)
					}
				}
				if len(projectFilesHistoryPathLines("Destination", item.destination, cols-2)) < 2 {
					t.Fatal("narrow destination path was not wrapped")
				}
			}
		})
	}
}

func TestProjectFilesHistoryShortModalKeepsSelectedItemVisible(t *testing.T) {
	items := make([]projectFilesItem, 5)
	for i := range items {
		items[i] = projectFilesItem{
			name:        fmt.Sprintf("source-%d.txt", i),
			destination: "/destination-root/very-long-destination-directory-0123456789/renamed-target-final-suffix.txt",
			state:       "copied",
		}
	}
	items[len(items)-1].state = "failed"
	items[len(items)-1].err = "permission denied"
	s := &tuiState{projectFiles: projectFilesState{
		open: true, step: "history", historyItem: len(items) - 1,
		history: []projectFilesBatch{{source: ducklord.FileEndpoint{Path: "/source-root/very-long-source-directory-0123456789"}, items: items}},
	}}
	var frame strings.Builder
	s.renderProjectFilesModal(&frame, 50, 11)
	screen := ducklord.NewTerminal(11, 50, 0)
	screen.Write([]byte(strings.ReplaceAll(frame.String(), "\n", "\r\n")))
	view := strings.Join(screen.RenderLines(11, 50), "\n")
	for _, want := range []string{"Failed", "source-4.txt", "renamed-target-final-suffix.txt", "Error: permission denied", "Source:", "Destination:"} {
		if !strings.Contains(view, want) {
			t.Fatalf("short history modal omitted %q: %q", want, view)
		}
	}
}
