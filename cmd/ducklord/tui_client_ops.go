package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hackerduck/duckway/internal/ducklord"
)

func (s *tuiState) beginAddClient() {
	hosts, err := ducklord.LoadSSHConfigHosts("")
	s.beginAddClientWithHosts(hosts, err)
}

func (s *tuiState) beginAddClientWithHosts(hosts []ducklord.SSHHost, err error) {
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	// Add host owns keyboard input, even when invoked from a focused terminal.
	// Save the exact PTY origin so cancel and completion can restore it safely.
	s.addClientRestoreFocused = s.focused
	s.addClientRestoreAttachKey = s.activeAttachKey
	if s.addClientRestoreFocused && s.addClientRestoreAttachKey != "" {
		s.addClientRestoreSession, _ = s.sessionForKey(s.addClientRestoreAttachKey)
	} else {
		s.addClientRestoreSession = ducklord.RemoteSession{}
	}
	s.focused = false
	s.addClientMode = true
	s.addClientStep = "mode"
	s.addClientProvisionMode = ""
	s.addClientModeSelected = 0
	s.addClientLine = ""
	s.addClientSelected = 0
	s.addClientErr = ""
	s.addClientHosts = hosts
	s.newSessionMode = false
	s.outputErr = ""
}

func (s *tuiState) cancelAddClient() {
	s.cancelAddClientWork()
	s.addClientMode = false
	s.addClientStep = ""
	s.addClientProvisionMode = ""
	s.addClientLine = ""
	s.addClientSelected = 0
	s.addClientErr = ""
	s.addClientHosts = nil
	if s.addClientRestoreFocused && s.addClientRestoreAttachKey != "" && s.activeAttachKey == s.addClientRestoreAttachKey {
		origin := s.addClientRestoreSession
		current, ok := s.sessionForKey(s.addClientRestoreAttachKey)
		if ok && origin.Client != "" && origin.InstanceID != "" && origin.SessionID != "" && origin.RuntimeGeneration != 0 && current.Client == origin.Client &&
			current.InstanceID == origin.InstanceID && current.SessionID == origin.SessionID &&
			current.RuntimeGeneration == origin.RuntimeGeneration {
			s.focused = true
		} else {
			// A reused session key may now identify a different runtime. Leave
			// keyboard ownership with workspace navigation rather than the old PTY.
			s.focused, s.workspaceProjectFocus, s.activeAttachKey = false, true, ""
		}
	}
	s.addClientRestoreFocused = false
	s.addClientRestoreAttachKey = ""
	s.addClientRestoreSession = ducklord.RemoteSession{}
}

func (s *tuiState) cancelAddClientWork() {
	if s.addClientBusy {
		s.addClientRequestID++ // fence a completion racing with cancellation
	}
	if s.addClientCancel != nil {
		s.addClientCancel()
		s.addClientCancel = nil
	}
	s.addClientBusy = false
}

func (s *tuiState) beginRemoveClient() {
	if s.cfg == nil || len(s.cfg.Clients) == 0 {
		s.outputErr = "no Ducklion host configured"
		return
	}
	s.removeClientMode = true
	s.removeClientConfirm = ""
	s.removeClientSelected = 0
	if selected := s.selectedClientName(); selected != "" {
		for index, client := range s.cfg.Clients {
			if client.Name == selected {
				s.removeClientSelected = index
				break
			}
		}
	}
	s.outputErr = ""
}

func (s *tuiState) cancelRemoveClient() {
	s.removeClientMode = false
	s.removeClientConfirm = ""
	s.removeClientSelected = 0
}

func (s *tuiState) handleRemoveClientInput(input []byte) string {
	if len(s.cfg.Clients) == 0 {
		s.cancelRemoveClient()
		return "cancel"
	}
	s.removeClientSelected = min(max(s.removeClientSelected, 0), len(s.cfg.Clients)-1)
	switch string(input) {
	case "\x03":
		s.cancelRemoveClient()
		return "cancel"
	case "\x1b":
		if s.removeClientConfirm != "" {
			s.removeClientConfirm = ""
			return "back"
		}
		s.cancelRemoveClient()
		return "cancel"
	case "j", "\x1b[B":
		if s.removeClientConfirm == "" && s.removeClientSelected < len(s.cfg.Clients)-1 {
			s.removeClientSelected++
		}
	case "k", "\x1b[A":
		if s.removeClientConfirm == "" && s.removeClientSelected > 0 {
			s.removeClientSelected--
		}
	case "\r", "\n":
		name := s.cfg.Clients[s.removeClientSelected].Name
		if s.removeClientConfirm == "" {
			s.removeClientConfirm = name
			return "confirm"
		}
		if s.removeClientConfirm == name {
			return "remove"
		}
	}
	return ""
}

