package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklord"
)

const projectTransferReadLimit = 8 << 20

func (s *tuiState) beginProjectTransfer(action string) {
	s.projectTransferAction = action
	s.projectTransferPath = ""
	s.projectTransferData = nil
	s.projectTransferErrReset()
	s.workspacePaneStep = "project-transfer-path"
}

func (s *tuiState) projectTransferErrReset() { s.workspacePaneErr = "" }

func (s *tuiState) projectTransferProject() (*ducklord.LocalProject, error) {
	nav, err := s.workspaceNavigation()
	if err != nil {
		return nil, err
	}
	p := s.activity().ProjectLayout.Project(nav.CurrentProjectID())
	if p == nil || p.ID == ducklord.DefaultProjectID {
		return nil, fmt.Errorf("select a custom Project")
	}
	return p, nil
}

func (s *tuiState) makeProjectTransfer() ([]byte, error) {
	p, err := s.projectTransferProject()
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(s.cfgPath)
	projectNotes, err := ducklord.LoadScopedNotes(root, ducklord.NotesProject, p.ID)
	if err != nil {
		return nil, err
	}
	members := s.activity().ProjectLayout.SessionsForProject(p.ID)
	sessionNotes := make([]ducklord.ProjectSessionNotes, 0, len(members))
	for _, identity := range members {
		key, e := ducklord.SessionNotesIdentity(identity)
		if e != nil {
			continue
		}
		entries, e := ducklord.LoadScopedNotes(root, ducklord.NotesSession, key)
		if e != nil {
			return nil, e
		}
		if len(entries) > 0 {
			sessionNotes = append(sessionNotes, ducklord.ProjectSessionNotes{Session: identity, Entries: entries})
		}
	}
	member := map[ducklord.SessionIdentity]bool{}
	for _, identity := range members {
		member[identity] = true
	}
	bookmarks := make([]ducklord.ProjectOutputBookmark, 0)
	for _, bookmark := range s.activity().TerminalBookmarks {
		if member[bookmark.Session] {
			bookmarks = append(bookmarks, ducklord.ProjectOutputBookmark{Session: bookmark.Session, Label: bookmark.Label, OutputAnchor: bookmark.Anchor, OutputFingerprint: bookmark.Fingerprint})
		}
	}
	return ducklord.ExportProject(*p, projectNotes, sessionNotes, bookmarks)
}

func (s *tuiState) previewProjectImport() error {
	f, err := os.Open(s.projectTransferPath)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, projectTransferReadLimit+1))
	if err != nil {
		return err
	}
	if len(data) > projectTransferReadLimit {
		return fmt.Errorf("project transfer exceeds size limit")
	}
	available := map[ducklord.SessionIdentity]bool{}
	for _, session := range s.sessions {
		if identity, ok := ducklord.IdentityFromSession(session); ok {
			available[identity] = true
		}
	}
	names := map[string]bool{}
	for _, p := range s.activity().ProjectLayout.Projects {
		names[p.Name] = true
	}
	preview, err := ducklord.PreviewProjectImport(data, available, names)
	if err != nil {
		return err
	}
	transfer, err := ducklord.DecodeProjectTransfer(data)
	if err != nil {
		return err
	}
	s.projectTransferData, s.projectTransferPreview = data, preview
	s.projectTransfer = transfer
	return nil
}

