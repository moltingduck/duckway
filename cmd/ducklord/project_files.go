package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	done                      chan projectFilesEvent
	dragSource                int
	dragSourceIndex           int
	dragArmed                 bool
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
	if sess := s.activePTYSession(); sess.Cwd != "" {
		p.endpoint = ducklord.FileEndpoint{Path: sess.Cwd}
		if sess.Client != "" {
			for i := range s.cfg.Clients {
				if s.cfg.Clients[i].Name == sess.Client {
					q.endpoint = ducklord.FileEndpoint{Client: &s.cfg.Clients[i], Path: sess.Cwd}
					break
				}
			}
		}
	}
	if nav, err := s.workspaceNavigation(); err == nil {
		s.projectFiles.originProject, s.projectFiles.originPane, s.projectFiles.originTab = nav.CurrentProjectID(), nav.CurrentPaneID(), nav.CurrentTabID()
		if shelf, e := ducklord.ProjectExchangePath(s.cfgPath, nav.CurrentProjectID()); e == nil {
			q.label, q.endpoint = "PROJECT SHELF", ducklord.FileEndpoint{Path: shelf}
		} else {
			q.status = "Project shelf unavailable: " + sanitizeTerminalText(e.Error())
		}
	}
	if q.endpoint.Path == "" {
		q.endpoint.Path = p.endpoint.Path
	}
	s.projectFiles.originFocused, s.projectFiles.originAttachKey = s.focused, s.activeAttachKey
	s.projectFiles.originProjectFocus = s.workspaceProjectFocus
	s.projectFiles.left, s.projectFiles.right = p, q
	s.projectFiles.active, s.projectFiles.step, s.projectFiles.open = 0, "browse", true
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
	if p.query == "" {
		return p.entries
	}
	out := make([]ducklord.FileEntry, 0)
	q := strings.ToLower(p.query)
	for _, e := range p.entries {
		if strings.Contains(strings.ToLower(e.Name), q) {
			out = append(out, e)
		}
	}
	return out
}
func (s *tuiState) loadProjectFilesPane(parent context.Context, side int) {
	p := s.projectFiles.left
	if side == 1 {
		p = s.projectFiles.right
	}
	s.projectFiles.panegen[side]++
	gen := s.projectFiles.panegen[side]
	globalGen := s.projectFiles.generation
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
		return
	}
	if i <= len(s.cfg.Clients) {
		p.endpoint.Client = &s.cfg.Clients[i-1]
		return
	}
	if nav, err := s.workspaceNavigation(); err == nil {
		if path, e := ducklord.ProjectExchangePath(s.cfgPath, nav.CurrentProjectID()); e == nil {
			p.endpoint.Client = nil
			p.endpoint.Path = path
			p.label = "PROJECT SHELF"
		} else {
			p.status = "Project shelf unavailable: " + sanitizeTerminalText(e.Error())
		}
	}
}

func projectClip(v string, width int) string {
	v = sanitizeTerminalText(v)
	r := []rune(v)
	if width < 1 {
		return ""
	}
	if len(r) <= width {
		return v
	}
	if width == 1 {
		return string(r[:1])
	}
	return string(r[:width-1]) + "…"
}

