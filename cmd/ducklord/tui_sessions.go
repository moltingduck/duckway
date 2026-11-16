package main

import (
	"context"
	"sort"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) refreshSessions(ctx context.Context) {
	oldKey := s.currentKey()
	// Keep the selected PTY identity stable while the session slice is replaced.
	s.selectedKey = oldKey
	var all []ducklord.RemoteSession
	// A successful Sessions call is a full inventory for that Host. Keep
	// candidates separate from transient failures, which must not retire panes.
	missingCandidates := make(map[ducklord.SessionIdentity]bool)
	authoritative := make(map[ducklord.SessionIdentity]bool)
	uncertain := make(map[ducklord.SessionIdentity]bool)
	for _, session := range s.sessions {
		if s.disconnectedHosts[session.Client] {
			all = append(all, session)
			if identity, ok := ducklord.IdentityFromSession(session); ok {
				uncertain[identity] = true
			}
		}
	}
	for _, c := range s.cfg.Clients {
		if s.disconnectedHosts[c.Name] {
			continue
		}
		sessions, err := s.runner.Sessions(ctx, c, 8)
		if err != nil {
			retained := false
			for _, prior := range s.sessions {
				if prior.Client != c.Name || prior.Name == "(offline)" {
					continue
				}
				if identity, ok := ducklord.IdentityFromSession(prior); ok {
					uncertain[identity] = true
				}
				prior.Status = "disconnected"
				prior.Error = err.Error()
				prior.Updated = false
				all = append(all, prior)
				retained = true
			}
			if !retained {
				all = append(all, ducklord.RemoteSession{Client: c.Name, Group: c.Group, Name: "(offline)", Status: "error", Error: err.Error()})
			}
			continue
		}
		for _, prior := range s.sessions {
			if prior.Client == c.Name {
				if identity, ok := ducklord.IdentityFromSession(prior); ok {
					missingCandidates[identity] = true
				}
			}
		}
		for _, current := range sessions {
			if identity, ok := ducklord.IdentityFromSession(current); ok && current.Status != string(model.StatusStopped) {
				authoritative[identity] = true
			}
		}
		all = append(all, sessions...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Group != all[j].Group {
			return all[i].Group < all[j].Group
		}
		if all[i].Client != all[j].Client {
			return all[i].Client < all[j].Client
		}
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		return sessionKey(all[i]) < sessionKey(all[j])
	})
	activityChanged := false
	for identity := range missingCandidates {
		if authoritative[identity] || uncertain[identity] {
			continue
		}
		if len(s.activity().ProjectLayout.ProjectsFor(identity)) != 0 {
			s.activity().ProjectLayout.Destroy(identity)
			activityChanged = true
		}
	}
	for i := range all {
		key := sessionKey(all[i])
		if all[i].TailHash != "" && s.hashes[key] != "" && s.hashes[key] != all[i].TailHash {
			all[i].Updated = true
		}
		if all[i].TailHash != "" {
			s.hashes[key] = all[i].TailHash
		}
		all[i].LastLine = sanitizeTerminalText(all[i].LastLine)
		all[i].Error = sanitizeTerminalText(all[i].Error)
		if all[i].Error != "" {
			// A failed Host query is not an authoritative Session deletion or
			// notification update. Keep the last known Project and unread state.
			continue
		}
		if identity, ok := ducklord.IdentityFromSession(all[i]); ok {
			if all[i].Status == string(model.StatusStopped) {
				if len(s.activity().ProjectLayout.ProjectsFor(identity)) != 0 {
					s.activity().ProjectLayout.Destroy(identity)
					activityChanged = true
				}
			} else if len(s.activity().ProjectLayout.ProjectsFor(identity)) == 0 {
				if err := s.activity().ProjectLayout.Discover(identity); err == nil {
					activityChanged = true
				}
			}
		}
		var changed bool
		all[i].Unread, changed = s.reconcileNotificationState(all[i], s.sessionFreshlyDisplayed(all[i]))
		if !all[i].Unread {
			delete(s.promoted, sessionKey(all[i]))
		}
		activityChanged = activityChanged || changed
	}
	s.sessions = all
	organizationChanged := s.applyOrganizationOrder()
	if activityChanged || organizationChanged {
		if err := s.saveActivityState(); err != nil {
			s.localWarning = "could not save Project and notification state: " + sanitizeTerminalText(err.Error())
		}
	}
	if oldKey != "" {
		for i, sess := range s.sessions {
			if sessionKey(sess) == oldKey {
				s.selected = i
				break
			}
		}
	}
	if s.selected >= len(s.sessions) {
		s.selected = len(s.sessions) - 1
	}
	if s.selected < 0 {
		s.selected = 0
	}
	s.selectedKey = s.currentKey()
	if s.searchMode {
		s.syncSearchSelection()
	}
	if s.workspaceNav != nil && s.workspaceNav.InDetailMode() {
		s.syncDetailSelection()
	}
	s.reconcilePendingWorkspacePlacements()
}

