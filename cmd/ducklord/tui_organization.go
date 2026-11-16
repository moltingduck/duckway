package main

import (
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklord"
)

var hostRowPalette = []string{"\033[38;5;39m", "\033[38;5;214m", "\033[38;5;170m", "\033[38;5;42m", "\033[38;5;141m", "\033[38;5;81m", "\033[38;5;203m", "\033[38;5;118m", "\033[38;5;207m", "\033[38;5;45m", "\033[38;5;221m", "\033[38;5;135m"}

const disconnectedRowColor = "\033[38;5;244m"

func (s *tuiState) hostRowStyle(host string) string {
	if s.disconnectedHosts[host] {
		return disconnectedRowColor
	}
	for _, session := range s.sessions {
		if session.Client == host && session.Status == "disconnected" {
			return disconnectedRowColor
		}
	}
	return s.hostRowColor(host)
}

func (s *tuiState) hostRowColor(host string) string {
	hosts, seen := make([]string, 0), make(map[string]bool)
	if s.cfg != nil {
		for _, client := range s.cfg.Clients {
			if client.Name != "" && !seen[client.Name] {
				hosts = append(hosts, client.Name)
				seen[client.Name] = true
			}
		}
	}
	for _, session := range s.sessions {
		if session.Client != "" && !seen[session.Client] {
			hosts = append(hosts, session.Client)
			seen[session.Client] = true
		}
	}
	sort.Strings(hosts)
	for index, candidate := range hosts {
		if candidate == host {
			return hostRowPalette[index%len(hostRowPalette)]
		}
	}
	return hostRowPalette[0]
}

func (s *tuiState) applyOrganizationOrder() bool {
	organization := &s.activity().Organization
	selectedGroup := ""
	selectedKey := s.selectedKey
	if selectedKey == "" && len(s.sessions) > 0 && s.selected >= 0 && s.selected < len(s.sessions) {
		selectedKey = sessionKey(s.sessions[s.selected])
	}
	for _, session := range s.sessions {
		if sessionKey(session) == selectedKey {
			selectedGroup = s.organizationGroupID(session)
			break
		}
	}
	if organization.Mode == "" {
		organization.Mode = ducklord.OrganizationCustom
	}
	changed := false
	sessionRank := make(map[string]int, len(organization.SessionOrder)+len(s.sessions))
	for _, identity := range organization.SessionOrder {
		if key := identity.Key(); key != "" {
			if _, exists := sessionRank[key]; !exists {
				sessionRank[key] = len(sessionRank)
			}
		}
	}
	for _, session := range s.sessions {
		identity, ok := ducklord.IdentityFromSession(session)
		if !ok {
			continue
		}
		if _, exists := sessionRank[identity.Key()]; exists {
			continue
		}
		sessionRank[identity.Key()] = len(sessionRank)
		organization.SessionOrder = append(organization.SessionOrder, identity)
		changed = true
	}
	if s.workspacePreview {
		s.sortQuickSessions()
		return changed
	}
	mode := s.organizationMode()
	if organization.GroupOrders == nil {
		organization.GroupOrders = make(map[ducklord.OrganizationMode][]string)
	}
	groupOrder := organization.GroupOrders[mode]
	groupRank := make(map[string]int, len(groupOrder)+len(s.sessions))
	for _, group := range groupOrder {
		if _, exists := groupRank[group]; !exists {
			groupRank[group] = len(groupRank)
		}
	}
	for _, session := range s.sessions {
		group := s.organizationGroupID(session)
		if _, exists := groupRank[group]; !exists {
			groupRank[group] = len(groupRank)
			groupOrder = append(groupOrder, group)
			changed = true
		}
	}
	organization.GroupOrders[mode] = groupOrder
	sort.SliceStable(s.sessions, func(i, j int) bool {
		leftGroup, rightGroup := s.organizationGroupID(s.sessions[i]), s.organizationGroupID(s.sessions[j])
		if groupRank[leftGroup] != groupRank[rightGroup] {
			return groupRank[leftGroup] < groupRank[rightGroup]
		}
		if s.cfg.PromoteUnread() && leftGroup != selectedGroup && s.sessions[i].Unread != s.sessions[j].Unread {
			return s.sessions[i].Unread
		}
		left, leftOK := ducklord.IdentityFromSession(s.sessions[i])
		right, rightOK := ducklord.IdentityFromSession(s.sessions[j])
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && sessionRank[left.Key()] != sessionRank[right.Key()] {
			return sessionRank[left.Key()] < sessionRank[right.Key()]
		}
		return sessionKey(s.sessions[i]) < sessionKey(s.sessions[j])
	})
	return changed
}

