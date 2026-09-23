package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
	"golang.org/x/sys/unix"
)

type workspacePaneIntent struct {
	projectID      string
	tabID          string
	targetID       string
	placement      ducklord.PanePlacement
	originIdentity ducklord.SessionIdentity

	moveConfirmationBackStep string
}

func (s *tuiState) beginWorkspacePane() {
	if !s.workspacePreview || !s.workspaceProjectFocus {
		return
	}
	if s.hostScoped {
		s.outputErr = "Project pane placement requires the full Ducklord workspace"
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	s.workspacePaneIntent = workspacePaneIntent{projectID: nav.CurrentProjectID(), targetID: nav.CurrentPaneID()}
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "placement", 0
	s.workspacePaneErr = ""
}

func (s *tuiState) beginWorkspaceMove()   { s.beginWorkspacePaneAction("move-placement") }
func (s *tuiState) beginWorkspaceDetach() { s.beginWorkspacePaneAction("detach-confirm") }

// detachFocusedSessionPane handles the terminal prefix shortcut without
// opening the Project action modal. The built-in Default Project keeps its
// existing detach behavior (the Project-focused action remains available).
func (s *tuiState) detachFocusedSessionPane() bool {
	if !s.workspacePreview || !s.focused || s.hostScoped {
		return false
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return true
	}
	projectID, paneID := nav.CurrentProjectID(), nav.CurrentPaneID()
	if projectID == ducklord.DefaultProjectID {
		s.outputErr = "detach Session panes from Project focus"
		return true
	}
	_, ok := s.activity().ProjectLayout.PaneSession(projectID, paneID)
	if !ok {
		s.outputErr = "select a Session pane first"
		return true
	}
	next := s.activity().Clone()
	if _, err := next.ProjectLayout.DetachPane(projectID, paneID); err != nil {
		s.outputErr = err.Error()
		return true
	}
	if err := s.activityStore.Save(next); err != nil {
		s.outputErr = "save Session pane: " + err.Error()
		return true
	}
	s.activityState = next
	s.workspacePaneChanged = true
	// A detach supersedes any deferred Help/Notes lease from the old pane;
	// allowing that restore to run after the replacement attach would reclaim
	// focus and leave the surviving pane's control writer unused.
	s.helpFocusRestorePending = false
	s.helpPendingInputKey = ""
	s.notesFocusRestorePending = false
	if _, pane, ok := next.ProjectLayout.FirstSessionPane(projectID); ok {
		_ = nav.SelectPane(projectID, pane)
		s.workspaceProjectFocus = false
		s.workspaceAttachFromProject = true
		// Keep the selected surviving pane in place while the old attach is
		// torn down; clearAttachIdentity uses this flag to restore Project focus.
		s.workspaceFocusFromProject = false
	} else {
		// The detached project has no valid Session owner. Leave navigation on
		// Project focus so the cleanup path cannot retain the removed attach.
		_ = nav.SelectProject(projectID)
		s.workspaceProjectFocus = true
	}
	s.outputErr = ""
	return true
}

func (s *tuiState) beginWorkspacePaneAction(step string) {
	if !s.workspacePreview || !s.workspaceProjectFocus {
		return
	}
	if s.hostScoped {
		s.outputErr = "Project pane actions require the full Ducklord workspace"
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	projectID, paneID := nav.CurrentProjectID(), nav.CurrentPaneID()
	identity, ok := s.activity().ProjectLayout.PaneSession(projectID, paneID)
	if !ok {
		s.outputErr = "select a Session pane first"
		return
	}
	s.workspacePaneIntent = workspacePaneIntent{projectID: projectID}
	s.workspacePaneSourceID, s.workspacePaneIdentity = paneID, identity
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, step, 0
	if step == "detach-confirm" {
		s.workspacePaneIndex = 1
	}
	s.workspacePaneErr = ""
}

func paneLeafIDs(pane *ducklord.SessionPane, exclude string, ids *[]string) {
	if pane == nil {
		return
	}
	if pane.Session != nil && pane.ID != exclude {
		*ids = append(*ids, pane.ID)
	}
	paneLeafIDs(pane.First, exclude, ids)
	paneLeafIDs(pane.Second, exclude, ids)
}

func (s *tuiState) workspaceMoveTargets() []string {
	project := s.activity().ProjectLayout.Project(s.workspacePaneIntent.projectID)
	if project == nil {
		return nil
	}
	var ids []string
	for _, tab := range project.Tabs {
		paneLeafIDs(tab.Root, s.workspacePaneSourceID, &ids)
	}
	return ids
}

func (s *tuiState) commitWorkspacePaneAction(detach bool) error {
	if s.workspacePaneCandidate.Client != "" && !s.workspacePaneCandidateCurrent(s.workspacePaneCandidate) {
		return fmt.Errorf("selected Session changed or is unavailable; try again")
	}
	next := s.activity().Clone()
	identity, ok := next.ProjectLayout.PaneSession(s.workspacePaneIntent.projectID, s.workspacePaneSourceID)
	if !ok || identity != s.workspacePaneIdentity {
		return fmt.Errorf("source Session pane changed; reopen this action")
	}
	var paneID string
	var err error
	if detach {
		_, err = next.ProjectLayout.DetachPane(s.workspacePaneIntent.projectID, s.workspacePaneSourceID)
	} else {
		paneID, err = next.ProjectLayout.MovePane(s.workspacePaneIntent.projectID, s.workspacePaneSourceID, s.workspacePaneIntent.placement, s.workspacePaneIntent.targetID)
	}
	if err != nil {
		return err
	}
	if err = s.activityStore.Save(next); err != nil {
		return fmt.Errorf("save Session pane: %w", err)
	}
	s.activityState = next
	s.workspacePaneChanged = true
	if nav, navErr := s.workspaceNavigation(); navErr == nil {
		if !detach {
			_ = nav.SelectPane(s.workspacePaneIntent.projectID, paneID)
		}
	} else {
		s.outputErr = "Session pane saved; navigation will refresh: " + sanitizeTerminalText(navErr.Error())
	}
	return nil
}

func (s *tuiState) beginWorkspaceProject() {
	if !s.workspacePreview || !s.workspaceProjectFocus {
		return
	}
	if s.hostScoped {
		s.outputErr = "Project creation requires the full Ducklord workspace"
		return
	}
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "project-create", 0
	s.workspacePaneName, s.workspacePaneErr = "", ""
	s.workspacePaneIntent = workspacePaneIntent{}
	s.workspacePaneHosts = []string{}
}

func (s *tuiState) beginWorkspaceProjectDelete() {
	if !s.workspacePreview || !s.workspaceProjectFocus || s.hostScoped {
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	projectID := nav.CurrentProjectID()
	if projectID == ducklord.DefaultProjectID {
		s.outputErr = "Default Project cannot be deleted"
		return
	}
	if s.activity().ProjectLayout.Project(projectID) == nil {
		s.outputErr = "selected Project no longer exists"
		return
	}
	s.workspacePaneIntent = workspacePaneIntent{projectID: projectID}
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "project-delete-confirm", 1 // Cancel is safest.
	s.workspacePaneErr = ""
}

func (s *tuiState) commitWorkspaceProjectDelete() error {
	projectID := s.workspacePaneIntent.projectID
	if projectID == "" || projectID == ducklord.DefaultProjectID || s.activity().ProjectLayout.Project(projectID) == nil {
		return fmt.Errorf("project changed; reopen delete confirmation")
	}
	focusWasOnDeletedProject := s.workspaceNav != nil && s.workspaceNav.NotificationFocusProjectID() == projectID
	next := s.activity().Clone()
	if err := next.ProjectLayout.RemoveProject(projectID); err != nil {
		return err
	}
	if err := s.activityStore.Save(next); err != nil {
		return fmt.Errorf("save Project deletion: %w", err)
	}
	s.activityState = next
	s.workspacePaneChanged = true
	if _, err := s.workspaceNavigation(); err != nil {
		s.outputErr = "Project deleted; navigation will refresh: " + sanitizeTerminalText(err.Error())
	} else {
		s.outputErr = "Project deleted locally; remote Sessions continue running"
		if focusWasOnDeletedProject {
			s.outputErr += "; notification focus turned off"
		}
	}
	return nil
}

func (s *tuiState) workspacePaneCandidates() []ducklord.RemoteSession {
	seen := make(map[ducklord.SessionIdentity]int)
	var candidates []ducklord.RemoteSession
	for _, session := range s.sessions {
		if s.workspacePaneIntent.projectID != "" && !s.workspaceProjectAllowsHost(s.workspacePaneIntent.projectID, session.Client) {
			continue
		}
		identity, ok := ducklord.IdentityFromSession(session)
		if !ok || !workspacePaneKnownSession(session) {
			continue
		}
		if query := strings.ToLower(strings.TrimSpace(s.workspacePaneQuery)); query != "" &&
			!strings.Contains(strings.ToLower(session.Name+" "+session.Client+" "+session.SessionID), query) {
			continue
		}
		if index, exists := seen[identity]; exists {
			if s.hostIsLive(session.Client) && !s.hostIsLive(candidates[index].Client) {
				candidates[index] = session
			}
			continue
		}
		seen[identity] = len(candidates)
		candidates = append(candidates, session)
	}
	return candidates
}

// Pane placement is local organization, not a remote control operation. A
// previously observed Session remains placeable while its Host is offline.
func workspacePaneKnownSession(session ducklord.RemoteSession) bool {
	return session.RuntimeGeneration != 0 && session.Name != "(offline)" &&
		(session.Status == "running" || session.Status == "disconnected")
}

func paneIDForSession(pane *ducklord.SessionPane, identity ducklord.SessionIdentity) string {
	if pane == nil {
		return ""
	}
	if pane.Session != nil && *pane.Session == identity {
		return pane.ID
	}
	if id := paneIDForSession(pane.First, identity); id != "" {
		return id
	}
	return paneIDForSession(pane.Second, identity)
}

func (s *tuiState) projectPaneForSession(identity ducklord.SessionIdentity) string {
	project := s.activity().ProjectLayout.Project(s.workspacePaneIntent.projectID)
	if project == nil {
		return ""
	}
	for _, tab := range project.Tabs {
		if id := paneIDForSession(tab.Root, identity); id != "" {
			return id
		}
	}
	return ""
}

func (s *tuiState) workspacePaneChoices() []string {
	switch s.workspacePaneStep {
	case "project-hosts-create", "project-hosts-edit":
		choices := []string{}
		for _, host := range s.workspaceProjectHostOptions() {
			mark := "[ ] "
			if slices.Contains(s.workspacePaneHosts, host) {
				mark = "[x] "
			}
			choices = append(choices, mark+displayField(host))
		}
		return append(choices, "Save Project Hosts")
	case "placement":
		return []string{"New Terminal tab", "Split vertically", "Split horizontally"}
	case "drop-placement":
		if s.workspacePaneIntent.targetID == "" {
			return []string{"New Terminal tab"}
		}
		return []string{"New Terminal tab", "Split vertically", "Split horizontally"}
	case "source":
		return []string{"New shell session", "Add existing session"}
	case "move-placement":
		return []string{"New Terminal tab", "Split vertically", "Split horizontally"}
	case "move-target":
		ids := s.workspaceMoveTargets()
		choices := make([]string, 0, len(ids))
		for _, id := range ids {
			identity, _ := s.activity().ProjectLayout.PaneSession(s.workspacePaneIntent.projectID, id)
			label := identity.SessionID
			for _, session := range s.sessions {
				if candidate, ok := ducklord.IdentityFromSession(session); ok && candidate == identity {
					label = displayField(session.Name) + " @" + displayField(session.Client)
					break
				}
			}
			choices = append(choices, label)
		}
		return choices
	case "move-confirm":
		return []string{"Move local pane", "Cancel"}
	case "existing-move-confirm":
		return []string{"Move existing pane here", "Cancel"}
	case "detach-confirm":
		return []string{"Detach local pane", "Cancel"}
	case "project-delete-confirm":
		return []string{"Delete local Project", "Cancel"}
	case "existing":
		candidates := s.workspacePaneCandidates()
		choices := make([]string, 0, len(candidates))
		for _, session := range candidates {
			label := fmt.Sprintf("%s @%s  [%s]", displayField(session.Name), displayField(session.Client), session.SessionID)
			if session.Status == "disconnected" || !s.hostIsLive(session.Client) {
				label += "  (offline)"
			}
			if identity, ok := ducklord.IdentityFromSession(session); ok && s.projectPaneForSession(identity) != "" {
				label += "  (move in Project)"
			}
			choices = append(choices, label)
		}
		return choices
	}
	return nil
}

func (s *tuiState) renderWorkspacePaneModal(out io.Writer, cols, rows int) {
	if s.workspacePaneMode && s.workspacePaneStep == "notes" {
		s.renderNotesModal(out, cols, rows)
		return
	}
	if s.workspacePaneMode && s.workspacePaneStep == "context-config" {
		s.renderWorkspaceAreaConfig(out, cols, rows)
		return
	}
	if s.workspacePaneMode && strings.HasPrefix(s.workspacePaneStep, "project-transfer-") {
		s.renderProjectTransfer(out, cols, rows)
		return
	}
	if s.workspacePaneStep == "tab-rename" {
		s.renderWorkspaceTabRenameModal(out, cols, rows)
		return
	}
	if s.workspacePaneStep == "project-rename" {
		s.renderWorkspaceProjectRenameModal(out, cols, rows)
		return
	}
	if !s.workspacePaneMode {
		return
	}
	s.resetModalMouse()
	if s.workspacePaneStep == "project-create" {
		lines := []modalRenderLine{{modalTitle, "  Create Project"}, {modalInput, "  name › " + s.workspacePaneName + "_"}}
		if s.workspacePaneErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  Enter choose Hosts · Esc/Ctrl+C close"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if s.workspacePaneStep == "project-delete-confirm" {
		project := s.activity().ProjectLayout.Project(s.workspacePaneIntent.projectID)
		name := "Project unavailable"
		if project != nil {
			name = displayField(project.Name)
		}
		lines := []modalRenderLine{{modalTitle, "  Delete Project"}, {modalMuted, "  " + name},
			{modalMuted, "  Removes local panes only; remote Sessions keep running."},
			{modalMuted, "  Sessions with no other Project return to Default."}}
		for i, choice := range s.workspacePaneChoices() {
			s.modalChoice(len(lines), &s.workspacePaneIndex, i, "\r")
			style, prefix := "", "  "
			if i == s.workspacePaneIndex {
				style, prefix = modalSelected, "› "
			}
			lines = append(lines, modalRenderLine{style, prefix + choice})
		}
		if s.workspacePaneErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter confirm · Esc/Ctrl+C close"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	choices := s.workspacePaneChoices()
	title := "  Add Session pane"
	if strings.HasPrefix(s.workspacePaneStep, "project-hosts-") {
		title = "  Project SSH Hosts · select multiple"
	}
	if strings.HasPrefix(s.workspacePaneStep, "move-") {
		title = "  Move Session pane"
	}
	if s.workspacePaneStep == "drop-placement" {
		title = "  Place dragged Session pane"
	}
	if s.workspacePaneStep == "detach-confirm" {
		title = "  Detach Session pane"
	}
	lines := []modalRenderLine{{modalTitle, title}}
	if strings.HasPrefix(s.workspacePaneStep, "project-hosts-") {
		lines = append(lines, modalRenderLine{modalMuted, "  Enter/Space toggle Hosts; choose Save when done."}, modalRenderLine{modalMuted, "  Removing a Host requires detaching its panes first."})
	}
	if s.workspacePaneStep == "drop-placement" {
		candidate := s.workspacePaneCandidate
		lines = append(lines, modalRenderLine{modalMuted, "  Session: " + displayField(candidate.Name) + " @" + displayField(candidate.Client)})
	}
	if s.workspacePaneStep == "detach-confirm" {
		lines = append(lines, modalRenderLine{modalMuted, "  Remote session keeps running."})
	}
	if s.workspacePaneStep == "move-confirm" {
		lines = append(lines, modalRenderLine{modalMuted, "  Local view only; remote session stays running."})
	}
	if s.workspacePaneStep == "existing-move-confirm" {
		lines = append(lines, modalRenderLine{modalMuted, "  Already in this Project; move its pane, not duplicate it."})
		label := s.workspacePaneIdentity.SessionID
		for _, session := range s.sessions {
			if identity, ok := ducklord.IdentityFromSession(session); ok && identity == s.workspacePaneIdentity {
				label = displayField(session.Name) + " @" + displayField(session.Client)
				break
			}
		}
		lines = append(lines, modalRenderLine{modalMuted, "  Session: " + label})
	}
	if s.workspacePaneStep == "existing" {
		lines = append(lines, modalRenderLine{modalInput, "  find › " + s.workspacePaneQuery + "_"})
	}
	start := 0
	maxChoices := max(1, rows-6)
	if s.workspacePaneStep == "existing" {
		maxChoices = max(1, rows-7)
	}
	if s.workspacePaneStep == "project-hosts-create" || s.workspacePaneStep == "project-hosts-edit" {
		maxChoices = max(1, rows-8)
	}
	if len(choices) > maxChoices {
		start = min(max(0, s.workspacePaneIndex-maxChoices/2), len(choices)-maxChoices)
	}
	end := min(len(choices), start+maxChoices)
	for i := start; i < end; i++ {
		s.modalChoice(len(lines), &s.workspacePaneIndex, i, "\r")
		choice := choices[i]
		style, prefix := "", "  "
		if i == s.workspacePaneIndex {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, prefix + choice})
	}
	if len(choices) == 0 {
		empty := "  No eligible sessions"
		if s.workspacePaneStep == "move-target" {
			empty = "  No other pane to split; choose a new tab"
		}
		lines = append(lines, modalRenderLine{modalMuted, empty})
	}
	if start > 0 || end < len(choices) {
		lines = append(lines, modalRenderLine{modalMuted, fmt.Sprintf("  %d above · %d below", start, len(choices)-end)})
	}
	if s.workspacePaneErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + s.workspacePaneErr})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter continue · Esc back · Ctrl+C close"})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) closeWorkspacePane() {
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneErr = false, "", ""
	s.workspacePaneName = ""
	s.workspacePaneHosts = nil
	s.workspacePaneQuery = ""
	s.workspacePaneIntent = workspacePaneIntent{}
	s.workspacePaneSourceID = ""
	s.workspacePaneIdentity = ducklord.SessionIdentity{}
	s.workspacePaneCandidate = ducklord.RemoteSession{}
	s.projectTransferPath = ""
	s.projectTransferData = nil
	s.projectTransfer = ducklord.ProjectTransfer{}
	s.projectTransferPreview = ducklord.ProjectImportPreview{}
	s.projectTransferAction = ""
	s.workspacePaneIndex = 0
}

// beginFocusedDefaultDetachRestore records the source Session pane for the
// event loop's direct control reopen.  The modal may have caused an in-flight
// control open to be discarded, so restoration must retain the exact source
// key instead of replaying navigation input.
func (s *tuiState) beginFocusedDefaultDetachRestore(attachKey string) {
	s.activeAttachKey = attachKey
	s.workspaceProjectFocus = false
	s.workspaceFocusFromProject = false
	s.workspaceAttachFromProject = false
	s.workspacePaneRestoreFocused = false
	s.workspacePaneRestoreAttachKey = ""
	s.workspacePaneFocusRestoreKey = attachKey
	s.workspacePaneFocusRestorePending = true
}

func (s *tuiState) closeNotesModal() {
	projectID, paneID := s.notesPreviousProjectID, s.notesPreviousPaneID
	previousProjectFocus := s.notesPreviousProjectFocus
	previousFocused := s.notesPreviousFocused
	previousAttachKey := s.notesPreviousAttachKey
	previousAttachValid := s.notesPreviousAttachValid
	s.notesFormActive = false
	s.notesPickerMode = ""
	s.notesPickerChoices = nil
	s.notesPickerLabels = nil
	s.notesPickerIndex = 0
	s.closeWorkspacePane()
	s.workspaceProjectFocus = previousProjectFocus
	originPresent := false
	if previousAttachKey == "" {
		s.activeAttachKey = ""
		// Focused fixtures may intentionally have no attach key; retain the
		// deferred restoration contract while leaving the key empty.
		originPresent = previousFocused
	} else if _, ok := s.sessionForKey(previousAttachKey); ok {
		originPresent = true
		s.activeAttachKey = previousAttachKey
	} else if previousAttachValid {
		// The focused origin may have disappeared while Notes was open. Do not
		// leave its attachment key or queue a restore against a stale session;
		// the navigation fallback below is the safe route.
		s.activeAttachKey = ""
	} else {
		// Some unit fixtures use synthetic keys without a session catalogue.
		// Preserve the normal deferred restoration contract for those states.
		originPresent = true
		s.activeAttachKey = previousAttachKey
	}
	s.focused = previousFocused && originPresent
	s.notesFocusRestorePending = false
	s.notesFocusRestoreReselect = false
	s.notesPendingInputKey = ""
	s.notesPendingInput = nil
	s.notesPreviousAttachValid = false
	if previousFocused && originPresent {
		s.focused = false
		s.notesFocusRestorePending = true
		s.notesFocusRestoreReselect = true
		s.notesPendingInputKey = previousAttachKey
		s.notesPendingInput = nil
	}
	if projectID == "" {
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = "leave Notes: " + err.Error()
		return
	}
	if s.activity().ProjectLayout.HasPane(projectID, paneID) {
		if err := nav.SelectPane(projectID, paneID); err != nil {
			s.outputErr = "select prior pane: " + err.Error()
		}
	} else if _, fallback, ok := s.activity().ProjectLayout.FirstSessionPane(nav.CurrentProjectID()); ok {
		if err := nav.SelectPane(nav.CurrentProjectID(), fallback); err != nil {
			s.outputErr = "select prior pane: " + err.Error()
		}
	}
}

func (s *tuiState) renderNotesModal(out io.Writer, cols, rows int) {
	s.resetModalMouse()
	if s.notesPickerMode != "" {
		lines := []modalRenderLine{{modalTitle, "  ✦ Choose Notes Scope ✦"}, {modalMuted, "  ↑/↓ select · Enter confirm · Esc cancel"}}
		for i, choice := range s.notesPickerChoices {
			label := choice
			if i < len(s.notesPickerLabels) && s.notesPickerLabels[i] != "" {
				label = s.notesPickerLabels[i]
			}
			style := ""
			prefix := "  "
			if i == s.notesPickerIndex {
				style = modalSelected
				prefix = "› "
			}
			lines = append(lines, modalRenderLine{style, prefix + label})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if s.notesFormActive {
		field := "Title"
		if s.notesFormField == 1 {
			field = "Content"
		}
		lines := []modalRenderLine{{modalTitle, "  Note"}, {modalMuted, "  Title"}, {modalInput, "  " + s.notesFormDisplay(0)}, {modalMuted, "  Content"}, {modalInput, "  " + s.notesFormDisplay(1)}, {modalStatus, "  Editing " + field + " · cursor " + strconv.Itoa(s.notesFormCursor) + "/" + strconv.Itoa(s.notesFormValueLen())}}
		if s.outputErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.outputErr})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  Enter next/save · Tab field · ←/→ cursor"}, modalRenderLine{modalMuted, "  Ctrl+S save · Ctrl+K clear · Esc cancel"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	breadcrumb := "Global"
	switch s.notesScope {
	case ducklord.NotesProject:
		breadcrumb = "Global  ›  Project " + displayField(s.notesProjectID)
	case ducklord.NotesSession:
		breadcrumb = "Global  ›  Project " + displayField(s.notesProjectID) + "  ›  Session " + displayField(s.notesSessionName())
	}
	lines := []modalRenderLine{{modalTitle, "  ✦ Notes Codex ✦"}, {modalMuted, "  " + breadcrumb}}
	if s.notesSearchActive {
		lines = append(lines, modalRenderLine{modalInput, "  /" + s.notesQuery + "_"})
	}
	footer := []string{"  Enter copy content · ↑/↓ j/k select · ←/→ h/l scope", "  g/p/s scope · / search · a add · e edit · E notebook · Esc close"}
	availableRows := rows - 2 - len(lines) - len(footer)
	if s.outputErr != "" {
		availableRows--
	}
	maxEntries := max(1, availableRows/2)
	start := min(max(0, s.workspacePaneIndex-maxEntries/2), max(0, len(s.notesEntries)-maxEntries))
	end := min(len(s.notesEntries), start+maxEntries)
	for i := start; i < end; i++ {
		style, prefix := "", "  "
		if i == s.workspacePaneIndex {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, prefix + displayField(s.notesEntries[i].Title)})
		origin := ""
		if i < len(s.notesEntryOrigins) {
			origin = " · " + s.notesEntryOrigins[i]
		}
		lines = append(lines, modalRenderLine{modalMuted, "    " + notesEntryExcerpt(s.notesEntries[i].Body) + origin})
	}
	if len(s.notesEntries) == 0 {
		lines = append(lines, modalRenderLine{modalMuted, "  No Notes"})
	}
	if s.outputErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + s.outputErr})
	}
	for _, line := range footer {
		lines = append(lines, modalRenderLine{modalMuted, line})
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) notesFormValue() string {
	if s.notesFormField == 1 {
		return s.notesFormBody
	}
	return s.notesFormTitle
}