// applySessionUpdate reports whether the update became authoritative. Callers
// must not apply output or lifecycle side effects for a rejected snapshot.
func (s *tuiState) notificationDeltas(identity string, before map[model.NotificationCategory]uint64, known bool,
	current map[model.NotificationCategory]uint64) map[model.NotificationCategory]uint64 {
	if s.notificationObserved == nil {
		s.notificationObserved = make(map[string]map[model.NotificationCategory]uint64)
	}
	observed, seen := s.notificationObserved[identity]
	if observed == nil {
		observed = make(map[model.NotificationCategory]uint64)
		s.notificationObserved[identity] = observed
	}
	deltas := make(map[model.NotificationCategory]uint64)
	for category, value := range current {
		baseline := before[category]
		if !known && !seen {
			baseline = value // First inventory is history, not a new local delivery.
		}
		if observed[category] > baseline {
			baseline = observed[category]
		}
		if value > baseline {
			deltas[category] = value - baseline
		}
		observed[category] = value
	}
	return deltas
}

func (s *tuiState) applySessionUpdate(update ducklord.SessionUpdate) bool {
	if update.Client == "" {
		return false
	}
	if s.cfg != nil {
		if _, configured := s.cfg.Client(update.Client); !configured {
			return false
		}
	}
	previous := s.hostSync[update.Client]
	if update.Generation < previous.Generation || update.Generation == previous.Generation && update.InstanceID == previous.InstanceID && update.Revision < previous.Revision {
		return false
	}
	if update.State == "live" {
		for _, session := range update.Sessions {
			if session.InstanceID != update.InstanceID || session.Client != update.Client {
				s.localWarning = "ignored inconsistent Ducklion session inventory"
				return false
			}
		}
		for client, snapshot := range s.hostSync {
			if client != update.Client && snapshot.State == "live" && snapshot.InstanceID == update.InstanceID && snapshot.Revision > update.Revision {
				return false // another alias has already observed a newer full inventory
			}
		}
	}
	s.hostSync[update.Client] = update
	if s.newSessionMode && s.newSessionDiscovering && s.workspaceNewSessionIntent == nil && s.newSessionClient == update.Client &&
		(update.State != "live" || update.Generation != previous.Generation || update.InstanceID != previous.InstanceID) {
		s.cancelCreateDiscovery()
		s.newSessionStep = "host"
		s.newSessionErr = "host connection changed; choose it again"
	}
	if s.newSessionMode && s.newSessionPathBusy && s.newSessionClient == update.Client &&
		(update.State != "live" || update.Generation != previous.Generation || update.InstanceID != previous.InstanceID) {
		s.cancelPathSuggestions()
		s.newSessionPathSuggestions = nil
		s.newSessionStep = "host"
		s.newSessionErr = "host connection changed; choose it again"
	}
	if update.State != "live" {
		if s.activePTYSession().Client == update.Client {
			s.outputFresh = false
		}
		s.reconcilePendingWorkspacePlacements()
		return true // retain the last authoritative rows while reconnecting
	}
	oldKey := s.currentKey()
	// Sorting must protect the selected PTY's group by identity, not by an index
	// that belongs to the previous session slice.
	s.selectedKey = oldKey
	previousActivity := make(map[string]map[model.NotificationCategory]uint64)
	previousIdentities := make(map[ducklord.SessionIdentity]bool)
	for _, identity := range s.activity().ProjectLayout.SessionsForInstance(update.InstanceID) {
		previousIdentities[identity] = true
	}
	for _, session := range s.sessions {
		if identity, ok := ducklord.IdentityFromSession(session); session.Client == update.Client && ok {
			previousActivity[identity.Key()] = session.ActivitySequences
			if session.InstanceID == update.InstanceID {
				previousIdentities[identity] = true
			}
		}
	}
	all := make([]ducklord.RemoteSession, 0, len(s.sessions)+len(update.Sessions))
	for _, session := range s.sessions {
		if session.Client != update.Client {
			all = append(all, session)
		}
	}
	activityChanged := false
	for _, session := range update.Sessions {
		identity, identityOK := ducklord.IdentityFromSession(session)
		if identityOK && session.Status != string(model.StatusStopped) {
			delete(previousIdentities, identity)
		}
		if identityOK && session.Status != string(model.StatusStopped) && len(s.activity().ProjectLayout.ProjectsFor(identity)) == 0 {
			if err := s.activity().ProjectLayout.Discover(identity); err == nil {
				activityChanged = true
			}
		}
		before, known := previousActivity[identity.Key()]
		var deltas map[model.NotificationCategory]uint64
		if identityOK {
			deltas = s.notificationDeltas(identity.Key(), before, known, session.ActivitySequences)
		}
		if deltas[model.NotificationTaskFailed] > 0 && s.shouldDeliverNotification(session, model.NotificationTaskFailed) {
			s.outputErr = sanitizeTerminalText(session.Name) + ": agent turn failed"
		} else if deltas[model.NotificationTaskCompleted] > 0 && s.shouldDeliverNotification(session, model.NotificationTaskCompleted) {
			s.outputErr = sanitizeTerminalText(session.Name) + ": agent turn completed"
		}
		activeFresh := s.sessionFreshlyDisplayed(session)
		unread, changed := s.reconcileNotificationState(session, activeFresh)
		session.Unread = unread
		if deltas[model.NotificationApprovalRequired] > 0 && s.shouldDeliverNotification(session, model.NotificationApprovalRequired) {
			s.approvalNotifyPending = true // the run loop opens the approvals panel when it can
		}
		for category, delta := range deltas {
			if !s.shouldDeliverNotification(session, category) {
				continue
			}
			for sequence := uint64(0); sequence < min(delta, uint64(1024)); sequence++ {
				s.deliverNotification(session, category)
			}
			if delta > 1024 {
				s.localWarning = "notification burst exceeded local delivery capacity; check unread Sessions"
			}
		}
		if !unread {
			delete(s.promoted, sessionKey(session))
		} else {
			for category, delta := range deltas {
				if delta > 0 && s.shouldDeliverNotification(session, category) {
					if s.promoted == nil {
						s.promoted = make(map[string]bool)
					}
					s.promoted[sessionKey(session)] = true
				}
			}
		}
		activityChanged = activityChanged || changed
		if session.SessionID == update.ChangedSessionID && sessionKey(session) != oldKey && !activeFresh {
			session.Updated = true
		}
		all = append(all, session)
	}
	// A live SessionUpdate contains the host's full authoritative inventory.
	// Reconnecting/offline updates are handled above and must never prune panes.
	for identity := range previousIdentities {
		for key := range s.promoted {
			if strings.HasSuffix(key, "/"+identity.Key()) {
				delete(s.promoted, key)
			}
		}
		if len(s.activity().ProjectLayout.ProjectsFor(identity)) != 0 {
			s.activity().ProjectLayout.Destroy(identity)
			activityChanged = true
		}
	}
	if activityChanged {
		if err := s.saveActivityState(); err != nil {
			s.localWarning = "could not save Project and notification state: " + sanitizeTerminalText(err.Error())
		}
	}
	sortRemoteSessions(all)
	s.sessions = all
	if s.applyOrganizationOrder() {
		_ = s.saveActivityState()
	}
	s.restoreSelection(oldKey)
	if s.searchMode {
		s.syncSearchSelection()
	}
	if s.workspaceNav != nil && s.workspaceNav.InDetailMode() {
		s.syncDetailSelection()
	}
	s.reconcilePendingWorkspacePlacements()
	if attachedKey := s.effectiveAttachKey(); attachedKey != "" {
		activeExists := false
		for _, session := range s.sessions {
			if sessionKey(session) == attachedKey {
				activeExists = true
				break
			}
		}
		if !activeExists {
			s.activeAttachKey = ""
			s.activeAttachFresh = false
			s.pendingAttachKey = ""
			s.focused = false
			s.outputFresh = false
			s.outputErr = "active session was removed remotely"
		}
	}
	return true
}

