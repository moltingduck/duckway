package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestProjectFilesInputSelectionAndConflictPreview(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", active: 0, left: projectFilesPane{endpoint: ducklord.FileEndpoint{Path: dir}, entries: []ducklord.FileEntry{{Name: "a.txt"}}, marked: map[string]bool{}}, right: projectFilesPane{endpoint: ducklord.FileEndpoint{Path: dir}, marked: map[string]bool{}}}
	s.handleProjectFilesInput([]byte(" "))
	if !s.projectFiles.left.marked["a.txt"] {
		t.Fatal("Space should select the current file")
	}
	s.handleProjectFilesInput([]byte("c"))
	if s.projectFiles.step != "preview" || s.projectFiles.conflict != "skip" {
		t.Fatalf("preview = %#v", s.projectFiles)
	}
	s.handleProjectFilesInput([]byte("r"))
	if s.projectFiles.conflict != "rename" {
		t.Fatal("r should select rename")
	}
}

func TestProjectFilesCopyPreviewWaitsForLoadedMarkedSource(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "one.txt"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &tuiState{activeAttachKey: "shell"}
	s.projectFiles = projectFilesState{
		open: true, step: "browse", generation: 1, panegen: [2]uint64{1, 0},
		left:  projectFilesPane{endpoint: ducklord.FileEndpoint{Path: source}, marked: map[string]bool{}, loading: true, status: "Loading…"},
		right: projectFilesPane{endpoint: ducklord.FileEndpoint{Path: destination}, marked: map[string]bool{}},
		done:  make(chan projectFilesEvent, 2),
	}
	t.Cleanup(s.closeProjectFiles)

	s.handleProjectFilesInput([]byte("c"))
	if s.projectFiles.step != "browse" || s.projectFiles.status != "Loading files…" || !s.projectFiles.open || s.focused || s.activeAttachKey != "shell" {
		t.Fatalf("loading copy preview changed modal ownership: %#v", s.projectFiles)
	}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 1, panegen: 1, side: 0, entries: []ducklord.FileEntry{{Name: "one.txt"}}})
	s.handleProjectFilesInput([]byte("c"))
	if s.projectFiles.step != "browse" || s.projectFiles.status != "Select files first" {
		t.Fatalf("unmarked copy preview = %#v", s.projectFiles)
	}
	s.handleProjectFilesInput([]byte(" "))
	s.handleProjectFilesInput([]byte("c"))
	if s.projectFiles.step != "preview" {
		t.Fatalf("marked source did not open preview: %#v", s.projectFiles)
	}
	s.handleProjectFilesInput([]byte("\r"))
	if s.projectFiles.step != "busy" {
		t.Fatalf("confirmed preview did not start copy: %#v", s.projectFiles)
	}
	deadline := time.After(5 * time.Second)
	for s.projectFiles.step == "busy" {
		select {
		case event := <-s.projectFiles.done:
			s.applyProjectFilesEvent(event)
		case <-deadline:
			t.Fatal("copy did not complete")
		}
	}
	if s.projectFiles.step != "browse" || !strings.Contains(s.projectFiles.status, "Copied 1, skipped 0") {
		t.Fatalf("copy completion = %#v", s.projectFiles)
	}
	if _, err := os.Stat(filepath.Join(destination, "one.txt")); err != nil {
		t.Fatalf("copied file missing: %v", err)
	}
}

func TestProjectFilesCopyPreviewRejectsEmptySource(t *testing.T) {
	s := &tuiState{activeAttachKey: "shell"}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{marked: map[string]bool{}}, right: projectFilesPane{marked: map[string]bool{}}}
	s.handleProjectFilesInput([]byte("c"))
	if s.projectFiles.step != "browse" || s.projectFiles.status != "No files to copy" || !s.projectFiles.open || s.focused || s.activeAttachKey != "shell" {
		t.Fatalf("empty source opened or released modal: %#v", s.projectFiles)
	}
}

func TestProjectFilesEmptyPreviewConfirmationReturnsToBrowse(t *testing.T) {
	s := &tuiState{activeAttachKey: "shell"}
	s.projectFiles = projectFilesState{open: true, step: "preview", copySource: 0,
		left:  projectFilesPane{marked: map[string]bool{}},
		right: projectFilesPane{marked: map[string]bool{}},
	}
	s.handleProjectFilesInput([]byte("\r"))
	if s.projectFiles.step != "browse" || s.projectFiles.status != "Select files first" || !s.projectFiles.open || s.focused || s.activeAttachKey != "shell" {
		t.Fatalf("empty preview confirmation did not return to modal browser: %#v", s.projectFiles)
	}
	var out bytes.Buffer
	s.renderProjectFilesModal(&out, 100, 20)
	if !strings.Contains(out.String(), "Select files first") {
		t.Fatalf("browse prompt omitted empty-preview status: %q", out.String())
	}
}