func (s *tuiState) applyProjectFilesEvent(event projectFilesEvent) {
	if !s.projectFiles.open || event.generation != s.projectFiles.generation || (!event.copy && event.panegen != s.projectFiles.panegen[event.side]) {
		return
	}
	if event.copy {
		s.projectFiles.copyCancel = nil
		if event.err != nil {
			s.projectFiles.status = sanitizeTerminalText(event.err.Error())
		} else {
			s.projectFiles.status = fmt.Sprintf("Copied %d item(s)", len(event.result))
		}
		s.projectFiles.step = "browse"
		s.loadProjectFilesPane(context.Background(), 1-s.projectFiles.active)
		return
	}
	if event.side == 0 {
		s.projectFiles.left.entries = event.entries
		s.projectFiles.left.loading = false
	} else {
		s.projectFiles.right.entries = event.entries
		s.projectFiles.right.loading = false
	}
	var pane *projectFilesPane = &s.projectFiles.left
	if event.side == 1 {
		pane = &s.projectFiles.right
	}
	if event.err != nil {
		pane.status = sanitizeTerminalText(event.err.Error())
		s.projectFiles.status = pane.status
	} else {
		pane.status = fmt.Sprintf("%d items", len(event.entries))
		s.projectFiles.status = pane.status
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
	if s.projectFiles.step == "filter" {
		if text == "\r" || text == "\n" || text == "\x1b" {
			s.projectFiles.step = "browse"
			return true
		}
		if text == "\b" || text == "\x7f" {
			if len(p.query) > 0 {
				p.query = p.query[:len(p.query)-1]
			}
		} else if len(text) == 1 && text[0] >= 0x20 {
			p.query += text
		}
		p.selected = 0
		return true
	}
	if s.projectFiles.step == "path" {
		if text == "\r" || text == "\n" {
			if filepath.IsAbs(p.endpoint.Path) {
				p.entries = nil
				s.projectFiles.step = "browse"
				s.loadProjectFilesPane(context.Background(), s.projectFiles.active)
			}
			return true
		}
		if text == "\b" || text == "\x7f" {
			if len(p.endpoint.Path) > 0 {
				p.endpoint.Path = p.endpoint.Path[:len(p.endpoint.Path)-1]
			}
		} else if len(text) == 1 && text[0] >= 0x20 {
			p.endpoint.Path += text
		}
		return true
	}
	if s.projectFiles.step == "endpoints" {
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
		if text == "\r" || text == "\n" {
			s.applyProjectFilesEndpoint(s.projectFiles.endpointIndex)
			s.projectFiles.step = "browse"
		} else if text == "\x1b" {
			s.projectFiles.step = "browse"
		}
		return true
	}
	if s.projectFiles.step == "preview" {
		switch text {
		case "\x1b", "q":
			s.projectFiles.step = "browse"
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
	case "\x03":
		if s.projectFiles.step == "busy" {
			if s.projectFiles.copyCancel != nil {
				s.projectFiles.copyCancel()
			}
			s.projectFiles.copyCancel = nil
			s.projectFiles.generation++
			s.projectFiles.step = "browse"
			s.projectFiles.status = "Cancelled"
		} else {
			s.closeProjectFiles()
		}
		return true
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
		p.query = ""
		s.projectFiles.step = "filter"
		return true
	case "h":
		s.projectFiles.step = "endpoints"
		return true
	case "g":
		s.projectFiles.step = "path"
		return true
	case " ":
		if len(entries) > 0 {
			p.marked[entries[p.selected].Name] = !p.marked[entries[p.selected].Name]
		}
		return true
	case "c":
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
	case "\r", "\n":
		if len(entries) == 0 {
			return true
		}
		e := entries[p.selected]
		if e.IsDir {
			p.endpoint.Path = filepath.Join(p.endpoint.Path, e.Name)
			p.selected = 0
			p.entries = nil
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
	if !s.projectFiles.open || !s.projectFiles.dragArmed {
		return nil
	}
	s.projectFiles.dragArmed = false
	if x < 0 || y < 0 {
		return nil
	}
	target := 1 - s.projectFiles.dragSource
	for _, r := range s.modalMouseRegions {
		if r.row != y || x < r.left || x > r.right || r.action.selection == nil {
			continue
		}
		if r.action.selection == &s.projectFiles.left.selected {
			target = 0
		} else if r.action.selection == &s.projectFiles.right.selected {
			target = 1
		} else {
			continue
		}
		if target == s.projectFiles.dragSource || r.action.index < 0 {
			return nil
		}
		var tp *projectFilesPane
		if target == 0 {
			tp = &s.projectFiles.left
		} else {
			tp = &s.projectFiles.right
		}
		entries := s.visibleProjectEntries(tp)
		if r.action.index >= len(entries) || !entries[r.action.index].IsDir {
			return nil
		}
		var sp *projectFilesPane
		if s.projectFiles.dragSource == 0 {
			sp = &s.projectFiles.left
		} else {
			sp = &s.projectFiles.right
		}
		if s.projectFiles.dragSourceIndex >= 0 && s.projectFiles.dragSourceIndex < len(s.visibleProjectEntries(sp)) {
			e := s.visibleProjectEntries(sp)[s.projectFiles.dragSourceIndex]
			sp.marked[e.Name] = true
		}
		tp.endpoint.Path = filepath.Join(tp.endpoint.Path, entries[r.action.index].Name)
		s.projectFiles.active, s.projectFiles.copySource = target, s.projectFiles.dragSource
		s.projectFiles.step, s.projectFiles.conflict = "preview", "skip"
		return []byte("c")
	}
	return nil
}

func (s *tuiState) startProjectFilesCopy() {
	source, destination := s.projectFiles.left, s.projectFiles.right
	if s.projectFiles.copySource == 1 {
		source, destination = s.projectFiles.right, s.projectFiles.left
	}
	names := make([]string, 0, len(source.marked))
	for name, marked := range source.marked {
		if marked {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		s.projectFiles.status = "Select files first"
		return
	}
	s.projectFiles.step, s.projectFiles.status = "busy", "Copying…"
	s.projectFiles.generation++
	ctx, cancel := context.WithCancel(context.Background())
	s.projectFiles.cancel = cancel
	s.projectFiles.copyCancel = cancel
	gen := s.projectFiles.generation
	done := s.projectFiles.done
	req := ducklord.FileCopyRequest{Source: source.endpoint, Destination: destination.endpoint, Names: names, Conflict: s.projectFiles.conflict}
	go func() {
		result, err := ducklord.CopyFiles(ctx, req)
		if done != nil {
			select {
			case done <- projectFilesEvent{generation: gen, result: result, err: err, copy: true}:
			case <-ctx.Done():
			}
		}
	}()
}

func (s *tuiState) renderProjectFilesModal(out io.Writer, cols, rows int) {
	if !s.projectFiles.open {
		return
	}
	s.modalMouseLines = make(map[int]modalMouseAction)
	width := min(72, max(8, cols-2))
	cw := max(8, (width-4)/2)
	leftLabel, rightLabel := s.projectFiles.left.label, s.projectFiles.right.label
	lines := []modalRenderLine{{modalTitle, "Project files"}, {modalMuted, projectClip(leftLabel, cw) + strings.Repeat(" ", max(1, width-4-cw)) + projectClip(rightLabel, cw)}, {modalInput, projectClip(s.projectFiles.left.endpoint.Path, cw) + "  " + projectClip(s.projectFiles.right.endpoint.Path, cw)}}
	leftStatus, rightStatus := s.projectFiles.left.status, s.projectFiles.right.status
	if leftStatus == "" {
		leftStatus = s.projectFiles.status
	}
	if rightStatus == "" {
		rightStatus = s.projectFiles.status
	}
	lines = append(lines, modalRenderLine{modalMuted, projectClip(leftStatus, cw) + "  " + projectClip(rightStatus, cw)})
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
	if s.projectFiles.step == "preview" {
		src, dst := s.projectFiles.left, s.projectFiles.right
		if s.projectFiles.copySource == 1 {
			src, dst = dst, src
		}
		lines = append(lines, modalRenderLine{modalTitle, "Copy preview"}, modalRenderLine{modalMuted, "From: " + projectClip(src.endpoint.Path, width-8)}, modalRenderLine{modalMuted, "To: " + projectClip(dst.endpoint.Path, width-6)}, modalRenderLine{modalMuted, "Conflict: [s] Skip  [r] Rename  [o] Overwrite (" + s.projectFiles.conflict + ")"})
		for name, marked := range src.marked {
			if marked {
				lines = append(lines, modalRenderLine{modalMuted, "  " + projectClip(name, width-4)})
			}
		}
	}
	left, right := s.visibleProjectEntries(&s.projectFiles.left), s.visibleProjectEntries(&s.projectFiles.right)
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	maxRows := max(1, rows-12)
	offsetL, offsetR := 0, 0
	if s.projectFiles.left.selected >= maxRows {
		offsetL = s.projectFiles.left.selected - maxRows + 1
	}
	if s.projectFiles.right.selected >= maxRows {
		offsetR = s.projectFiles.right.selected - maxRows + 1
	}
	offset := offsetL
	if offsetR > offset {
		offset = offsetR
	}
	for i := 0; i < n-offset && i < maxRows; i++ {
		li, ri := i+offset, i+offset
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
		lines = append(lines, modalRenderLine{modalMuted, projectClip(l, cw) + "  " + projectClip(r, cw)})
	}
	lines = append(lines, modalRenderLine{modalMuted, s.projectFiles.status}, modalRenderLine{modalMuted, "Tab switch column · h endpoint · g path · / filter · Space select"}, modalRenderLine{modalMuted, "c copy preview · Enter open/confirm · Esc back/close · Ctrl+C cancel"})
	s.renderModalBox(out, cols, rows, lines)
	// Register independent hit regions after box rendering; modalChoice cannot represent two columns on one row.
	visible := min(len(lines), rows-2)
	top := max(1, (rows-visible-2)/2+1)
	entryStart := 4
	if s.projectFiles.step == "preview" {
		entryStart += 4 + len(s.projectFiles.left.marked)
	}
	for i := 0; i < n-offset && i < maxRows; i++ {
		row := top + entryStart + i + 1
		if i+offset < len(left) {
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left: top*0 + max(1, (cols-width)/2+2), right: max(1, (cols-width)/2+2) + cw - 1, row: row, action: modalMouseAction{selection: &s.projectFiles.left.selected, index: i + offset, key: " "}})
		}
		if i+offset < len(right) {
			x := max(1, (cols-width)/2+2) + cw + 2
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left: x, right: x + cw - 1, row: row, action: modalMouseAction{selection: &s.projectFiles.right.selected, index: i + offset, key: " "}})
		}
	}
}