func (s *tuiState) handleAddClientInput(input []byte) string {
	if s.addClientStep == "mode" {
		switch string(input) {
		case "j", "\x1b[B":
			s.addClientModeSelected = 1
		case "k", "\x1b[A":
			s.addClientModeSelected = 0
		case "\r", "\n":
			if s.addClientModeSelected == 0 {
				s.addClientProvisionMode = "standalone"
			} else {
				s.addClientProvisionMode = "integrated"
			}
			s.addClientStep = "host"
			return "next"
		case "\x1b":
			return "cancel"
		}
		return ""
	}
	if string(input) == "\x1b" && s.addClientStep == "host" {
		s.addClientStep = "mode"
		s.addClientLine = ""
		return "back"
	}
	switch string(input) {
	case "\x1b[B":
		if s.addClientSelected < len(s.addClientHosts)-1 {
			s.addClientSelected++
		}
		s.addClientLine = ""
		return ""
	case "\x1b[A":
		if s.addClientSelected > 0 {
			s.addClientSelected--
		}
		s.addClientLine = ""
		return ""
	case "\r", "\n":
		if s.addClientLine == "" && len(s.addClientHosts) > 0 {
			s.addClientLine = strconv.Itoa(s.addClientSelected + 1)
		}
	}
	return s.handleLineInput(input, &s.addClientLine)
}

func (s *tuiState) submitAddClient(ctx context.Context) error {
	client, err := s.clientFromAddLine(strings.TrimSpace(s.addClientLine))
	if err != nil {
		return err
	}
	result := runAddClientWithMode(ctx, s.runner, client, s.addClientProvisionMode, nil)
	if result.err != nil {
		return result.err
	}
	return s.applyAddClientResult(result)
}

func (s *tuiState) startAddClient(ctx context.Context, done chan<- addClientDoneEvent) error {
	client, err := s.clientFromAddLine(strings.TrimSpace(s.addClientLine))
	if err != nil {
		return err
	}
	s.cancelAddClientWork()
	s.addClientRequestID++
	id := s.addClientRequestID
	workCtx, cancel := context.WithCancel(ctx)
	s.addClientCancel = cancel
	s.addClientBusy = true
	s.addClientErr = "probing " + client.Name + "..."
	mode := s.addClientProvisionMode
	go func() {
		result := runAddClientWithMode(workCtx, s.runner, client, mode, func(status string) {
			select {
			case done <- addClientDoneEvent{id: id, progress: true, status: status}:
			case <-workCtx.Done():
			}
		})
		result.id = id
		select {
		case done <- result:
		case <-ctx.Done():
		}
	}()
	return nil
}

// completeAddClient applies a matching completion on the TUI event loop. It
// returns true only when a new host was durably committed.
func (s *tuiState) completeAddClient(result addClientDoneEvent) bool {
	if !s.addClientMode || !s.addClientBusy || result.id != s.addClientRequestID {
		return false
	}
	s.addClientBusy = false
	if s.addClientCancel != nil {
		s.addClientCancel()
		s.addClientCancel = nil
	}
	if result.err != nil {
		s.addClientErr = sanitizeTerminalText(result.err.Error())
		return false
	}
	if err := s.applyAddClientResult(result); err != nil {
		s.addClientErr = sanitizeTerminalText(err.Error())
		return false
	}
	return true
}

func runAddClient(ctx context.Context, runner remoteRunner, client ducklord.Client) addClientDoneEvent {
	return runAddClientWithProgress(ctx, runner, client, nil)
}

func runAddClientWithProgress(ctx context.Context, runner remoteRunner, client ducklord.Client, progress func(string)) addClientDoneEvent {
	return runAddClientWithMode(ctx, runner, client, "", progress)
}

func runAddClientWithMode(ctx context.Context, runner remoteRunner, client ducklord.Client, mode string, progress func(string)) addClientDoneEvent {
	result := addClientDoneEvent{client: client}
	probe, err := runner.ProbeDucklion(ctx, client)
	if err != nil {
		result.err = err
		return result
	}
	var installErr error
	installed := false
	if !probe.Available {
		if mode == "integrated" || mode == "" && probe.DuckwayPresent {
			result.err = fmt.Errorf("duckway is installed on %s but Ducklion is unavailable; run duckway start or duckway integrate ducklion on that host", client.Name)
			return result
		}
		if progress != nil {
			progress("installing ducklion on " + client.Name + "...")
		}
		installer, ok := runner.(remoteDucklionInstaller)
		if !ok {
			result.err = fmt.Errorf("cannot securely install Ducklion on %s: remote artifact installer is unavailable", client.Name)
			return result
		}
		binary, err := downloadDucklion(ctx, runner, probe.OS, probe.Arch)
		if err != nil {
			result.err = fmt.Errorf("download Ducklion for %s: %w", client.Name, err)
			return result
		}
		installedPath, err := installer.InstallDucklionBytes(ctx, client, binary, "")
		if err != nil {
			result.err = fmt.Errorf("ducklion was not installed on %s; host was not saved: %w", client.Name, err)
			return result
		} else {
			installed = true
			client.Ducklion = installedPath
			if progress != nil {
				progress("verifying ducklion on " + client.Name + "...")
			}
			probe, err = runner.ProbeDucklion(ctx, client)
			if err != nil {
				result.client, result.installed = client, true
				result.err = fmt.Errorf("ducklion was installed remotely, but host %s was not saved because verification failed: %w", client.Name, err)
				return result
			}
		}
	}
	if probe.Manager != "standalone" && probe.Manager != "integrated" {
		result.err = fmt.Errorf("ducklion management mode on %s is unavailable; host was not saved", client.Name)
		return result
	}
	if mode != "" && probe.Manager != mode {
		result.err = fmt.Errorf("ducklion on %s is %s-managed, not %s; host was not saved", client.Name, probe.Manager, mode)
		return result
	}
	if installed && probe.Manager != "standalone" {
		result.err = fmt.Errorf("installed Ducklion on %s did not report standalone management; host was not saved", client.Name)
		return result
	}
	if probe.Available && probe.Command != "" {
		client.Ducklion = probe.Command
	}
	if progress != nil {
		progress("connecting to Ducklion bridge on " + client.Name + "...")
	}
	sessions, err := runner.Sessions(ctx, client, 8)
	if err != nil {
		result.err = fmt.Errorf("ducklion bridge on %s is unavailable; host was not saved: %w", client.Name, err)
		return result
	}
	probe.ListOK = true
	probe.Sessions = len(sessions)
	result.client, result.probe, result.installed, result.installErr = client, probe, installed, installErr
	return result
}