func nextNotesScope(scope ducklord.NotesScope) ducklord.NotesScope {
	switch scope {
	case ducklord.NotesGlobal:
		return ducklord.NotesProject
	case ducklord.NotesProject:
		return ducklord.NotesSession
	default:
		return ducklord.NotesSession
	}
}

func previousNotesScope(scope ducklord.NotesScope) ducklord.NotesScope {
	switch scope {
	case ducklord.NotesSession:
		return ducklord.NotesProject
	case ducklord.NotesProject:
		return ducklord.NotesGlobal
	default:
		return ducklord.NotesGlobal
	}
}

func (s *tuiState) notesFormValueLen() int { return utf8.RuneCountInString(s.notesFormValue()) }

func (s *tuiState) notesFormDisplay(field int) string {
	var value string
	if field == 1 {
		value = s.notesFormBody
	} else {
		value = s.notesFormTitle
	}
	if field != s.notesFormField {
		return value
	}
	runes := []rune(value)
	cursor := min(max(0, s.notesFormCursor), len(runes))
	return string(runes[:cursor]) + "│" + string(runes[cursor:])
}

func (s *tuiState) notesFormSetField(field int) {
	s.notesFormField = field
	s.notesFormCursor = s.notesFormValueLen()
}

func (s *tuiState) notesFormSetValue(value string) {
	if s.notesFormField == 1 {
		s.notesFormBody = value
	} else {
		s.notesFormTitle = value
	}
}