func TestProjectFilesHidesNonTransferableEntriesFromBrowser(t *testing.T) {
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{
		entries: []ducklord.FileEntry{{Name: "regular.txt"}, {Name: "socket", NonTransferable: true}},
		marked:  map[string]bool{},
	}}
	visible := s.visibleProjectEntries(&s.projectFiles.left)
	if len(visible) != 1 || visible[0].Name != "regular.txt" {
		t.Fatalf("visible entries = %#v, want only transferable entry", visible)
	}
	if len(s.projectFiles.left.entries) != 2 {
		t.Fatal("browser filtering discarded the raw listing needed for backend conflict checks")
	}
	s.handleProjectFilesInput([]byte(" "))
	if !s.projectFiles.left.marked["regular.txt"] || s.projectFiles.left.marked["socket"] {
		t.Fatalf("selection included a nontransferable entry: %#v", s.projectFiles.left.marked)
	}
}

func TestProjectFilesCloseCancelsAndRestoresOrigin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &tuiState{focused: true, activeAttachKey: "session-key", workspaceProjectFocus: false}
	s.projectFiles = projectFilesState{open: true, cancel: cancel, originFocused: true, originAttachKey: "session-key", originProjectFocus: false}
	s.closeProjectFiles()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("close should cancel the request")
	}
	if s.projectFiles.open || !s.focused || s.activeAttachKey != "session-key" {
		t.Fatalf("origin not restored: %#v", s.projectFiles)
	}
}

func TestProjectFilesPathUnicodeClearsSelectionAndCancelsPreviousLoad(t *testing.T) {
	dir := t.TempDir()
	previous, previousCancel := context.WithCancel(context.Background())
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{
		endpoint: ducklord.FileEndpoint{Path: dir}, query: "old", marked: map[string]bool{"old.txt": true}, selected: 2,
	}, cancels: [2]context.CancelFunc{previousCancel}}
	s.handleProjectFilesInput([]byte("g"))
	s.handleProjectFilesInput([]byte("文"))
	s.handleProjectFilesInput([]byte("\b"))
	if s.projectFiles.left.endpoint.Path != dir {
		t.Fatalf("unicode backspace split the path: %q", s.projectFiles.left.endpoint.Path)
	}
	s.handleProjectFilesInput([]byte("\r"))
	select {
	case <-previous.Done():
	default:
		t.Fatal("new pane load did not cancel the superseded request")
	}
	if s.projectFiles.left.query != "" || s.projectFiles.left.selected != 0 || len(s.projectFiles.left.marked) != 0 {
		t.Fatalf("path change retained selection: %#v", s.projectFiles.left)
	}
}

func TestProjectFilesPathCancelPreservesCurrentDirectoryFilter(t *testing.T) {
	dir := t.TempDir()
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{
		endpoint: ducklord.FileEndpoint{Path: dir}, query: "keep", selected: 1, marked: map[string]bool{"keep.txt": true},
	}}
	s.handleProjectFilesInput([]byte("g"))
	s.handleProjectFilesInput([]byte("文"))
	s.handleProjectFilesInput([]byte("\x1b"))
	if s.projectFiles.step != "browse" || s.projectFiles.left.endpoint.Path != dir || s.projectFiles.left.query != "keep" || s.projectFiles.left.selected != 1 || !s.projectFiles.left.marked["keep.txt"] {
		t.Fatalf("cancelled path edit changed current-directory state: %#v", s.projectFiles.left)
	}
}

