package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
	"github.com/hackerduck/duckway/internal/projectregistry"
)

func (s *tuiState) beginCreate() {
	// Opening the create modal transfers keyboard ownership away from the
	// currently focused PTY. Otherwise the next modal key can be forwarded to
	// the old session before newSessionMode gets a chance to handle it.
	s.focused = false
	if s.workspaceNewSessionIntent == nil && s.workspacePreview && s.workspaceProjectFocus && !s.hostScoped {
		if nav, err := s.workspaceNavigation(); err == nil {
			s.workspaceNewSessionIntent = &workspacePaneIntent{projectID: nav.CurrentProjectID(), placement: ducklord.PlaceNewTab}
		}
	}
	hosts := s.workspaceCreateHosts()
	if len(hosts) == 0 {
		s.workspaceNewSessionIntent = nil
		s.outputErr = "no Hosts associated with this Project; edit Project Hosts first"
		return
	}
	clientName := s.selectedClientName()
	allowed := false
	for _, host := range hosts {
		if host.Name == clientName {
			allowed = true
		}
	}
	if !allowed {
		clientName = hosts[0].Name
	}
	if clientName == "" {
		s.workspaceNewSessionIntent = nil
		s.outputErr = "no ducklord client configured"
		return
	}
	s.newSessionMode = true
	s.newSessionClient = clientName
	s.newSessionLine = ""
	s.newSessionSelected = 0
	for i, host := range hosts {
		if host.Name == clientName {
			s.newSessionSelected = i
			break
		}
	}
	s.newSessionErr = ""
	s.newSessionStarting = false
	s.cancelCreateDiscovery()
	s.newSessionStep = "host"
	s.newSessionKind = model.KindShell
	s.newSessionAgent = ""
	s.newSessionCommand = nil
	s.newSessionProjects = nil
	s.newSessionProject = ducklord.RemoteProject{}
	s.newSessionAgents = nil
	s.newSessionCWD = ""
	s.cancelPathSuggestions()
	s.newSessionPathSuggestions = nil
	s.newSessionPathSelected = 0
	s.newSessionPathCompletion = ""
	s.outputErr = ""
}

func (s *tuiState) beginQuickShell(ctx context.Context, done chan<- createDiscoveryEvent, suffix string) {
	if !s.workspacePreview {
		s.quickShellOrigin = nil
		return
	}
	origin := s.quickShellOrigin
	s.quickShellOrigin = nil
	// The origin is captured when the first prefix byte is accepted while the
	// Session is focused. Control handoff can briefly clear the live focused
	// flag before the repeated suffix arrives; the captured origin must survive
	// that asynchronous transition so the completed quick command does not
	// fall through to the ordinary Add Session pane.
	if !s.focused && origin == nil {
		return
	}
	if origin == nil {
		if s.workspaceNav != nil || s.activityState != nil {
			return
		}
		session := s.activePTYSession()
		if session.Client == "" || session.Cwd == "" || !s.hostIsLive(session.Client) {
			return
		}
		origin = &workspacePaneIntent{targetID: s.activeAttachKey}
		placement := ducklord.PlaceNewTab
		if suffix == "-" {
			placement = ducklord.PlaceHorizontal
		}
		if suffix == "\\" {
			placement = ducklord.PlaceVertical
		}
		origin.placement = placement
		s.newSessionMode, s.newSessionKind = true, model.KindShell
		s.newSessionClient, s.newSessionCWD = session.Client, session.Cwd
		s.workspaceNewSessionIntent = origin
		s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "quick-shell", client: session.Client, project: ducklord.RemoteProject{Name: session.Cwd, Path: session.Cwd, Source: "path"}}, func(workCtx context.Context) createDiscoveryEvent {
			client, err := mustClient(s.cfg, session.Client)
			if err != nil {
				return createDiscoveryEvent{err: err}
			}
			agents, err := s.runner.Agents(workCtx, client, session.Cwd)
			return createDiscoveryEvent{agents: agents, err: err}
		})
		return
	}
	identity, ok := s.activity().ProjectLayout.PaneSession(origin.projectID, origin.targetID)
	if !ok || identity != origin.originIdentity {
		return
	}
	var session ducklord.RemoteSession
	for _, candidate := range s.sessions {
		if id, valid := ducklord.IdentityFromSession(candidate); valid && id == identity {
			session = candidate
			break
		}
	}
	if session.Client == "" || session.Cwd == "" || !s.hostIsLive(session.Client) {
		return
	}
	placement := ducklord.PlaceNewTab
	if suffix == "-" {
		placement = ducklord.PlaceHorizontal
	}
	if suffix == "\\" {
		placement = ducklord.PlaceVertical
	}
	origin.placement = placement
	s.newSessionMode, s.newSessionKind = true, model.KindShell
	s.newSessionClient, s.newSessionCWD = session.Client, session.Cwd
	s.newSessionProject = ducklord.RemoteProject{Name: session.Cwd, Path: session.Cwd, Source: "path"}
	s.newSessionStep, s.newSessionErr = "handle", "starting shell..."
	s.workspaceNewSessionIntent = origin
	s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "quick-shell", client: session.Client, project: s.newSessionProject}, func(workCtx context.Context) createDiscoveryEvent {
		client, err := mustClient(s.cfg, session.Client)
		if err != nil {
			return createDiscoveryEvent{err: err}
		}
		agents, err := s.runner.Agents(workCtx, client, session.Cwd)
		return createDiscoveryEvent{agents: agents, err: err}
	})
}

