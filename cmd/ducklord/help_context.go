package main

import "strings"

// helpEntry is a searchable description of one supported route or operation.
// action is populated only when the row has a real input sequence to dispatch.
type helpEntry struct {
	category string
	action   string
	label    string
	detail   string
}

func helpCatalog(workspace bool) []helpEntry {
	entries := []helpEntry{
		{"SESSION LIST & GROUPS", "list_search", "Search sessions", "filter the visible list"},
		{"", "", "Open selected Session", "Enter attaches the selected Session; in custom organization, Enter on a group expands or collapses it"},
		{"", "", "Open Project files", "f opens file browsing from navigation; unavailable while a Terminal has focus"},
		{"", "refresh", "Refresh sessions and hosts", ""},
		{"SESSION", "session_create", "Create session", "wizard selects Agent or Shell, host, directory bookmark or remote Browse path (missing directories can be created recursively), then add the path as a bookmark or use it once and choose runtime/handle; arrows choose, Enter advances, Esc/Ctrl+C cancels"},
		{"", "session_actions", "Open selected session actions", "menu varies by state: open or view PTY, reconnect output, yield now/when idle, notifications, rename handle, restart, end, or destroy; lifecycle choices may include now, wait, or force-cancel"},
		{"", "session_notifications", "Session notification settings", "Enter toggles categories or opens delivery override; Space toggles categories only; a/r enables all, x disables all; s saves; q/Esc cancels; delivery can inherit Host"},
		{"", "session_yield", "Yield session now", ""},
		{"", "session_yield_wait", "Yield session when idle", ""},
		{"", "session_restart", "Restart session", ""},
		{"", "session_end", "End session", ""},
		{"", "session_destroy", "Destroy session", ""},
		{"HOST", "host_actions", "Open host list and actions", "Enter opens actions for the selected Host; Connections stages connect/disconnect with Space, then Enter applies; other actions include Reconnect, PTY log retention, Agent notification hooks, and Add/Remove host"},
		{"", "host_add", "Add host configuration", ""},
		{"", "host_remove", "Remove host configuration", ""},
		{"", "shortcut_settings", "Configure shortcuts", "arrows choose an action; Enter edits its binding; Enter saves and Esc returns; after save y/Enter restarts now, n/Esc keeps current bindings"},
		{"", "notification_settings", "Global notification settings", "configure per-class delivery and sound paths; Host levels can inherit Global; Other-Project focus has a threshold; s stages save and prompts for restart"},
		{"PROJECT PANE", "project_focus", "Focus Project pane", ""},
		{"", "", "Scroll Project list", "PageUp/PageDown or the mouse wheel moves by one visible page; the Session list uses the same keys"},
		{"", "project_create", "Create Project", ""},
		{"", "project_delete", "Delete local Project", ""},
		{"", "project_notification_focus", "Toggle Project notification focus", ""},
		{"", "project_add_pane", "Add Session pane", ""},
		{"", "project_move_pane", "Move Session pane", ""},
		{"", "project_detach_pane", "Detach Session pane", "detach is unavailable from Default Project"},
		{"", "project_prev_tab", "Previous Terminal tab", ""},
		{"", "project_next_tab", "Next Terminal tab", ""},
		{"", "project_prev_pane", "Previous visible Session pane", ""},
		{"", "project_next_pane", "Next visible Session pane", ""},
		{"", "project_hosts", "Configure Project Hosts", "open Project SSH host settings"},
		{"", "", "Open Notes", "in navigation, o opens Project Notes from Project focus, or the selected Session Notes from the Session list; focused Terminals receive plain o"},
		{"", "detail_list", "Open or close detailed session list", ""},
		{"", "detail_search", "Search detailed list", "session, host, or Project"},
		{"", "detail_filter", "Cycle detailed-list state filter", ""},
		{"", "detail_previous", "Preview previous session", ""},
		{"", "detail_next", "Preview next session", ""},
		{"", "detail_focus", "Focus preview session", ""},
		{"", "detail_jump", "Jump to selected session Project", ""},
		{"TERMINAL AREA", "pty_copy", "Freeze terminal for drag selection", ""},
		{"", "pty_unfocus", "Return to navigation", ""},
		{"", "pane_prefix", "Begin Terminal prefix command", "in a focused Terminal; choose one of the prefix shortcuts below"},
		{"", "prefix+o", "Open Notes", "Project scope from Project focus; selected-session scope from Session list or terminal"},
		{"", "prefix+O", "Open focused-session Notes", "terminal only"},
		{"", "prefix+?", "Open local help", "terminal output stays in the background"},
		{"", "prefix+space", "Open command palette", "type to fuzzy-filter Notes, Help, Host list, Project files, Projects, and sessions; Up/Down or j/k moves; Enter opens; Esc/Ctrl+C closes; Backspace does not edit the query"},
		{"", "prefix+f", "Open Project files", "browse local, Host, and Project shelf files; filter, select, copy, and review history"},
		{"", "prefix+slash", "Search terminal output", "search retained output; Up/Down selects a match and scrolls it into view"},
		{"", "prefix+m", "Add terminal output bookmark", ""},
		{"", "prefix+M", "Open terminal output bookmarks", "unavailable anchors remain listed and use current-history fallback"},
		{"", "prefix+-", "Create horizontal Session pane", "one press opens creation flow; doubled suffix creates a quick shell on the same live Host and CWD"},
		{"", "prefix+\\", "Create vertical Session pane", "one press opens creation flow; doubled suffix creates a quick shell on the same live Host and CWD"},
		{"", "prefix+t", "Create Terminal tab", "one press opens creation flow; doubled suffix creates a quick shell tab when focused on a live Session"},
		{"", "prefix+--", "Create horizontal quick-shell pane", "press the pane prefix and - twice within 500 ms on a focused live Session; reuses its Host and CWD"},
		{"", "prefix+\\\\", "Create vertical quick-shell pane", "press the pane prefix and \\ twice within 500 ms on a focused live Session; reuses its Host and CWD"},
		{"", "prefix+tt", "Create quick-shell Terminal tab", "press the pane prefix and t twice within 500 ms on a focused live Session; reuses its Host and CWD"},
		{"", "prefix+,", "Rename Terminal tab", ""},
		{"", "prefix+c", "Configure focused item or pane", "right-click opens configuration; drag Project rows to reorder Projects and Terminal tabs to reorder tabs"},
		{"", "prefix+d", "Detach focused Session pane", "switches to Project focus; Default Project does not support detaching Session panes"},
		{"", "prefix+n", "Next Terminal tab", ""},
		{"", "prefix+p", "Previous Terminal tab", ""},
		{"", "prefix+up", "Focus Session pane above", ""},
		{"", "prefix+down", "Focus Session pane below", ""},
		{"", "prefix+left", "Focus Session pane left", ""},
		{"", "prefix+right", "Focus Session pane right", ""},
		{"", "", "Project configuration menu", "the configured workspace-menu shortcut opens Create/Rename Project, Project Hosts, Export/Import, Workspace appearance, and shortcut settings"},
		{"NOTES", "", "Browse scoped Notes", "Global, Project, and focused-Session books are independent notes.md files; g/p/s selects scope directly; Left/Right or h/l changes scope; j/k or Up/Down selects entries"},
		{"", "", "Add note", "a opens the entry form"},
		{"", "", "Edit selected note", "e opens the selected entry form"},
		{"", "", "Edit full Notes book", "E opens the complete scoped notes.md in $EDITOR (defaults to vim); the editor runs on the terminal"},
		{"", "", "Copy selected note content", "Enter copies entry content to the system clipboard and OSC 52; if native clipboard tools fail, terminal clipboard support may still work"},
		{"", "", "Search Notes title and body", "/; searches the selected book and descendant books under Global and Project"},
		{"", "", "Navigate Notes", "j/k or Up/Down selects entries; / searches"},
		{"", "", "Notes edit form", "Tab changes fields; arrows move cursor; Enter advances/saves; Ctrl+S saves; Ctrl+K clears; Esc cancels"},
		{"PROJECT FILES", "", "Browse Project files", "Tab switches columns; arrows/j/k select; Enter opens or chooses a bookmark; Browse opens path entry; Backspace/Delete goes to parent; Esc closes; Ctrl+C cancels; h opens endpoint picker; g changes path; / filters; Space marks; i toggles folder icons; c previews copy; l opens history"},
		{"", "", "Review Project transfer history", "Left/Right or h/l chooses batch; Up/Down or j/k chooses item; x clears history and highlights"},
		{"", "", "Resolve Project file conflicts", "In copy preview, s skips, r renames, o overwrites; Enter applies; Esc cancels"},
		{"PROJECT TRANSFER", "", "Export Project", "Project configuration offers Export; includes layout, scoped Notes, Session descriptors, and bookmark metadata; excludes credentials and terminal output; Enter confirms destination"},
		{"", "", "Import Project", "Project configuration offers Import; validate and preview changes; Enter applies, adding a suffix for name collisions and replacing existing Session notebooks; Esc cancels; no remote Sessions are created"},
		{"HOST SKILLS", "", "Host Skills dashboard", "Tab/Left/Right switches panes; arrows/j/k select; Enter/Space expands. Selected skill: p push, n none, m rename, i import, a add source, f fetch, t target, r pull. Target pane: t target; remote row r pulls and x deletes; d deploys. Sources are public HTTPS URLs; insecure TLS is explicit; review file diffs before applying"},
		{"", "", "Host Skills legacy agent list", "Contextual legacy list supports e edit and R pull by ID; these keys are not Host Skills dashboard actions"},
		{"HOST RESOURCES", "", "View Host resources", "Host actions → Resources; r retries load, Enter retries an error, Esc returns to Host actions; Ctrl+C closes"},
		{"", "", "Command palette navigation", "typing fuzzy-filters; Up/Down or j/k moves; Enter selects; Esc/Ctrl+C closes; input has no Backspace editing"},
		{"APPLICATION", "help", "Open or close this help", "Esc and Ctrl+C leave help pinned"},
		{"", "quit", "Quit Ducklord", ""},
	}
	if workspace {
		entries = append(entries,
			helpEntry{"PROJECT PANE", "", "Activate selected Project pane", "Enter attaches the selected Session pane; an empty Project opens the Terminal tab creation flow"},
			helpEntry{"SESSION LIST & GROUPS", "list_sort", "Cycle time / importance / Host / type", ""},
			helpEntry{"", "list_sort_direction", "Reverse event-time direction", ""},
		)
	} else {
		entries = append(entries,
			helpEntry{"SESSION LIST & GROUPS", "list_organize", "Cycle list organization", "custom / host / type"},
			helpEntry{"", "list_groups", "Manage custom groups", "custom organization only; create or rename groups, move the selected session between groups, delete groups, or move groups up/down; arrows or j/k choose and Enter selects; q/Esc closes"},
			helpEntry{"", "list_reorder_up", "Move session up", "custom ordering"},
			helpEntry{"", "list_reorder_down", "Move session down", "custom ordering"},
		)
	}
	return entries
}