func TestProjectFilesEndpointPickerResetsAndShowsConfiguredName(t *testing.T) {
	dir := t.TempDir()
	s := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a"}, {Name: "client-b"}}}}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{label: "LOCAL", endpoint: ducklord.FileEndpoint{Path: dir}, entries: []ducklord.FileEntry{{Name: "stale"}}, query: "stale", selected: 1, marked: map[string]bool{"stale": true}}, done: make(chan projectFilesEvent, 1)}
	s.projectFiles.endpointIndex = 2
	s.handleProjectFilesInput([]byte("h"))
	if s.projectFiles.endpointIndex != 0 {
		t.Fatalf("picker started at index %d, want Local", s.projectFiles.endpointIndex)
	}
	s.handleProjectFilesInput([]byte("j"))
	s.handleProjectFilesInput([]byte("\r"))
	if got := s.projectFiles.left.label; got != "client-a" {
		t.Fatalf("endpoint label = %q, want configured host name", got)
	}
	if s.projectFiles.left.endpoint.Client == nil || s.projectFiles.left.endpoint.Client.Name != "client-a" {
		t.Fatalf("wrong endpoint: %#v", s.projectFiles.left.endpoint)
	}
	if got := s.projectFiles.left.endpoint.Path; got != "/" {
		t.Fatalf("new remote endpoint retained local path %q", got)
	}
	if s.projectFiles.left.query != "" || s.projectFiles.left.selected != 0 || len(s.projectFiles.left.entries) != 0 || len(s.projectFiles.left.marked) != 0 {
		t.Fatalf("endpoint switch retained stale selection: %#v", s.projectFiles.left)
	}
	s.closeProjectFiles()
}

func TestProjectFilesEndpointUsesActiveRemoteDirectoryOnlyForMatchingHost(t *testing.T) {
	identity := ducklord.SessionIdentity{InstanceID: "11111111-1111-4111-8111-111111111111", SessionID: "ABC123"}
	session := ducklord.RemoteSession{Client: "client-a", InstanceID: identity.InstanceID, SessionID: identity.SessionID, Cwd: "/work/active"}
	s := &tuiState{
		workspacePreview: true,
		activeAttachKey:  sessionKey(session),
		cfg:              &ducklord.Config{Clients: []ducklord.Client{{Name: "client-a"}, {Name: "client-b"}}},
		sessions:         []ducklord.RemoteSession{session},
		projectFiles:     projectFilesState{open: true, active: 0, left: projectFilesPane{marked: map[string]bool{}}, done: make(chan projectFilesEvent, 2)},
	}
	s.applyProjectFilesEndpoint(1)
	if got := s.projectFiles.left.endpoint.Path; got != "/work/active" {
		t.Fatalf("matching remote endpoint path = %q, want active cwd", got)
	}
	s.applyProjectFilesEndpoint(2)
	if got := s.projectFiles.left.endpoint.Path; got != "/" {
		t.Fatalf("other remote endpoint path = %q, want root", got)
	}
	wantLocal, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s.applyProjectFilesEndpoint(0)
	if got := s.projectFiles.left.endpoint.Path; got != wantLocal {
		t.Fatalf("local endpoint path = %q, want controller cwd %q", got, wantLocal)
	}
	s.closeProjectFiles()
}

func TestProjectFilesRejectsStalePaneEvents(t *testing.T) {
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, generation: 7, panegen: [2]uint64{3, 4}, left: projectFilesPane{entries: []ducklord.FileEntry{{Name: "old"}}}}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 7, panegen: 2, side: 0, entries: []ducklord.FileEntry{{Name: "stale"}}})
	if got := s.projectFiles.left.entries[0].Name; got != "old" {
		t.Fatalf("stale pane result replaced entries: %q", got)
	}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 7, panegen: 3, side: 0, entries: []ducklord.FileEntry{{Name: "fresh"}}})
	if got := s.projectFiles.left.entries[0].Name; got != "fresh" {
		t.Fatalf("current pane result was not applied: %q", got)
	}
}

func TestProjectFilesCtrlCCancelsCopyAndFencesCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "busy", generation: 9, copyCancel: cancel}
	s.handleProjectFilesInput([]byte("\x03"))
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Ctrl+C did not cancel the copy context")
	}
	if s.projectFiles.step != "busy" || !s.projectFiles.copyCancelling || s.projectFiles.status != "Cancelling…" {
		t.Fatalf("cancel must wait for backend acknowledgement: %#v", s.projectFiles)
	}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 9, copy: true})
	if s.projectFiles.step != "browse" || s.projectFiles.status != "Cancelled: copied 0, skipped 0" {
		t.Fatalf("cancellation acknowledgement state = %#v", s.projectFiles)
	}
}

