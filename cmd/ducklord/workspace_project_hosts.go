package main

import (
	"fmt"
	"slices"
	"sort"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) workspaceProjectAllowsHost(projectID, host string) bool {
	project := s.activity().ProjectLayout.Project(projectID)
	return project != nil && (projectID == ducklord.DefaultProjectID || project.Hosts == nil || slices.Contains(project.Hosts, host))
}

func (s *tuiState) workspaceCreateHosts() []ducklord.Client {
	var hosts []ducklord.Client
	for _, client := range s.cfg.Clients {
		if s.workspaceNewSessionIntent == nil || s.workspaceProjectAllowsHost(s.workspaceNewSessionIntent.projectID, client.Name) {
			hosts = append(hosts, client)
		}
	}
	return hosts
}

func (s *tuiState) workspaceProjectHostOptions() []string {
	seen := map[string]bool{}
	var hosts []string
	for _, client := range s.cfg.Clients {
		if !seen[client.Name] {
			hosts = append(hosts, client.Name)
			seen[client.Name] = true
		}
	}
	// Keep removed configuration entries visible so associations can be edited.
	for _, host := range s.workspacePaneHosts {
		if !seen[host] {
			hosts = append(hosts, host)
			seen[host] = true
		}
	}
	if project := s.activity().ProjectLayout.Project(s.workspacePaneIntent.projectID); project != nil {
		for _, host := range project.Hosts {
			if !seen[host] {
				hosts = append(hosts, host)
				seen[host] = true
			}
		}
	}
	sort.Strings(hosts)
	return hosts
}

func (s *tuiState) beginWorkspaceProjectHosts() {
	if !s.workspacePreview || !s.workspaceProjectFocus || s.hostScoped {
		return
	}
	nav, err := s.workspaceNavigation()
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	project := s.activity().ProjectLayout.Project(nav.CurrentProjectID())
	if project == nil || project.ID == ducklord.DefaultProjectID {
		s.outputErr = "Default Project includes all Hosts"
		return
	}
	s.workspacePaneIntent = workspacePaneIntent{projectID: project.ID}
	s.workspacePaneHosts = append([]string{}, project.Hosts...)
	if project.Hosts == nil {
		s.workspacePaneHosts = s.workspaceProjectHostOptions()
	}
	s.workspacePaneMode, s.workspacePaneStep, s.workspacePaneIndex = true, "project-hosts-edit", 0
	s.workspacePaneErr = ""
}

func (s *tuiState) commitWorkspaceProjectHosts() error {
	next := s.activity().Clone()
	projectID := s.workspacePaneIntent.projectID
	if s.workspacePaneStep == "project-hosts-create" {
		var err error
		projectID, err = next.ProjectLayout.AddProject(s.workspacePaneName)
		if err != nil {
			return err
		}
	}
	project := next.ProjectLayout.Project(projectID)
	if project == nil || projectID == ducklord.DefaultProjectID {
		return fmt.Errorf("project changed; reopen Host selection")
	}
	removesHost := project.Hosts == nil
	for _, host := range project.Hosts {
		if !slices.Contains(s.workspacePaneHosts, host) {
			removesHost = true
		}
	}
	for _, tab := range project.Tabs {
		if !removesHost {
			break
		}
		for _, identity := range next.ProjectLayout.TabSessions(projectID, tab.ID) {
			found := false
			for _, session := range s.sessions {
				candidate, ok := ducklord.IdentityFromSession(session)
				if ok && candidate == identity && slices.Contains(s.workspacePaneHosts, session.Client) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("a Session pane still uses an excluded or unavailable Host; detach its pane before removing the Host")
			}
		}
	}
	project.Hosts = append([]string{}, s.workspacePaneHosts...)
	sort.Strings(project.Hosts)
	if err := s.activityStore.Save(next); err != nil {
		return fmt.Errorf("save Project Hosts: %w", err)
	}
	s.activityState, s.workspacePaneChanged = next, true
	s.closeWorkspacePane()
	if nav, err := s.workspaceNavigation(); err == nil {
		_ = nav.SelectProject(projectID)
	}
	return nil
}
