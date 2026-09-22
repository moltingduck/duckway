package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestWorkspaceProjectHostsFilterAndPersist(t *testing.T) {
	s, projectID, a, b := workspacePaneTestState(t)
	s.cfg.Clients = append(s.cfg.Clients, ducklord.Client{Name: "other", Host: "other"})
	b.Client = "other"
	s.sessions[1] = b
	project := s.activity().ProjectLayout.Project(projectID)
	project.Hosts = []string{"host"}
	s.workspacePaneIntent = workspacePaneIntent{projectID: projectID}
	if got := s.workspacePaneCandidates(); !reflect.DeepEqual(got, []ducklord.RemoteSession{a}) {
		t.Fatalf("candidates: %+v", got)
	}
	if err := s.placeWorkspacePane(workspacePaneIntent{projectID: projectID, placement: ducklord.PlaceNewTab}, b); err == nil {
		t.Fatal("unassociated Host placed")
	}
	s.workspaceNewSessionIntent = &workspacePaneIntent{projectID: projectID}
	if hosts := s.workspaceCreateHosts(); len(hosts) != 1 || hosts[0].Name != "host" {
		t.Fatalf("hosts: %+v", hosts)
	}
	if _, err := s.resolveCreateClient("other"); err == nil {
		t.Fatal("typed excluded Host accepted")
	}
	if _, err := s.resolveCreateClient("2"); err == nil {
		t.Fatal("excluded Host number accepted")
	}
	if got, err := s.resolveCreateClient("1"); err != nil || got != "host" {
		t.Fatalf("number selection: %q %v", got, err)
	}
	s.workspacePaneStep = "project-hosts-edit"
	s.workspacePaneHosts = []string{"host", "other"}
	if err := s.commitWorkspaceProjectHosts(); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.activityStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.ProjectLayout.Project(projectID).Hosts, []string{"host", "other"}) {
		t.Fatal("Host associations not persisted")
	}
	s.workspacePaneIntent = workspacePaneIntent{projectID: projectID}
	s.workspacePaneStep = "project-hosts-edit"
	s.workspacePaneHosts = []string{"other"}
	if err := s.commitWorkspaceProjectHosts(); err == nil || !strings.Contains(err.Error(), "detach") {
		t.Fatalf("removing used Host: %v", err)
	}
	if len(s.activity().ProjectLayout.Project(projectID).Tabs) != 1 || len(s.sessions) != 2 {
		t.Fatal("Host removal changed Sessions")
	}
}

func TestWorkspaceProjectEmptyHostsRemainRestricted(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	project := s.activity().ProjectLayout.Project(projectID)
	project.Hosts = []string{}
	if s.workspaceProjectAllowsHost(projectID, "host") {
		t.Fatal("empty Hosts allowed host")
	}
	clone := s.activity().Clone()
	if clone.ProjectLayout.Project(projectID).Hosts == nil {
		t.Fatal("clone lost explicit empty selection")
	}
	if err := s.activityStore.Save(clone); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.activityStore.Load()
	if err != nil || loaded.ProjectLayout.Project(projectID).Hosts == nil {
		t.Fatalf("save/load lost empty selection: %v", err)
	}
	project.Hosts = nil
	if !s.workspaceProjectAllowsHost(projectID, "host") || !s.workspaceProjectAllowsHost(ducklord.DefaultProjectID, "other") {
		t.Fatal("legacy or Default compatibility broken")
	}
}

func TestWorkspaceProjectDirectCreateUsesAssociatedHosts(t *testing.T) {
	s, projectID, _, _ := workspacePaneTestState(t)
	s.focused = true
	s.cfg.Clients = append(s.cfg.Clients, ducklord.Client{Name: "other", Host: "other"})
	s.activity().ProjectLayout.Project(projectID).Hosts = []string{"other"}
	nav, err := s.workspaceNavigation()
	if err != nil {
		t.Fatal(err)
	}
	if err := nav.SelectProject(projectID); err != nil {
		t.Fatal(err)
	}
	s.beginCreate()
	if s.focused || !s.newSessionMode || s.workspaceNewSessionIntent == nil || s.workspaceNewSessionIntent.projectID != projectID || s.newSessionClient != "other" {
		t.Fatal("direct create did not inherit Project scope")
	}
	if got := s.createModalChoices(); len(got) != 1 || !strings.Contains(got[0], "other") {
		t.Fatalf("filtered chooser: %v", got)
	}
	s.newSessionLine = "other"
	s.syncCreateSelectionToInput()
	if s.newSessionSelected != 0 {
		t.Fatal("typed Host used unfiltered index")
	}
	s.cancelCreate()
	s.activity().ProjectLayout.Project(projectID).Hosts = []string{}
	s.beginCreate()
	if s.newSessionMode || s.workspaceNewSessionIntent != nil || !strings.Contains(s.outputErr, "edit Project Hosts") {
		t.Fatal("empty Project Hosts should block create with actionable error")
	}
}

func TestWorkspaceProjectHostCreationCancellationAndMultipleHosts(t *testing.T) {
	s, _, _, _ := workspacePaneTestState(t)
	s.cfg.Clients = append(s.cfg.Clients, ducklord.Client{Name: "other", Host: "other"})
	before := len(s.activity().ProjectLayout.Projects)
	s.beginWorkspaceProject()
	s.handleWorkspacePaneInput([]byte("Multi"))
	s.handleWorkspacePaneInput([]byte("\r"))
	s.handleWorkspacePaneInput([]byte("\r"))
	s.handleWorkspacePaneInput([]byte("\x1b[B"))
	s.handleWorkspacePaneInput([]byte("\r"))
	if len(s.activity().ProjectLayout.Projects) != before {
		t.Fatal("project persisted before save")
	}
	s.handleWorkspacePaneInput([]byte("\x1b[B"))
	s.handleWorkspacePaneInput([]byte("\r"))
	nav, _ := s.workspaceNavigation()
	if got := s.activity().ProjectLayout.Project(nav.CurrentProjectID()).Hosts; !reflect.DeepEqual(got, []string{"host", "other"}) {
		t.Fatalf("multi Host creation: %v", got)
	}
	s.beginWorkspaceProject()
	s.handleWorkspacePaneInput([]byte("Cancel"))
	s.handleWorkspacePaneInput([]byte("\r"))
	s.handleWorkspacePaneInput([]byte("\x03"))
	if len(s.activity().ProjectLayout.Projects) != before+1 {
		t.Fatal("cancel created Project")
	}
}