func (s *tuiState) cancelCreate() {
	s.workspaceNewSessionIntent = nil
	s.cancelCreateDiscovery()
	s.newSessionMode = false
	s.newSessionLine = ""
	s.newSessionSelected = 0
	s.newSessionErr = ""
	s.newSessionStarting = false
	s.newSessionStep = ""
	s.newSessionKind = ""
	s.newSessionAgent = ""
	s.newSessionCommand = nil
	s.newSessionProjects = nil
	s.newSessionProject = ducklord.RemoteProject{}
	s.newSessionAgents = nil
	s.newSessionCWD = ""
	s.cancelPathSuggestions()
	s.newSessionPathSuggestions = nil
	s.newSessionPathSelected = 0
	s.newSessionPathCompletion = ""
}

func (s *tuiState) failQuickShell(message string) {
	s.cancelCreate()
	s.newSessionErr = message
	s.outputErr = message
}

func (s *tuiState) cancelCreateDiscovery() {
	if s.newSessionCancel != nil {
		s.newSessionCancel()
		s.newSessionCancel = nil
	}
	s.newSessionRequestID++
	s.newSessionDiscovering = false
}

func (s *tuiState) cancelPathSuggestions() {
	if s.newSessionPathCancel != nil {
		s.newSessionPathCancel()
		s.newSessionPathCancel = nil
	}
	s.newSessionPathRequestID++
	s.newSessionPathBusy = false
}

func (s *tuiState) requestPathSuggestions(ctx context.Context, done chan<- pathSuggestionEvent) {
	s.cancelPathSuggestions()
	if !s.newSessionMode || s.newSessionStep != "path" || strings.TrimSpace(s.newSessionLine) == "" {
		s.newSessionPathSuggestions = nil
		return
	}
	if !s.hostIsLive(s.newSessionClient) {
		s.newSessionErr = "host is reconnecting; choose it again"
		s.newSessionStep = "host"
		return
	}
	client, err := mustClient(s.cfg, s.newSessionClient)
	if err != nil {
		s.newSessionErr = err.Error()
		return
	}
	requestCtx, cancel := context.WithCancel(ctx)
	s.newSessionPathCancel = cancel
	s.newSessionPathBusy = true
	s.newSessionPathSelected = 0
	id := s.newSessionPathRequestID
	generation, instance := s.hostFingerprint(s.newSessionClient)
	query := s.newSessionLine
	s.createWorkers.Add(1)
	go func() {
		defer s.createWorkers.Done()
		timer := time.NewTimer(120 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-requestCtx.Done():
			return
		}
		paths, suggestErr := s.runner.SuggestProjectPaths(requestCtx, client, query)
		event := pathSuggestionEvent{id: id, generation: generation, client: client.Name, instance: instance, query: query, paths: paths, err: suggestErr}
		select {
		case done <- event:
		case <-requestCtx.Done():
		case <-ctx.Done():
		}
	}()
}

func (s *tuiState) hostFingerprint(client string) (uint64, string) {
	update := s.hostSync[client]
	return update.Generation, update.InstanceID
}

func (s *tuiState) bumpHostConnectionEpoch(client string) {
	if s.hostConnectionEpoch == nil {
		s.hostConnectionEpoch = make(map[string]uint64)
	}
	s.hostConnectionEpoch[client]++
}

func (s *tuiState) lifecycleResultCurrent(result lifecycleDoneEvent, requestID, watchEpoch uint64) bool {
	if result.id != requestID || s.disconnectedHosts[result.client] || result.watchEpoch != watchEpoch ||
		result.connectionEpoch != s.hostConnectionEpoch[result.client] {
		return false
	}
	current := s.hostSync[result.client]
	if current.Generation != result.hostGeneration || current.InstanceID != result.hostInstance {
		return false
	}
	return current.State == "" || current.State == "live"
}

func (s *tuiState) invalidateCreateStart(update ducklord.SessionUpdate) bool {
	if !s.newSessionStarting || update.Client != s.newSessionClient {
		return false
	}
	if update.State == "live" && update.Generation == s.newSessionStartGeneration && update.InstanceID == s.newSessionStartInstance {
		return false
	}
	s.newSessionStarting = false
	s.workspaceNewSessionIntent = nil
	s.newSessionStep = "host"
	s.newSessionErr = "host changed while starting; creation was canceled—refresh the session list before retrying"
	return true
}