// Invalidate both an opening request and an established writer when inventory
// removes the active Session. A stale completion must not leave input blocked.
func cancelRemovedPTYControl(s *tuiState, previousActive string, control **ducklord.ControlSession, openCancel *context.CancelFunc, controlID *int) bool {
	if previousActive == "" || s.effectiveAttachKey() != "" {
		return false
	}
	*controlID++
	if *openCancel != nil {
		(*openCancel)()
		*openCancel = nil
	}
	if *control != nil {
		_ = (*control).Stdin.Close()
		*control = nil
	}
	return true
}

// Pending focus owns keyboard input until its writer and output are ready.
// Reject early input explicitly: interpreting it as navigation could change
// the target session while an asynchronous control request is still opening.
func handlePendingPTYInput(s *tuiState, input []byte, workspaceOutput bool, control **ducklord.ControlSession, openCancel *context.CancelFunc, controlID *int) bool {
	// Workspace modals own input for their entire lifetime. A pending or newly
	// opened PTY control must never consume modal keys or report them as dropped.
	if s.centralModalOpen() {
		return false
	}
	if s.notesFocusRestorePending {
		if string(input) != "\x1d" && !s.shortcut("pty_unfocus", string(input)) && string(input) != "\x1b" && string(input) != "\x03" {
			return queueNotesPendingInput(s, input)
		}
		dropNotesPendingInput(s)
		s.notesFocusRestorePending = false
	}
	if s.helpFocusRestorePending {
		if s.helpMode {
			return false
		}
		if s.focused {
			s.helpFocusRestorePending = false
			s.helpPendingInputKey = ""
			return false
		}
		if s.helpPendingInputKey != "" && s.helpPendingInputKey != s.activeAttachKey {
			s.helpFocusRestorePending = false
			s.helpPendingInputKey = ""
			return false
		}
		return true
	}
	opening := *openCancel != nil
	if s.focused || !opening && (!workspaceOutput || *control == nil) {
		return false
	}
	if string(input) == "\x1d" || s.shortcut("pty_unfocus", string(input)) || string(input) == "\x1b" || string(input) == "\x03" {
		*controlID++ // fence a completion already queued by the canceled request
		if opening {
			(*openCancel)()
			*openCancel = nil
		}
		if *control != nil {
			_ = (*control).Stdin.Close()
			*control = nil
		}
		s.clearAttachIdentity()
		s.outputErr = "PTY focus canceled while waiting for control or output"
	} else if opening {
		s.outputErr = "opening PTY control; input was not sent"
	} else {
		s.outputErr = "waiting for Session pane output; input was not sent"
	}
	return true
}