func (s *tuiState) helpActionAvailable(action string) bool {
	if action == "" {
		return false
	}
	if s.helpSearchActive {
		return action == "help"
	}
	if s.helpMode && !s.focused && action != "help" && shortcutInput(s.cfg.Shortcut(action)) == "/" {
		return false
	}
	if strings.HasPrefix(action, "prefix+") {
		if !s.workspacePreview || shortcutInput(s.cfg.Shortcut("pane_prefix")) == "" {
			return false
		}
		if action == "prefix+--" || action == "prefix+\\\\" || action == "prefix+tt" {
			if !s.focused {
				return false
			}
			session := s.activePTYSession()
			return session.Client != "" && session.Cwd != ""
		}
		if s.focused {
			return action == "prefix+o" || action == "prefix+O" || action == "prefix+?" || action == "prefix+space" || action == "prefix+f" || action == "prefix+slash" || action == "prefix+m" || action == "prefix+M" || action == "prefix+-" || action == "prefix+\\" || action == "prefix+t" || action == "prefix+," || action == "prefix+c" || action == "prefix+d" || action == "prefix+n" || action == "prefix+p" || strings.HasPrefix(action, "prefix+up") || strings.HasPrefix(action, "prefix+down") || strings.HasPrefix(action, "prefix+left") || strings.HasPrefix(action, "prefix+right")
		}
		return action != "prefix+O" && action != "prefix+d" && action != "prefix+m" && action != "prefix+M"
	}
	if s.focused {
		return action == "pty_unfocus" || s.workspacePreview && (action == "pane_prefix" || strings.HasPrefix(action, "prefix+"))
	}
	if s.workspacePreview {
		if nav, err := s.workspaceNavigation(); err == nil && nav.InDetailMode() {
			if s.detailSearchFocused {
				return action == "pane_prefix" || strings.HasPrefix(action, "prefix+")
			}
			if action == "help" || action == "detail_list" || action == "detail_search" || action == "detail_filter" {
				return true
			}
			if strings.HasPrefix(action, "detail_") {
				return s.detailSelected.Key() != ""
			}
			return action == "pane_prefix" || strings.HasPrefix(action, "prefix+")
		}
		if action == "help" || action == "detail_list" || action == "project_focus" || action == "pane_prefix" || strings.HasPrefix(action, "prefix+") {
			return true
		}
		if s.workspaceProjectFocus {
			return action == "pty_copy" || strings.HasPrefix(action, "project_")
		}
	}
	if strings.HasPrefix(action, "project_") || strings.HasPrefix(action, "detail_") || strings.HasPrefix(action, "prefix+") || action == "pane_prefix" || action == "pty_unfocus" || action == "pty_copy" {
		return false
	}
	if s.hostScoped && (action == "host_add" || action == "host_remove" || action == "session_create" || action == "shortcut_settings" || action == "notification_settings") {
		return false
	}
	if strings.HasPrefix(action, "session_") && action != "session_create" {
		return s.selectedGroupID == "" && s.selected >= 0 && s.selected < len(s.sessions)
	}
	return true
}

// helpActionAvailableFromTerminalOrigin determines highlighting for routes that
// were available before help reclaimed keyboard focus from a Terminal. Mouse
// dispatch still uses helpActionAvailable, which checks current focus.
func (s *tuiState) helpActionAvailableFromTerminalOrigin(action string) bool {
	if !s.helpOriginFocused || !s.helpFocusRestorePending {
		return false
	}
	focused := s.focused
	s.focused = true
	available := s.helpActionAvailable(action)
	s.focused = focused
	return available
}