func (s *tuiState) beginCreateDiscovery(ctx context.Context, done chan<- createDiscoveryEvent, event createDiscoveryEvent, work func(context.Context) createDiscoveryEvent) {
	s.cancelCreateDiscovery()
	requestCtx, cancel := context.WithCancel(ctx)
	s.newSessionCancel = cancel
	s.newSessionDiscovering = true
	event.id = s.newSessionRequestID
	event.generation, event.instance = s.hostFingerprint(event.client)
	s.createWorkers.Add(1)
	go func() {
		defer s.createWorkers.Done()
		result := work(requestCtx)
		result.id, result.generation, result.instance = event.id, event.generation, event.instance
		result.kind, result.client = event.kind, event.client
		if result.failureStep == "" {
			result.failureStep = event.failureStep
		}
		if result.project.Path == "" {
			result.project = event.project
		}
		select {
		case done <- result:
		case <-requestCtx.Done():
		case <-ctx.Done():
		}
	}()
}

func (s *tuiState) submitCreateStep(ctx context.Context, done chan<- createDiscoveryEvent) (sessionName, clientName string, args []string, ready bool, err error) {
	line := strings.TrimSpace(s.newSessionLine)
	switch s.newSessionStep {
	case "", "kind":
		switch strings.ToLower(line) {
		case "", "1", "agent":
			s.newSessionKind = model.KindAgent
		case "2", "shell":
			s.newSessionKind = model.KindShell
		default:
			return "", "", nil, false, fmt.Errorf("choose 1 (agent) or 2 (shell)")
		}
		s.newSessionStep, s.newSessionLine, s.newSessionErr = "host", "", "choose a connected host"
		for i, client := range s.workspaceCreateHosts() {
			if client.Name == s.newSessionClient {
				s.newSessionSelected = i
				break
			}
		}
		return "", "", nil, false, nil
	case "host":
		clientName, err := s.resolveCreateClient(line)
		if err != nil {
			return "", "", nil, false, err
		}
		// The prompt remains visible while discovery runs, so a second Enter
		// must not cancel and restart the active request.
		if s.newSessionDiscovering {
			return "", "", nil, false, nil
		}
		if !s.hostIsLive(clientName) {
			return "", "", nil, false, fmt.Errorf("host %s is reconnecting; wait for synchronization", clientName)
		}
		_, err = mustClient(s.cfg, clientName)
		if err != nil {
			return "", "", nil, false, err
		}
		s.newSessionClient = clientName
		s.newSessionLine = ""
		s.newSessionSelected = 0
		s.startCreateHostDiscovery(ctx, done)
		return "", "", nil, false, nil
	case "project":
		if line == "browse" || line == "path" {
			s.newSessionStep, s.newSessionLine, s.newSessionSelected = "path", "", 0
			s.newSessionErr = "type a remote directory path"
			return "", "", nil, false, nil
		}
		if n, numberErr := strconv.Atoi(line); numberErr == nil && n == len(s.newSessionProjects)+1 {
			s.newSessionStep, s.newSessionLine, s.newSessionSelected = "path", "", 0
			s.newSessionErr = "type a remote directory path"
			return "", "", nil, false, nil
		}
		project, err := s.resolveCreateProject(line)
		if err != nil {
			return "", "", nil, false, err
		}
		client, err := mustClient(s.cfg, s.newSessionClient)
		if err != nil {
			return "", "", nil, false, err
		}
		s.newSessionProject = project
		s.newSessionCWD = project.Path
		s.newSessionLine = ""
		s.newSessionSelected = 0
		s.newSessionErr = "checking available runtime..."
		s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "agents", client: s.newSessionClient, project: project}, func(workCtx context.Context) createDiscoveryEvent {
			agents, agentErr := s.runner.Agents(workCtx, client, project.Path)
			return createDiscoveryEvent{agents: agents, err: agentErr}
		})
		return "", "", nil, false, nil
	case "path":
		path := strings.TrimSpace(line)
		if path == "" || !filepath.IsAbs(path) {
			return "", "", nil, false, fmt.Errorf("choose an absolute remote directory path")
		}
		s.cancelPathSuggestions()
		s.newSessionCWD = filepath.Clean(path)
		s.newSessionProject = ducklord.RemoteProject{Path: s.newSessionCWD, Source: "path"}
		client, clientErr := mustClient(s.cfg, s.newSessionClient)
		if clientErr != nil {
			return "", "", nil, false, clientErr
		}
		s.newSessionLine, s.newSessionErr = "", "checking remote directory..."
		project := s.newSessionProject
		s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "path-status", client: s.newSessionClient, project: project, failureStep: "path"}, func(workCtx context.Context) createDiscoveryEvent {
			status, checkErr := s.runner.EnsureDirectory(workCtx, client, project.Path, false)
			return createDiscoveryEvent{directory: status, project: project, err: checkErr}
		})
		return "", "", nil, false, nil
	case "path-confirm":
		switch strings.ToLower(line) {
		case "", "1", "create", "yes":
			client, clientErr := mustClient(s.cfg, s.newSessionClient)
			if clientErr != nil {
				return "", "", nil, false, clientErr
			}
			project := s.newSessionProject
			s.newSessionErr = "creating directory and missing parents..."
			s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "path-create", client: s.newSessionClient, project: project, failureStep: "path-confirm"}, func(workCtx context.Context) createDiscoveryEvent {
				status, createErr := s.runner.EnsureDirectory(workCtx, client, project.Path, true)
				return createDiscoveryEvent{directory: status, project: project, err: createErr}
			})
		case "2", "back", "no":
			s.newSessionStep, s.newSessionLine = "path", s.newSessionCWD
			s.newSessionErr = "directory was not created"
		default:
			return "", "", nil, false, fmt.Errorf("choose Create or Back")
		}
		return "", "", nil, false, nil
	case "project-policy":
		switch strings.ToLower(line) {
		case "", "1", "add":
			s.newSessionStep, s.newSessionLine = "project-name", ""
			s.newSessionErr = "empty name uses " + projectregistry.DefaultProjectName(filepath.Base(s.newSessionCWD))
			return "", "", nil, false, nil
		case "2", "once", "use":
			project := s.newSessionProject
			client, clientErr := mustClient(s.cfg, s.newSessionClient)
			if clientErr != nil {
				return "", "", nil, false, clientErr
			}
			s.newSessionErr = "checking available runtime..."
			s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "agents", client: s.newSessionClient, project: project, failureStep: "project-policy"}, func(workCtx context.Context) createDiscoveryEvent {
				agents, agentErr := s.runner.Agents(workCtx, client, project.Path)
				return createDiscoveryEvent{agents: agents, err: agentErr}
			})
			return "", "", nil, false, nil
		default:
			return "", "", nil, false, fmt.Errorf("choose 1 (add bookmark) or 2 (use once)")
		}
	case "project-name":
		name := line
		if name == "" {
			name = projectregistry.DefaultProjectName(filepath.Base(s.newSessionCWD))
		}
		client, clientErr := mustClient(s.cfg, s.newSessionClient)
		if clientErr != nil {
			return "", "", nil, false, clientErr
		}
		path := s.newSessionCWD
		s.newSessionErr = "adding remote bookmark..."
		s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "add-project", client: s.newSessionClient}, func(workCtx context.Context) createDiscoveryEvent {
			project, addErr := s.runner.AddProject(workCtx, client, path, name)
			return createDiscoveryEvent{project: project, failureStep: "project-name", err: addErr}
		})
		return "", "", nil, false, nil
	case "agent":
		agent, command, err := s.resolveCreateAgent(line)
		if err != nil {
			return "", "", nil, false, err
		}
		s.newSessionAgent = agent
		s.newSessionCommand = command
		s.newSessionStep = "handle"
		s.newSessionLine = ""
		s.newSessionSelected = 0
		s.newSessionErr = fmt.Sprintf("agent: %s; empty handle uses %s", agent, defaultSessionHandle(s.newSessionCWD))
		return "", "", nil, false, nil
	case "handle":
		name := line
		if name == "" {
			name = defaultSessionHandle(s.newSessionCWD)
		}
		if err := validateSessionHandle(name); err != nil {
			return "", "", nil, false, err
		}
		client, clientErr := mustClient(s.cfg, s.newSessionClient)
		if clientErr != nil {
			return "", "", nil, false, clientErr
		}
		s.newSessionErr = "revalidating directory and runtime..."
		project, agentType, kind := s.newSessionProject, s.newSessionAgent, s.newSessionKind
		s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "validate", client: s.newSessionClient, project: project, sessionName: name}, func(workCtx context.Context) createDiscoveryEvent {
			if project.Source != "path" {
				projects, projectErr := s.runner.Projects(workCtx, client)
				if projectErr != nil {
					return createDiscoveryEvent{sessionName: name, failureStep: "project", err: fmt.Errorf("bookmark revalidation failed: %w", projectErr)}
				}
				if !containsProject(projects, project) {
					return createDiscoveryEvent{sessionName: name, failureStep: "project", err: fmt.Errorf("selected bookmark is no longer available")}
				}
			}
			failureStep := "agent"
			if kind == model.KindShell {
				failureStep = "project"
			}
			agents, agentErr := s.runner.Agents(workCtx, client, project.Path)
			if agentErr != nil {
				return createDiscoveryEvent{sessionName: name, failureStep: failureStep, err: fmt.Errorf("runtime revalidation failed: %w", agentErr)}
			}
			resolved, ok := findRemoteAgent(agents, agentType)
			if !ok {
				return createDiscoveryEvent{sessionName: name, failureStep: failureStep, err: fmt.Errorf("selected runtime %s is no longer available", agentType)}
			}
			var startArgs []string
			if kind == model.KindShell {
				startArgs, agentErr = buildStartArgsProject(name, model.KindShell, "", project.Name, project.Path, resolved.Command)
			} else {
				startArgs, agentErr = buildStartArgsProject(name, model.KindAgent, resolved.Type, project.Name, project.Path, resolved.Command)
			}
			return createDiscoveryEvent{sessionName: name, args: startArgs, err: agentErr}
		})
		return "", "", nil, false, nil
	default:
		return "", "", nil, false, fmt.Errorf("unknown create step %q", s.newSessionStep)
	}
}

