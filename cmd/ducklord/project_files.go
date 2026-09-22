package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	dragSource                int
	dragSourceIndex           int
	dragArmed                 bool
	pendingDestination        ducklord.FileEndpoint
	hasPendingDestination     bool
	copyDestination           int
	copyCancelling            bool
}

type projectFilesEvent struct {
	generation uint64
	panegen    uint64
	side       int
	entries    []ducklord.FileEntry
	result     []ducklord.FileCopyResult
	err        error
	copy       bool
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
	s.loadProjectFilesPane(context.Background(), 0)
	s.loadProjectFilesPane(context.Background(), 1)
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
	s.loadProjectFilesPane(context.Background(), s.projectFiles.active)
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
		s.projectFiles.copyCancel = nil
		copied, skipped := 0, 0
		for _, result := range event.result {
			if result.Skipped {
				skipped++
			} else {
				copied++
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
		s.projectFiles.step = "browse"
		// A cancelled copy can have completed work. Always reload the destination
		// that the request actually used, including a dragged child directory.
		s.loadProjectFilesPane(context.Background(), s.projectFiles.copyDestination)
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
				s.loadProjectFilesPane(context.Background(), s.projectFiles.active)
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
	case "/":
		s.projectFiles.editorOriginal = p.query
		p.query = ""
		s.projectFiles.step = "filter"
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
		// must not survive a cancelled preview.
		s.projectFiles.pendingDestination = ducklord.FileEndpoint{}
		s.projectFiles.hasPendingDestination = false
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
			s.loadProjectFilesPane(context.Background(), s.projectFiles.active)
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
			s.loadProjectFilesPane(context.Background(), s.projectFiles.active)
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
	names := make([]string, 0, len(source.marked))
	for name, marked := range source.marked {
		if marked {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		s.projectFiles.status = "Select files first"
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
	ctx, cancel := context.WithCancel(context.Background())
	s.projectFiles.cancel = cancel
	s.projectFiles.copyCancel = cancel
	gen := s.projectFiles.generation
	done := s.projectFiles.done
	req := ducklord.FileCopyRequest{Source: snapshotProjectFilesEndpoint(source.endpoint), Destination: snapshotProjectFilesEndpoint(destination.endpoint), Names: names, Conflict: s.projectFiles.conflict}
	go func() {
		result, err := ducklord.CopyFiles(ctx, req)
		if done != nil {
			// Deliver cancellation acknowledgement so busy state cannot overlap a
			// second copy while the backend unwinds.
			done <- projectFilesEvent{generation: gen, result: result, err: err, copy: true}
		}
	}()
}

func (s *tuiState) renderProjectFilesModal(out io.Writer, cols, rows int) {
	if !s.projectFiles.open {
		return
	}
	s.modalMouseLines = make(map[int]modalMouseAction)
	// Match the width used by the shared modal renderer and mouse coordinate
	// mapping. A two-cell separator leaves two independently addressable panes.
	width := min(110, max(8, cols-2))
	cw := max(1, (width-4)/2)
	if s.projectFiles.step == "preview" {
		src, dst := s.projectFiles.left, s.projectFiles.right
		if s.projectFiles.copySource == 1 {
			src, dst = dst, src
		}
		if s.projectFiles.hasPendingDestination {
			dst.endpoint = snapshotProjectFilesEndpoint(s.projectFiles.pendingDestination)
		}
		// Put confirmation first so it remains reachable on a short terminal;
		// selected names are informational and may be clipped below it.
		lines := []modalRenderLine{
			{modalTitle, "Copy preview · Enter confirm · Esc cancel"},
			{modalMuted, "From: " + projectClip(projectFilesEndpointDescription(src), width-6)},
			{modalMuted, "To: " + projectClip(projectFilesEndpointDescription(dst), width-4)},
			{modalMuted, "Conflict: [s] Skip  [r] Rename  [o] Overwrite (" + s.projectFiles.conflict + ")"},
		}
		names := make([]string, 0, len(src.marked))
		for name, marked := range src.marked {
			if marked {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		available := max(0, rows-2-len(lines))
		for i, name := range names {
			if i >= available {
				break
			}
			lines = append(lines, modalRenderLine{modalMuted, "  " + projectClip(name, width-4)})
		}
		if len(names) > available && available > 0 {
			lines[len(lines)-1] = modalRenderLine{modalMuted, fmt.Sprintf("  +%d more selected", len(names)-available+1)}
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}

	leftLabel, rightLabel := s.projectFiles.left.label, s.projectFiles.right.label
	lines := []modalRenderLine{
		{modalTitle, "Project files"},
		{modalMuted, modalCellPad(projectClip(leftLabel, cw), cw) + "  " + modalCellPad(projectClip(rightLabel, cw), cw)},
		{modalInput, modalCellPad(projectClip(s.projectFiles.left.endpoint.Path, cw), cw) + "  " + modalCellPad(projectClip(s.projectFiles.right.endpoint.Path, cw), cw)},
	}
	leftStatus, rightStatus := s.projectFiles.left.status, s.projectFiles.right.status
	if leftStatus == "" {
		leftStatus = s.projectFiles.status
	}
	if rightStatus == "" {
		rightStatus = s.projectFiles.status
	}
	lines = append(lines, modalRenderLine{modalMuted, modalCellPad(projectClip(leftStatus, cw), cw) + "  " + modalCellPad(projectClip(rightStatus, cw), cw)})
	if s.projectFiles.step == "path" {
		lines = append(lines, modalRenderLine{modalInput, "Path: " + projectClip(s.projectFilesPane().endpoint.Path, width-8) + "  (Enter apply, Esc cancel)"})
	}
	if s.projectFiles.step == "filter" {
		lines = append(lines, modalRenderLine{modalInput, "Filter: " + projectClip(s.projectFilesPane().query, width-8) + "  (Enter apply, Esc cancel)"})
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
	left, right := s.visibleProjectEntries(&s.projectFiles.left), s.visibleProjectEntries(&s.projectFiles.right)
	n := max(len(left), len(right))
	// The common renderer can show rows-2 content lines. Reserve the footer
	// after every step-specific header so no invisible entry receives a hitbox.
	maxRows := max(0, rows-2-len(lines)-3)
	offsetL, offsetR := 0, 0
	if maxRows > 0 && s.projectFiles.left.selected >= maxRows {
		offsetL = s.projectFiles.left.selected - maxRows + 1
	}
	if maxRows > 0 && s.projectFiles.right.selected >= maxRows {
		offsetR = s.projectFiles.right.selected - maxRows + 1
	}
	for i := 0; i < n && i < maxRows; i++ {
		li, ri := i+offsetL, i+offsetR
		l, r := "", ""
		if li < len(left) {
			l = left[li].Name
			if left[li].IsDir {
				l += "/"
			}
			if s.projectFiles.left.marked[left[li].Name] {
				l = "[x] " + l
			}
		}
		if ri < len(right) {
			r = right[ri].Name
			if right[ri].IsDir {
				r += "/"
			}
			if s.projectFiles.right.marked[right[ri].Name] {
				r = "[x] " + r
			}
		}
		if li == s.projectFiles.left.selected && s.projectFiles.active == 0 {
			l = "› " + l
		}
		if ri == s.projectFiles.right.selected && s.projectFiles.active == 1 {
			r = "› " + r
		}
		lines = append(lines, modalRenderLine{modalMuted, modalCellPad(projectClip(l, cw), cw) + "  " + modalCellPad(projectClip(r, cw), cw)})
	}
	lines = append(lines, modalRenderLine{modalMuted, s.projectFiles.status}, modalRenderLine{modalMuted, "Tab switch column · h endpoint · g path · / filter · Space select"}, modalRenderLine{modalMuted, "c copy preview · Enter open/confirm · Esc back/close · Ctrl+C cancel"})
	s.renderModalBox(out, cols, rows, lines)
	// Register independent hit regions after box rendering; modalChoice cannot represent two columns on one row.
	if s.projectFiles.step != "browse" {
		return
	}
	visible := min(len(lines), rows-2)
	top := max(1, (rows-visible-2)/2+1)
	entryStart := 4
	for i := 0; i < n && i < maxRows; i++ {
		row := top + entryStart + i + 1
		base := max(1, (cols-width)/2+2)
		if i+offsetL < len(left) {
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left: base, right: base + cw - 1, row: row, action: modalMouseAction{selection: &s.projectFiles.left.selected, index: i + offsetL, key: " "}})
		}
		if i+offsetR < len(right) {
			x := base + cw + 2
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left: x, right: x + cw - 1, row: row, action: modalMouseAction{selection: &s.projectFiles.right.selected, index: i + offsetR, key: " "}})
		}
	}
	// An empty destination is still a valid drop root.
	if maxRows > 0 {
		base, row := max(1, (cols-width)/2+2), top+entryStart+1
		if len(left) == 0 {
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left: base, right: base + cw - 1, row: row, action: modalMouseAction{selection: &s.projectFiles.left.selected, index: -1, key: " "}})
		}
		if len(right) == 0 {
			x := base + cw + 2
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left: x, right: x + cw - 1, row: row, action: modalMouseAction{selection: &s.projectFiles.right.selected, index: -1, key: " "}})
		}
	}
}