func notesEntryExcerpt(body string) string {
	const maxExcerptRunes = 72
	compact := strings.Join(strings.Fields(body), " ")
	compact = displayField(compact)
	if compact == "" {
		return "(blank folio)"
	}
	runes := []rune(compact)
	if len(runes) > maxExcerptRunes {
		return string(runes[:maxExcerptRunes-1]) + "…"
	}
	return compact
}

// placeWorkspacePane is one atomic local transaction. The remote Session is
// never started, stopped, or yielded by placing its local view.
func (s *tuiState) placeWorkspacePane(intent workspacePaneIntent, session ducklord.RemoteSession) error {
	return s.commitWorkspacePanePlacement(intent, session, false)
}

func (s *tuiState) placeCreatedWorkspacePane(intent workspacePaneIntent, session ducklord.RemoteSession) error {
	if intent.originIdentity != (ducklord.SessionIdentity{}) {
		identity, ok := s.activity().ProjectLayout.PaneSession(intent.projectID, intent.targetID)
		tabID, tabOK := s.activity().ProjectLayout.PaneTabID(intent.projectID, intent.targetID)
		if !ok || !tabOK || tabID != intent.tabID || identity != intent.originIdentity {
			return fmt.Errorf("origin Session pane disappeared, moved, or changed before placement")
		}
	}
	return s.commitWorkspacePanePlacement(intent, session, true)
}