func (s *tuiState) cycleOrganizationMode() {
	oldKey := s.currentKey()
	s.selectedGroupID = ""
	s.dragSession, s.dragTargetGroup = ducklord.SessionIdentity{}, ""
	switch s.organizationMode() {
	case ducklord.OrganizationCustom:
		s.activity().Organization.Mode = ducklord.OrganizationHost
	case ducklord.OrganizationHost:
		s.activity().Organization.Mode = ducklord.OrganizationType
	default:
		s.activity().Organization.Mode = ducklord.OrganizationCustom
	}
	s.applyOrganizationOrder()
	s.restoreSelection(oldKey)
	if err := s.saveActivityState(); err != nil {
		s.outputErr = "session organization changed locally but was not saved: " + err.Error()
	} else {
		s.outputErr = "session organization: " + string(s.organizationMode())
	}
}

func (s *tuiState) moveSelectedSession(direction int) {
	if direction != -1 && direction != 1 || len(s.sessions) == 0 {
		return
	}
	selected := s.currentSession()
	selectedIdentity, selectedOK := ducklord.IdentityFromSession(selected)
	if !selectedOK {
		s.outputErr = "session cannot be reordered without a stable identity"
		return
	}
	if s.organizationMode() == ducklord.OrganizationCustom {
		rows := s.sessionListRows()
		currentRow := -1
		for index, row := range rows {
			if !row.isGroup && row.sessionIndex == s.selected {
				currentRow = index
				break
			}
		}
		currentGroup := s.organizationGroupID(selected)
		for index := currentRow + direction; currentRow >= 0 && index >= 0 && index < len(rows); index += direction {
			row := rows[index]
			if row.isGroup {
				if row.groupID == currentGroup {
					continue
				}
				s.dragSession = selectedIdentity
				s.moveDraggedSessionBefore(row.groupID, ducklord.SessionIdentity{})
				return
			}
			break
		}
	}
	before := s.activity().Clone()
	targetIndex := s.selected + direction
	if s.organizationMode() != ducklord.OrganizationCustom {
		group := s.organizationGroupID(selected)
		for targetIndex >= 0 && targetIndex < len(s.sessions) && s.organizationGroupID(s.sessions[targetIndex]) != group {
			targetIndex += direction
		}
	}
	if targetIndex < 0 || targetIndex >= len(s.sessions) {
		s.outputErr = "session is already at the group boundary"
		return
	}
	targetIdentity, targetOK := ducklord.IdentityFromSession(s.sessions[targetIndex])
	if !targetOK {
		s.outputErr = "session cannot be reordered without a stable identity"
		return
	}
	s.applyOrganizationOrder()
	order := s.activity().Organization.SessionOrder
	selectedOrder, targetOrder := -1, -1
	for index, identity := range order {
		switch identity.Key() {
		case selectedIdentity.Key():
			selectedOrder = index
		case targetIdentity.Key():
			targetOrder = index
		}
	}
	if selectedOrder < 0 || targetOrder < 0 {
		s.outputErr = "session order is unavailable"
		return
	}
	order[selectedOrder], order[targetOrder] = order[targetOrder], order[selectedOrder]
	s.activity().Organization.SessionOrder = order
	if s.organizationMode() == ducklord.OrganizationCustom {
		targetGroup := s.organizationGroupID(s.sessions[targetIndex])
		if targetGroup == ducklord.UngroupedGroupID {
			delete(s.activity().Organization.Membership, selectedIdentity)
		} else {
			s.activity().Organization.Membership[selectedIdentity] = targetGroup
		}
	}
	key := sessionKey(selected)
	s.applyOrganizationOrder()
	s.restoreSelection(key)
	if err := s.saveActivityState(); err != nil {
		s.activityState = before
		s.applyOrganizationOrder()
		s.restoreSelection(key)
		s.outputErr = "session order was not changed: " + err.Error()
	} else {
		s.outputErr = "session order saved"
	}
}