func (s *tuiState) startCreateHostDiscovery(ctx context.Context, done chan<- createDiscoveryEvent) {
	client, err := mustClient(s.cfg, s.newSessionClient)
	if err != nil {
		s.newSessionErr = err.Error()
		return
	}
	s.newSessionErr = "loading bookmarks..."
	needHome := s.newSessionKind == model.KindShell || s.workspaceNewSessionIntent != nil
	s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "projects", client: s.newSessionClient}, func(workCtx context.Context) createDiscoveryEvent {
		home := ""
		if needHome {
			resolver, ok := s.runner.(interface {
				HomeDir(context.Context, ducklord.Client) (string, error)
			})
			if ok {
				var homeErr error
				home, homeErr = resolver.HomeDir(workCtx, client)
				if homeErr != nil {
					return createDiscoveryEvent{err: homeErr}
				}
			}
		}
		projects, projectErr := s.runner.Projects(workCtx, client)
		return createDiscoveryEvent{projects: projects, homePath: home, err: projectErr}
	})
}

func (s *tuiState) retryWorkspaceCreateDiscovery(ctx context.Context, done chan<- createDiscoveryEvent, previous ducklord.SessionUpdate, update ducklord.SessionUpdate) bool {
	if s.workspaceNewSessionIntent == nil || !s.newSessionMode || s.newSessionStep != "host" || s.newSessionClient != update.Client || update.State != "live" {
		return false
	}
	if previous.State == "live" && previous.Generation == update.Generation && previous.InstanceID == update.InstanceID {
		return false
	}
	// A host inventory update can race with the discovery started by the
	// user's Enter.  Do not cancel that active request and replace it: doing so
	// fences its result and leaves the wizard stuck on the host selector.  The
	// reconnect path sets this error while cancelling discovery, so it remains
	// eligible for a retry below.
	if s.newSessionDiscovering && s.newSessionCancel != nil && s.newSessionErr != "host connection changed; choose it again" {
		return false
	}
	s.startCreateHostDiscovery(ctx, done)
	return true
}