func (s *tuiState) applyAddClientResult(result addClientDoneEvent) error {
	client, probe, installed, installErr := result.client, result.probe, result.installed, result.installErr
	next := s.cfg.Clone()
	if err := next.AddClient(client); err != nil {
		return err
	}
	if err := ducklord.SaveConfig(s.cfgPath, next); err != nil {
		if installed {
			return fmt.Errorf("ducklion was installed remotely, but host %s was not saved: %w", client.Name, err)
		}
		return err
	}
	*s.cfg = *next
	s.cancelAddClient()
	switch {
	case installed && probe.Available:
		s.outputErr = fmt.Sprintf("added %s; installed ducklion %s (%s, %d session(s))", client.Name, client.Ducklion, probe.Version, probe.Sessions)
	case probe.Available && probe.ListOK:
		s.outputErr = fmt.Sprintf("added %s (%s, %s, %d session(s))", client.Name, probe.Command, probe.Version, probe.Sessions)
	case probe.Available:
		s.outputErr = fmt.Sprintf("added %s (%s, %s; list check failed: %s)", client.Name, probe.Command, probe.Version, probe.ListError)
	case installErr != nil:
		s.outputErr = fmt.Sprintf("added %s; ducklion missing and install failed: %v", client.Name, installErr)
	default:
		s.outputErr = fmt.Sprintf("added %s; ducklion missing on remote", client.Name)
	}
	return nil
}

func (s *tuiState) removeClient(ctx context.Context, clientName string) error {
	if clientName == "" {
		return fmt.Errorf("no client selected")
	}
	next := s.cfg.Clone()
	if !next.RemoveClient(clientName) {
		return fmt.Errorf("unknown client %q", clientName)
	}
	if err := ducklord.SaveConfig(s.cfgPath, next); err != nil {
		return err
	}
	*s.cfg = *next
	s.cancelRemoveClient()
	s.outputText = ""
	s.outputForKey = ""
	s.refreshSessions(ctx)
	if !s.pooledOutput {
		s.refreshSelectedOutput(ctx)
	}
	s.outputErr = fmt.Sprintf("removed host entry %s from config", clientName)
	return nil
}

func (s *tuiState) removeSelectedClient(ctx context.Context) error {
	return s.removeClient(ctx, s.selectedClientName())
}

func (s *tuiState) clientFromAddLine(input string) (ducklord.Client, error) {
	if input == "" {
		return ducklord.Client{}, fmt.Errorf("ssh host is required")
	}
	target := input
	sshCommand := "ssh"
	if n, err := strconv.Atoi(input); err == nil {
		if n < 1 || n > len(s.addClientHosts) {
			return ducklord.Client{}, fmt.Errorf("ssh host number out of range")
		}
		target = s.addClientHosts[n-1].Name
	} else if fields, err := splitCommandLine(input); err == nil && len(fields) >= 2 && fields[0] == "ssh" {
		target = fields[len(fields)-1]
		if strings.HasPrefix(target, "-") {
			return ducklord.Client{}, fmt.Errorf("full ssh command must end with a host target")
		}
		sshCommand = strings.Join(fields[:len(fields)-1], " ")
	} else if err != nil {
		return ducklord.Client{}, err
	}
	user := ""
	host := target
	if strings.Contains(target, "@") {
		user, host, _ = strings.Cut(target, "@")
	}
	name := uniqueClientName(s.cfg, safeSlug(host))
	client := ducklord.Client{Name: name, Host: host, User: user, Group: "remote", Ducklion: "ducklion", SSH: sshCommand}
	if err := client.Normalize(); err != nil {
		return ducklord.Client{}, err
	}
	return client, nil
}

func uniqueClientName(cfg *ducklord.Config, base string) string {
	if base == "" {
		base = "remote"
	}
	if _, ok := cfg.Client(base); !ok {
		return base
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s-%d", base, i)
		if _, ok := cfg.Client(name); !ok {
			return name
		}
	}
}