func importedProjectName(name string, existing map[string]bool) string {
	if !existing[name] {
		return name
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (imported", name)
		if i > 1 {
			candidate += fmt.Sprintf(" %d", i)
		}
		candidate += ")"
		if !existing[candidate] {
			return candidate
		}
	}
}

func (s *tuiState) commitProjectImport() error {
	p := s.projectTransfer.Project
	names := map[string]bool{}
	ids := map[string]bool{}
	for _, existing := range s.activity().ProjectLayout.Projects {
		names[existing.Name], ids[existing.ID] = true, true
	}
	p.Name = importedProjectName(p.Name, names)
	if ids[p.ID] || p.ID == "" {
		p.ID = uuid.NewString()
	}
	// Transfer files can be imported back into the same Ducklord instance.
	// Local tab and pane IDs are only unique within the saved layout, so give
	// the imported view fresh IDs before validating and persisting it.
	for i := range p.Tabs {
		p.Tabs[i].ID = uuid.NewString()
		remapProjectTransferPaneIDs(p.Tabs[i].Root)
	}
	next := s.activity().Clone()
	next.ProjectLayout.Projects = append(next.ProjectLayout.Projects, p)
	for _, bookmark := range s.projectTransfer.Bookmarks {
		if err := next.AddTerminalBookmark(ducklord.TerminalBookmark{Session: bookmark.Session, Label: bookmark.Label, Anchor: bookmark.OutputAnchor, Fingerprint: bookmark.OutputFingerprint}); err != nil {
			return err
		}
	}
	root := filepath.Dir(s.cfgPath)
	type noteBackup struct {
		scope    ducklord.NotesScope
		identity string
		data     []byte
		exists   bool
	}
	backups := make([]noteBackup, 0, 1+len(s.projectTransfer.SessionNotes))
	addBackup := func(scope ducklord.NotesScope, identity string) error {
		data, err := ducklord.ReadScopedNotesRaw(root, scope, identity)
		exists := err == nil
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		backups = append(backups, noteBackup{scope: scope, identity: identity, data: data, exists: exists})
		return nil
	}
	if err := addBackup(ducklord.NotesProject, p.ID); err != nil {
		return err
	}
	for _, notes := range s.projectTransfer.SessionNotes {
		key, err := ducklord.SessionNotesIdentity(notes.Session)
		if err != nil {
			return err
		}
		if err := addBackup(ducklord.NotesSession, key); err != nil {
			return err
		}
	}
	rollback := func() error {
		var rollbackErr error
		for _, b := range backups {
			rollbackErr = errors.Join(rollbackErr, ducklord.RestoreScopedNotes(root, b.scope, b.identity, b.data, b.exists))
		}
		return rollbackErr
	}
	if err := ducklord.SaveScopedNotes(root, ducklord.NotesProject, p.ID, ducklord.FormatNotes(s.projectTransfer.ProjectNotes)); err != nil {
		return errors.Join(err, rollback())
	}
	for _, notes := range s.projectTransfer.SessionNotes {
		key, err := ducklord.SessionNotesIdentity(notes.Session)
		if err != nil {
			return errors.Join(err, rollback())
		}
		if err := ducklord.SaveScopedNotes(root, ducklord.NotesSession, key, ducklord.FormatNotes(notes.Entries)); err != nil {
			return errors.Join(err, rollback())
		}
	}
	if err := s.activityStore.Save(next); err != nil {
		return errors.Join(err, rollback())
	}
	s.activityState = next
	return nil
}

func remapProjectTransferPaneIDs(p *ducklord.SessionPane) {
	if p == nil {
		return
	}
	p.ID = uuid.NewString()
	remapProjectTransferPaneIDs(p.First)
	remapProjectTransferPaneIDs(p.Second)
}

func (s *tuiState) renderProjectTransfer(out io.Writer, cols, rows int) {
	s.resetModalMouse()
	if s.workspacePaneStep == "project-transfer-path" {
		title := "  Export Project"
		if s.projectTransferAction == "import" {
			title = "  Import Project"
		}
		lines := []modalRenderLine{{modalTitle, title}, {modalInput, "  path › " + s.projectTransferPath + "_"}}
		if s.workspacePaneErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  Enter continue · Esc/Ctrl+C cancel"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if s.projectTransferAction == "export" {
		lines := []modalRenderLine{{modalTitle, "  Confirm Project Export"}, {modalMuted, "  Write project layout, notes, and bookmark metadata to:"}, {modalMuted, "  " + displayField(s.projectTransferPath)}, {modalMuted, "  Terminal output and secrets are never included."}, {modalSelected, "  Write export file"}, {modalMuted, "  Enter write export · Esc/Ctrl+C cancel"}}
		if s.workspacePaneErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	p := s.projectTransferPreview
	sessionCount := 0
	// PreviewProjectImport has already validated and walked the layout; count
	// the identities from the transfer for a compact, useful summary.
	seen := map[ducklord.SessionIdentity]bool{}
	for _, tab := range p.Project.Tabs {
		collectTransferPaneSessions(tab.Root, seen)
	}
	sessionCount = len(seen)
	lines := []modalRenderLine{{modalTitle, "  Project Import Preview"}, {modalMuted, "  Project: " + displayField(p.Project.Name)}, {modalMuted, fmt.Sprintf("  Tabs: %d · Sessions: %d · Project notes: %d · Session notebooks: %d · Bookmarks: %d", len(p.Project.Tabs), sessionCount, len(s.projectTransfer.ProjectNotes), len(s.projectTransfer.SessionNotes), len(s.projectTransfer.Bookmarks))}}
	if p.NameCollision {
		lines = append(lines, modalRenderLine{modalDanger, "  Name collision: imported name will receive a deterministic suffix."})
	}
	if conflicts := s.projectImportSessionNoteConflicts(); conflicts > 0 {
		lines = append(lines, modalRenderLine{modalDanger, fmt.Sprintf("  Warning: %d existing session notebook(s) will be replaced.", conflicts)})
	}
	if len(p.Unavailable) > 0 {
		lines = append(lines, modalRenderLine{modalDanger, fmt.Sprintf("  Unavailable sessions: %d (layout is retained)", len(p.Unavailable))})
		for _, identity := range p.Unavailable {
			lines = append(lines, modalRenderLine{modalMuted, "    " + displayField(identity.Key())})
		}
	}
	lines = append(lines, modalRenderLine{modalSelected, "  Import and persist"}, modalRenderLine{modalMuted, "  Enter import and persist · Esc/Ctrl+C cancel"})
	if s.workspacePaneErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) projectImportSessionNoteConflicts() int {
	root := filepath.Dir(s.cfgPath)
	conflicts := 0
	seen := map[string]bool{}
	for _, notes := range s.projectTransfer.SessionNotes {
		key, err := ducklord.SessionNotesIdentity(notes.Session)
		if err != nil || seen[key] {
			continue
		}
		seen[key] = true
		if _, err := ducklord.ReadScopedNotesRaw(root, ducklord.NotesSession, key); err == nil {
			conflicts++
		}
	}
	return conflicts
}

func collectTransferPaneSessions(p *ducklord.SessionPane, out map[ducklord.SessionIdentity]bool) {
	if p == nil {
		return
	}
	if p.Session != nil {
		out[*p.Session] = true
		return
	}
	collectTransferPaneSessions(p.First, out)
	collectTransferPaneSessions(p.Second, out)
}

func (s *tuiState) handleProjectTransferInput(input []byte) bool {
	key := string(input)
	if key == "\x03" || key == "\x1b" {
		s.closeWorkspacePane()
		return true
	}
	if s.workspacePaneStep == "project-transfer-path" {
		switch key {
		case "\x7f", "\b":
			if r := []rune(s.projectTransferPath); len(r) > 0 {
				s.projectTransferPath = string(r[:len(r)-1])
			}
		case "\r", "\n":
			if strings.TrimSpace(s.projectTransferPath) == "" {
				s.workspacePaneErr = "path is required"
				return true
			}
			if s.projectTransferAction == "export" {
				data, err := s.makeProjectTransfer()
				if err != nil {
					s.workspacePaneErr = err.Error()
					return true
				}
				s.projectTransferData = data
				s.workspacePaneStep = "project-transfer-confirm"
			} else if err := s.previewProjectImport(); err != nil {
				s.workspacePaneErr = err.Error()
				return true
			} else {
				s.workspacePaneStep = "project-transfer-preview"
			}
		default:
			if utf8.Valid(input) {
				for _, r := range string(input) {
					if !unicode.IsControl(r) && len([]rune(s.projectTransferPath)) < 4096 {
						s.projectTransferPath += string(r)
					}
				}
			}
		}
		return true
	}
	if key == "\r" || key == "\n" {
		if s.projectTransferAction == "export" {
			if err := ducklord.WriteProjectTransfer(s.projectTransferPath, s.projectTransferData); err != nil {
				s.workspacePaneErr = err.Error()
				return true
			}
			s.workspacePaneErr = "Export written"
			s.closeWorkspacePane()
		} else if err := s.commitProjectImport(); err != nil {
			s.workspacePaneErr = err.Error()
			return true
		} else {
			s.closeWorkspacePane()
		}
	}
	return true
}