func containsProject(projects []ducklord.RemoteProject, selected ducklord.RemoteProject) bool {
	for _, project := range projects {
		if project.Name == selected.Name && project.Path == selected.Path && project.Source == selected.Source {
			return true
		}
	}
	return false
}

func findRemoteAgent(agents []ducklord.RemoteAgent, agentType string) (ducklord.RemoteAgent, bool) {
	for _, agent := range agents {
		if agent.Type == agentType {
			return agent, true
		}
	}
	return ducklord.RemoteAgent{}, false
}

func appendRemoteProjectOnce(projects []ducklord.RemoteProject, added ducklord.RemoteProject) []ducklord.RemoteProject {
	for index, project := range projects {
		if project.Path == added.Path {
			projects[index] = added
			return projects
		}
	}
	return append(projects, added)
}

func (s *tuiState) applyCreateDiscovery(event createDiscoveryEvent) (clientName, sessionName string, args []string, ready bool) {
	if !s.newSessionMode || !s.newSessionDiscovering || event.id != s.newSessionRequestID {
		return "", "", nil, false
	}
	generation, instance := s.hostFingerprint(event.client)
	// A workspace split keeps the host connection alive while the focused
	// session inventory is refreshed.  That refresh can publish a newer
	// connection identity during HomeDir/Projects discovery; it does not make
	// the selected host unusable.  Preserve the discovery result for this
	// route, while retaining the strict fence for the general wizard.
	if s.workspaceNewSessionIntent == nil &&
		(generation != event.generation || instance != event.instance || !s.hostIsLive(event.client)) {
		s.cancelCreateDiscovery()
		s.newSessionStep = "host"
		if event.kind == "add-project" && event.project.Path != "" {
			s.newSessionErr = "bookmark was added, but the host changed; choose it again"
		} else {
			s.newSessionErr = "host changed while loading; choose it again"
		}
		return "", "", nil, false
	}
	s.newSessionDiscovering = false
	s.newSessionCancel = nil
	if event.err != nil {
		if event.kind == "quick-shell" {
			s.failQuickShell(sanitizeTerminalText(event.err.Error()))
			return "", "", nil, false
		}
		s.newSessionErr = event.err.Error()
		switch event.kind {
		case "projects":
			s.newSessionStep = "host"
		case "agents":
			s.newSessionStep = event.failureStep
			if s.newSessionStep == "" {
				s.newSessionStep = "project"
			}
		case "add-project":
			if event.project.Path != "" {
				s.newSessionProject = event.project
				s.newSessionProjects = appendRemoteProjectOnce(s.newSessionProjects, event.project)
				s.newSessionStep = "project"
				s.newSessionErr = "bookmark was added, but runtime discovery failed: " + event.err.Error()
			} else {
				s.newSessionStep = event.failureStep
			}
		case "validate":
			s.newSessionStep = event.failureStep
			if s.newSessionStep == "" {
				s.newSessionStep = "handle"
			}
		case "path-status", "path-create":
			s.newSessionStep = event.failureStep
			if event.kind == "path-status" {
				s.newSessionLine = event.project.Path
			}
		}
		return "", "", nil, false
	}
	switch event.kind {
	case "quick-shell":
		shell, ok := findRemoteAgent(event.agents, "shell")
		if !ok {
			s.failQuickShell("remote shell is unavailable")
			return "", "", nil, false
		}
		args, err := buildStartArgsProject(defaultSessionHandle(event.project.Path), model.KindShell, "", event.project.Name, event.project.Path, shell.Command)
		if err != nil {
			s.failQuickShell(sanitizeTerminalText(err.Error()))
			return "", "", nil, false
		}
		return event.client, defaultSessionHandle(event.project.Path), args, true
	case "path-status":
		if event.directory.Exists {
			s.newSessionStep, s.newSessionSelected = "project-policy", 0
			s.newSessionErr = "directory exists; add it to bookmarks or use it once"
		} else {
			s.newSessionStep, s.newSessionSelected = "path-confirm", 0
			s.newSessionErr = "directory does not exist; confirm recursive creation"
		}
	case "path-create":
		if !event.directory.Exists {
			s.newSessionStep, s.newSessionErr = "path-confirm", "Ducklion did not create the directory"
			return "", "", nil, false
		}
		s.newSessionStep, s.newSessionSelected = "project-policy", 0
		if event.directory.Created {
			s.newSessionErr = "directory created recursively; add it to bookmarks or use it once"
		} else {
			s.newSessionErr = "directory already exists; add it to bookmarks or use it once"
		}
	case "projects":
		projects := event.projects
		if (s.newSessionKind == model.KindShell || s.workspaceNewSessionIntent != nil) && event.homePath != "" {
			projects = append([]ducklord.RemoteProject{{Name: "Host home", Path: event.homePath, Source: "path"}}, projects...)
		}
		if s.newSessionKind == model.KindAgent {
			filtered := projects[:0]
			for _, project := range projects {
				if project.Source != "ducklion-default" {
					filtered = append(filtered, project)
				}
			}
			projects = filtered
		}
		if len(projects) == 0 {
			s.newSessionStep = "path"
			s.newSessionLine = ""
			s.newSessionErr = "no configured bookmarks; type an absolute remote directory path"
			return "", "", nil, false
		}
		s.newSessionProjects = append([]ducklord.RemoteProject(nil), projects...)
		s.newSessionStep = "project"
		s.newSessionSelected = 0
		s.newSessionErr = fmt.Sprintf("host: %s; choose a directory", event.client)
	case "add-project":
		s.newSessionProject = event.project
		s.newSessionCWD = event.project.Path
		s.newSessionProjects = appendRemoteProjectOnce(s.newSessionProjects, event.project)
		s.newSessionStep = "project"
		s.newSessionLine = event.project.Name
		s.newSessionErr = "bookmark added; press Enter to continue"
	case "agents":
		if s.newSessionKind == model.KindShell {
			shell, ok := findRemoteAgent(event.agents, "shell")
			if !ok {
				s.newSessionStep, s.newSessionErr = "project", "remote shell is unavailable"
				return "", "", nil, false
			}
			s.newSessionAgent, s.newSessionCommand = shell.Type, append([]string(nil), shell.Command...)
			s.newSessionLine, s.newSessionSelected = "", 0
			s.newSessionStep, s.newSessionErr = "handle", fmt.Sprintf("shell directory: %s; empty handle uses %s", event.project.Path, defaultSessionHandle(event.project.Path))
			return "", "", nil, false
		}
		agents := make([]ducklord.RemoteAgent, 0, len(event.agents))
		for _, agent := range event.agents {
			if agent.Type != "shell" && agent.Type != "zsh" && agent.Type != "bash" && agent.Type != "sh" {
				agents = append(agents, agent)
			}
		}
		if len(agents) == 0 {
			s.newSessionStep, s.newSessionErr = "project", "this directory has no available agent runtime"
			return "", "", nil, false
		}
		s.newSessionAgents = agents
		s.newSessionStep, s.newSessionErr = "agent", fmt.Sprintf("directory: %s; choose an available agent", event.project.Path)
		s.newSessionSelected = 0
	case "validate":
		return event.client, event.sessionName, event.args, true
	}
	return "", "", nil, false
}