var groupMenuActions = []struct{ id, label string }{
	{"create", "Create group"},
	{"move", "Move selected session"},
	{"rename", "Rename group"},
	{"delete", "Delete group"},
	{"group-up", "Move group up"},
	{"group-down", "Move group down"},
}

func (s *tuiState) beginGroupMenu() {
	if s.organizationMode() != ducklord.OrganizationCustom {
		s.outputErr = "custom groups are available in custom organization mode"
		return
	}
	identity, _ := ducklord.IdentityFromSession(s.currentSession())
	s.groupMenu = true
	s.groupMenuStep = "action"
	s.groupMenuAction = ""
	s.groupMenuIndex = 0
	s.groupMenuLine = ""
	s.groupMenuErr = ""
	s.groupMenuTarget = ""
	s.groupMenuSession = identity
	s.actionMenu = false
	s.outputErr = ""
}

func (s *tuiState) closeGroupMenu() {
	s.groupMenu = false
	s.groupMenuStep = ""
	s.groupMenuAction = ""
	s.groupMenuIndex = 0
	s.groupMenuLine = ""
	s.groupMenuErr = ""
	s.groupMenuTarget = ""
	s.groupMenuSession = ducklord.SessionIdentity{}
}

func (s *tuiState) customGroupsInOrder() []ducklord.CustomGroup {
	groups := s.activity().Organization.Groups
	byID := make(map[string]ducklord.CustomGroup, len(groups))
	for _, group := range groups {
		byID[group.ID] = group
	}
	ordered := make([]ducklord.CustomGroup, 0, len(groups))
	seen := make(map[string]bool, len(groups))
	for _, id := range s.activity().Organization.GroupOrders[ducklord.OrganizationCustom] {
		if group, ok := byID[id]; ok {
			ordered = append(ordered, group)
			seen[id] = true
		}
	}
	for _, group := range groups {
		if !seen[group.ID] {
			ordered = append(ordered, group)
		}
	}
	return ordered
}

func (s *tuiState) groupMenuChoices() []ducklord.CustomGroup {
	groups := s.customGroupsInOrder()
	if s.groupMenuStep == "group" && s.groupMenuAction == "move" {
		return append([]ducklord.CustomGroup{{ID: ducklord.UngroupedGroupID, Name: "Ungrouped"}}, groups...)
	}
	return groups
}

func (s *tuiState) handleGroupMenuInput(input []byte) {
	text := string(input)
	if s.groupMenuStep == "name" {
		if action := s.handleLineInput(input, &s.groupMenuLine); action == "submit" {
			s.commitGroupName()
		} else if action == "cancel" {
			s.closeGroupMenu()
		}
		return
	}
	if text == "\x1b" || text == "\x03" || text == "q" {
		s.closeGroupMenu()
		return
	}
	choiceCount := len(groupMenuActions)
	if s.groupMenuStep == "group" {
		choiceCount = len(s.groupMenuChoices())
	}
	switch text {
	case "j", "\x1b[B":
		if s.groupMenuIndex < choiceCount-1 {
			s.groupMenuIndex++
		}
		return
	case "k", "\x1b[A":
		if s.groupMenuIndex > 0 {
			s.groupMenuIndex--
		}
		return
	case "\r", "\n":
	default:
		return
	}
	if choiceCount == 0 {
		s.groupMenuErr = "create a custom group first"
		return
	}
	if s.groupMenuStep == "action" {
		s.groupMenuAction = groupMenuActions[s.groupMenuIndex].id
		if s.groupMenuAction == "move" && s.groupMenuSession.Key() == "" {
			s.groupMenuErr = "moving a session requires a synchronized session identity"
			return
		}
		s.groupMenuIndex = 0
		s.groupMenuErr = ""
		if s.groupMenuAction == "create" {
			s.groupMenuStep = "name"
		} else {
			s.groupMenuStep = "group"
		}
		return
	}
	choices := s.groupMenuChoices()
	s.groupMenuTarget = choices[s.groupMenuIndex].ID
	if s.groupMenuAction == "rename" {
		s.groupMenuStep = "name"
		s.groupMenuIndex = 0
		return
	}
	s.commitGroupOperation()
}