func (s *tuiState) effectiveAttachKey() string {
	if s.activeAttachKey != "" {
		return s.activeAttachKey
	}
	return s.pendingAttachKey
}

func (s *tuiState) clearAttachIdentity() {
	s.panePrefixPending = false
	if s.clearOutputFocus != nil {
		s.clearOutputFocus()
	}
	if s.workspacePaneFocusRestorePending {
		s.workspaceFocusFromProject = false
		s.workspaceProjectFocus = false
		return
	}
	if s.workspaceNav != nil && s.workspaceNav.InDetailMode() && s.detailSelected.Key() != "" {
		_ = s.workspaceNav.PreviewDetail(s.detailSelected)
	}
	// A quick-shell request owns focus across the asynchronous create/start
	// sequence.  The ordinary attach teardown restores Project focus when an
	// attach was entered from the Project pane, but doing that here would let a
	// stale control completion steal focus from the captured Session route.
	quickShellPlacementPending := s.workspaceNewSessionIntent != nil || len(s.pendingWorkspacePlacements) != 0 || s.workspacePlacementFocusPending
	if s.workspacePlacementFocusPending {
		// Inventory refresh can select the newly placed Session before the old
		// pooled control is rejected. Keep that new route until the synthetic
		// Enter acquires its control; only the stale output focus was cleared above.
		s.workspaceFocusFromProject = false
		return
	}
	if s.workspacePreview && s.workspaceNav != nil && s.activeAttachKey != "" && !quickShellPlacementPending {
		if s.workspaceFocusFromProject {
			_ = s.workspaceNav.SelectProject(s.workspaceNav.CurrentProjectID())
			s.workspaceProjectFocus = true
		}
	}
	s.workspaceFocusFromProject = false
	s.activeAttachKey = ""
	s.activeAttachFresh = false
	s.pendingAttachKey = ""
	s.workspacePaneFocusRestoreSession = ducklord.RemoteSession{}
}