func (s *tuiState) commitWorkspacePanePlacement(intent workspacePaneIntent, session ducklord.RemoteSession, created bool) error {
	if !s.workspaceProjectAllowsHost(intent.projectID, session.Client) {
		return fmt.Errorf("host is not associated with this Project; edit Project Hosts first")
	}
	identity, ok := ducklord.IdentityFromSession(session)
	if !ok {
		return fmt.Errorf("session has no stable identity")
	}
	if !s.workspacePaneCandidateCurrent(session) {
		return fmt.Errorf("session changed or is unavailable; refresh before placing its pane")
	}
	next := s.activity().Clone()
	var paneID string
	var err error
	// Inventory discovery adds newly created Sessions to Default before their
	// pending placement runs. Reposition that implicit pane for this creation
	// only; existing-session actions still require explicit move confirmation.
	if created && intent.projectID == ducklord.DefaultProjectID {
		paneID = s.projectPaneID(intent.projectID, identity)
	}
	if paneID != "" {
		if intent.placement != ducklord.PlaceNewTab {
			paneID, err = next.ProjectLayout.MovePane(intent.projectID, paneID, intent.placement, intent.targetID)
		}
	} else {
		paneID, err = next.ProjectLayout.Place(intent.projectID, identity, intent.placement, intent.targetID)
	}
	if err != nil {
		return err
	}
	if err := s.activityStore.Save(next); err != nil {
		return fmt.Errorf("save Session pane: %w", err)
	}
	s.activityState = next
	s.workspacePaneChanged = true
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = "Session pane saved; navigation will refresh: " + sanitizeTerminalText(err.Error())
		return nil
	}
	if err := nav.SelectPane(intent.projectID, paneID); err != nil {
		s.outputErr = "Session pane saved; navigation will refresh: " + sanitizeTerminalText(err.Error())
	} else if created {
		// Placement completes the captured quick-shell route.  Keep the newly
		// created pane as the Session owner even if an older async attach/control
		// event arrives after the inventory update.
		s.activeAttachKey = sessionKey(session)
		s.workspaceProjectFocus = false
		s.workspaceFocusFromProject = false
		s.workspaceAttachFromProject = true
		s.workspacePlacementFocusPending = true
		s.workspacePlacementInputPending = true
	}
	return nil
}

