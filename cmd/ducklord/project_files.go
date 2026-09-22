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
	generation                uint64
	cancel                    context.CancelFunc
	cancels                   [2]context.CancelFunc
	done                      chan projectFilesEvent
	dragSource                int
	dragArmed                 bool
}

type projectFilesEvent struct {
	generation uint64
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
		s.projectFiles.originProject, s.projectFiles.originPane = nav.CurrentProjectID(), nav.CurrentPaneID()
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
	return err == nil && nav.CurrentProjectID() == s.projectFiles.originProject
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
	gen := s.projectFiles.generation
	ctx, cancel := context.WithCancel(parent)
	s.projectFiles.cancels[side] = cancel
	s.projectFiles.cancel = cancel
	go func() {
		entries, err := ducklord.ListFiles(ctx, p.endpoint)
		if s.projectFiles.done != nil {
			s.projectFiles.done <- projectFilesEvent{generation: gen, side: side, entries: entries, err: err}
		}
	}()
}

func (s *tuiState) applyProjectFilesEvent(event projectFilesEvent) {
	if !s.projectFiles.open || event.generation != s.projectFiles.generation {
		return
	}
	if event.copy {
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
		if text == "\r" || text == "\n" || text == "\x1b" {
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
			if s.projectFiles.cancel != nil {
				s.projectFiles.cancel()
			}
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
	s.projectFiles.active = 1 - s.projectFiles.dragSource
	s.projectFiles.step = "preview"
	s.projectFiles.conflict = "skip"
	return []byte("c")
}

func (s *tuiState) startProjectFilesCopy() {
	source, destination := s.projectFiles.left, s.projectFiles.right
	if s.projectFiles.active == 1 {
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
	ctx, cancel := context.WithCancel(context.Background())
	s.projectFiles.cancel = cancel
	gen := s.projectFiles.generation
	req := ducklord.FileCopyRequest{Source: source.endpoint, Destination: destination.endpoint, Names: names, Conflict: s.projectFiles.conflict}
	go func() {
		result, err := ducklord.CopyFiles(ctx, req)
		if s.projectFiles.done != nil {
			s.projectFiles.done <- projectFilesEvent{generation: gen, result: result, err: err, copy: true}
		}
	}()
}

func (s *tuiState) renderProjectFilesModal(out io.Writer, cols, rows int) {
	if !s.projectFiles.open {
		return
	}
	// Keep this renderer independent of terminal width: modalRenderBox clips safely.
	s.modalMouseLines = make(map[int]modalMouseAction)
	lines := []modalRenderLine{{modalTitle, "Project files"}, {modalMuted, "LOCAL                         DESTINATION"}, {modalInput, fmt.Sprintf("%-29s  %s", sanitizeTerminalText(s.projectFiles.left.endpoint.Path), sanitizeTerminalText(s.projectFiles.right.endpoint.Path))}}
	leftStatus, rightStatus := s.projectFiles.left.status, s.projectFiles.right.status
	if leftStatus == "" {
		leftStatus = s.projectFiles.status
	}
	if rightStatus == "" {
		rightStatus = s.projectFiles.status
	}
	lines = append(lines, modalRenderLine{modalMuted, fmt.Sprintf("%-29s  %s", sanitizeTerminalText(leftStatus), sanitizeTerminalText(rightStatus))})
	if s.projectFiles.step == "preview" {
		lines = append(lines, modalRenderLine{modalTitle, "Copy preview"}, modalRenderLine{modalMuted, "Conflict policy: [s] Skip  [r] Rename  [o] Overwrite"})
	}
	left, right := s.visibleProjectEntries(&s.projectFiles.left), s.visibleProjectEntries(&s.projectFiles.right)
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	for i := 0; i < n && i < rows-9; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i].Name
		}
		if i < len(right) {
			r = right[i].Name
		}
		if i == s.projectFiles.left.selected && s.projectFiles.active == 0 {
			l = "› " + l
		}
		if i == s.projectFiles.right.selected && s.projectFiles.active == 1 {
			r = "› " + r
		}
		if i < len(left) {
			s.modalChoice(len(lines), &s.projectFiles.left.selected, i, " ")
		}
		if i < len(right) {
			s.modalChoice(len(lines), &s.projectFiles.right.selected, i, " ")
		}
		lines = append(lines, modalRenderLine{modalMuted, fmt.Sprintf("%-29s  %s", sanitizeTerminalText(l), sanitizeTerminalText(r))})
	}
	lines = append(lines, modalRenderLine{modalMuted, s.projectFiles.status}, modalRenderLine{modalMuted, "Tab switch column · h endpoint · g path · / filter · Space select"}, modalRenderLine{modalMuted, "c copy preview · Enter open/confirm · Esc back/close · Ctrl+C cancel"})
	s.renderModalBox(out, cols, rows, lines)
}