func (s *tuiState) resolveCreateAgent(input string) (string, []string, error) {
	if len(s.newSessionAgents) == 0 {
		return "", nil, fmt.Errorf("no agent types are available")
	}
	if input == "" && len(s.newSessionAgents) == 1 {
		return s.newSessionAgents[0].Type, append([]string(nil), s.newSessionAgents[0].Command...), nil
	}
	if n, err := strconv.Atoi(input); err == nil {
		if n < 1 || n > len(s.newSessionAgents) {
			return "", nil, fmt.Errorf("agent number out of range")
		}
		agent := s.newSessionAgents[n-1]
		return agent.Type, append([]string(nil), agent.Command...), nil
	}
	normalized := strings.ToLower(strings.ReplaceAll(input, "-", "_"))
	if normalized == "claude" {
		normalized = "claude_code"
	}
	for _, agent := range s.newSessionAgents {
		if normalized == agent.Type {
			return agent.Type, append([]string(nil), agent.Command...), nil
		}
	}
	return "", nil, fmt.Errorf("agent %q is not available for this directory", input)
}

func defaultSessionHandle(cwd string) string {
	cleaned := filepath.Clean(strings.TrimSpace(cwd))
	name := filepath.Base(cleaned)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "session"
	}
	return name
}

func validateSessionHandle(handle string) error {
	handle = strings.TrimSpace(handle)
	if handle == "" || utf8.RuneCountInString(handle) > 128 {
		return fmt.Errorf("handle must contain 1 to 128 characters")
	}
	for _, r := range handle {
		if unicode.IsControl(r) {
			return fmt.Errorf("handle cannot contain control characters")
		}
	}
	return nil
}

