package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

type projectFilesPane struct {
	label    string
	endpoint ducklord.FileEndpoint
	entries  []ducklord.FileEntry
	selected int
	marked   map[string]bool
	query    string
	status   string
	loading  bool
}

func (s *projectFilesState) applyCopyProgress(progress ducklord.FileCopyProgress) {
	for i := range s.items {
		if s.items[i].name != progress.Name {
			continue
		}
		s.items[i].state = progress.State
		if progress.Destination != "" {
			s.items[i].destination = progress.Destination
		}
		break
	}
	current := min(progress.Total, progress.Completed+1)
	if progress.Total <= 0 {
		s.status = "Copying"
	} else {
		s.status = fmt.Sprintf("Copying %d of %d", current, progress.Total)
	}
	if len(s.history) > 0 {
		b := s.history[0]
		b.items = append([]projectFilesItem(nil), s.items...)
		s.history[0] = b
	}
}

type projectFilesState struct {
	open                      bool
	left, right               projectFilesPane
	active                    int
	step                      string // browse, endpoints, path, preview, busy
	conflict                  string
	status                    string
	originFocused             bool
	originProjectFocus        bool
	originAttachKey           string
	originProject, originPane string
	originTab                 string
	generation                uint64
	panegen                   [2]uint64
	cancel                    context.CancelFunc
	cancels                   [2]context.CancelFunc
	copyCancel                context.CancelFunc
	copySource                int
	endpointIndex             int
	editorOriginal            string
	done                      chan projectFilesEvent
	lifecycle                 context.Context
	dragSource                int
	dragSourceIndex           int
	dragArmed                 bool
	pendingDestination        ducklord.FileEndpoint
	hasPendingDestination     bool
	copyDestination           int
	copyCancelling            bool
	asciiFolders              bool
	historyProject            string
	history                   []projectFilesBatch
	historyBatch              int
	historyItem               int
	items                     []projectFilesItem
	copyStarted               time.Time
}

func sendProjectFilesCompletion(lifecycle context.Context, done chan<- projectFilesEvent, event projectFilesEvent) {
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	select {
	case done <- event:
	case <-lifecycle.Done():
	}
}

// projectFilesLifecycle is the root for modal work. Tests and callers that do
// not run through runTUI still get a usable context, while normal TUI workers
// stop when the application context ends.
func (s *tuiState) projectFilesLifecycle() context.Context {
	if s.projectFiles.lifecycle != nil {
		return s.projectFiles.lifecycle
	}
	return context.Background()
}

func (s *tuiState) projectFilesWorkerContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(s.projectFilesLifecycle())
}

type projectFilesEvent struct {
	generation uint64
	panegen    uint64
	side       int
	entries    []ducklord.FileEntry
	result     []ducklord.FileCopyResult
	err        error
	copy       bool
	progress   *ducklord.FileCopyProgress
}

func (s *tuiState) openProjectFiles() {
	if s.projectFiles.open {
		return
	}
	p := projectFilesPane{label: "LOCAL", marked: map[string]bool{}}
	if cwd, err := os.Getwd(); err == nil {
		p.endpoint.Path = cwd
	}
	q := projectFilesPane{label: "DESTINATION", marked: map[string]bool{}}
	p.status, q.status = "Loading…", "Loading…"
	p.loading, q.loading = true, true
	if sess := s.activePTYSession(); sess.Cwd != "" && sess.Client != "" {
		// Keep LOCAL rooted in Duckway's own cwd. A remote session cwd is only
		// meaningful for its remote endpoint.
		if sess.Client != "" {
			for i := range s.cfg.Clients {
				if s.cfg.Clients[i].Name == sess.Client {
					q.endpoint = ducklord.FileEndpoint{Client: &s.cfg.Clients[i], Path: sess.Cwd}
					q.label = s.cfg.Clients[i].Name
					break
				}
			}
		}
	}
	if nav, err := s.workspaceNavigation(); err == nil {
		s.projectFiles.originProject, s.projectFiles.originPane, s.projectFiles.originTab = nav.CurrentProjectID(), nav.CurrentPaneID(), nav.CurrentTabID()
		s.projectFiles.historyProject = nav.CurrentProjectID()
		s.projectFiles.history = s.projectFilesHistory[s.projectFiles.historyProject]
		if q.endpoint.Path == "" {
			if shelf, e := ducklord.ProjectExchangePath(s.cfgPath, nav.CurrentProjectID()); e == nil {
				q.label, q.endpoint = "PROJECT SHELF", ducklord.FileEndpoint{Path: shelf}
			} else {
				q.status = "Project shelf unavailable: " + sanitizeTerminalText(e.Error())
			}
		}
	}
	if q.endpoint.Path == "" {
		q.endpoint.Path = p.endpoint.Path
	}
	s.projectFiles.originFocused, s.projectFiles.originAttachKey = s.focused, s.activeAttachKey
	s.projectFiles.originProjectFocus = s.workspaceProjectFocus
	s.projectFiles.left, s.projectFiles.right = p, q
	s.projectFiles.active, s.projectFiles.step, s.projectFiles.open = 0, "browse", true
	s.projectFiles.pendingDestination = ducklord.FileEndpoint{}
	s.projectFiles.hasPendingDestination = false
	s.projectFiles.copyCancelling = false
	s.projectFiles.status = "Loading…"
	s.focused = false
	s.projectFiles.generation++
	s.loadProjectFilesPane(s.projectFilesLifecycle(), 0)
	s.loadProjectFilesPane(s.projectFilesLifecycle(), 1)
}