// handleWorkspacePaneInput returns whether the existing Create wizard should
// open. Modal input never reaches a PTY.
func (s *tuiState) handleWorkspacePaneInput(input []byte) (openCreate bool) {
	if strings.HasPrefix(s.workspacePaneStep, "project-transfer-") {
		s.handleProjectTransferInput(input)
		return false
	}
	if s.workspacePaneStep == "notes" {
		return s.handleNotesInput(input)
	}
	if s.workspacePaneStep == "context-config" {
		s.handleWorkspaceAreaConfig(input)
		return false
	}
	if s.workspacePaneStep == "tab-rename" {
		return s.handleWorkspaceTabRenameInput(input)
	}
	if s.workspacePaneStep == "project-rename" {
		return s.handleWorkspaceProjectRenameInput(input)
	}
	key := string(input)
	if key == " " && strings.HasPrefix(s.workspacePaneStep, "project-hosts-") {
		key = "\r"
	}
	if s.workspacePaneStep == "project-create" {
		switch key {
		case "\x03", "\x1b":
			s.closeWorkspacePane()
		case "\x7f", "\b":
			runes := []rune(s.workspacePaneName)
			if len(runes) > 0 {
				s.workspacePaneName = string(runes[:len(runes)-1])
			}
		case "\r":
			next := s.activity().Clone()
			_, err := next.ProjectLayout.AddProject(s.workspacePaneName)
			if err != nil {
				s.workspacePaneErr = sanitizeTerminalText(err.Error())
				return false
			}
			s.workspacePaneStep, s.workspacePaneIndex = "project-hosts-create", 0
		default:
			if utf8.Valid(input) && !strings.ContainsRune(key, '\x1b') {
				for _, r := range key {
					if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && utf8.RuneCountInString(s.workspacePaneName) < 128 {
						s.workspacePaneName += string(r)
					}
				}
			}
		}
		return false
	}
	choices := s.workspacePaneChoices()
	switch key {
	case "\x03":
		s.closeWorkspacePane()
	case "\x1b":
		restoreFocusedPTY := s.workspacePaneStep == "detach-confirm" && s.workspacePaneRestoreFocused
		restoreAttachKey := s.workspacePaneRestoreAttachKey
		if restoreFocusedPTY && restoreAttachKey == "" {
			// Keep Esc restoration tied to the Session that opened the modal;
			// closing through Project focus must not synthesize a navigation replay.
			restoreAttachKey = s.effectiveAttachKey()
		}
		s.workspacePaneErr = ""
		s.workspacePaneIndex = 0
		switch s.workspacePaneStep {
		case "project-hosts-create":
			s.workspacePaneStep = "project-create"
		case "existing":
			s.workspacePaneStep = "source"
			s.workspacePaneQuery = ""
		case "source":
			s.workspacePaneStep = "placement"
		case "move-target":
			s.workspacePaneStep = "move-placement"
		case "move-confirm":
			if s.workspacePaneIntent.placement == ducklord.PlaceNewTab {
				s.workspacePaneStep = "move-placement"
			} else {
				s.workspacePaneStep = "move-target"
			}
		case "existing-move-confirm":
			if s.workspacePaneIntent.moveConfirmationBackStep == "drop-placement" {
				s.workspacePaneStep = "drop-placement"
			} else {
				s.workspacePaneStep = "existing"
			}
		default:
			s.closeWorkspacePane()
		}
		if restoreFocusedPTY {
			s.beginFocusedDefaultDetachRestore(restoreAttachKey)
		}
	case "\x1b[B":
		if len(choices) > 0 {
			s.workspacePaneIndex = min(len(choices)-1, s.workspacePaneIndex+1)
		}
	case "\x1b[A":
		s.workspacePaneIndex = max(0, s.workspacePaneIndex-1)
	case "j", "k":
		if s.workspacePaneStep != "existing" {
			if key == "j" {
				if len(choices) > 0 {
					s.workspacePaneIndex = min(len(choices)-1, s.workspacePaneIndex+1)
				}
			} else {
				s.workspacePaneIndex = max(0, s.workspacePaneIndex-1)
			}
			return false
		}
		fallthrough
	case "\x7f", "\b":
		if key == "\x7f" || key == "\b" {
			runes := []rune(s.workspacePaneQuery)
			if len(runes) > 0 {
				s.workspacePaneQuery = string(runes[:len(runes)-1])
			}
		} else {
			s.workspacePaneQuery += key
		}
		s.workspacePaneIndex = 0
	case "\r":
		if len(choices) == 0 {
			return false
		}
		index := max(0, min(s.workspacePaneIndex, len(choices)-1))
		switch s.workspacePaneStep {
		case "project-hosts-create", "project-hosts-edit":
			hosts := s.workspaceProjectHostOptions()
			if index == len(hosts) {
				if err := s.commitWorkspaceProjectHosts(); err != nil {
					s.workspacePaneErr = sanitizeTerminalText(err.Error())
				}
				return false
			}
			host := hosts[index]
			if i := slices.Index(s.workspacePaneHosts, host); i >= 0 {
				s.workspacePaneHosts = slices.Delete(s.workspacePaneHosts, i, i+1)
			} else {
				s.workspacePaneHosts = append(s.workspacePaneHosts, host)
			}
			s.workspacePaneErr = ""
		case "move-placement":
			s.workspacePaneIntent.placement = []ducklord.PanePlacement{ducklord.PlaceNewTab, ducklord.PlaceVertical, ducklord.PlaceHorizontal}[index]
			s.workspacePaneIndex, s.workspacePaneErr = 0, ""
			if s.workspacePaneIntent.placement == ducklord.PlaceNewTab {
				s.workspacePaneIntent.targetID = ""
				s.workspacePaneStep = "move-confirm"
				s.workspacePaneIndex = 1
			} else {
				s.workspacePaneStep = "move-target"
			}
		case "move-target":
			s.workspacePaneIntent.targetID = s.workspaceMoveTargets()[index]
			s.workspacePaneStep, s.workspacePaneIndex = "move-confirm", 1
		case "move-confirm", "detach-confirm", "existing-move-confirm":
			if index == 1 {
				s.closeWorkspacePane()
				return false
			}
			if err := s.commitWorkspacePaneAction(s.workspacePaneStep == "detach-confirm"); err != nil {
				s.workspacePaneErr = sanitizeTerminalText(err.Error())
				return false
			}
			s.closeWorkspacePane()
		case "project-delete-confirm":
			if index == 1 {
				s.closeWorkspacePane()
				return false
			}
			if err := s.commitWorkspaceProjectDelete(); err != nil {
				s.workspacePaneErr = sanitizeTerminalText(err.Error())
				return false
			}
			s.closeWorkspacePane()
		case "placement":
			s.workspacePaneIntent.placement = []ducklord.PanePlacement{ducklord.PlaceNewTab, ducklord.PlaceVertical, ducklord.PlaceHorizontal}[index]
			if s.workspacePaneIntent.placement != ducklord.PlaceNewTab && s.workspacePaneIntent.targetID == "" {
				s.workspacePaneErr = "choose a Project with a Session pane to split"
				return false
			}
			s.workspacePaneStep, s.workspacePaneIndex, s.workspacePaneErr = "source", 0, ""
		case "drop-placement":
			s.workspacePaneIntent.moveConfirmationBackStep = "drop-placement"
			s.workspacePaneIntent.placement = []ducklord.PanePlacement{ducklord.PlaceNewTab, ducklord.PlaceVertical, ducklord.PlaceHorizontal}[index]
			if err := s.placeDroppedWorkspacePane(); err != nil {
				s.workspacePaneErr = sanitizeTerminalText(err.Error())
				return false
			}
			if s.workspacePaneStep == "drop-placement" {
				s.closeWorkspacePane()
			}
		case "source":
			if index == 0 {
				intent := s.workspacePaneIntent
				s.closeWorkspacePane()
				s.workspaceNewSessionIntent = &intent
				return true
			}
			s.workspacePaneStep, s.workspacePaneIndex, s.workspacePaneErr, s.workspacePaneQuery = "existing", 0, "", ""
		case "existing":
			candidate := s.workspacePaneCandidates()[index]
			identity, _ := ducklord.IdentityFromSession(candidate)
			if sourceID := s.projectPaneForSession(identity); sourceID != "" {
				if s.workspacePaneIntent.placement != ducklord.PlaceNewTab && sourceID == s.workspacePaneIntent.targetID {
					s.workspacePaneErr = "select a different pane to split"
					return false
				}
				s.workspacePaneSourceID, s.workspacePaneIdentity, s.workspacePaneCandidate = sourceID, identity, candidate
				s.workspacePaneIntent.moveConfirmationBackStep = "existing"
				s.workspacePaneStep, s.workspacePaneIndex, s.workspacePaneErr = "existing-move-confirm", 1, ""
				return false
			}
			if err := s.placeWorkspacePane(s.workspacePaneIntent, candidate); err != nil {
				s.workspacePaneErr = sanitizeTerminalText(err.Error())
				return false
			}
			s.closeWorkspacePane()
		}
	default:
		if s.workspacePaneStep == "existing" && utf8.Valid(input) && !strings.ContainsRune(key, '\x1b') {
			for _, r := range key {
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && utf8.RuneCountInString(s.workspacePaneQuery) < 128 {
					s.workspacePaneQuery += string(r)
				}
			}
			s.workspacePaneIndex = 0
		}
	}
	return false
}