func TestProjectFilesMouseDragUsesDirectoryTarget(t *testing.T) {
	sourceDir, targetDir := t.TempDir(), t.TempDir()
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{
		endpoint: ducklord.FileEndpoint{Path: sourceDir}, entries: []ducklord.FileEntry{{Name: "file.txt"}, {Name: "nested", IsDir: true}}, marked: map[string]bool{},
	}, right: projectFilesPane{
		endpoint: ducklord.FileEndpoint{Path: targetDir}, entries: []ducklord.FileEntry{{Name: "drop", IsDir: true}}, marked: map[string]bool{},
	}}
	var rendered bytes.Buffer
	s.renderProjectFilesModal(&rendered, 150, 32)
	var source, target modalMouseRegion
	for _, region := range s.modalMouseRegions {
		if region.action.selection == &s.projectFiles.left.selected && region.action.index == 1 {
			source = region
		}
		if region.action.selection == &s.projectFiles.right.selected && region.action.index == 0 {
			target = region
		}
	}
	if source.row == 0 || target.row == 0 {
		t.Fatalf("missing drag regions: %#v", s.modalMouseRegions)
	}
	s.modalMouseInput(source.left, source.row)
	s.projectFilesMouseRelease(target.left, target.row)
	if s.projectFiles.step != "preview" || !s.projectFiles.left.marked["nested"] {
		t.Fatalf("drag did not select source and open preview: %#v", s.projectFiles)
	}
	if got := s.projectFiles.right.endpoint.Path; got != targetDir {
		t.Fatalf("drag must not mutate browse destination path = %q", got)
	}
	if got := s.projectFiles.pendingDestination.Path; got != filepath.Join(targetDir, "drop") {
		t.Fatalf("drag destination = %q", got)
	}
}

func TestProjectFilesWideModalKeepsColumnsAndOnlyBrowseHasEntryMouse(t *testing.T) {
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{label: "LOCAL", endpoint: ducklord.FileEndpoint{Path: "/left"}, entries: []ducklord.FileEntry{{Name: "a"}}, marked: map[string]bool{}}, right: projectFilesPane{label: "REMOTE", endpoint: ducklord.FileEndpoint{Path: "/right"}, entries: []ducklord.FileEntry{{Name: "right-entry"}}, marked: map[string]bool{}}}
	var out bytes.Buffer
	s.renderProjectFilesModal(&out, 150, 32)
	if !strings.Contains(out.String(), "right-entry") || len(s.modalMouseRegions) < 2 {
		t.Fatalf("wide modal lost right column or regions: %q %#v", out.String(), s.modalMouseRegions)
	}
	s.projectFiles.step = "filter"
	out.Reset()
	s.renderProjectFilesModal(&out, 150, 32)
	for _, r := range s.modalMouseRegions {
		if r.action.selection != nil {
			t.Fatalf("editor registered entry region: %#v", r)
		}
	}
}

func TestProjectFilesEditorsAcceptSpacesAndCtrlCCancels(t *testing.T) {
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "filter", left: projectFilesPane{marked: map[string]bool{}}}
	s.handleProjectFilesInput([]byte("two words"))
	if s.projectFiles.left.query != "two words" {
		t.Fatalf("query = %q", s.projectFiles.left.query)
	}
	s.handleProjectFilesInput([]byte("\x1b[A"))
	if s.projectFiles.left.query != "two words" {
		t.Fatalf("control bytes entered query = %q", s.projectFiles.left.query)
	}
	s.handleProjectFilesInput([]byte("\x03"))
	if s.projectFiles.open {
		t.Fatal("Ctrl+C in editor did not close modal")
	}
}

func TestProjectFilesPreviewClearsDragDestinationAndShowsIt(t *testing.T) {
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "preview", copySource: 0, hasPendingDestination: true,
		pendingDestination: ducklord.FileEndpoint{Path: "/shared"},
		left:               projectFilesPane{label: "client-a", endpoint: ducklord.FileEndpoint{Path: "/shared"}, marked: map[string]bool{"one": true}},
		right:              projectFilesPane{label: "client-b", endpoint: ducklord.FileEndpoint{Path: "/shared"}, marked: map[string]bool{}},
	}
	var out bytes.Buffer
	s.renderProjectFilesModal(&out, 120, 8)
	if !strings.Contains(out.String(), "From: client-a: /shared") || !strings.Contains(out.String(), "To: client-b: /shared") || !strings.Contains(out.String(), "Enter confirm") {
		t.Fatalf("preview omitted actual target or controls: %q", out.String())
	}
	s.handleProjectFilesInput([]byte("\x1b"))
	if s.projectFiles.hasPendingDestination || s.projectFiles.right.endpoint.Path != "/shared" {
		t.Fatalf("Esc retained drag target or changed browsing pane: %#v", s.projectFiles)
	}
	s.projectFiles.pendingDestination = ducklord.FileEndpoint{Path: "/stale"}
	s.projectFiles.hasPendingDestination = true
	s.handleProjectFilesInput([]byte("c"))
	if s.projectFiles.hasPendingDestination {
		t.Fatal("keyboard copy retained a previous drag target")
	}
}