func (s *tuiState) closeProjectFiles() {
	if !s.projectFiles.open {
		return
	}
	if s.projectFiles.cancel != nil {
		s.projectFiles.cancel()
		s.projectFiles.cancel = nil
	}
	if s.projectFiles.copyCancel != nil {
		s.projectFiles.copyCancel()
		s.projectFiles.copyCancel = nil
	}
	for i := range s.projectFiles.cancels {
		if s.projectFiles.cancels[i] != nil {
			s.projectFiles.cancels[i]()
		}
		s.projectFiles.cancels[i] = nil
	}
	s.projectFiles.open = false
	s.projectFiles.generation++
	if s.originProjectStillPresent() {
		s.workspaceProjectFocus = s.projectFiles.originProjectFocus
		s.focused, s.activeAttachKey = s.projectFiles.originFocused, s.projectFiles.originAttachKey
	} else {
		s.focused, s.activeAttachKey, s.workspaceProjectFocus = false, "", true
	}
}

func (s *tuiState) originProjectStillPresent() bool {
	if s.projectFiles.originProject == "" {
		return true
	}
	nav, err := s.workspaceNavigation()
	if err != nil || nav.CurrentProjectID() != s.projectFiles.originProject || nav.CurrentPaneID() != s.projectFiles.originPane || (s.projectFiles.originTab != "" && nav.CurrentTabID() != s.projectFiles.originTab) {
		return false
	}
	if s.projectFiles.originAttachKey != "" {
		sess, ok := s.sessionForKey(s.projectFiles.originAttachKey)
		return ok && canRead(sess) && s.hostIsLive(sess.Client)
	}
	return true
}

func (s *tuiState) projectFilesPane() *projectFilesPane {
	if s.projectFiles.active == 0 {
		return &s.projectFiles.left
	}
	return &s.projectFiles.right
}
func (s *tuiState) visibleProjectEntries(p *projectFilesPane) []ducklord.FileEntry {
	out := make([]ducklord.FileEntry, 0, len(p.entries))
	q := strings.ToLower(p.query)
	for _, e := range p.entries {
		if !e.NonTransferable && (q == "" || strings.Contains(strings.ToLower(e.Name), q)) {
			out = append(out, e)
		}
	}
	return out
}