func (s *tuiState) followSelectedPreview(ctx context.Context) {
	s.clearAttachIdentity()
	s.refreshSelectedOutput(ctx)
}

func (s *tuiState) applyPreviewOutput(result previewOutputEvent, currentID uint64) bool {
	if result.id != currentID || s.focused || result.key != s.currentKey() || result.generation != s.currentSession().RuntimeGeneration {
		return false
	}
	if result.err != nil {
		s.outputErr = result.err.Error()
		return true
	}
	rows, cols := s.activePTYSize()
	s.terminal = ducklord.NewTerminal(int(rows), int(cols), ducklord.DefaultTerminalScrollback)
	s.ptyScrollOffset = 0
	s.terminal.Write([]byte(result.text))
	s.outputText = s.terminal.Text()
	s.outputForKey = result.key
	s.outputErr = ""
	s.outputStale = false
	s.outputFresh = true
	s.terminalGeneration = result.generation
	s.terminalOffset = 0
	s.terminalCursorValid = false
	if s.sessionFreshlyDisplayed(s.currentSession()) {
		s.markActivitySeen(s.currentSession())
	}
	return true
}

func (s *tuiState) sessionFreshlyDisplayed(session ducklord.RemoteSession) bool {
	_, identityOK := ducklord.IdentityFromSession(session)
	workspaceFocused := !s.workspacePreview || s.focused && s.activeAttachKey == sessionKey(session)
	return identityOK && workspaceFocused && !s.copyMode && !s.centralModalOpen() && s.outputFresh && !s.outputStale && s.terminal != nil &&
		canRead(session) && s.hostIsLive(session.Client) && s.outputForKey == sessionKey(session) &&
		s.terminalGeneration > 0 && s.terminalGeneration == session.RuntimeGeneration
}

func (s *tuiState) hostIsLive(client string) bool {
	update, known := s.hostSync[client]
	return !known || update.State == "live"
}

func sortRemoteSessions(sessions []ducklord.RemoteSession) {
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].Group != sessions[j].Group {
			return sessions[i].Group < sessions[j].Group
		}
		if sessions[i].Client != sessions[j].Client {
			return sessions[i].Client < sessions[j].Client
		}
		if sessions[i].Name != sessions[j].Name {
			return sessions[i].Name < sessions[j].Name
		}
		return sessionKey(sessions[i]) < sessionKey(sessions[j])
	})
}