func (s *tuiState) resolveCreateClient(input string) (string, error) {
	hosts := s.workspaceCreateHosts()
	if len(hosts) == 0 {
		return "", fmt.Errorf("no Hosts associated with this Project")
	}
	if input == "" {
		input = s.newSessionClient
	}
	if n, err := strconv.Atoi(input); err == nil {
		if n < 1 || n > len(hosts) {
			return "", fmt.Errorf("host number out of range")
		}
		return hosts[n-1].Name, nil
	}
	for _, host := range hosts {
		if host.Name == input {
			return input, nil
		}
	}
	return "", fmt.Errorf("unknown or unassociated Host %q", input)
}

func (s *tuiState) resolveCreateProject(input string) (ducklord.RemoteProject, error) {
	if input == "" {
		if len(s.newSessionProjects) == 1 {
			return s.newSessionProjects[0], nil
		}
		return ducklord.RemoteProject{}, fmt.Errorf("directory name or number is required")
	}
	if n, err := strconv.Atoi(input); err == nil {
		if n < 1 || n > len(s.newSessionProjects) {
			return ducklord.RemoteProject{}, fmt.Errorf("directory number out of range")
		}
		return s.newSessionProjects[n-1], nil
	}
	for _, project := range s.newSessionProjects {
		if input == project.Name || input == project.Path {
			return project, nil
		}
	}
	return ducklord.RemoteProject{}, fmt.Errorf("unknown directory %q; choose a listed directory", input)
}

func (s *tuiState) createHeader() string {
	switch s.newSessionStep {
	case "kind":
		return "new session: choose agent or shell"
	case "host":
		return "new session: choose host number/name"
	case "project":
		return fmt.Sprintf("new %s session: host=%s  choose directory", s.newSessionKind, displayField(s.newSessionClient))
	case "path":
		return fmt.Sprintf("new %s session: browse remote path on %s", s.newSessionKind, displayField(s.newSessionClient))
	case "path-confirm":
		return fmt.Sprintf("CREATE REMOTE DIRECTORY · %s · %s", displayField(s.newSessionClient), displayField(s.newSessionCWD))
	case "project-policy":
		return "remote path selected: add to bookmarks or use once"
	case "project-name":
		return "add remote path to bookmarks"
	case "agent":
		label := "agent"
		if s.newSessionKind == model.KindShell {
			label = "shell"
		}
		return fmt.Sprintf("new session: host=%s directory=%s  choose %s", displayField(s.newSessionClient), displayField(s.newSessionCWD), label)
	case "handle":
		return fmt.Sprintf("new session: agent=%s  handle (default %s)", displayField(s.newSessionAgent), displayField(defaultSessionHandle(s.newSessionCWD)))
	default:
		return "new session"
	}
}

func (s *tuiState) createPromptLabel() string {
	switch s.newSessionStep {
	case "kind":
		return "type"
	case "host":
		return "host"
	case "project":
		return "directory"
	case "path":
		return "path"
	case "path-confirm":
		return "choice"
	case "project-policy":
		return "choice"
	case "project-name":
		return "bookmark name"
	case "agent":
		if s.newSessionKind == model.KindShell {
			return "shell"
		}
		return "agent"
	case "handle":
		return "handle"
	default:
		return "host"
	}
}

func (s *tuiState) handleCreateInput(b []byte) string {
	switch string(b) {
	case "\x03":
		return "cancel"
	case "\x1b":
		if s.newSessionStep == "kind" || s.newSessionStep == "host" {
			return "cancel"
		}
		s.backCreateStep()
		return "back"
	case "\x1b[D", "\x1b[C":
		return ""
	}
	if s.newSessionStep == "path" {
		switch string(b) {
		case "\x1b[B":
			if s.newSessionPathSelected < len(s.newSessionPathSuggestions)-1 {
				s.newSessionPathSelected++
			}
			s.newSessionPathCompletion = ""
			return ""
		case "\x1b[A":
			if s.newSessionPathSelected > 0 {
				s.newSessionPathSelected--
			}
			s.newSessionPathCompletion = ""
			return ""
		case "\r", "\n":
			if len(s.newSessionPathSuggestions) > 0 && s.newSessionPathSelected < len(s.newSessionPathSuggestions) {
				selected := s.newSessionPathSuggestions[s.newSessionPathSelected]
				if s.newSessionPathCompletion != selected {
					s.cancelPathSuggestions()
					s.newSessionLine = selected
					s.newSessionPathCompletion = selected
					return "complete"
				}
			}
		}
	}
	choices := s.createModalChoices()
	switch string(b) {
	case "\x1b[B":
		if len(choices) > 0 && s.newSessionSelected < len(choices)-1 {
			s.newSessionSelected++
		}
		s.newSessionLine = ""
		return ""
	case "\x1b[A":
		if s.newSessionSelected > 0 {
			s.newSessionSelected--
		}
		s.newSessionLine = ""
		return ""
	case "\r", "\n":
		if s.newSessionLine == "" && len(choices) > 0 && s.newSessionStep != "handle" {
			s.newSessionLine = strconv.Itoa(s.createModalSelectedIndex(len(choices)) + 1)
		}
	}
	action := s.handleLineInput(b, &s.newSessionLine)
	if action == "" {
		if s.newSessionStep == "path" {
			s.newSessionPathCompletion = ""
		}
		s.syncCreateSelectionToInput()
	}
	return action
}