func (s *tuiState) commitGroupName() {
	name := strings.TrimSpace(s.groupMenuLine)
	if err := ducklord.ValidateCustomGroupName(name); err != nil {
		s.groupMenuErr = err.Error()
		return
	}
	next := s.activity().Clone()
	message := "group created"
	if s.groupMenuAction == "create" {
		id := uuid.NewString()
		next.Organization.Groups = append(next.Organization.Groups, ducklord.CustomGroup{ID: id, Name: name})
		next.Organization.GroupOrders[ducklord.OrganizationCustom] = append(next.Organization.GroupOrders[ducklord.OrganizationCustom], id)
	} else {
		found := false
		for index := range next.Organization.Groups {
			if next.Organization.Groups[index].ID == s.groupMenuTarget {
				next.Organization.Groups[index].Name = name
				found = true
				break
			}
		}
		if !found {
			s.groupMenuErr = "selected group no longer exists"
			return
		}
		message = "group renamed"
	}
	s.commitGroupState(next, message)
}

func (s *tuiState) commitGroupOperation() {
	next := s.activity().Clone()
	target := s.groupMenuTarget
	switch s.groupMenuAction {
	case "move":
		found := false
		for _, session := range s.sessions {
			if identity, ok := ducklord.IdentityFromSession(session); ok && identity == s.groupMenuSession {
				found = true
				break
			}
		}
		if !found {
			s.groupMenuErr = "selected session no longer exists"
			return
		}
		if target == ducklord.UngroupedGroupID {
			delete(next.Organization.Membership, s.groupMenuSession)
		} else {
			next.Organization.Membership[s.groupMenuSession] = target
		}
		s.commitGroupState(next, "session moved")
	case "delete":
		found := false
		groups := next.Organization.Groups[:0]
		for _, group := range next.Organization.Groups {
			if group.ID == target {
				found = true
				continue
			}
			groups = append(groups, group)
		}
		if !found {
			s.groupMenuErr = "selected group no longer exists"
			return
		}
		next.Organization.Groups = groups
		for identity, group := range next.Organization.Membership {
			if group == target {
				delete(next.Organization.Membership, identity)
			}
		}
		order := next.Organization.GroupOrders[ducklord.OrganizationCustom][:0]
		for _, id := range next.Organization.GroupOrders[ducklord.OrganizationCustom] {
			if id != target {
				order = append(order, id)
			}
		}
		next.Organization.GroupOrders[ducklord.OrganizationCustom] = order
		collapsed := next.Organization.Collapsed[ducklord.OrganizationCustom][:0]
		for _, id := range next.Organization.Collapsed[ducklord.OrganizationCustom] {
			if id != target {
				collapsed = append(collapsed, id)
			}
		}
		next.Organization.Collapsed[ducklord.OrganizationCustom] = collapsed
		s.commitGroupState(next, "group deleted; sessions moved to Ungrouped")
	case "group-up", "group-down":
		order := next.Organization.GroupOrders[ducklord.OrganizationCustom]
		index := -1
		for i, id := range order {
			if id == target {
				index = i
				break
			}
		}
		delta := -1
		if s.groupMenuAction == "group-down" {
			delta = 1
		}
		neighbor := index + delta
		if index < 0 || neighbor < 0 || neighbor >= len(order) {
			s.groupMenuErr = "group is already at the boundary"
			return
		}
		order[index], order[neighbor] = order[neighbor], order[index]
		next.Organization.GroupOrders[ducklord.OrganizationCustom] = order
		s.commitGroupState(next, "group order saved")
	}
}

func (s *tuiState) commitGroupState(next *ducklord.ActivityState, message string) {
	if err := s.activityStore.Save(next); err != nil {
		s.groupMenuErr = "not saved: " + err.Error()
		return
	}
	selectedIdentity := s.groupMenuSession
	s.activityState = next
	s.applyOrganizationOrder()
	selectedKey := ""
	for _, session := range s.sessions {
		if identity, ok := ducklord.IdentityFromSession(session); ok && identity == selectedIdentity {
			selectedKey = sessionKey(session)
			break
		}
	}
	s.restoreSelection(selectedKey)
	s.closeGroupMenu()
	s.outputErr = message
}

func (s *tuiState) restoreSelection(key string) {
	if key != "" {
		for i, session := range s.sessions {
			if sessionKey(session) == key {
				s.selected = i
				s.selectedKey = key
				return
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
}