func (s *tuiState) handleNotesInput(input []byte) bool {
	if s.notesPickerMode != "" {
		return s.handleNotesPicker(input)
	}
	if s.helpMode {
		return false
	}
	if s.notesFormActive {
		key := string(input)
		switch key {
		case "\x1b", "\x03":
			s.notesFormActive, s.outputErr = false, ""
		case "\t":
			s.notesFormSetField((s.notesFormField + 1) % 2)
		case "\x13":
			s.saveNotesForm()
		case "\x0b":
			s.notesFormSetValue("")
			s.notesFormCursor = 0
		case "\x7f", "\b":
			r := []rune(s.notesFormValue())
			if s.notesFormCursor > 0 {
				cursor := min(s.notesFormCursor, len(r))
				s.notesFormSetValue(string(append(r[:cursor-1], r[cursor:]...)))
				s.notesFormCursor = cursor - 1
			}
		case "\x1b[3~":
			r := []rune(s.notesFormValue())
			cursor := min(max(0, s.notesFormCursor), len(r))
			if cursor < len(r) {
				s.notesFormSetValue(string(append(r[:cursor], r[cursor+1:]...)))
				s.notesFormCursor = cursor
			}
		case "\x1b[D":
			s.notesFormCursor = max(0, s.notesFormCursor-1)
		case "\x1b[C":
			s.notesFormCursor = min(s.notesFormValueLen(), s.notesFormCursor+1)
		case "\r":
			if s.notesFormField == 0 {
				s.notesFormSetField(1)
			} else {
				s.saveNotesForm()
			}
		default:
			if utf8.Valid(input) {
				for _, r := range string(input) {
					if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
						value := []rune(s.notesFormValue())
						cursor := min(max(0, s.notesFormCursor), len(value))
						value = append(value, 0)
						copy(value[cursor+1:], value[cursor:])
						value[cursor] = r
						s.notesFormSetValue(string(value))
						s.notesFormCursor = cursor + 1
					}
				}
			}
		}
		return true
	}
	if s.notesSearchActive {
		key := string(input)
		if key == "\x1b" {
			s.notesSearchActive = false
			s.notesQuery = ""
			s.loadNotesScope()
			return true
		}
		if key == "\r" {
			s.notesSearchActive = false
			s.loadNotesScope()
			return true
		}
		if key == "\x7f" {
			r := []rune(s.notesQuery)
			if len(r) > 0 {
				s.notesQuery = string(r[:len(r)-1])
			}
			s.loadNotesScope()
			return true
		}
		if utf8.Valid(input) {
			for _, r := range string(input) {
				if !unicode.IsControl(r) && utf8.RuneCountInString(s.notesQuery) < 128 {
					s.notesQuery += string(r)
				}
			}
			s.loadNotesScope()
			return true
		}
	}
	if string(input) == "\x1b" || string(input) == "\x03" {
		s.closeNotesModal()
		return true
	}
	switch string(input) {
	case "/":
		s.notesSearchActive = true
		return true
	case "g", "p", "s":
		switch string(input) {
		case "g":
			s.notesScope = ducklord.NotesGlobal
		case "p":
			s.notesScope = ducklord.NotesProject
		case "s":
			s.notesScope = ducklord.NotesSession
		}
		s.loadNotesScope()
		return true
	case "j", "\x1b[B":
		if s.workspacePaneIndex < len(s.notesEntries)-1 {
			s.workspacePaneIndex++
		}
	case "k", "\x1b[A":
		if s.workspacePaneIndex > 0 {
			s.workspacePaneIndex--
		}
	case "h", "l", "\x1b[D", "\x1b[C":
		// Horizontal movement changes notebook level; vertical movement remains
		// entry selection. This keeps the hierarchy usable without a second menu.
		left := string(input) == "h" || string(input) == "\x1b[D"
		if left {
			if s.notesScope == ducklord.NotesSession {
				ps := s.activity().ProjectLayout.ProjectsFor(s.notesSessionIdentity)
				if len(ps) > 1 {
					return s.openNotesPicker("project", ps)
				}
				if len(ps) == 1 {
					s.notesProjectID = ps[0]
				}
			}
			s.notesScope = previousNotesScope(s.notesScope)
		} else {
			if s.notesScope == ducklord.NotesGlobal {
				var ps []string
				for _, p := range s.activity().ProjectLayout.Projects {
					ps = append(ps, p.ID)
				}
				if len(ps) > 1 {
					return s.openNotesPicker("project", ps)
				}
			}
			if s.notesScope == ducklord.NotesProject {
				var ss []string
				sessions := s.activity().ProjectLayout.SessionsForProject(s.notesProjectID)
				for _, id := range sessions {
					ss = append(ss, id.Key())
				}
				// A Project with one Session still has a useful child scope;
				// selecting right from its Project notebook should open it.
				if len(ss) == 0 && len(sessions) == 1 {
					ss = append(ss, sessions[0].Key())
				}
				if len(ss) > 0 {
					return s.openNotesPicker("session", ss)
				}
			}
			s.notesScope = nextNotesScope(s.notesScope)
		}
		s.workspacePaneIndex = 0
		s.loadNotesScope()
	case "\r":
		if s.workspacePaneIndex < 0 || s.workspacePaneIndex >= len(s.notesEntries) {
			s.outputErr = "Notes notebook is empty"
			return true
		}
		s.outputErr = emitNotesClipboard(os.Stdout, s.notesEntries[s.workspacePaneIndex].Body)
	case "a":
		s.outputErr = ""
		s.notesFormActive, s.notesFormIndex, s.notesFormRawIndex, s.notesFormField, s.notesFormCursor = true, -1, -1, 0, 0
		s.notesFormTitle, s.notesFormBody = "", ""
	case "e":
		s.outputErr = ""
		index := s.workspacePaneIndex
		if index < 0 || index >= len(s.notesEntries) {
			index = -1
		}
		if index < 0 {
			s.notesFormActive, s.notesFormIndex, s.notesFormRawIndex, s.notesFormField, s.notesFormCursor = true, -1, -1, 0, 0
			s.notesFormTitle, s.notesFormBody = "", ""
			return true
		}
		if index < len(s.notesEntryScopes) && s.notesEntryScopes[index] != s.notesScope {
			s.outputErr = "Search result belongs to " + s.notesEntryOrigins[index] + "; clear search before editing"
			return true
		}
		s.notesFormActive, s.notesFormIndex, s.notesFormField = true, index, 0
		s.notesFormRawIndex = s.notesEntryIndexes[index]
		s.notesFormTitle, s.notesFormBody = s.notesEntries[index].Title, s.notesEntries[index].Body
		s.notesFormCursor = s.notesFormValueLen()
	case "E":
		s.outputErr = ""
		s.editNotesNotebook()
	default:
		return false
	}
	return true
}

func (s *tuiState) handleNotesPicker(input []byte) bool {
	key := string(input)
	if key == "\x1b" || key == "\x03" {
		s.notesPickerMode = ""
		s.notesPickerChoices = nil
		s.notesPickerLabels = nil
		return true
	}
	if key == "j" || key == "\x1b[B" {
		if s.notesPickerIndex < len(s.notesPickerChoices)-1 {
			s.notesPickerIndex++
		}
		return true
	}
	if key == "k" || key == "\x1b[A" {
		if s.notesPickerIndex > 0 {
			s.notesPickerIndex--
		}
		return true
	}
	if key != "\r" || len(s.notesPickerChoices) == 0 {
		return true
	}
	choice := s.notesPickerChoices[s.notesPickerIndex]
	mode := s.notesPickerMode
	s.notesPickerMode = ""
	s.notesPickerChoices = nil
	s.notesPickerLabels = nil
	if mode == "project" {
		s.notesProjectID = choice
		s.notesScope = ducklord.NotesProject
	} else {
		var found ducklord.SessionIdentity
		for _, identity := range s.activity().ProjectLayout.SessionsForProject(s.notesProjectID) {
			if identity.Key() == choice {
				found = identity
				break
			}
		}
		if found.Key() == "" {
			s.outputErr = "selected Session is no longer available"
			return true
		}
		s.notesSessionIdentity = found
		s.notesSessionID = found.SessionID
		s.notesScope = ducklord.NotesSession
	}
	s.loadNotesScope()
	return true
}

func (s *tuiState) openNotesPicker(mode string, choices []string) bool {
	if len(choices) == 0 {
		return false
	}
	if len(choices) == 1 {
		s.notesPickerMode = mode
		s.notesPickerChoices = choices
		s.notesPickerLabels = s.notesPickerDisplayLabels(mode, choices)
		s.notesPickerIndex = 0
		return s.handleNotesPicker([]byte("\r"))
	}
	s.notesPickerMode = mode
	s.notesPickerChoices = append([]string(nil), choices...)
	s.notesPickerLabels = s.notesPickerDisplayLabels(mode, choices)
	s.notesPickerIndex = 0
	return true
}

func (s *tuiState) sessionName(identity ducklord.SessionIdentity) string {
	for _, session := range s.sessions {
		candidate, ok := ducklord.IdentityFromSession(session)
		if ok && candidate == identity && strings.TrimSpace(session.Name) != "" {
			return session.Name
		}
	}
	return identity.SessionID
}

func (s *tuiState) notesSessionName() string {
	if s.notesSessionIdentity.Key() == "" {
		return s.notesSessionID
	}
	return s.sessionName(s.notesSessionIdentity)
}

