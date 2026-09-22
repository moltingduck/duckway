package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) beginHostSkills() {
	s.hostMenuStep, s.hostMenuIndex = "skills-dashboard", 4
	s.hostSkillsIndex, s.hostSkillsSourceIndex, s.hostSkillsField = 0, -1, 0
	s.hostSkillsTargetIndex = 0
	s.hostSkillsSelected, s.hostSkillsRemote, s.hostSkillsErr, s.hostSkillsRepoErr = make(map[string]bool), nil, "", ""
	s.hostSkillsPane, s.hostSkillsTargetRemoteIndex = 0, 0
	s.hostSkillsRemoteByTarget = make(map[string][]string)
	s.hostSkillsTargetExpanded = make(map[string]bool)
	s.hostSkillsRepository = ducklord.DefaultSkillRepository()
	if err := ducklord.EnsureSkillRepository(s.hostSkillsRepository); err != nil {
		s.hostSkillsRepoErr = "managed skill repository: " + err.Error()
		s.hostSkillsErr = s.hostSkillsRepoErr
	}
	if i := s.hostClientIndex(); i >= 0 && len(s.cfg.Clients[i].SkillTargets) > 0 {
		s.hostSkillsTargetExpanded[s.cfg.Clients[i].SkillTargets[0].ID] = true
		if s.hostSkillsDone != nil {
			s.refreshHostSkills()
		}
	}
}