func (s *tuiState) organizationMode() ducklord.OrganizationMode {
	mode := s.activity().Organization.Mode
	if mode != ducklord.OrganizationCustom && mode != ducklord.OrganizationHost && mode != ducklord.OrganizationType {
		return ducklord.OrganizationCustom
	}
	return mode
}

func (s *tuiState) organizationGroupID(session ducklord.RemoteSession) string {
	switch s.organizationMode() {
	case ducklord.OrganizationHost:
		return session.Client
	case ducklord.OrganizationType:
		if session.Kind == string(model.KindShell) {
			return "shell"
		}
		return "agent"
	default:
		if identity, ok := ducklord.IdentityFromSession(session); ok {
			if group := s.activity().Organization.Membership[identity]; group != "" {
				return group
			}
		}
		return ducklord.UngroupedGroupID
	}
}

func (s *tuiState) organizationGroupLabel(session ducklord.RemoteSession) string {
	id := s.organizationGroupID(session)
	if s.organizationMode() != ducklord.OrganizationCustom {
		return id
	}
	if id == ducklord.UngroupedGroupID {
		return "Ungrouped"
	}
	for _, group := range s.activity().Organization.Groups {
		if group.ID == id {
			return s.customGroupDisplayName(group)
		}
	}
	return "Ungrouped"
}

type sessionListRow struct {
	groupID      string
	groupLabel   string
	sessionIndex int
	isGroup      bool
}

func (s *tuiState) groupCollapsed(groupID string) bool {
	for _, id := range s.activity().Organization.Collapsed[s.organizationMode()] {
		if id == groupID {
			return true
		}
	}
	return false
}

func (s *tuiState) sessionListRows() []sessionListRow {
	groupLabels := make(map[string]string)
	groupSessions := make(map[string][]int)
	order := append([]string(nil), s.activity().Organization.GroupOrders[s.organizationMode()]...)
	seen := make(map[string]bool)
	if s.organizationMode() == ducklord.OrganizationCustom {
		groupLabels[ducklord.UngroupedGroupID] = "Ungrouped"
		for _, group := range s.activity().Organization.Groups {
			groupLabels[group.ID] = s.customGroupDisplayName(group)
		}
	}
	for index, session := range s.sessions {
		id := s.organizationGroupID(session)
		if groupLabels[id] == "" {
			groupLabels[id] = s.organizationGroupLabel(session)
		}
		groupSessions[id] = append(groupSessions[id], index)
		if !seen[id] {
			order = append(order, id)
		}
		seen[id] = true
	}
	rows := make([]sessionListRow, 0, len(s.sessions)+len(order))
	emitted := make(map[string]bool)
	for _, id := range order {
		if emitted[id] || groupLabels[id] == "" {
			continue
		}
		emitted[id] = true
		rows = append(rows, sessionListRow{groupID: id, groupLabel: groupLabels[id], sessionIndex: -1, isGroup: true})
		if !s.groupCollapsed(id) {
			for _, index := range groupSessions[id] {
				rows = append(rows, sessionListRow{groupID: id, sessionIndex: index})
			}
		}
	}
	return rows
}

func (s *tuiState) moveListSelection(direction int) bool {
	rows := s.sessionListRows()
	if len(rows) == 0 {
		return false
	}
	current := 0
	for i, row := range rows {
		if s.selectedGroupID != "" && row.isGroup && row.groupID == s.selectedGroupID || s.selectedGroupID == "" && !row.isGroup && row.sessionIndex == s.selected {
			current = i
			break
		}
	}
	next := max(0, min(len(rows)-1, current+direction))
	if next == current {
		return false
	}
	row := rows[next]
	if row.isGroup {
		s.selectedGroupID = row.groupID
	} else {
		s.selectedGroupID = ""
		s.selected = row.sessionIndex
		s.selectedKey = s.currentKey()
	}
	return true
}

func (s *tuiState) toggleSelectedGroup() {
	s.setSelectedGroupCollapsed(!s.groupCollapsed(s.selectedGroupID))
}