func (s *tuiState) notesPickerDisplayLabels(mode string, choices []string) []string {
	if mode != "session" {
		return nil
	}
	labels := make([]string, len(choices))
	for i, choice := range choices {
		var identity ducklord.SessionIdentity
		if err := identity.UnmarshalText([]byte(choice)); err == nil {
			labels[i] = s.sessionName(identity)
		}
		if labels[i] == "" {
			labels[i] = choice
		}
	}
	return labels
}

func (s *tuiState) saveNotesForm() {
	title := strings.TrimSpace(s.notesFormTitle)
	if title == "" {
		s.outputErr = "note title is required"
		return
	}
	root, identity := filepath.Dir(s.cfgPath), s.notesStorageIdentity()
	if s.notesScope == ducklord.NotesSession && identity == "" {
		s.outputErr = "Session Notes unavailable: no valid focused session"
		return
	}
	entries, revision, err := ducklord.ScopedNotesSnapshot(root, s.notesScope, identity)
	if err != nil {
		s.outputErr = "read note revision: " + err.Error()
		return
	}
	note := ducklord.NoteEntry{Title: title, Body: s.notesFormBody}
	if s.notesFormIndex < 0 {
		_, err = ducklord.AppendScopedNoteIfRevision(root, s.notesScope, identity, revision, note)
	} else if s.notesFormRawIndex >= 0 && s.notesFormRawIndex < len(entries) {
		_, err = ducklord.ReplaceScopedNoteIfRevision(root, s.notesScope, identity, s.notesFormRawIndex, &revision, note)
	} else {
		err = fmt.Errorf("note changed while editing; reopen and retry")
	}
	if err != nil {
		s.outputErr = "save note: " + err.Error()
		return
	}
	formIndex := s.notesFormIndex
	s.notesFormActive, s.outputErr = false, ""
	s.loadNotesScope()
	if formIndex < 0 {
		s.workspacePaneIndex = max(0, len(s.notesEntries)-1)
	} else {
		s.workspacePaneIndex = min(formIndex, max(0, len(s.notesEntries)-1))
	}
}

func (s *tuiState) loadNotesScope() {
	// Queries search the selected book first, then descendants. Each result
	// carries its source so copy remains safe even when books share titles.
	if strings.TrimSpace(s.notesQuery) != "" && (s.notesScope == ducklord.NotesGlobal || s.notesScope == ducklord.NotesProject) {
		s.loadNotesHierarchy()
		return
	}
	identity := s.notesProjectID
	if s.notesScope == ducklord.NotesSession {
		identity = s.notesStorageIdentity()
		if identity == "" {
			s.outputErr = "Session Notes unavailable: no valid focused session"
			s.notesEntries = nil
			return
		}
	}
	entries, err := ducklord.LoadScopedNotes(filepath.Dir(s.cfgPath), s.notesScope, func() string {
		if s.notesScope == ducklord.NotesGlobal {
			return ""
		}
		return identity
	}())
	if err != nil {
		s.outputErr = "load Notes: " + err.Error()
		return
	}
	filtered := entries
	indexes := make([]int, 0, len(entries))
	for i := range entries {
		indexes = append(indexes, i)
	}
	if q := strings.ToLower(strings.TrimSpace(s.notesQuery)); q != "" {
		filtered = nil
		indexes = nil
		for i, e := range entries {
			if strings.Contains(strings.ToLower(e.Title), q) || strings.Contains(strings.ToLower(e.Body), q) {
				filtered = append(filtered, e)
				indexes = append(indexes, i)
			}
		}
	}
	s.notesEntries = filtered
	s.notesEntryIndexes = indexes
	s.notesEntryScopes = make([]ducklord.NotesScope, len(filtered))
	s.notesEntryIdentities = make([]string, len(filtered))
	s.notesEntryOrigins = make([]string, len(filtered))
	for i := range filtered {
		s.notesEntryScopes[i] = s.notesScope
		s.notesEntryIdentities[i] = identity
		s.notesEntryOrigins[i] = string(s.notesScope)
	}
	if s.workspacePaneIndex >= len(filtered) {
		s.workspacePaneIndex = max(0, len(filtered)-1)
	}
	if s.notesQuery != "" && len(filtered) == 0 {
		s.outputErr = "No Notes match \"" + s.notesQuery + "\""
	} else if s.notesScope == ducklord.NotesSession {
		s.outputErr = "Session notebook"
	}
}

func (s *tuiState) loadNotesHierarchy() {
	q := strings.ToLower(strings.TrimSpace(s.notesQuery))
	root := filepath.Dir(s.cfgPath)
	var entries []ducklord.NoteEntry
	var indexes []int
	var scopes []ducklord.NotesScope
	var ids, origins []string
	add := func(scope ducklord.NotesScope, id, origin string, list []ducklord.NoteEntry) {
		for i, e := range list {
			if strings.Contains(strings.ToLower(e.Title), q) || strings.Contains(strings.ToLower(e.Body), q) {
				entries = append(entries, e)
				indexes = append(indexes, i)
				scopes = append(scopes, scope)
				ids = append(ids, id)
				origins = append(origins, origin)
			}
		}
	}
	load := func(scope ducklord.NotesScope, id, origin string) []ducklord.NoteEntry {
		v, err := ducklord.LoadScopedNotes(root, scope, id)
		if err != nil {
			return nil
		}
		add(scope, id, origin, v)
		return v
	}
	if s.notesScope == ducklord.NotesGlobal {
		load(ducklord.NotesGlobal, "", "Global")
	}
	projects := []string{s.notesProjectID}
	if s.notesScope == ducklord.NotesGlobal {
		projects = nil
		for _, p := range s.activity().ProjectLayout.Projects {
			projects = append(projects, p.ID)
		}
	}
	for _, pid := range projects {
		load(ducklord.NotesProject, pid, "Project "+pid)
		for _, ident := range s.activity().ProjectLayout.SessionsForProject(pid) {
			sid, err := ducklord.SessionNotesIdentity(ident)
			if err == nil {
				load(ducklord.NotesSession, sid, "Project "+pid+" › Session "+s.sessionName(ident))
			}
		}
	}
	s.notesEntries, s.notesEntryIndexes, s.notesEntryScopes, s.notesEntryIdentities, s.notesEntryOrigins = entries, indexes, scopes, ids, origins
	if s.workspacePaneIndex >= len(entries) {
		s.workspacePaneIndex = max(0, len(entries)-1)
	}
	if len(entries) == 0 {
		s.outputErr = "No Notes match \"" + s.notesQuery + "\""
	} else {
		s.outputErr = ""
	}
}

func (s *tuiState) syncCurrentNotes() bool {
	nav, err := s.workspaceNavigation()
	if err != nil || s.hostScoped {
		return false
	}
	projectID := nav.CurrentProjectID()
	if projectID == "" || (!s.notesFormActive && !s.workspacePaneMode && !s.activity().ProjectLayout.IsNotePane(projectID, nav.CurrentPaneID())) {
		return false
	}
	if identity, ok := s.activity().ProjectLayout.PaneSession(projectID, nav.CurrentPaneID()); ok {
		s.notesSessionID = identity.SessionID
		s.notesSessionIdentity = identity
	} else {
		// A Notes pane has no focused session. Retain an identity only when it
		// was captured from the pane recorded as the return destination by
		// prefix+o; this keeps normal entry usable without carrying stale state.
		captured := false
		if s.notesProjectID == projectID && s.notesPreviousPaneID != "" {
			previous, previousOK := s.activity().ProjectLayout.PaneSession(projectID, s.notesPreviousPaneID)
			captured = previousOK && previous == s.notesSessionIdentity
		}
		if captured {
			return s.syncCurrentNotesProject(projectID)
		}
		s.notesSessionID = ""
		s.notesSessionIdentity = ducklord.SessionIdentity{}
	}
	return s.syncCurrentNotesProject(projectID)
}

func (s *tuiState) syncCurrentNotesProject(projectID string) bool {
	if s.notesProjectID == projectID && s.notesScope == ducklord.NotesProject {
		return true
	}
	entries, loadErr := ducklord.LoadNotes(filepath.Dir(s.cfgPath), projectID)
	if loadErr != nil {
		s.outputErr = "load Notes: " + loadErr.Error()
		return true
	}
	s.notesProjectID, s.notesEntries, s.workspacePaneIndex = projectID, entries, 0
	s.notesScope = ducklord.NotesProject
	s.loadNotesScope()
	return true
}