func (s *tuiState) hostSkillNames() []string {
	repo := s.hostSkillsRepository
	if repo == "" {
		repo = ducklord.DefaultSkillRepository()
	}
	entries, err := os.ReadDir(repo)
	if err != nil {
		s.hostSkillsRepoErr = "managed skill repository: " + err.Error()
		return nil
	}
	s.hostSkillsRepoErr = ""
	seen := make(map[string]bool)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || !ducklord.SafeIdentifier(e.Name()) {
			continue
		}
		if _, err := ducklord.InspectSkill(filepath.Join(repo, e.Name()), e.Name()); err == nil {
			seen[e.Name()] = true
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func (s *tuiState) selectedHostSkillTarget() (*ducklord.SkillInstallTarget, bool) {
	i := s.hostClientIndex()
	if i < 0 || s.hostSkillsTargetIndex < 0 || s.hostSkillsTargetIndex >= len(s.cfg.Clients[i].SkillTargets) {
		return nil, false
	}
	return &s.cfg.Clients[i].SkillTargets[s.hostSkillsTargetIndex], true
}

func (s *tuiState) hostSkillManagement(id string) string {
	if target, ok := s.selectedHostSkillTarget(); ok {
		for _, skill := range target.Skills {
			if skill.ID == id {
				return skill.Management
			}
		}
		// Migration compatibility for the former host-wide selection.
		for _, selected := range s.cfg.Clients[s.hostClientIndex()].SelectedSkills {
			if selected == id {
				return "push"
			}
		}
	}
	return "none"
}

func (s *tuiState) setHostSkillManagement(id, management string) error {
	if !ducklord.SafeIdentifier(id) || (management != "none" && management != "push" && management != "pull") {
		return fmt.Errorf("invalid skill management state")
	}
	target, ok := s.selectedHostSkillTarget()
	if !ok {
		return fmt.Errorf("choose an agent target first")
	}
	updated := make([]ducklord.ManagedHostSkill, 0, len(target.Skills)+1)
	for _, skill := range target.Skills {
		if skill.ID != id {
			updated = append(updated, skill)
		}
	}
	// Keep an explicit none state. It distinguishes a user-managed target from
	// an untouched legacy target whose selected_skills should still migrate as
	// push defaults.
	updated = append(updated, ducklord.ManagedHostSkill{ID: id, Management: management})
	sort.Slice(updated, func(i, j int) bool { return updated[i].ID < updated[j].ID })
	target.Skills = updated
	return s.saveHostSkillConfig()
}

func (s *tuiState) renderHostSkillsModal(out io.Writer, cols, rows int) {
	lines := []modalRenderLine{{modalTitle, "  Skills · " + displayField(s.hostMenuTarget)}}
	switch s.hostMenuStep {
	case "skills-dashboard":
		s.renderHostSkillsDashboard(out, cols, rows, lines)
		return
	case "skills-agents":
		lines = append(lines, modalRenderLine{modalStatus, "  Choose an agent installation to manage its skills"})
		if i := s.hostClientIndex(); i >= 0 {
			for index, target := range s.cfg.Clients[i].SkillTargets {
				style, prefix := modalInput, "  "
				if index == s.hostSkillsTargetIndex {
					style, prefix = modalSelected, "› "
				}
				lines = append(lines, modalRenderLine{style, prefix + target.ID + " · " + target.Path})
			}
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter skills · t edit target · Esc host settings"})
	case "skills-source":
		lines = append(lines, modalRenderLine{modalStatus, "  Public HTTPS source · archive format: .tar.gz"})
		lines = append(lines, hostSkillsFieldLine(s.hostSkillsField == 0, "Skill ID: "+s.hostSkillsDraftSkillID))
		lines = append(lines, hostSkillsFieldLine(s.hostSkillsField == 1, "URL: "+s.hostSkillsDraftURL))
		lines = append(lines, hostSkillsCheckboxLine(s.hostSkillsField == 2, fmt.Sprintf("[ %s ] INSECURE TLS (skip certificate verification)", boolMark(s.hostSkillsDraftInsecure))))
		lines = append(lines, modalRenderLine{modalMuted, "  Tab/↑↓ field · Space toggle · Enter save · Esc back"})
	case "skills-target":
		lines = append(lines, modalRenderLine{modalStatus, "  Named host target"}, hostSkillsFieldLine(s.hostSkillsField == 0, "ID: "+s.hostSkillsDraftTargetID), hostSkillsFieldLine(s.hostSkillsField == 1, "Path: "+s.hostSkillsDraftTargetPath), modalRenderLine{modalMuted, "  Tab/↑↓ field · Enter save · Esc back"})
	case "skills-import":
		lines = append(lines, modalRenderLine{modalStatus, "  Import local managed skill"}, hostSkillsFieldLine(s.hostSkillsField == 0, "Directory: "+s.hostSkillsDraftImportPath), hostSkillsFieldLine(s.hostSkillsField == 1, "Skill ID (optional): "+s.hostSkillsDraftImportID), modalRenderLine{modalMuted, "  Tab/↑↓ field · Enter preview import · Esc back"})
	case "skills-download-id":
		lines = append(lines, modalRenderLine{modalStatus, "  Download remote skill"}, hostSkillsFieldLine(true, "Skill ID: "+s.hostSkillsDraftImportID), modalRenderLine{modalMuted, "  Enter choose target · Esc back"})
	case "skills-target-select":
		lines = append(lines, modalRenderLine{modalStatus, "  Download target · choose a named target"})
		if i := s.hostClientIndex(); i >= 0 {
			for n, target := range s.cfg.Clients[i].SkillTargets {
				style, prefix := modalInput, "  "
				if n == s.hostSkillsTargetIndex {
					style, prefix = modalSelected, "› "
				}
				lines = append(lines, modalRenderLine{style, prefix + target.ID + " = " + target.Path})
			}
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter preview download · Esc back"})
	case "skills-preview":
		lines = append(lines, modalRenderLine{modalSelected, "  Preview " + s.hostSkillsPreviewAction + " for " + s.hostSkillsPreview.Identifier})
		for _, line := range strings.Split(s.hostSkillsPreview.Diff, "\n") {
			if line != "" {
				lines = append(lines, modalRenderLine{modalMuted, "  " + line})
			}
		}
		lines = append(lines, modalRenderLine{modalMuted, "  Enter confirm and apply · Esc cancel"})
	case "skills-result":
		lines = append(lines, modalRenderLine{modalStatus, "  " + s.hostSkillsErr}, modalRenderLine{modalMuted, "  Enter or Esc return to agent list"})
	case "skills-busy":
		lines = append(lines, modalRenderLine{modalStatus, "  Working on Host Skills…"}, modalRenderLine{modalMuted, "  Esc/Ctrl-C cancel · terminal output remains in the background"})
	case "skills-rename":
		lines = append(lines, modalRenderLine{modalStatus, "  Rename managed skill"}, hostSkillsFieldLine(true, "New ID: "+s.hostSkillsRenameDraft), modalRenderLine{modalMuted, "  Enter continue · Esc cancel"})
	case "skills-rename-confirm":
		lines = append(lines, modalRenderLine{modalStatus, "  Confirm managed skill rename"}, modalRenderLine{modalSelected, "  " + s.hostSkillsRenameOld + " → " + s.hostSkillsRenameDraft}, modalRenderLine{modalMuted, "  Enter or y confirm · Esc cancel"})
	case "skills-delete-confirm":
		host, agent, path := s.hostMenuTarget, "(none)", "(none)"
		if target, ok := s.selectedHostSkillTarget(); ok {
			agent, path = target.ID, target.Path
		}
		lines = append(lines,
			modalRenderLine{modalStatus, "  Delete remote skill from target?"},
			modalRenderLine{modalMuted, "  Host: " + host},
			modalRenderLine{modalMuted, "  Agent: " + agent},
			modalRenderLine{modalMuted, "  Target path: " + path},
			modalRenderLine{modalSelected, "  Skill ID: " + s.hostSkillsPendingID},
			modalRenderLine{modalMuted, "  Enter confirm · Esc cancel"})
	default:
		names := s.hostSkillNames()
		targetLabel := ""
		if target, ok := s.selectedHostSkillTarget(); ok {
			targetLabel = " · " + target.ID
		}
		lines[0] = modalRenderLine{modalTitle, "  Skills · " + displayField(s.hostMenuTarget) + targetLabel}
		lines = append(lines, modalRenderLine{modalMuted, "  Status: none = client managed · push = upload from Ducklord · pull = downloaded to Ducklord"}, modalRenderLine{modalMuted, "  Repository: " + s.hostSkillsRepository})
		if s.hostSkillsRepoErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostSkillsRepoErr})
		} else if len(names) == 0 {
			lines = append(lines, modalRenderLine{modalMuted, "  (no managed skills found)"})
		}
		for i, name := range names {
			style, prefix := modalInput, "  "
			if i == s.hostSkillsIndex {
				style, prefix = modalSelected, "› "
			}
			lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s[%-4s] %s", prefix, s.hostSkillManagement(name), name)})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  Targets: " + s.hostSkillsTargetSummary()})
		for i, src := range s.cfg.SkillSources {
			style, prefix := modalInput, "  "
			if len(names)+i == s.hostSkillsIndex {
				style, prefix = modalSelected, "› "
			}
			mark := ""
			if src.InsecureHTTPS {
				mark = " · INSECURE TLS"
			}
			lines = append(lines, modalRenderLine{style, fmt.Sprintf("%sSource %d [%s]: %s%s", prefix, i+1, src.SkillID, src.URL, mark)})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · p push · n none · r pull selected · R pull by ID · d deploy pushes · i import · a/e/f source · t target · Esc agents"})
	}
	if s.hostSkillsErr != "" && s.hostSkillsErr != s.hostSkillsRepoErr && s.hostMenuStep != "skills-result" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostSkillsErr})
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderHostSkillsDashboard(out io.Writer, cols, rows int, lines []modalRenderLine) {
	lines = append(lines, modalRenderLine{modalStatus, "  Ducklord Managed Repository                         Agent Targets"})
	names := s.hostSkillNames()
	type targetRow struct {
		text        string
		remoteIndex int
		targetIndex int
	}
	var targetRows []targetRow
	if i := s.hostClientIndex(); i >= 0 {
		for targetIndex, target := range s.cfg.Clients[i].SkillTargets {
			prefix := "▸"
			if s.hostSkillsTargetExpanded[target.ID] {
				prefix = "▾"
			}
			targetRows = append(targetRows, targetRow{text: fmt.Sprintf("%s %s %s", prefix, target.ID, target.Path), targetIndex: targetIndex})
			if s.hostSkillsTargetExpanded[target.ID] {
				for remoteIndex, name := range s.remoteSkillsForTarget(target) {
					targetRows = append(targetRows, targetRow{text: "    " + name, remoteIndex: remoteIndex + 1, targetIndex: targetIndex})
				}
			}
		}
	}
	maxRows := max(len(names), len(targetRows))
	if maxRows == 0 {
		maxRows = 1
	}
	for row := 0; row < maxRows; row++ {
		left := "  "
		if row < len(names) {
			left = fmt.Sprintf("%-2s[%-4s] %s", "  ", s.hostSkillManagement(names[row]), names[row])
			if s.hostSkillsPane == 0 && row == s.hostSkillsIndex {
				left = fmt.Sprintf("› [%-4s] %s", s.hostSkillManagement(names[row]), names[row])
			}
		}
		right := ""
		if row < len(targetRows) {
			r := targetRows[row]
			right = r.text
			if s.hostSkillsPane == 1 && r.targetIndex == s.hostSkillsTargetIndex && r.remoteIndex == s.hostSkillsTargetRemoteIndex {
				right = "› " + right
			}
		}
		lines = append(lines, modalRenderLine{modalInput, left + "    " + right})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  Tab/←/→ pane · ↑/↓ select · Enter/Space expand · p push · n none · m rename"})
	lines = append(lines, modalRenderLine{modalMuted, "  i import · a/e/f source · t target · r pull · x delete remote · d deploy · Esc host settings"})
	if s.hostSkillsErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostSkillsErr})
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) remoteSkillsForTarget(target ducklord.SkillInstallTarget) []string {
	seen := make(map[string]bool)
	var out []string
	for _, skill := range target.Skills {
		if ducklord.SafeIdentifier(skill.ID) && !seen[skill.ID] {
			seen[skill.ID] = true
			out = append(out, skill.ID)
		}
	}
	for _, id := range s.hostSkillsRemoteByTarget[target.ID] {
		if ducklord.SafeIdentifier(id) && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func hostSkillsFieldLine(active bool, label string) modalRenderLine {
	style, prefix, cursor := modalInput, "  ", ""
	if active {
		style, prefix, cursor = modalSelected, "› ", "▏"
	}
	return modalRenderLine{style, prefix + label + cursor}
}

func hostSkillsCheckboxLine(active bool, label string) modalRenderLine {
	style, prefix := modalInput, "  "
	if active {
		style, prefix = modalSelected, "› "
	}
	return modalRenderLine{style, prefix + label}
}

func boolMark(v bool) string {
	if v {
		return "✓"
	}
	return " "
}
func (s *tuiState) hostSkillsTargetSummary() string {
	c, ok := s.cfg.Client(s.hostMenuTarget)
	if !ok || len(c.SkillTargets) == 0 {
		return "none configured"
	}
	p := make([]string, 0, len(c.SkillTargets))
	for _, t := range c.SkillTargets {
		p = append(p, t.ID+"="+t.Path)
	}
	return strings.Join(p, ", ")
}
func (s *tuiState) saveHostSkills() error {
	i := s.hostClientIndex()
	if i < 0 {
		return fmt.Errorf("host %q not found", s.hostMenuTarget)
	}
	v := make([]string, 0, len(s.hostSkillsSelected))
	for n, on := range s.hostSkillsSelected {
		if on {
			v = append(v, n)
		}
	}
	sort.Strings(v)
	s.cfg.Clients[i].SelectedSkills = v
	return s.saveHostSkillConfig()
}
func (s *tuiState) saveHostSkillConfig() error {
	if s.cfgPath == "" {
		return nil
	}
	return ducklord.SaveConfig(s.cfgPath, s.cfg)
}
func (s *tuiState) hostClientIndex() int {
	for i := range s.cfg.Clients {
		if s.cfg.Clients[i].Name == s.hostMenuTarget {
			return i
		}
	}
	return -1
}
func (s *tuiState) selectedHostSkill() (string, bool) {
	n := s.hostSkillNames()
	if s.hostSkillsIndex < 0 || s.hostSkillsIndex >= len(n) {
		return "", false
	}
	return n[s.hostSkillsIndex], true
}

func (s *tuiState) selectedHostSkillSource() (int, bool) {
	n := s.hostSkillNames()
	i := s.hostSkillsIndex - len(n)
	if i < 0 || i >= len(s.cfg.SkillSources) {
		return -1, false
	}
	s.hostSkillsSourceIndex = i
	return i, true
}

func (s *tuiState) handleHostSkillsInput(input []byte) string {
	t := string(input)
	if s.hostMenuStep == "skills-busy" {
		if t == "\x03" || t == "\x1b" {
			s.cancelHostSkillsOperation()
		}
		return ""
	}
	if t == "\x03" {
		s.cleanupHostSkills()
		s.hostMenuMode = false
		return ""
	}
	if t == "\x1b" {
		switch s.hostMenuStep {
		case "skills-source", "skills-target", "skills-target-select", "skills-import", "skills-download-id":
			s.hostMenuStep = "skills-dashboard"
		case "skills-result":
			s.hostMenuStep = "skills-dashboard"
			if strings.HasPrefix(s.hostSkillsErr, "Deployed ") {
				s.refreshHostSkills()
			}
		case "skills-agents":
			s.hostMenuStep, s.hostMenuIndex = "actions", 4
		case "skills-preview":
			if s.hostSkillsPreview != nil {
				ducklord.CleanupSkillPreview(s.hostSkillsPreview)
				s.hostSkillsPreview = nil
			}
			s.hostMenuStep = "skills-dashboard"
		case "skills-dashboard":
			s.hostMenuStep, s.hostMenuIndex = "actions", 4
		case "skills-rename", "skills-rename-confirm", "skills-delete-confirm":
			s.hostMenuStep = "skills-dashboard"
		case "skills-list":
			s.hostMenuStep, s.hostMenuIndex = "skills-agents", 0
		default:
			s.hostMenuStep, s.hostMenuIndex = "skills-agents", 0
		}
		return ""
	}
	if s.hostMenuStep == "skills-dashboard" {
		return s.handleHostSkillsDashboardInput(t)
	}
	if s.hostMenuStep == "skills-rename" {
		return s.handleHostSkillRename(t)
	}
	if s.hostMenuStep == "skills-rename-confirm" {
		if t == "\r" || t == "\n" || t == "y" || t == "Y" {
			s.applyHostSkillRename()
		}
		return ""
	}
	if s.hostMenuStep == "skills-delete-confirm" {
		if t == "\r" || t == "\n" {
			s.deleteHostSkillRemote()
		} else if t == "y" || t == "Y" {
			s.deleteHostSkillRemote()
		}
		return ""
	}
	if s.hostMenuStep == "skills-source" {
		return s.handleHostSkillSource(t)
	}
	if s.hostMenuStep == "skills-target" {
		return s.handleHostSkillTarget(t)
	}
	if s.hostMenuStep == "skills-import" {
		return s.handleHostSkillImport(t)
	}
	if s.hostMenuStep == "skills-download-id" {
		return s.handleHostSkillDownloadID(t)
	}
	if s.hostMenuStep == "skills-target-select" {
		return s.handleHostSkillTargetSelect(t)
	}
	if s.hostMenuStep == "skills-preview" {
		if t == "\r" || t == "\n" {
			info, err := ducklord.CommitSkillPreview(s.hostSkillsPreview)
			s.hostSkillsPreview = nil
			if err != nil {
				s.hostSkillsErr = err.Error()
			} else {
				if s.hostSkillsPreviewAction == "download" {
					if stateErr := s.setHostSkillManagement(info.Identifier, "pull"); stateErr != nil {
						s.hostSkillsErr = stateErr.Error()
					} else {
						s.hostSkillsErr = "Applied " + info.Identifier
					}
				} else {
					s.hostSkillsErr = "Applied " + info.Identifier
				}
			}
			s.hostMenuStep = "skills-result"
		}
		return ""
	}
	if s.hostMenuStep == "skills-result" {
		if t == "\r" || t == "\n" {
			s.hostMenuStep = "skills-dashboard"
			// A deploy can finish while the initial target discovery is still
			// in flight. That list event is intentionally ignored while the
			// operation owns the modal, so refresh the selected target after the
			// success screen returns control to the dashboard. The refresh is
			// asynchronous and does not move pane or tree selection.
			if strings.HasPrefix(s.hostSkillsErr, "Deployed ") {
				s.refreshHostSkills()
			}
		}
		return ""
	}
	if s.hostMenuStep == "skills-agents" {
		i := s.hostClientIndex()
		if i < 0 {
			s.hostSkillsErr = "host not found"
			return ""
		}
		if t == "t" {
			s.beginHostSkillTarget()
			return ""
		}
		if len(s.cfg.Clients[i].SkillTargets) == 0 {
			s.hostSkillsErr = "No agent target configured; press t to add one"
			return ""
		}
		switch t {
		case "j", "\x1b[B":
			s.hostSkillsTargetIndex = min(len(s.cfg.Clients[i].SkillTargets)-1, s.hostSkillsTargetIndex+1)
		case "k", "\x1b[A":
			s.hostSkillsTargetIndex = max(0, s.hostSkillsTargetIndex-1)
		case "\r", "\n":
			s.hostSkillsIndex, s.hostSkillsErr, s.hostMenuStep = 0, "", "skills-dashboard"
			s.refreshHostSkills()
		}
		return ""
	}
	n := s.hostSkillNames()
	if s.hostSkillsRepoErr != "" {
		return ""
	}
	switch t {
	case "j", "\x1b[B":
		s.hostSkillsIndex = min(max(0, len(n)+len(s.cfg.SkillSources)-1), s.hostSkillsIndex+1)
	case "k", "\x1b[A":
		s.hostSkillsIndex = max(0, s.hostSkillsIndex-1)
	case "p":
		if id, ok := s.selectedHostSkill(); ok {
			if err := s.setHostSkillManagement(id, "push"); err != nil {
				s.hostSkillsErr = err.Error()
			}
		}
	case "n":
		if id, ok := s.selectedHostSkill(); ok {
			if err := s.setHostSkillManagement(id, "none"); err != nil {
				s.hostSkillsErr = err.Error()
			}
		}
	case "a":
		s.hostSkillsDraftSkillID, s.hostSkillsDraftURL, s.hostSkillsDraftInsecure, s.hostSkillsField, s.hostSkillsErr = "", "", false, 0, ""
		s.hostSkillsSourceIndex = -1
		s.hostMenuStep = "skills-source"
	case "e":
		if i, ok := s.selectedHostSkillSource(); ok {
			v := s.cfg.SkillSources[i]
			s.hostSkillsDraftSkillID, s.hostSkillsDraftURL, s.hostSkillsDraftInsecure, s.hostSkillsField = v.SkillID, v.URL, v.InsecureHTTPS, 0
			s.hostMenuStep = "skills-source"
		}
	case "t":
		s.beginHostSkillTarget()
	case "i":
		s.hostSkillsDraftImportPath, s.hostSkillsDraftImportID, s.hostSkillsField, s.hostSkillsErr = "", "", 0, ""
		s.hostMenuStep = "skills-import"
	case "d":
		s.deployHostSkills()
	case "s":
		s.deployHostSkills()
	case "f":
		s.prepareHostSkillFetch()
	case "r":
		s.hostSkillsErr = ""
		if i := s.hostClientIndex(); i < 0 || len(s.cfg.Clients[i].SkillTargets) == 0 {
			s.hostSkillsErr = "No named target configured"
		} else if id, ok := s.selectedHostSkill(); ok {
			s.hostSkillsDraftImportID = id
			s.prepareHostSkillDownload()
		} else {
			s.hostSkillsDraftImportID = ""
			s.hostMenuStep = "skills-download-id"
		}
	case "R":
		s.hostSkillsErr = ""
		if i := s.hostClientIndex(); i < 0 || len(s.cfg.Clients[i].SkillTargets) == 0 {
			s.hostSkillsErr = "No named target configured"
		} else {
			s.hostSkillsDraftImportID = ""
			s.hostMenuStep = "skills-download-id"
		}
	case "\r", "\n":
		if i, ok := s.selectedHostSkillSource(); ok {
			v := s.cfg.SkillSources[i]
			s.hostSkillsDraftSkillID, s.hostSkillsDraftURL, s.hostSkillsDraftInsecure = v.SkillID, v.URL, v.InsecureHTTPS
			s.hostSkillsField = 0
			s.hostMenuStep = "skills-source"
		}
	}
	return ""
}

func (s *tuiState) handleHostSkillsDashboardInput(t string) string {
	i := s.hostClientIndex()
	if i < 0 {
		s.hostSkillsErr = "host not found"
		return ""
	}
	targets := s.cfg.Clients[i].SkillTargets
	if t == "\t" || t == "\x1b[C" || t == "\x1b[D" {
		s.hostSkillsPane = 1 - s.hostSkillsPane
		return ""
	}
	if t == "j" || t == "\x1b[B" {
		if s.hostSkillsPane == 0 {
			n := len(s.hostSkillNames())
			if n > 0 {
				s.hostSkillsIndex = min(n-1, s.hostSkillsIndex+1)
			}
		} else if len(targets) > 0 {
			type item struct{ target, remote int }
			items := make([]item, 0, len(targets))
			for ti, target := range targets {
				items = append(items, item{ti, 0})
				if s.hostSkillsTargetExpanded[target.ID] {
					for ri := range s.remoteSkillsForTarget(target) {
						items = append(items, item{ti, ri + 1})
					}
				}
			}
			for n, v := range items {
				if v.target == s.hostSkillsTargetIndex && v.remote == s.hostSkillsTargetRemoteIndex && n+1 < len(items) {
					s.hostSkillsTargetIndex, s.hostSkillsTargetRemoteIndex = items[n+1].target, items[n+1].remote
					break
				}
			}
		}
		return ""
	}
	if t == "k" || t == "\x1b[A" {
		if s.hostSkillsPane == 0 {
			s.hostSkillsIndex = max(0, s.hostSkillsIndex-1)
		} else {
			type item struct{ target, remote int }
			items := make([]item, 0, len(targets))
			for ti, target := range targets {
				items = append(items, item{ti, 0})
				if s.hostSkillsTargetExpanded[target.ID] {
					for ri := range s.remoteSkillsForTarget(target) {
						items = append(items, item{ti, ri + 1})
					}
				}
			}
			for n, v := range items {
				if v.target == s.hostSkillsTargetIndex && v.remote == s.hostSkillsTargetRemoteIndex && n > 0 {
					s.hostSkillsTargetIndex, s.hostSkillsTargetRemoteIndex = items[n-1].target, items[n-1].remote
					break
				}
			}
		}
		return ""
	}
	if t == "d" {
		s.deployHostSkills()
		return ""
	}
	if s.hostSkillsPane == 0 {
		id, ok := s.selectedHostSkill()
		if !ok {
			return ""
		}
		switch t {
		case "p", "n":
			management := "none"
			if t == "p" {
				management = "push"
			}
			if err := s.setHostSkillManagement(id, management); err != nil {
				s.hostSkillsErr = err.Error()
			}
		case "m":
			s.hostSkillsRenameOld, s.hostSkillsRenameDraft = id, id
			s.hostMenuStep = "skills-rename"
		case "i":
			s.hostSkillsDraftImportPath, s.hostSkillsDraftImportID, s.hostSkillsField, s.hostSkillsErr = "", "", 0, ""
			s.hostMenuStep = "skills-import"
		case "a":
			s.hostSkillsDraftSkillID, s.hostSkillsDraftURL, s.hostSkillsDraftInsecure, s.hostSkillsField, s.hostSkillsErr = "", "", false, 0, ""
			s.hostSkillsSourceIndex, s.hostMenuStep = -1, "skills-source"
		case "t":
			s.beginHostSkillTarget()
		case "f":
			s.prepareHostSkillFetch()
		case "r":
			s.hostSkillsDraftImportID = id
			s.prepareHostSkillDownload()
		}
		return ""
	}
	if len(targets) == 0 {
		s.hostSkillsErr = "No named target configured"
		return ""
	}
	if s.hostSkillsTargetIndex >= len(targets) {
		s.hostSkillsTargetIndex = 0
	}
	target := targets[s.hostSkillsTargetIndex]
	if s.hostSkillsTargetRemoteIndex == 0 {
		if t == "\r" || t == "\n" || t == " " {
			expanded := s.hostSkillsTargetExpanded[target.ID]
			if expanded && s.remoteSkillsForTarget(target) == nil {
				s.refreshHostSkills()
			} else {
				expanded = !expanded
				s.hostSkillsTargetExpanded[target.ID] = expanded
				if expanded {
					s.refreshHostSkills()
				}
			}
		}
		if t == "t" {
			s.beginHostSkillTarget()
		}
		return ""
	}
	remote := s.remoteSkillsForTarget(target)
	if s.hostSkillsTargetRemoteIndex > len(remote) {
		return ""
	}
	id := remote[s.hostSkillsTargetRemoteIndex-1]
	switch t {
	case "r":
		s.hostSkillsDraftImportID = id
		s.prepareHostSkillDownload()
	case "x":
		s.hostSkillsPendingID = id
		s.hostMenuStep = "skills-delete-confirm"
	}
	return ""
}

func (s *tuiState) handleHostSkillRename(t string) string {
	if t == "\x7f" || t == "\b" {
		s.hostSkillsRenameDraft = trimLastRune(s.hostSkillsRenameDraft)
		return ""
	}
	if t == "\r" || t == "\n" {
		id := strings.TrimSpace(s.hostSkillsRenameDraft)
		if !ducklord.SafeIdentifier(id) {
			s.hostSkillsErr = "Skill ID must be a safe identifier"
			return ""
		}
		s.hostSkillsRenameDraft = id
		s.hostMenuStep = "skills-rename-confirm"
		return ""
	}
	for _, r := range t {
		if r >= 32 && len(s.hostSkillsRenameDraft) < 128 {
			s.hostSkillsRenameDraft += string(r)
		}
	}
	return ""
}

func (s *tuiState) applyHostSkillRename() {
	if s.hostSkillsRenameDraft == s.hostSkillsRenameOld {
		s.hostMenuStep = "skills-dashboard"
		return
	}
	oldID, newID := s.hostSkillsRenameOld, s.hostSkillsRenameDraft
	previous := s.cfg.Clone()
	if err := ducklord.RenameManagedSkill(s.hostSkillsRepository, s.cfg, oldID, newID); err != nil {
		s.hostSkillsErr = err.Error()
		return
	}
	if err := s.saveHostSkillConfig(); err != nil {
		rollbackErr := os.Rename(filepath.Join(s.hostSkillsRepository, newID), filepath.Join(s.hostSkillsRepository, oldID))
		*s.cfg = *previous
		if rollbackErr != nil {
			s.hostSkillsErr = fmt.Sprintf("%v (rollback failed: %v)", err, rollbackErr)
		} else {
			s.hostSkillsErr = err.Error()
		}
		return
	}
	s.hostSkillsErr = "Renamed " + oldID + " to " + newID
	s.hostMenuStep = "skills-dashboard"
}

// closeHostMenuWithCtrlC cancels any skill operation and closes the Host route,
// allowing the central dispatcher to restore the route's original pane focus.
func (s *tuiState) closeHostMenuWithCtrlC() {
	if strings.HasPrefix(s.hostMenuStep, "skills-") {
		s.cleanupHostSkills()
	}
	s.hostMenuMode = false
}

func (s *tuiState) handleHostSkillDownloadID(text string) string {
	if text == "\x7f" || text == "\b" {
		s.hostSkillsDraftImportID = trimLastRune(s.hostSkillsDraftImportID)
		return ""
	}
	if text == "\r" || text == "\n" {
		id := strings.TrimSpace(s.hostSkillsDraftImportID)
		if !ducklord.SafeIdentifier(id) {
			s.hostSkillsErr = "Skill ID must be a safe identifier"
			return ""
		}
		s.hostSkillsDraftImportID = id
		s.hostSkillsErr = ""
		s.prepareHostSkillDownload()
		return ""
	}
	for _, r := range text {
		if r >= 32 && len(s.hostSkillsDraftImportID) < 128 {
			s.hostSkillsDraftImportID += string(r)
		}
	}
	return ""
}
func (s *tuiState) beginHostSkillTarget() {
	s.hostSkillsDraftTargetID, s.hostSkillsDraftTargetPath = "", "/"
	if i := s.hostClientIndex(); i >= 0 && len(s.cfg.Clients[i].SkillTargets) > 0 {
		index := min(max(0, s.hostSkillsTargetIndex), len(s.cfg.Clients[i].SkillTargets)-1)
		v := s.cfg.Clients[i].SkillTargets[index]
		s.hostSkillsDraftTargetID, s.hostSkillsDraftTargetPath = v.ID, v.Path
	}
	s.hostSkillsField, s.hostSkillsErr, s.hostMenuStep = 0, "", "skills-target"
}
func (s *tuiState) handleHostSkillSource(t string) string {
	if t == "\t" || t == "\x1b[A" || t == "\x1b[B" {
		s.hostSkillsField = (s.hostSkillsField + 1) % 3
		return ""
	}
	if t == " " && s.hostSkillsField == 2 {
		s.hostSkillsDraftInsecure = !s.hostSkillsDraftInsecure
		return ""
	}
	if t == "\x7f" || t == "\b" {
		if s.hostSkillsField == 0 {
			s.hostSkillsDraftSkillID = trimLastRune(s.hostSkillsDraftSkillID)
		} else if s.hostSkillsField == 1 {
			s.hostSkillsDraftURL = trimLastRune(s.hostSkillsDraftURL)
		}
		return ""
	}
	if t == "\r" || t == "\n" {
		id := strings.TrimSpace(s.hostSkillsDraftSkillID)
		u, e := url.Parse(strings.TrimSpace(s.hostSkillsDraftURL))
		if !ducklord.SafeIdentifier(id) || e != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
			s.hostSkillsErr = "Skill ID and public HTTPS URL are required"
			return ""
		}
		v := ducklord.SkillTrackingSource{SkillID: id, URL: u.String(), InsecureHTTPS: s.hostSkillsDraftInsecure}
		if s.hostSkillsSourceIndex >= 0 && s.hostSkillsSourceIndex < len(s.cfg.SkillSources) {
			s.cfg.SkillSources[s.hostSkillsSourceIndex] = v
		} else {
			s.cfg.SkillSources = append(s.cfg.SkillSources, v)
		}
		if e := s.saveHostSkillConfig(); e != nil {
			s.hostSkillsErr = e.Error()
			return ""
		}
		s.hostMenuStep, s.hostSkillsErr = "skills-dashboard", ""
		return ""
	}
	if s.hostSkillsField == 0 {
		for _, r := range t {
			if len(s.hostSkillsDraftSkillID) < 128 && r >= 32 {
				s.hostSkillsDraftSkillID += string(r)
			}
		}
	} else if s.hostSkillsField == 1 {
		for _, r := range t {
			if len(s.hostSkillsDraftURL) < 512 && r >= 32 {
				s.hostSkillsDraftURL += string(r)
			}
		}
	}
	return ""
}
func (s *tuiState) handleHostSkillTarget(t string) string {
	if t == "\t" || t == "\x1b[A" || t == "\x1b[B" {
		s.hostSkillsField = 1 - s.hostSkillsField
		return ""
	}
	if t == "\x7f" || t == "\b" {
		if s.hostSkillsField == 0 {
			s.hostSkillsDraftTargetID = trimLastRune(s.hostSkillsDraftTargetID)
		} else {
			s.hostSkillsDraftTargetPath = trimLastRune(s.hostSkillsDraftTargetPath)
		}
		return ""
	}
	if t == "\r" || t == "\n" {
		id, path := strings.TrimSpace(s.hostSkillsDraftTargetID), strings.TrimSpace(s.hostSkillsDraftTargetPath)
		if id == "" || !filepath.IsAbs(path) {
			s.hostSkillsErr = "Target ID and absolute path are required"
			return ""
		}
		i := s.hostClientIndex()
		if i < 0 {
			s.hostSkillsErr = "host not found"
			return ""
		}
		found := false
		for j := range s.cfg.Clients[i].SkillTargets {
			if s.cfg.Clients[i].SkillTargets[j].ID == id {
				s.cfg.Clients[i].SkillTargets[j].Path = path
				found = true
			}
		}
		if !found {
			s.cfg.Clients[i].SkillTargets = append(s.cfg.Clients[i].SkillTargets, ducklord.SkillInstallTarget{ID: id, Path: path})
		}
		for j, target := range s.cfg.Clients[i].SkillTargets {
			if target.ID == id {
				s.hostSkillsTargetIndex = j
				break
			}
		}
		if e := s.saveHostSkillConfig(); e != nil {
			s.hostSkillsErr = e.Error()
			return ""
		}
		s.hostMenuStep, s.hostSkillsErr = "skills-agents", ""
		return ""
	}
	for _, r := range t {
		if r >= 32 {
			if s.hostSkillsField == 0 {
				s.hostSkillsDraftTargetID += string(r)
			} else {
				s.hostSkillsDraftTargetPath += string(r)
			}
		}
	}
	return ""
}

func (s *tuiState) handleHostSkillImport(t string) string {
	if t == "\t" || t == "\x1b[A" || t == "\x1b[B" {
		s.hostSkillsField = 1 - s.hostSkillsField
		return ""
	}
	if t == "\x7f" || t == "\b" {
		if s.hostSkillsField == 0 {
			s.hostSkillsDraftImportPath = trimLastRune(s.hostSkillsDraftImportPath)
		} else {
			s.hostSkillsDraftImportID = trimLastRune(s.hostSkillsDraftImportID)
		}
		return ""
	}
	if t == "\r" || t == "\n" {
		path := strings.TrimSpace(s.hostSkillsDraftImportPath)
		if path == "" {
			s.hostSkillsErr = "Local skill directory is required"
			return ""
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			s.hostSkillsErr = "Local skill directory was not found"
			return ""
		}
		id := strings.TrimSpace(s.hostSkillsDraftImportID)
		if id == "" {
			id = filepath.Base(filepath.Clean(path))
		}
		if !ducklord.SafeIdentifier(id) {
			s.hostSkillsErr = "Skill ID must be a safe identifier"
			return ""
		}
		preview, err := ducklord.PrepareSkillPreview(s.hostSkillsRepository, id, path)
		if err != nil {
			s.hostSkillsErr = err.Error()
			return ""
		}
		s.hostSkillsPreview = &preview
		s.hostSkillsPreviewAction = "import"
		s.hostMenuStep = "skills-preview"
		return ""
	}
	for _, r := range t {
		if r < 32 {
			continue
		}
		if s.hostSkillsField == 0 && len(s.hostSkillsDraftImportPath) < 512 {
			s.hostSkillsDraftImportPath += string(r)
		}
		if s.hostSkillsField == 1 && len(s.hostSkillsDraftImportID) < 128 {
			s.hostSkillsDraftImportID += string(r)
		}
	}
	return ""
}

func (s *tuiState) handleHostSkillTargetSelect(text string) string {
	i := s.hostClientIndex()
	if i < 0 || len(s.cfg.Clients[i].SkillTargets) == 0 {
		s.hostSkillsErr = "No named target configured"
		s.hostMenuStep = "skills-dashboard"
		return ""
	}
	switch text {
	case "j", "\x1b[B":
		s.hostSkillsTargetIndex = min(len(s.cfg.Clients[i].SkillTargets)-1, s.hostSkillsTargetIndex+1)
	case "k", "\x1b[A":
		s.hostSkillsTargetIndex = max(0, s.hostSkillsTargetIndex-1)
	case "\r", "\n":
		s.prepareHostSkillDownload()
	}
	return ""
}
func (s *tuiState) applyHostSkillsEvent(event hostSkillsEvent) {
	if event.host != "" && event.host != s.hostMenuTarget {
		return
	}
	ownedBySelection := s.hostSkillsEventOwnedBySelection(event)
	if event.action != "list" {
		s.hostSkillsBusy = false
		if s.hostSkillsCancel != nil {
			s.hostSkillsCancel()
			s.hostSkillsCancel = nil
		}
	}
	if event.action == "list" {
		// Cache completion for the target that started discovery. Only the
		// still-selected target may change shared modal status or visible data.
		// Identity-less completions cannot be associated with a target safely.
		if event.targetID == "" {
			return
		}
		if event.err != nil {
			if !ownedBySelection {
				return
			}
			s.hostSkillsErr = sanitizeTerminalText(event.err.Error())
			s.hostSkillsRemote = nil
		} else {
			if !ownedBySelection {
				s.cacheHostSkillsRemote(event.targetID, event.names)
				return
			}
			s.hostSkillsErr = ""
			s.hostSkillsRemote = event.names
			s.cacheHostSkillsRemote(event.targetID, event.names)
		}
		s.hostMenuStep = "skills-dashboard"
	} else if event.action == "delete" {
		if !ownedBySelection {
			if event.err == nil && event.skillID != "" {
				s.removeCachedHostSkill(event.targetID, event.skillID)
			}
			return
		}
		if event.err != nil {
			s.hostSkillsErr = sanitizeTerminalText(event.err.Error())
		} else {
			s.hostSkillsErr = "Deleted " + event.skillID
			s.removeCachedHostSkill(event.targetID, event.skillID)
		}
		s.hostMenuStep = "skills-dashboard"
	} else if event.err != nil {
		s.hostSkillsErr = sanitizeTerminalText(event.err.Error())
		s.hostMenuStep = "skills-result"
	} else if event.preview != nil {
		if s.hostSkillsPreview != nil {
			ducklord.CleanupSkillPreview(s.hostSkillsPreview)
		}
		s.hostSkillsPreview = event.preview
		s.hostSkillsPreviewAction = event.action
		s.hostMenuStep = "skills-preview"
	} else {
		s.hostSkillsErr = fmt.Sprintf("Deployed %d selected skill(s)", event.count)
		s.hostMenuStep = "skills-result"
	}
}

func (s *tuiState) cacheHostSkillsRemote(targetID string, names []string) {
	if targetID == "" {
		return
	}
	if s.hostSkillsRemoteByTarget == nil {
		s.hostSkillsRemoteByTarget = make(map[string][]string)
	}
	s.hostSkillsRemoteByTarget[targetID] = append([]string(nil), names...)
}

func (s *tuiState) removeCachedHostSkill(targetID, skillID string) {
	if targetID == "" || skillID == "" || s.hostSkillsRemoteByTarget == nil {
		return
	}
	current := s.hostSkillsRemoteByTarget[targetID]
	filtered := make([]string, 0, len(current))
	for _, id := range current {
		if id != skillID {
			filtered = append(filtered, id)
		}
	}
	s.hostSkillsRemoteByTarget[targetID] = filtered
}

func (s *tuiState) hostSkillsEventOwnedBySelection(event hostSkillsEvent) bool {
	if event.targetID == "" {
		return true
	}
	target, ok := s.selectedHostSkillTarget()
	return ok && target.ID == event.targetID
}

func (s *tuiState) remoteSkillsForTargetID(targetID string) []string {
	if targetID == "" {
		return nil
	}
	for _, target := range s.cfg.Clients[s.hostClientIndex()].SkillTargets {
		if target.ID == targetID {
			return s.remoteSkillsForTarget(target)
		}
	}
	return append([]string(nil), s.hostSkillsRemoteByTarget[targetID]...)
}

func (s *tuiState) startHostSkillsOperation(action string, work func(context.Context) hostSkillsEvent) {
	if s.hostSkillsBusy {
		return
	}
	if s.hostSkillsDone == nil {
		event := work(context.Background())
		event.action = action
		s.applyHostSkillsEvent(event)
		return
	}
	s.hostSkillsRequestID++
	id := s.hostSkillsRequestID
	ctx, cancel := context.WithCancel(context.Background())
	s.hostSkillsCancel = cancel
	s.hostSkillsBusy = true
	s.hostSkillsErr = ""
	s.hostMenuStep = "skills-busy"
	done := s.hostSkillsDone
	go func() {
		event := work(ctx)
		event.id = id
		event.action = action
		// A cancelled operation may finish after producing a staged preview. It
		// will never reach the event loop, so release that staging directory here.
		if ctx.Err() != nil {
			ducklord.CleanupSkillPreview(event.preview)
			event.preview = nil
		}
		select {
		case done <- event:
		case <-ctx.Done():
		}
	}()
}

func (s *tuiState) cancelHostSkillsOperation() {
	if s.hostSkillsCancel != nil {
		s.hostSkillsCancel()
		s.hostSkillsCancel = nil
	}
	s.hostSkillsRequestID++
	s.hostSkillsListRequestID++
	if s.hostSkillsListCancel != nil {
		s.hostSkillsListCancel()
		s.hostSkillsListCancel = nil
	}
	s.hostSkillsBusy = false
	if s.hostSkillsPreview != nil {
		ducklord.CleanupSkillPreview(s.hostSkillsPreview)
		s.hostSkillsPreview = nil
	}
	s.hostSkillsErr = "Host Skills operation cancelled"
	s.hostMenuStep = "skills-dashboard"
}

func (s *tuiState) cleanupHostSkills() {
	if s.hostSkillsCancel != nil {
		s.hostSkillsCancel()
		s.hostSkillsCancel = nil
	}
	s.hostSkillsRequestID++
	s.hostSkillsListRequestID++
	if s.hostSkillsListCancel != nil {
		s.hostSkillsListCancel()
		s.hostSkillsListCancel = nil
	}
	s.hostSkillsBusy = false
	if s.hostSkillsPreview != nil {
		ducklord.CleanupSkillPreview(s.hostSkillsPreview)
		s.hostSkillsPreview = nil
	}
}

func (s *tuiState) refreshHostSkills() {
	if s.hostSkillsDone == nil {
		return // unit routes remain deterministic; TUI refreshes through its event channel.
	}
	i := s.hostClientIndex()
	target, ok := s.selectedHostSkillTarget()
	if i < 0 || !ok {
		return
	}
	client, path := s.cfg.Clients[i], target.Path
	host, targetID := s.hostMenuTarget, target.ID
	s.hostSkillsListRequestID++
	id := s.hostSkillsListRequestID
	ctx, cancel := context.WithCancel(context.Background())
	if s.hostSkillsListCancel != nil {
		s.hostSkillsListCancel()
	}
	s.hostSkillsListCancel = cancel
	done := s.hostSkillsDone
	go func() {
		names, err := ducklord.ListSkillsSSH(ctx, client, path)
		event := hostSkillsEvent{id: id, action: "list", host: host, names: names, targetID: targetID, err: err}
		if ctx.Err() != nil {
			return
		}
		select {
		case done <- event:
		case <-ctx.Done():
		}
	}()
}

func (s *tuiState) deleteHostSkillRemote() {
	i := s.hostClientIndex()
	target, ok := s.selectedHostSkillTarget()
	if i < 0 || !ok || !ducklord.SafeIdentifier(s.hostSkillsPendingID) {
		s.hostSkillsErr = "Choose a remote skill first"
		s.hostMenuStep = "skills-dashboard"
		return
	}
	c := s.cfg.Clients[i]
	host, path, id := s.hostMenuTarget, target.Path, s.hostSkillsPendingID
	s.startHostSkillsOperation("delete", func(ctx context.Context) hostSkillsEvent {
		err := ducklord.DeleteSkillSSH(ctx, c, path, id)
		return hostSkillsEvent{err: err, host: host, targetID: target.ID, skillID: id}
	})
}

func (s *tuiState) deployHostSkills() {
	var cfg *ducklord.Config
	if s.cfg != nil {
		cfg = s.cfg.Clone()
	}
	host, repository := s.hostMenuTarget, s.hostSkillsRepository
	target, ok := s.selectedHostSkillTarget()
	if !ok {
		s.hostSkillsErr = "Choose an agent target first"
		return
	}
	targetID := target.ID
	s.startHostSkillsOperation("deploy", func(ctx context.Context) hostSkillsEvent {
		v, err := ducklord.DeployTargetSkills(ctx, cfg, host, targetID, repository)
		return hostSkillsEvent{count: len(v), err: err}
	})
}
func (s *tuiState) prepareHostSkillFetch() {
	if i, ok := s.selectedHostSkillSource(); ok {
		src := s.cfg.SkillSources[i]
		repository := s.hostSkillsRepository
		s.startHostSkillsOperation("fetch/update", func(ctx context.Context) hostSkillsEvent {
			p, err := ducklord.PrepareHTTPSArchive(ctx, src.URL, src.InsecureHTTPS, repository, src.SkillID)
			return hostSkillsEvent{preview: &p, err: err}
		})
		return
	}
	id, ok := s.selectedHostSkill()
	if !ok {
		s.hostSkillsErr = "Select a managed skill first"
		return
	}
	var src ducklord.SkillTrackingSource
	for _, v := range s.cfg.SkillSources {
		if v.SkillID == id {
			src = v
			break
		}
	}
	if src.SkillID == "" {
		s.hostSkillsErr = "No HTTPS source configured for " + id
		return
	}
	repository := s.hostSkillsRepository
	s.startHostSkillsOperation("fetch/update", func(ctx context.Context) hostSkillsEvent {
		p, err := ducklord.PrepareHTTPSArchive(ctx, src.URL, src.InsecureHTTPS, repository, id)
		return hostSkillsEvent{preview: &p, err: err}
	})
}
func (s *tuiState) prepareHostSkillDownload() {
	id := strings.TrimSpace(s.hostSkillsDraftImportID)
	if id == "" {
		var ok bool
		id, ok = s.selectedHostSkill()
		if !ok {
			s.hostSkillsErr = "Enter a safe skill ID first"
			return
		}
	}
	if !ducklord.SafeIdentifier(id) {
		s.hostSkillsErr = "Skill ID must be a safe identifier"
		return
	}
	i := s.hostClientIndex()
	if i < 0 || len(s.cfg.Clients[i].SkillTargets) == 0 {
		s.hostSkillsErr = "No named target configured"
		return
	}
	cfgSnapshot := s.cfg.Clone()
	c := cfgSnapshot.Clients[i]
	target := c.SkillTargets[min(max(0, s.hostSkillsTargetIndex), len(c.SkillTargets)-1)]
	repository := s.hostSkillsRepository
	s.startHostSkillsOperation("download", func(ctx context.Context) hostSkillsEvent {
		p, err := ducklord.PrepareDownloadSkillSSH(ctx, c, repository, id, target.Path)
		return hostSkillsEvent{preview: &p, err: err}
	})
}