func (s *tuiState) setSelectedGroupCollapsed(want bool) {
	id := s.selectedGroupID
	if id == "" {
		return
	}
	valid := false
	for _, row := range s.sessionListRows() {
		if row.isGroup && row.groupID == id {
			valid = true
			break
		}
	}
	if !valid {
		s.selectedGroupID = ""
		s.outputErr = "selected group no longer exists"
		return
	}
	if s.groupCollapsed(id) == want {
		return
	}
	next := s.activity().Clone()
	mode := s.organizationMode()
	collapsed := next.Organization.Collapsed[mode]
	found := -1
	for i, candidate := range collapsed {
		if candidate == id {
			found = i
			break
		}
	}
	if !want && found >= 0 {
		collapsed = append(collapsed[:found], collapsed[found+1:]...)
	} else if want && found < 0 {
		collapsed = append(collapsed, id)
	}
	next.Organization.Collapsed[mode] = collapsed
	if err := s.activityStore.Save(next); err != nil {
		s.outputErr = "group collapse was not saved: " + err.Error()
		return
	}
	s.activityState = next
}

func (s *tuiState) moveDraggedSessionBefore(target string, before ducklord.SessionIdentity) {
	identity := s.dragSession
	s.dragSession, s.dragTargetGroup, s.dragTargetSession = ducklord.SessionIdentity{}, "", ducklord.SessionIdentity{}
	if identity.Key() == "" {
		return
	}
	if s.organizationMode() == ducklord.OrganizationCustom {
		validTarget := target == ducklord.UngroupedGroupID
		for _, group := range s.activity().Organization.Groups {
			validTarget = validTarget || group.ID == target
		}
		if !validTarget {
			s.outputErr = "drop target group no longer exists"
			return
		}
	}
	current, currentGroup := false, ""
	for _, session := range s.sessions {
		if candidate, ok := ducklord.IdentityFromSession(session); ok && candidate == identity {
			current = true
			currentGroup = s.organizationGroupID(session)
			break
		}
	}
	if !current {
		s.outputErr = "dragged session no longer exists"
		return
	}
	if s.organizationMode() != ducklord.OrganizationCustom && target != currentGroup {
		s.outputErr = "sessions can only be reordered inside this group"
		return
	}
	next := s.activity().Clone()
	if s.organizationMode() == ducklord.OrganizationCustom {
		if target == ducklord.UngroupedGroupID {
			delete(next.Organization.Membership, identity)
		} else {
			next.Organization.Membership[identity] = target
		}
	}
	originalOrder := append([]ducklord.SessionIdentity(nil), next.Organization.SessionOrder...)
	order := next.Organization.SessionOrder[:0]
	for _, candidate := range originalOrder {
		if candidate != identity {
			order = append(order, candidate)
		}
	}
	insert := len(order)
	if before.Key() != "" && before != identity {
		for index, candidate := range order {
			if candidate == before {
				insert = index
				break
			}
		}
	} else {
		for index, candidate := range order {
			for _, session := range s.sessions {
				if candidateSession, ok := ducklord.IdentityFromSession(session); ok && candidateSession == candidate && s.organizationGroupID(session) == target {
					insert = index + 1
				}
			}
		}
	}
	order = append(order, ducklord.SessionIdentity{})
	copy(order[insert+1:], order[insert:])
	order[insert] = identity
	next.Organization.SessionOrder = order
	if err := s.activityStore.Save(next); err != nil {
		s.outputErr = "session move was not saved: " + err.Error()
		return
	}
	s.activityState = next
	s.applyOrganizationOrder()
	selectedKey := ""
	for index, session := range s.sessions {
		if candidate, ok := ducklord.IdentityFromSession(session); ok && candidate == identity {
			s.selected = index
			selectedKey = sessionKey(session)
			break
		}
	}
	s.selectedGroupID = ""
	s.selectedKey = selectedKey
	s.outputErr = "session order saved"
}