func (s *tuiState) backCreateStep() {
	s.cancelPathSuggestions()
	s.newSessionPathCompletion = ""
	s.newSessionSelected = 0
	s.newSessionLine = ""
	switch s.newSessionStep {
	case "host":
		s.newSessionStep = "kind"
	case "project":
		s.newSessionStep = "host"
	case "path":
		if len(s.newSessionProjects) > 0 {
			s.newSessionStep = "project"
		} else {
			s.newSessionStep = "host"
		}
	case "path-confirm":
		s.newSessionStep = "path"
		s.newSessionLine = s.newSessionCWD
	case "project-policy":
		s.newSessionStep = "path"
		s.newSessionLine = s.newSessionCWD
	case "project-name":
		s.newSessionStep = "project-policy"
	case "agent":
		if s.newSessionProject.Source == "path" {
			s.newSessionStep = "project-policy"
		} else {
			s.newSessionStep = "project"
		}
	case "handle":
		if len(s.newSessionAgents) > 0 {
			s.newSessionStep = "agent"
		} else if s.newSessionProject.Source == "path" {
			s.newSessionStep = "project-policy"
		} else {
			s.newSessionStep = "project"
		}
	}
	s.newSessionErr = "choose an option"
}

// shouldDiscardModalArrow preserves the modal-level arrow behavior for modal
// selectors while allowing the Notes form to receive its cursor movement.
func (s *tuiState) shouldDiscardModalArrow(input []byte) bool {
	if s.workspacePaneMode && s.workspacePaneStep == "notes" {
		return false
	}
	return (string(input) == "\x1b[D" || string(input) == "\x1b[C") && s.centralModalOpen()
}

func (s *tuiState) centralModalOpen() bool {
	return s.blockingModalOpen()
}

func (s *tuiState) blockingModalOpen() bool {
	return s.projectFiles.open || s.approvals.open || s.commandPaletteMode || s.workspacePaneMode || s.shortcutMode || s.notificationConfigMode || s.hostMenuMode || s.searchMode || s.terminalSearchMode || s.terminalBookmarkMode || s.terminalBookmarkListMode || s.addClientMode || s.removeClientMode || s.newSessionMode || s.notificationMode || s.groupMenu || s.actionMenu || s.sessionRenameMode || s.lifecycleConfirm != ""
}

// workspaceControlMayAccept gates asynchronous PTY ownership. Workspace
// modals keep ownership until they close and the deferred focus lease runs.
func (s *tuiState) workspaceControlMayAccept() bool {
	return !s.focused && !s.workspacePaneMode && !s.addClientMode && !s.newSessionMode && !s.helpMode && !s.projectFiles.open
}

func (s *tuiState) syncCreateSelectionToInput() {
	input := strings.TrimSpace(s.newSessionLine)
	if input == "" || s.newSessionStep == "handle" {
		return
	}
	if n, err := strconv.Atoi(input); err == nil {
		if count := len(s.createModalChoices()); n >= 1 && n <= count {
			s.newSessionSelected = n - 1
		}
		return
	}
	match := func(index int, values ...string) bool {
		for _, value := range values {
			if input == value {
				s.newSessionSelected = index
				return true
			}
		}
		return false
	}
	switch s.newSessionStep {
	case "kind":
		for index, value := range []string{"agent", "shell"} {
			if strings.EqualFold(input, value) {
				s.newSessionSelected = index
				return
			}
		}
	case "host":
		for index, client := range s.workspaceCreateHosts() {
			if match(index, client.Name) {
				return
			}
		}
	case "project":
		for index, project := range s.newSessionProjects {
			if match(index, project.Name, project.Path) {
				return
			}
		}
	case "agent":
		for index, agent := range s.newSessionAgents {
			if match(index, agent.Type) {
				return
			}
		}
	}
}

func (s *tuiState) handleLineInput(b []byte, line *string) string {
	if len(b) == 0 {
		return ""
	}
	switch string(b) {
	case "\r", "\n":
		return "submit"
	case "\x03", "\x1b":
		return "cancel"
	}
	if len(b) == 1 && (b[0] == '\b' || b[0] == 0x7f) {
		if *line != "" {
			runes := []rune(*line)
			*line = string(runes[:len(runes)-1])
		}
		return ""
	}
	if utf8.Valid(b) {
		for _, r := range string(b) {
			if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
				*line += string(r)
			}
		}
	}
	return ""
}