func TestProjectFilesCellClippingAndParentClearsFilter(t *testing.T) {
	if got := projectClip("界界", 3); got != "界…" {
		t.Fatalf("cell clipping = %q, want one wide rune and ellipsis", got)
	}
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "browse", left: projectFilesPane{endpoint: ducklord.FileEndpoint{Path: child}, query: "old", marked: map[string]bool{}}, done: make(chan projectFilesEvent, 1)}
	s.handleProjectFilesInput([]byte("\b"))
	if s.projectFiles.left.endpoint.Path != parent || s.projectFiles.left.query != "" {
		t.Fatalf("parent navigation = %#v", s.projectFiles.left)
	}
	s.closeProjectFiles()
}

func TestProjectFilesCopyResultCountsAndRefreshesDraggedDestination(t *testing.T) {
	target := t.TempDir()
	child := filepath.Join(target, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "busy", generation: 4, copyDestination: 1,
		right: projectFilesPane{endpoint: ducklord.FileEndpoint{Path: child}, marked: map[string]bool{}},
		done:  make(chan projectFilesEvent, 1),
	}
	s.applyProjectFilesEvent(projectFilesEvent{generation: 4, copy: true, result: []ducklord.FileCopyResult{{Name: "copied"}, {Name: "skipped", Skipped: true}}, err: errors.New("relay stopped")})
	if got := s.projectFiles.status; !strings.Contains(got, "Copied 1, skipped 1; partial: relay stopped") {
		t.Fatalf("partial result status = %q", got)
	}
	if s.projectFiles.right.endpoint.Path != child || s.projectFiles.panegen[1] == 0 {
		t.Fatalf("copy completion did not refresh actual destination: %#v", s.projectFiles)
	}
	s.closeProjectFiles()
}

func TestProjectFilesPreviewKeepsControlsBeforeLongSelection(t *testing.T) {
	marked := make(map[string]bool)
	for i := 0; i < 32; i++ {
		marked[fmt.Sprintf("file-%02d", i)] = true
	}
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "preview", left: projectFilesPane{endpoint: ducklord.FileEndpoint{Path: "/source"}, marked: marked}, right: projectFilesPane{endpoint: ducklord.FileEndpoint{Path: "/target"}, marked: map[string]bool{}}}
	var out bytes.Buffer
	s.renderProjectFilesModal(&out, 80, 5)
	if !strings.Contains(out.String(), "Enter confirm") {
		t.Fatalf("short preview lost confirmation control: %q", out.String())
	}
}

func TestProjectFilesFootersDescribeHandledActions(t *testing.T) {
	s := &tuiState{}
	s.projectFiles = projectFilesState{open: true, step: "preview", conflict: "skip", left: projectFilesPane{marked: map[string]bool{"one.txt": true}}, right: projectFilesPane{marked: map[string]bool{}}}
	var out bytes.Buffer
	s.renderProjectFilesModal(&out, 80, 24)
	screen := renderedModalScreen(out.Bytes(), 80, 24)
	if !strings.Contains(screen, "s skip · r rename · o overwrite · Esc/q cancel") {
		t.Fatalf("preview footer missing conflict keys: %q", screen)
	}
	s.handleProjectFilesInput([]byte("r"))
	if s.projectFiles.conflict != "rename" {
		t.Fatalf("r did not select rename: %q", s.projectFiles.conflict)
	}
	s.projectFiles.step, s.projectFiles.history = "history", []projectFilesBatch{{}}
	out.Reset()
	s.renderProjectFilesModal(&out, 80, 24)
	screen = renderedModalScreen(out.Bytes(), 80, 24)
	for _, want := range []string{"←/→ h/l batch · ↑/↓ j/k item", "x clear · Esc browser · Ctrl+C close"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("history footer missing handled key %q in %q", want, screen)
		}
	}
	s.projectFiles.step = "browse"
	s.projectFiles.left.entries = []ducklord.FileEntry{{Name: "source.txt"}}
	s.projectFiles.right.entries = []ducklord.FileEntry{{Name: "destination.txt"}}
	out.Reset()
	s.renderProjectFilesModal(&out, 80, 24)
	screen = renderedModalScreen(out.Bytes(), 80, 24)
	for _, want := range []string{
		"h endpoints · g path · / filter · Space select · i icons",
		"↑/↓ j/k select · Enter open · Backspace parent",
		"Tab column · c copy · l history · Esc/Ctrl+C close",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("browse footer missing handled key %q in %q", want, screen)
		}
	}
}