func notesTerminalTransition(out io.Writer, enter bool) {
	if enter {
		fmt.Fprint(out, "\033[?1049h\033[?25l\033[?1002h\033[?1006h")
		return
	}
	fmt.Fprint(out, "\033[?1000l\033[?1002l\033[?1003l\033[?1006l\033[?25h\033[?1049l")
}

var (
	notesMakeRaw = makeRaw
	notesRestore = restore
)

func (s *tuiState) editNotesNotebook() {
	s.outputErr = ""
	if s.notesScope != ducklord.NotesGlobal && s.notesScope != ducklord.NotesProject && s.notesScope != ducklord.NotesSession {
		s.notesScope = ducklord.NotesProject
	}
	if s.notesScope == ducklord.NotesSession && s.notesSessionIdentity.Key() == "" {
		s.outputErr = "Session Notes unavailable: no valid focused session"
		return
	}
	root := filepath.Dir(s.cfgPath)
	identity := s.notesStorageIdentity()
	if s.notesScope == ducklord.NotesSession && identity == "" {
		s.outputErr = "Session Notes unavailable: invalid focused session"
		return
	}
	unlock, err := ducklord.LockScopedNotes(root, s.notesScope, identity)
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	f, err := ducklord.OpenScopedNotes(root, s.notesScope, identity)
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	defer f.Close()
	s.runNotesEditor("/proc/self/fd/3", f)
	unlock()
	unlock = nil
	if s.outputErr != "" {
		return
	}
	_, loadErr := ducklord.LoadScopedNotes(root, s.notesScope, identity)
	if loadErr != nil {
		s.outputErr = "load Notes: " + loadErr.Error()
		return
	}
	s.loadNotesScope()
}

func (s *tuiState) editNotesEntry(index int) {
	s.outputErr = ""
	if s.notesScope != ducklord.NotesGlobal && s.notesScope != ducklord.NotesProject && s.notesScope != ducklord.NotesSession {
		s.notesScope = ducklord.NotesProject
	}
	root := filepath.Dir(s.cfgPath)
	if s.notesScope == ducklord.NotesSession && s.notesSessionIdentity.Key() == "" {
		s.outputErr = "Session Notes unavailable: no valid focused session"
		return
	}
	var selected ducklord.NoteEntry
	rawIndex := index
	if index >= 0 {
		if index >= len(s.notesEntries) {
			s.outputErr = "note changed while editing; reopen and retry"
			return
		}
		selected = s.notesEntries[index]
		if index < len(s.notesEntryIndexes) {
			rawIndex = s.notesEntryIndexes[index]
		}
	}
	identity := s.notesStorageIdentity()
	if s.notesScope == ducklord.NotesSession && identity == "" {
		s.outputErr = "Session Notes unavailable: invalid focused session"
		return
	}
	path, err := ducklord.PrepareScopedNotes(root, s.notesScope, identity)
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	diskEntries, revision, err := ducklord.ScopedNotesSnapshot(root, s.notesScope, identity)
	if err != nil {
		s.outputErr = "read note revision: " + err.Error()
		return
	}
	if index >= 0 && rawIndex >= len(diskEntries) {
		s.loadNotesScope()
		s.workspacePaneIndex = min(index, max(0, len(s.notesEntries)-1))
		s.outputErr = "note changed while editing; reopen and retry"
		return
	}
	if index >= 0 && (diskEntries[rawIndex].Title != selected.Title || diskEntries[rawIndex].Body != selected.Body) {
		s.loadNotesScope()
		s.outputErr = "note changed while editing; reopen and retry"
		return
	}
	dir := filepath.Dir(path)
	initial := "## \n"
	if index >= 0 {
		initial = ducklord.FormatNotes([]ducklord.NoteEntry{diskEntries[rawIndex]})
	}
	tmp, err := os.CreateTemp(dir, ".note-edit-*.md")
	if err != nil {
		s.outputErr = "prepare note editor: " + err.Error()
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		s.outputErr = "prepare note editor: " + err.Error()
		return
	}
	if _, err := tmp.WriteString(initial); err != nil {
		_ = tmp.Close()
		s.outputErr = "prepare note editor: " + err.Error()
		return
	}
	if err := tmp.Close(); err != nil {
		s.outputErr = "prepare note editor: " + err.Error()
		return
	}
	s.runNotesEditor(tmpPath, nil)
	if s.outputErr != "" {
		return
	}
	b, err := readNotesEditorFile(tmpPath)
	if err != nil {
		s.outputErr = "read edited note: " + err.Error()
		return
	}
	edited := ducklord.ParseNotes(string(b))
	if len(edited) != 1 || strings.TrimSpace(edited[0].Title) == "" {
		s.outputErr = "note must contain exactly one ## title"
		return
	}
	if index < 0 {
		_, err := ducklord.AppendScopedNoteIfRevision(root, s.notesScope, identity, revision, edited[0])
		if err != nil {
			s.outputErr = "save note: " + err.Error()
			return
		}
		s.loadNotesScope()
		s.workspacePaneIndex = max(0, len(s.notesEntries)-1)
		return
	}
	_, err = ducklord.ReplaceScopedNoteIfRevision(root, s.notesScope, identity, rawIndex, &revision, edited[0])
	if err != nil {
		s.outputErr = "save note: " + err.Error()
		return
	}
	s.loadNotesScope()
	s.workspacePaneIndex = min(index, len(s.notesEntries)-1)
}

func (s *tuiState) notesStorageIdentity() string {
	if s.notesScope != ducklord.NotesSession {
		return s.notesProjectID
	}
	identity := s.notesSessionIdentity
	if identity.InstanceID == "" {
		return ""
	}
	encoded, err := ducklord.SessionNotesIdentity(identity)
	if err != nil {
		return ""
	}
	return encoded
}

func (s *tuiState) runNotesEditor(path string, extra *os.File) {
	if s == nil {
		return
	}
	ed := strings.TrimSpace(os.Getenv("EDITOR"))
	if ed == "" {
		ed = "vim"
	}
	args, err := splitEditorArgs(ed)
	if err != nil || len(args) == 0 {
		s.outputErr = "invalid EDITOR"
		return
	}
	// Leave raw mode and the alternate screen while the editor owns the tty.
	// Re-enter both before returning to the event loop.
	// In the normal TUI path the original cooked state was captured before raw
	// mode was enabled. Reuse it: calling makeRaw here would capture raw mode.
	cooked := s.terminalCookedState
	if cooked == nil {
		// Keep direct, non-interactive callers and unit tests usable.
		var rawErr error
		cooked, rawErr = notesMakeRaw()
		if rawErr != nil {
			s.outputErr = "prepare editor terminal: " + rawErr.Error()
			return
		}
	}
	notesRestore(cooked)
	notesTerminalTransition(os.Stdout, false)
	defer func() {
		if _, err := notesMakeRaw(); err != nil {
			notesTerminalTransition(os.Stdout, false)
			s.outputErr = "restore editor terminal: " + err.Error()
			return
		}
		notesTerminalTransition(os.Stdout, true)
		// The editor may have changed the terminal contents; force the next
		// frame to be painted in full after returning to the alternate screen.
		s.frameOutput = frameOutput{}
		s.render(os.Stdout)
	}()
	cmd := exec.Command(args[0], append(args[1:], path)...)
	if extra != nil {
		cmd.ExtraFiles = []*os.File{extra}
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		s.outputErr = "editor: " + err.Error()
		return
	}
}

func readNotesEditorFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	return io.ReadAll(f)
}

// splitEditorArgs supports the common quoted EDITOR forms without invoking a shell.
func splitEditorArgs(command string) ([]string, error) {
	var args []string
	var b strings.Builder
	var quote rune
	escaped, have := false, false
	flush := func() {
		if have {
			args = append(args, b.String())
			b.Reset()
			have = false
		}
	}
	for _, r := range command {
		if escaped {
			b.WriteRune(r)
			escaped, have = false, true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped, have = true, true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			have = true
			continue
		}
		switch r {
		case '\'', '"':
			quote, have = r, true
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			b.WriteRune(r)
			have = true
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated quote or escape")
	}
	flush()
	return args, nil
}