func projectFilesMarkedNames(p projectFilesPane) []string {
	names := make([]string, 0, len(p.marked))
	for name, marked := range p.marked {
		if marked {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
func (s *tuiState) loadProjectFilesPane(parent context.Context, side int) {
	if previous := s.projectFiles.cancels[side]; previous != nil {
		previous()
	}
	p := s.projectFiles.left
	if side == 1 {
		p = s.projectFiles.right
	}
	s.projectFiles.panegen[side]++
	gen := s.projectFiles.panegen[side]
	globalGen := s.projectFiles.generation
	p.endpoint = snapshotProjectFilesEndpoint(p.endpoint)
	ctx, cancel := context.WithCancel(parent)
	done := s.projectFiles.done
	s.projectFiles.cancels[side] = cancel
	s.projectFiles.cancel = cancel
	go func() {
		entries, err := ducklord.ListFiles(ctx, p.endpoint)
		if done != nil {
			select {
			case done <- projectFilesEvent{generation: globalGen, panegen: gen, side: side, entries: entries, err: err}:
			case <-ctx.Done():
			}
		}
	}()
}

func snapshotProjectFilesEndpoint(endpoint ducklord.FileEndpoint) ducklord.FileEndpoint {
	if endpoint.Client == nil {
		return endpoint
	}
	client := *endpoint.Client
	endpoint.Client = &client
	return endpoint
}

func (s *tuiState) projectFilesEndpointNames() []string {
	n := []string{"LOCAL"}
	for _, c := range s.cfg.Clients {
		n = append(n, c.Name)
	}
	return append(n, "PROJECT SHELF")
}
func (s *tuiState) applyProjectFilesEndpoint(i int) {
	p := s.projectFilesPane()
	if i <= 0 {
		p.endpoint.Client = nil
		p.endpoint.Path = s.projectFilesLocalPath()
		p.label = "LOCAL"
	} else if i <= len(s.cfg.Clients) {
		client := s.cfg.Clients[i-1]
		p.endpoint.Client = &client
		p.endpoint.Path = s.projectFilesRemotePath(client.Name)
		p.label = client.Name
	} else if nav, err := s.workspaceNavigation(); err == nil {
		if path, e := ducklord.ProjectExchangePath(s.cfgPath, nav.CurrentProjectID()); e == nil {
			p.endpoint.Client = nil
			p.endpoint.Path = path
			p.label = "PROJECT SHELF"
		} else {
			p.status = "Project shelf unavailable: " + sanitizeTerminalText(e.Error())
			return
		}
	}
	// A filter only describes the directory that was visible when it was entered.
	// Switching endpoint starts a different directory, so do not leave entries
	// invisibly filtered by the previous one.
	p.entries, p.query, p.selected, p.marked, p.status, p.loading = nil, "", 0, map[string]bool{}, "Loading…", true
	s.loadProjectFilesPane(s.projectFilesLifecycle(), s.projectFiles.active)
}

func (s *tuiState) projectFilesLocalPath() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "/"
}

func (s *tuiState) projectFilesRemotePath(client string) string {
	if session := s.activePTYSession(); session.Client == client && session.Cwd != "" {
		return session.Cwd
	}
	return "/"
}

func removeLastRune(v string) string {
	r := []rune(v)
	if len(r) == 0 {
		return ""
	}
	return string(r[:len(r)-1])
}

func projectFilesEditorText(text string) bool {
	if text == "" || !utf8.ValidString(text) || strings.HasPrefix(text, "\x1b") {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func projectClip(v string, width int) string {
	v = sanitizeTerminalText(v)
	if width < 1 {
		return ""
	}
	if modalCellWidth(v) <= width {
		return v
	}
	return modalCellTruncate(v, width)
}

func projectFilesEndpointDescription(p projectFilesPane) string {
	label := p.label
	if label == "" {
		if p.endpoint.Client != nil {
			label = p.endpoint.Client.Name
		} else {
			label = "LOCAL"
		}
	}
	return label + ": " + p.endpoint.Path
}

func (s *tuiState) applyProjectFilesEvent(event projectFilesEvent) {
	if !s.projectFiles.open || event.generation != s.projectFiles.generation || (!event.copy && event.panegen != s.projectFiles.panegen[event.side]) {
		return
	}
	if event.copy {
		if event.progress != nil {
			s.projectFiles.applyCopyProgress(*event.progress)
			return
		}
		s.projectFiles.copyCancel = nil
		copied, skipped := 0, 0
		for _, result := range event.result {
			if result.Skipped {
				skipped++
			} else {
				copied++
			}
		}
		for i := range s.projectFiles.items {
			item := &s.projectFiles.items[i]
			if item.state == "queued" {
				if s.projectFiles.copyCancelling || event.err != nil {
					item.state = "not started"
				}
			}
		}
		if event.err != nil && !s.projectFiles.copyCancelling {
			for i := range s.projectFiles.items {
				if s.projectFiles.items[i].state == "copying" {
					s.projectFiles.items[i].state = "failed"
					s.projectFiles.items[i].err = sanitizeTerminalText(event.err.Error())
					break
				}
			}
		} else if s.projectFiles.copyCancelling {
			for i := range s.projectFiles.items {
				if s.projectFiles.items[i].state == "copying" {
					s.projectFiles.items[i].state = "cancelled"
				}
			}
		} else {
			for i := range s.projectFiles.items {
				if s.projectFiles.items[i].state == "copying" {
					s.projectFiles.items[i].state = "copied"
				}
			}
		}
		for _, result := range event.result {
			for i := range s.projectFiles.items {
				if s.projectFiles.items[i].name == result.Name {
					if result.Skipped {
						s.projectFiles.items[i].state = "skipped"
					} else {
						s.projectFiles.items[i].state = "copied"
					}
					if result.Destination != "" {
						s.projectFiles.items[i].destination = result.Destination
					}
				}
			}
		}
		if s.projectFiles.copyCancelling {
			s.projectFiles.copyCancelling = false
			s.projectFiles.status = fmt.Sprintf("Cancelled: copied %d, skipped %d", copied, skipped)
		} else if event.err != nil {
			s.projectFiles.status = fmt.Sprintf("Copied %d, skipped %d; partial: %s", copied, skipped, sanitizeTerminalText(event.err.Error()))
		} else {
			s.projectFiles.status = fmt.Sprintf("Copied %d, skipped %d", copied, skipped)
		}
		if len(s.projectFiles.history) > 0 {
			b := s.projectFiles.history[0]
			b.items = append([]projectFilesItem(nil), s.projectFiles.items...)
			if event.err != nil {
				b.err = sanitizeTerminalText(event.err.Error())
			}
			s.projectFiles.history[0] = b
			if h := s.projectFilesHistory[s.projectFilesProjectKey()]; len(h) > 0 {
				h[0] = b
				s.projectFilesHistory[s.projectFilesProjectKey()] = h
			}
		}
		s.projectFiles.step = "browse"
		// A cancelled copy can have completed work. Always reload the destination
		// that the request actually used, including a dragged child directory.
		s.loadProjectFilesPane(s.projectFilesLifecycle(), s.projectFiles.copyDestination)
		return
	}
	if event.side == 0 {
		s.projectFiles.left.entries = event.entries
		s.projectFiles.left.loading = false
	} else {
		s.projectFiles.right.entries = event.entries
		s.projectFiles.right.loading = false
	}
	s.projectFiles.cancels[event.side] = nil
	pane := &s.projectFiles.left
	if event.side == 1 {
		pane = &s.projectFiles.right
	}
	if event.err != nil {
		pane.status = sanitizeTerminalText(event.err.Error())
	} else {
		pane.status = fmt.Sprintf("%d items", len(event.entries))
	}
	if !s.projectFiles.left.loading && !s.projectFiles.right.loading && s.projectFiles.status == "Loading…" {
		s.projectFiles.status = ""
	}
	if pane.selected >= len(s.visibleProjectEntries(pane)) {
		pane.selected = max(0, len(s.visibleProjectEntries(pane))-1)
	}
}

func (s *tuiState) handleProjectFilesInput(b []byte) bool {
	if !s.projectFiles.open {
		return false
	}
	text := string(b)
	p := s.projectFilesPane()
	entries := s.visibleProjectEntries(p)
	// Ctrl-C has modal-wide cancellation semantics even while an editor owns
	// text input; it must never become editor text.
	if text == "\x03" {
		if s.projectFiles.step == "busy" {
			if s.projectFiles.copyCancel != nil && !s.projectFiles.copyCancelling {
				s.projectFiles.copyCancelling = true
				s.projectFiles.status = "Cancelling…"
				s.projectFiles.copyCancel()
			}
			return true
		}
		s.closeProjectFiles()
		return true
	}
	if s.projectFiles.step == "history" {
		switch text {
		case "\x1b":
			s.projectFiles.step = "browse"
		case "x":
			s.projectFilesHistory[s.projectFilesProjectKey()] = nil
			s.projectFiles.history = nil
			s.projectFiles.historyBatch, s.projectFiles.historyItem = 0, 0
		case "\x1b[C", "l":
			if len(s.projectFiles.history) > 0 {
				s.projectFiles.historyBatch = (s.projectFiles.historyBatch + 1) % len(s.projectFiles.history)
				s.projectFiles.historyItem = 0
			}
		case "\x1b[D", "h":
			if len(s.projectFiles.history) > 0 {
				s.projectFiles.historyBatch = (s.projectFiles.historyBatch + len(s.projectFiles.history) - 1) % len(s.projectFiles.history)
				s.projectFiles.historyItem = 0
			}
		case "\x1b[B", "j":
			if len(s.projectFiles.history) > 0 {
				items := s.projectFiles.history[s.projectFiles.historyBatch].items
				if s.projectFiles.historyItem < len(items)-1 {
					s.projectFiles.historyItem++
				}
			}
		case "\x1b[A", "k":
			if s.projectFiles.historyItem > 0 {
				s.projectFiles.historyItem--
			}
		}
		return true
	}
	if s.projectFiles.step == "filter" {
		if text == "\r" || text == "\n" {
			s.projectFiles.step = "browse"
			return true
		}
		if text == "\x1b" {
			p.query = s.projectFiles.editorOriginal
			s.projectFiles.step = "browse"
			return true
		}
		if text == "\b" || text == "\x7f" {
			p.query = removeLastRune(p.query)
		} else if projectFilesEditorText(text) {
			p.query += text
		}
		p.selected = 0
		return true
	}
	switch s.projectFiles.step {
	case "path":
		if text == "\r" || text == "\n" {
			if filepath.IsAbs(p.endpoint.Path) {
				p.entries, p.query = nil, ""
				p.selected, p.marked, p.loading, p.status = 0, map[string]bool{}, true, "Loading…"
				s.projectFiles.step = "browse"
				s.loadProjectFilesPane(s.projectFilesLifecycle(), s.projectFiles.active)
			}
			return true
		}
		if text == "\x1b" {
			p.endpoint.Path = s.projectFiles.editorOriginal
			s.projectFiles.step = "browse"
			return true
		}
		if text == "\b" || text == "\x7f" {
			p.endpoint.Path = removeLastRune(p.endpoint.Path)
		} else if projectFilesEditorText(text) {
			p.endpoint.Path += text
		}
		return true
	}
	switch s.projectFiles.step {
	case "endpoints":
		names := s.projectFilesEndpointNames()
		if text == "j" || text == "\x1b[B" {
			if s.projectFiles.endpointIndex < len(names)-1 {
				s.projectFiles.endpointIndex++
			}
			return true
		}
		if text == "k" || text == "\x1b[A" {
			if s.projectFiles.endpointIndex > 0 {
				s.projectFiles.endpointIndex--
			}
			return true
		}
		switch text {
		case "\r", "\n":
			s.applyProjectFilesEndpoint(s.projectFiles.endpointIndex)
			s.projectFiles.step = "browse"
		case "\x1b":
			s.projectFiles.step = "browse"
		}
		return true
	}
	if s.projectFiles.step == "preview" {
		switch text {
		case "\x1b", "q":
			s.projectFiles.step = "browse"
			s.projectFiles.pendingDestination = ducklord.FileEndpoint{}
			s.projectFiles.hasPendingDestination = false
		case "s":
			s.projectFiles.conflict = "skip"
		case "r":
			s.projectFiles.conflict = "rename"
		case "o":
			s.projectFiles.conflict = "overwrite"
		case "\r", "\n":
			s.startProjectFilesCopy()
		}
		return true
	}
	if s.projectFiles.step == "busy" && text != "\x03" {
		return true
	}
	switch text {
	case "\x1b":
		if s.projectFiles.step != "browse" {
			s.projectFiles.step = "browse"
		} else {
			s.closeProjectFiles()
		}
		return true
	case "\t":
		s.projectFiles.active = 1 - s.projectFiles.active
		return true
	case "l":
		s.projectFiles.history = append([]projectFilesBatch(nil), s.projectFilesHistory[s.projectFilesProjectKey()]...)
		s.projectFiles.historyBatch, s.projectFiles.historyItem = 0, 0
		s.projectFiles.step = "history"
		return true
	case "/":
		s.projectFiles.editorOriginal = p.query
		p.query = ""
		s.projectFiles.step = "filter"
		return true
	case "i":
		s.projectFiles.asciiFolders = !s.projectFiles.asciiFolders
		return true
	case "h":
		// Start at Local on every open so keyboard choices are deterministic.
		s.projectFiles.endpointIndex = 0
		s.projectFiles.step = "endpoints"
		return true
	case "g":
		s.projectFiles.editorOriginal = p.endpoint.Path
		s.projectFiles.step = "path"
		return true
	case " ":
		if len(entries) > 0 {
			p.marked[entries[p.selected].Name] = !p.marked[entries[p.selected].Name]
		}
		return true
	case "c":
		// Keyboard previews use the pane's current root. A previous drag target
		// must not survive a cancelled preview, including a rejected request.
		s.projectFiles.pendingDestination = ducklord.FileEndpoint{}
		s.projectFiles.hasPendingDestination = false
		if p.loading {
			s.projectFiles.status = "Loading files…"
			return true
		}
		if len(projectFilesMarkedNames(*p)) == 0 {
			if len(entries) == 0 {
				s.projectFiles.status = "No files to copy"
				return true
			}
			s.projectFiles.status = "Select files first"
			return true
		}
		s.projectFiles.copySource = s.projectFiles.active
		s.projectFiles.step = "preview"
		s.projectFiles.conflict = "skip"
		return true
	case "j", "\x1b[B":
		if p.selected < len(entries)-1 {
			p.selected++
		}
		return true
	case "k", "\x1b[A":
		if p.selected > 0 {
			p.selected--
		}
		return true
	case "\b", "\x7f":
		parent := filepath.Dir(p.endpoint.Path)
		if parent != p.endpoint.Path {
			p.endpoint.Path, p.query, p.selected, p.marked, p.entries, p.loading, p.status = parent, "", 0, map[string]bool{}, nil, true, "Loading…"
			s.loadProjectFilesPane(s.projectFilesLifecycle(), s.projectFiles.active)
		}
		return true
	case "\r", "\n":
		if len(entries) == 0 {
			return true
		}
		e := entries[p.selected]
		if e.IsDir {
			p.endpoint.Path = filepath.Join(p.endpoint.Path, e.Name)
			p.query = ""
			p.selected, p.marked, p.entries, p.loading, p.status = 0, map[string]bool{}, nil, true, "Loading…"
			s.loadProjectFilesPane(s.projectFilesLifecycle(), s.projectFiles.active)
		}
		return true
	}

	return true
}

// projectFilesMouseRelease turns a drag release over the other pane into the
// same conflict preview used by keyboard copy. The central parser calls this
// only for SGR button-release reports, so release bytes cannot reach a PTY.
func (s *tuiState) projectFilesMouseRelease(x, y int) []byte {
	if !s.projectFiles.open || s.projectFiles.step != "browse" || !s.projectFiles.dragArmed {
		return nil
	}
	s.projectFiles.dragArmed = false
	if x < 0 || y < 0 {
		return nil
	}
	for _, r := range s.modalMouseRegions {
		if r.row != y || x < r.left || x > r.right || r.action.selection == nil {
			continue
		}
		target := -1
		if r.action.selection == &s.projectFiles.left.selected {
			target = 0
		} else if r.action.selection == &s.projectFiles.right.selected {
			target = 1
		}
		if target < 0 || target == s.projectFiles.dragSource {
			return nil
		}
		tp := s.projectFiles.left
		if target == 1 {
			tp = s.projectFiles.right
		}
		destination := snapshotProjectFilesEndpoint(tp.endpoint)
		entries := s.visibleProjectEntries(&tp)
		if r.action.index >= 0 {
			if r.action.index >= len(entries) || !entries[r.action.index].IsDir {
				return nil
			}
			destination.Path = filepath.Join(destination.Path, entries[r.action.index].Name)
		}
		sp := &s.projectFiles.left
		if s.projectFiles.dragSource == 1 {
			sp = &s.projectFiles.right
		}
		if s.projectFiles.dragSourceIndex >= 0 && s.projectFiles.dragSourceIndex < len(s.visibleProjectEntries(sp)) {
			e := s.visibleProjectEntries(sp)[s.projectFiles.dragSourceIndex]
			sp.marked[e.Name] = true
		}
		s.projectFiles.pendingDestination, s.projectFiles.hasPendingDestination = destination, true
		s.projectFiles.copySource, s.projectFiles.step, s.projectFiles.conflict = s.projectFiles.dragSource, "preview", "skip"
		return []byte("c")
	}
	return nil
}
func (s *tuiState) startProjectFilesCopy() {
	source, destination := s.projectFiles.left, s.projectFiles.right
	s.projectFiles.copyDestination = 1
	if s.projectFiles.copySource == 1 {
		source, destination = s.projectFiles.right, s.projectFiles.left
		s.projectFiles.copyDestination = 0
	}
	if s.projectFiles.hasPendingDestination {
		destination.endpoint = snapshotProjectFilesEndpoint(s.projectFiles.pendingDestination)
	}
	names := projectFilesMarkedNames(source)
	if len(names) == 0 {
		s.projectFiles.step, s.projectFiles.status = "browse", "Select files first"
		s.projectFiles.pendingDestination = ducklord.FileEndpoint{}
		s.projectFiles.hasPendingDestination = false
		return
	}
	// Confirmation is the point at which a dragged child becomes the visible
	// destination. Esc leaves the browsing pane and its entries untouched.
	if s.projectFiles.copyDestination == 0 {
		s.projectFiles.left.endpoint = snapshotProjectFilesEndpoint(destination.endpoint)
	} else {
		s.projectFiles.right.endpoint = snapshotProjectFilesEndpoint(destination.endpoint)
	}
	s.projectFiles.step, s.projectFiles.status = "busy", "Copying…"
	s.projectFiles.copyCancelling = false
	s.projectFiles.pendingDestination = ducklord.FileEndpoint{}
	s.projectFiles.hasPendingDestination = false
	s.projectFiles.generation++
	for i, listCancel := range s.projectFiles.cancels {
		if listCancel != nil {
			listCancel()
			s.projectFiles.cancels[i] = nil
		}
	}
	ctx, cancel := s.projectFilesWorkerContext()
	s.projectFiles.cancel = cancel
	s.projectFiles.copyCancel = cancel
	gen := s.projectFiles.generation
	done := s.projectFiles.done
	lifecycle := s.projectFiles.lifecycle
	req := ducklord.FileCopyRequest{Source: snapshotProjectFilesEndpoint(source.endpoint), Destination: snapshotProjectFilesEndpoint(destination.endpoint), Names: names, Conflict: s.projectFiles.conflict}
	s.projectFiles.items = make([]projectFilesItem, len(names))
	for i, name := range names {
		s.projectFiles.items[i] = projectFilesItem{name: name, state: "queued"}
	}
	s.projectFiles.copyStarted = time.Now()
	s.projectFilesRememberBatch(projectFilesBatch{at: s.projectFiles.copyStarted, source: req.Source, destination: req.Destination, sourceLabel: source.label, destinationLabel: destination.label, names: append([]string(nil), names...), conflict: req.Conflict, items: append([]projectFilesItem(nil), s.projectFiles.items...)})
	req.Progress = func(progress ducklord.FileCopyProgress) {
		if done != nil {
			p := progress
			select {
			case done <- projectFilesEvent{generation: gen, copy: true, progress: &p}:
			case <-ctx.Done():
			}
		}
	}
	go func() {
		result, err := ducklord.CopyFiles(ctx, req)
		if done != nil {
			// Deliver cancellation acknowledgement so busy state cannot overlap a
			// second copy while the backend unwinds.
			sendProjectFilesCompletion(lifecycle, done, projectFilesEvent{generation: gen, result: result, err: err, copy: true})
		}
	}()
}

func (s *tuiState) renderProjectFilesModal(out io.Writer, cols, rows int) {
	s.modalMouseRegions = nil
	if !s.projectFiles.open {
		return
	}
	s.modalMouseLines = make(map[int]modalMouseAction)
	width := min(120, max(8, cols-2))
	if s.projectFiles.step == "preview" {
		src, dst := s.projectFiles.left, s.projectFiles.right
		if s.projectFiles.copySource == 1 {
			src, dst = dst, src
		}
		if s.projectFiles.hasPendingDestination {
			dst.endpoint = snapshotProjectFilesEndpoint(s.projectFiles.pendingDestination)
		}
		lines := []modalRenderLine{{modalTitle, "Copy preview · Enter confirm · Esc cancel"}, {modalMuted, "From: " + projectClip(projectFilesEndpointDescription(src), width-6)}, {modalMuted, "To: " + projectClip(projectFilesEndpointDescription(dst), width-4)}, {modalMuted, "Conflict policy: " + s.projectFiles.conflict + " (Overwrite means selected policy; replacement is not confirmed)"}}
		for _, name := range projectFilesMarkedNames(src) {
			if len(lines) >= rows-2 {
				break
			}
			lines = append(lines, modalRenderLine{modalMuted, "  " + projectClip(name, width-4)})
		}
		renderModalBoxWidthANSI(out, cols, rows, width, lines)
		return
	}
	if s.projectFiles.step == "history" {
		lines := s.projectFilesHistoryLines(width, rows)
		renderModalBoxWidthANSI(out, cols, rows, width, lines)
		return
	}
	if s.projectFiles.step == "path" || s.projectFiles.step == "filter" || s.projectFiles.step == "endpoints" {
		s.renderProjectFilesEditor(out, cols, rows, width)
		return
	}
	left, right := s.visibleProjectEntries(&s.projectFiles.left), s.visibleProjectEntries(&s.projectFiles.right)
	baseLines := []modalRenderLine{{modalTitle, "Project file exchange · Tab switch · l history"}}
	// Pane status reports the current directory listing. Keep the result of the
	// last transfer separate so a completed, partial, or cancelled copy remains
	// visible after the destination refresh replaces its pane status with a count.
	if s.projectFiles.status != "" {
		baseLines = append(baseLines, modalRenderLine{modalMuted, "Transfer: " + projectClip(s.projectFiles.status, width-2)})
	}
	_, cw, geometry := projectFilesGeometry(cols, rows, len(baseLines))
	stacked := width < 58
	panes := [2]projectFilesPane{s.projectFiles.left, s.projectFiles.right}
	entries := [2][]ducklord.FileEntry{left, right}
	visibleOffsets := [2]int{}
	lines := append([]modalRenderLine(nil), baseLines...)
	entryOffsets := [2]int{}
	panelSummary := func(side int) (string, string, string) {
		p := panes[side]
		active := ""
		if s.projectFiles.active == side {
			active = " · ACTIVE"
		}
		top := fmt.Sprintf(" %s%s%s ", []string{"LEFT", "RIGHT"}[side], active, s.projectFilesEndpointDirection(p.endpoint))
		host := "Host: " + projectFilesEndpointLabel(p)
		summary := fmt.Sprintf("%d visible · %d marked", len(entries[side]), len(projectFilesMarkedNames(p)))
		if p.query != "" {
			summary += " · Filter: " + p.query
		}
		if p.status != "" {
			summary += " · " + p.status
		} else if s.projectFiles.status != "" {
			summary += " · " + s.projectFiles.status
		}
		return top, host, summary
	}
	entryText := func(side, idx, panelWidth int) string {
		if idx < 0 || idx >= len(entries[side]) {
			return ""
		}
		p := panes[side]
		value := projectFilesEntryText(entries[side][idx], p, idx == p.selected, s.projectFiles.asciiFolders)
		if mark := s.projectFilesLatestMark(p, entries[side][idx]); mark != "" {
			return projectClip(value, panelWidth-12) + "  " + mark
		}
		return projectClip(value, panelWidth-2)
	}
	if stacked {
		for side := 0; side < 2; side++ {
			g := geometry[side]
			for len(lines) < g.start {
				lines = append(lines, modalRenderLine{modalMuted, ""})
			}
			top, host, summary := panelSummary(side)
			active := s.projectFiles.active == side
			lines = append(lines,
				modalRenderLine{modalMuted, projectFilesPanelTop(side, active, top, g.width)},
				modalRenderLine{modalMuted, projectFilesPanelText(side, active, projectClip(host, g.width-2), g.width)},
				modalRenderLine{modalMuted, projectFilesPanelText(side, active, projectClip("Path: "+panes[side].endpoint.Path, g.width-2), g.width)},
				modalRenderLine{modalMuted, projectFilesPanelText(side, active, projectClip(summary, g.width-2), g.width)},
			)
			panelRows := max(0, g.rows-2)
			visibleOffsets[side] = projectFilesVisibleOffset(panes[side].selected, panelRows)
			entryOffsets[side] = len(lines)
			for row := 0; row < panelRows; row++ {
				lines = append(lines, modalRenderLine{modalMuted, projectFilesPanelText(side, active, entryText(side, row+visibleOffsets[side], g.width), g.width)})
			}
			lines = append(lines, modalRenderLine{modalMuted, projectFilesPanelBottom(side, active, g.width)})
		}
	} else {
		for row := 0; row < 4; row++ {
			parts := [2]string{}
			for side := 0; side < 2; side++ {
				top, host, summary := panelSummary(side)
				active := s.projectFiles.active == side
				switch row {
				case 0:
					parts[side] = projectFilesPanelTop(side, active, top, cw)
				case 1:
					parts[side] = projectFilesPanelText(side, active, projectClip(host, cw-2), cw)
				case 2:
					parts[side] = projectFilesPanelText(side, active, projectClip("Path: "+panes[side].endpoint.Path, cw-2), cw)
				case 3:
					parts[side] = projectFilesPanelText(side, active, projectClip(summary, cw-2), cw)
				}
			}
			lines = append(lines, modalRenderLine{modalMuted, parts[0] + "  " + parts[1]})
		}
		panelRows := max(0, geometry[0].rows-2)
		for side := 0; side < 2; side++ {
			visibleOffsets[side] = projectFilesVisibleOffset(panes[side].selected, panelRows)
		}
		entryOffsets = [2]int{len(lines), len(lines)}
		for row := 0; row < panelRows; row++ {
			parts := [2]string{}
			for side := 0; side < 2; side++ {
				parts[side] = projectFilesPanelText(side, s.projectFiles.active == side, entryText(side, row+visibleOffsets[side], cw), cw)
			}
			lines = append(lines, modalRenderLine{modalMuted, parts[0] + "  " + parts[1]})
		}
		parts := [2]string{}
		for side := 0; side < 2; side++ {
			parts[side] = projectFilesPanelBottom(side, s.projectFiles.active == side, cw)
		}
		lines = append(lines, modalRenderLine{modalMuted, parts[0] + "  " + parts[1]})
	}
	lines = append(lines, modalRenderLine{modalMuted, "h endpoint · g path · / filter · Space select · i folder icons [D] · c copy preview"}, modalRenderLine{modalMuted, "Tab switch column · Enter open · Esc back/close · Ctrl+C cancel · l history"})
	if s.projectFiles.step == "path" {
		lines = append(lines, modalRenderLine{modalInput, "Path: " + projectClip(s.projectFilesPane().endpoint.Path, width-8) + "  Enter apply · Esc cancel"})
	}
	if s.projectFiles.step == "filter" {
		lines = append(lines, modalRenderLine{modalInput, "Filter: " + projectClip(s.projectFilesPane().query, width-8) + "  Enter apply · Esc cancel"})
	}
	if s.projectFiles.step == "endpoints" {
		lines = append(lines, modalRenderLine{modalTitle, "Endpoint picker"})
		for i, n := range s.projectFilesEndpointNames() {
			mark := "  "
			if i == s.projectFiles.endpointIndex {
				mark = "› "
			}
			lines = append(lines, modalRenderLine{modalMuted, mark + n})
		}
		lines = append(lines, modalRenderLine{modalMuted, "j/k choose · Enter apply · Esc cancel"})
	}
	renderModalBoxWidthANSI(out, cols, rows, width, lines)
	if s.projectFiles.step != "browse" {
		return
	}
	visible := min(len(lines), rows-2)
	top := max(1, (rows-visible-2)/2+1)
	boxLeft := max(1, (cols-width)/2+1)
	for side := 0; side < 2; side++ {
		g := geometry[side]
		panelRows := g.rows - 2
		for row := 0; row < max(0, panelRows); row++ {
			idx := row + visibleOffsets[side]
			if idx >= len(entries[side]) {
				if len(entries[side]) != 0 {
					continue
				}
				idx = -1
			}
			lineIndex := entryOffsets[side] + row
			if lineIndex >= visible {
				continue
			}
			sel := &s.projectFiles.left.selected
			if side == 1 {
				sel = &s.projectFiles.right.selected
			}
			l := boxLeft + g.left
			r := boxLeft + g.right
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left: l, right: r, row: top + lineIndex + 1, action: modalMouseAction{selection: sel, index: idx, key: " "}})
		}
	}
}

func (s *tuiState) renderProjectFilesEditor(out io.Writer, cols, rows, width int) {
	lines := []modalRenderLine{{modalTitle, "Project file exchange"}}
	switch s.projectFiles.step {
	case "path":
		p := s.projectFilesPane()
		lines = append(lines, modalRenderLine{modalInput, "Path: " + projectClip(p.endpoint.Path, width-8)}, modalRenderLine{modalMuted, "Enter apply · Esc cancel"})
	case "filter":
		p := s.projectFilesPane()
		lines = append(lines, modalRenderLine{modalInput, "Filter: " + projectClip(p.query, width-8)}, modalRenderLine{modalMuted, "Enter apply · Esc cancel"})
	case "endpoints":
		lines = append(lines, modalRenderLine{modalTitle, "Endpoint picker · Choose with j/k or arrows · Enter apply · Esc cancel"})
		names := s.projectFilesEndpointNames()
		maxVisible := max(1, rows-5)
		start := max(0, s.projectFiles.endpointIndex-maxVisible+1)
		end := min(len(names), start+maxVisible)
		for i := start; i < end; i++ {
			mark := "  "
			if i == s.projectFiles.endpointIndex {
				mark = "› "
			}
			lines = append(lines, modalRenderLine{modalMuted, mark + projectClip(names[i], width-6)})
		}
	}
	renderModalBox(out, cols, rows, lines)
}

func projectFilesPanelText(side int, active bool, value string, width int) string {
	if width < 2 {
		return projectFilesPanelColor(side, active) + modalCellPad(value, width) + modalReset
	}
	return projectFilesPanelColor(side, active) + "│" + modalCellPad(value, width-2) + "│" + modalReset
}

func projectFilesPanelTop(side int, active bool, value string, width int) string {
	if width < 2 {
		return projectFilesPanelColor(side, active) + modalCellPad(value, width) + modalReset
	}
	return projectFilesPanelColor(side, active) + "┌" + modalCellPad(projectClip(value, width-2), width-2) + "┐" + modalReset
}

func projectFilesPanelBottom(side int, active bool, width int) string {
	if width < 2 {
		return projectFilesPanelColor(side, active) + modalReset
	}
	return projectFilesPanelColor(side, active) + "└" + strings.Repeat("─", width-2) + "┘" + modalReset
}

func projectFilesEndpointLabel(p projectFilesPane) string {
	if p.label != "" {
		return p.label
	}
	if p.endpoint.Client != nil {
		return p.endpoint.Client.Name
	}
	return "LOCAL"
}

func projectFilesEntryText(e ducklord.FileEntry, p projectFilesPane, selected, ascii bool) string {
	icon := "  "
	if e.IsDir {
		icon = "📁"
		if ascii {
			icon = "[D]"
		}
	}
	mark := "[ ]"
	if p.marked[e.Name] {
		mark = "[x]"
	}
	cursor := "  "
	if selected {
		cursor = "› "
	}
	return cursor + mark + " " + icon + " " + e.Name
}

func (s *tuiState) projectFilesHistoryLines(width, rows int) []modalRenderLine {
	lines := []modalRenderLine{{modalTitle, "Transfer history · ←/→ batch · ↑/↓ item · x clear · Esc browser"}}
	if len(s.projectFiles.history) == 0 {
		return append(lines, modalRenderLine{modalMuted, "No batches for this Project"})
	}
	idx := min(s.projectFiles.historyBatch, len(s.projectFiles.history)-1)
	b := s.projectFiles.history[idx]
	batchText := fmt.Sprintf("Batch %d/%d · %s · %s → %s · %s", idx+1, len(s.projectFiles.history), b.at.Format("15:04:05"), b.sourceLabel, b.destinationLabel, b.conflict)
	if b.err != "" {
		batchText += " · Error: " + b.err
	}
	lines = append(lines, modalRenderLine{modalMuted, projectClip(batchText, width-4)})
	lines = append(lines, modalRenderLine{modalMuted, "Sent: " + projectClip(projectFilesEndpointDescription(projectFilesPane{label: b.sourceLabel, endpoint: b.source}), width-9)}, modalRenderLine{modalMuted, "Received: " + projectClip(projectFilesEndpointDescription(projectFilesPane{label: b.destinationLabel, endpoint: b.destination}), width-12)})
	if len(b.items) == 0 {
		lines = append(lines, modalRenderLine{modalMuted, "No items started"})
	}
	maxItems := max(0, rows-7)
	start := max(0, s.projectFiles.historyItem-maxItems+1)
	end := min(len(b.items), start+maxItems)
	for i := start; i < end; i++ {
		item := b.items[i]
		style := projectFilesStatusStyle(item.state)
		mark := "  "
		if i == s.projectFiles.historyItem {
			mark = "› "
		}
		text := mark + projectFilesItemRow(item)
		if item.destination != "" && item.destination != item.name {
			text += " → " + item.destination
		}
		if item.err != "" {
			text += " · " + item.err
		}
		lines = append(lines, modalRenderLine{style, projectClip(text, width-4)})
	}
	if b.err != "" && len(lines) < rows-2 {
		lines = append(lines, modalRenderLine{modalDanger, "Error: " + projectClip(b.err, width-10)})
	}
	return lines
}
