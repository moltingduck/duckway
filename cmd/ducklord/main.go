package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
	"github.com/hackerduck/duckway/internal/projectregistry"
	"github.com/hackerduck/duckway/internal/version"
	textwidth "golang.org/x/text/width"
)

var errRestartTUI = errors.New("restart ducklord TUI")

// Keep a lone ESC pending briefly so a normal arrow sequence can arrive in a
// separate read without making bare-Esc cancellation feel sluggish.
const inputEscapeAmbiguityTimeout = 25 * time.Millisecond

type remoteRunner interface {
	Sessions(context.Context, ducklord.Client, int) ([]ducklord.RemoteSession, error)
	Read(context.Context, ducklord.Client, string, int) (string, error)
	Send(context.Context, ducklord.Client, string, string) error
	Start(context.Context, ducklord.Client, []string) (string, error)
	Stop(context.Context, ducklord.Client, string) error
	Lifecycle(context.Context, ducklord.Client, string, protocol.SessionLifecycleOperation, protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error)
	LifecycleSelected(context.Context, ducklord.Client, ducklord.RemoteSession, protocol.SessionLifecycleOperation, protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error)
	Yield(context.Context, ducklord.Client, string, bool) (protocol.SessionYieldResult, error)
	YieldSelected(context.Context, ducklord.Client, ducklord.RemoteSession, bool) (protocol.SessionYieldResult, error)
	RenameSelected(context.Context, ducklord.Client, ducklord.RemoteSession, string) (protocol.SessionSummary, error)
	Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error)
	SuggestProjectPaths(context.Context, ducklord.Client, string) ([]string, error)
	EnsureDirectory(context.Context, ducklord.Client, string, bool) (ducklord.RemoteDirectoryStatus, error)
	AddProject(context.Context, ducklord.Client, string, string) (ducklord.RemoteProject, error)
	Agents(context.Context, ducklord.Client, string) ([]ducklord.RemoteAgent, error)
	ProbeDucklion(context.Context, ducklord.Client) (ducklord.DucklionProbe, error)
	HostLogRetention(context.Context, ducklord.Client) (int, error)
	HostResources(context.Context, ducklord.Client) (protocol.HostResourceStatus, error)
	SetHostLogRetention(context.Context, ducklord.Client, int) error
	ConfigureHostAgentHook(context.Context, ducklord.Client, string, string) (protocol.HostAgentHookConfigResult, error)
	HostAgentHookStatus(context.Context, ducklord.Client, string) (protocol.HostAgentHookStatus, error)
	InstallDucklion(context.Context, ducklord.Client, string, string) (string, error)
	Attach(ducklord.Client, string) error
	AttachStream(context.Context, ducklord.Client, string) (*ducklord.AttachSession, error)
}

type attachOutputEvent struct {
	id                int
	text              string
	runtimeGeneration uint64
	outputOffset      uint64
	startOffset       uint64
	done              bool
	err               error
}

type controlOpenEvent struct {
	id      int
	key     string
	control *ducklord.ControlSession
	err     error
}

type previewOutputEvent struct {
	id, generation uint64
	key            string
	text           string
	err            error
}

type startDoneEvent struct {
	id        int
	client    string
	sessionID string
	err       error
}

type pendingWorkspacePlacement struct {
	intent          workspacePaneIntent
	client          string
	instanceID      string
	sessionID       string
	generation      uint64
	connectionEpoch uint64
	createdAt       time.Time
}

type hostRetentionEvent struct {
	id         uint64
	epoch      uint64
	instanceID string
	host       string
	days       int
	err        error
	saved      bool
}
type hostResourceEvent struct {
	id         uint64
	epoch      uint64
	instanceID string
	host       string
	status     protocol.HostResourceStatus
	err        error
}

// publishHostResourceEvent never lets a refresh worker wait behind an older
// completion. The request, host epoch, and instance checks still decide
// whether a delivered event may affect state.
func publishHostResourceEvent(ctx context.Context, done chan<- hostResourceEvent, event hostResourceEvent) {
	select {
	case done <- event:
	case <-ctx.Done():
	default:
	}
}

type hostHookEvent struct {
	id, epoch                       uint64
	instanceID, host, agent, action string
	result                          protocol.HostAgentHookConfigResult
	err                             error
}

type hostSkillsEvent struct {
	id       uint64
	action   string
	host     string
	targetID string
	skillID  string
	preview  *ducklord.SkillPreview
	names    []string
	count    int
	err      error
}

type hostHookStatusEvent struct {
	id, epoch        uint64
	instanceID, host string
	statuses         map[string]protocol.HostAgentHookStatus
	err              error
}

func hostHookResultMessage(event hostHookEvent) string {
	if !event.result.Changed {
		return "Already in the requested state"
	}
	if event.action == "remove" {
		return "Host hook removed; agent settings were preserved"
	}
	return "Host configuration updated; activation awaits a real agent callback"
}

func hostHookStatusLine(status protocol.HostAgentHookStatus) string {
	if status.Agent != "codex" && status.Agent != "claude" {
		return "  Hook status unavailable"
	}
	installed := "not installed"
	if status.Installed {
		installed = "installed · pending activation"
		if status.Activation == "operational" {
			installed = "installed · operational (advisory)"
		}
	}
	callback := "no callback observed"
	if status.CallbackObserved {
		callback = "advisory callback observed"
		if status.CallbackUpdatedAtMS > 0 {
			callback += " " + time.UnixMilli(status.CallbackUpdatedAtMS).Local().Format("2006-01-02 15:04")
		}
	}
	return "  " + strings.ToUpper(status.Agent[:1]) + status.Agent[1:] + ": " + installed + " · " + callback
}

type createDiscoveryEvent struct {
	id, generation         uint64
	kind, client, instance string
	project                ducklord.RemoteProject
	projects               []ducklord.RemoteProject
	agents                 []ducklord.RemoteAgent
	sessionName            string
	failureStep            string
	args                   []string
	err                    error
	directory              ducklord.RemoteDirectoryStatus
	homePath               string
}

type hostSessionUpdate struct {
	update ducklord.SessionUpdate
	epoch  uint64
}

type hostWatchState struct {
	cancel context.CancelFunc
	epoch  uint64
}

type pathSuggestionEvent struct {
	id, generation          uint64
	client, instance, query string
	paths                   []string
	err                     error
}

type lifecycleDoneEvent struct {
	id              uint64
	watchEpoch      uint64
	connectionEpoch uint64
	hostGeneration  uint64
	hostInstance    string
	operation       protocol.SessionLifecycleOperation
	client          string
	result          protocol.SessionLifecycleResult
	err             error
}

type addClientDoneEvent struct {
	id         uint64
	progress   bool
	status     string
	client     ducklord.Client
	probe      ducklord.DucklionProbe
	installed  bool
	installErr error
	err        error
}

type resizeRequest struct {
	id     int
	rows   uint16
	cols   uint16
	resize func(uint16, uint16) (uint64, error)
}

type resizeDoneEvent struct {
	id         int
	rows, cols uint16
	err        error
	barrier    uint64
}

const maxResizeBufferedBytes = 256 << 10

func main() {
	runner := ducklord.NewRunner()
	err := run(os.Args[1:], os.Stdout, runner)
	runner.Close()
	if errors.Is(err, errRestartTUI) {
		executable, execErr := os.Executable()
		if execErr == nil {
			execErr = syscall.Exec(executable, os.Args, os.Environ())
		}
		log.Fatal("restart ducklord TUI: ", execErr)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func shouldDeferWorkspaceInput(s *tuiState, replayed bool) bool {
	// A placement fence belongs to the asynchronous PTY handoff. Keep the first
	// workspace key queued until the replacement control lease is accepted; the
	// replay is then routed through workspace navigation (for example Ctrl-]
	// followed by b, then p) instead of being consumed by the old PTY gate.
	return s.workspacePlacementInputPending && !s.focused && !replayed && !s.centralModalOpen() &&
		!s.workspaceProjectFocus
}

// shouldRouteWorkspaceProjectInputAsNavigation identifies the project-focus
// shortcut while a replacement PTY lease is still fenced. Route this key
// locally so the user can reach the Project pane during restoration.
func shouldRouteWorkspaceProjectInputAsNavigation(s *tuiState, input []byte) bool {
	return s.workspacePreview && !s.focused && !s.newSessionMode && !s.workspacePaneMode &&
		s.shortcut("project_focus", string(input))
}

func shouldRouteWorkspaceReplayAsNavigation(deferredReplay bool) bool {
	return deferredReplay
}

func run(args []string, out io.Writer, runner remoteRunner) error {
	globalOwner := ""
	if len(args) > 0 && args[0] == "--name" {
		if len(args) < 3 {
			return fmt.Errorf("--name requires a value and command")
		}
		globalOwner = args[1]
		args = args[2:]
	}
	if len(args) == 0 {
		printUsage(out)
		return fmt.Errorf("command is required")
	}
	switch args[0] {
	case "ssh-hosts":
		path := ""
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--config-file":
				if i+1 >= len(rest) {
					return fmt.Errorf("--config-file requires a value")
				}
				path = rest[i+1]
				i++
			default:
				return fmt.Errorf("unknown ssh-hosts option: %s", rest[i])
			}
		}
		hosts, err := ducklord.LoadSSHConfigHosts(path)
		if err != nil {
			return err
		}
		return printSSHHosts(out, hosts)
	case "clients":
		cfg, _, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		return printClients(out, cfg)
	case "import-ssh-hosts":
		cfgPath, sshConfig, err := parseImportSSHHostsArgs(args[1:])
		if err != nil {
			return err
		}
		lock, err := ducklord.AcquireInstanceLock()
		if err != nil {
			return err
		}
		defer lock.Close()
		cfg, err := loadOrEmptyConfig(cfgPath)
		if err != nil {
			return err
		}
		hosts, err := ducklord.LoadSSHConfigHosts(sshConfig)
		if err != nil {
			return err
		}
		n := importSSHHosts(cfg, hosts)
		if err := ducklord.SaveConfig(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(out, "Imported %d SSH host(s) into %s\n", n, resolvedConfigPath(cfgPath))
		return nil
	case "sessions":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		clientName, jsonOutput, err := parseSessionsArgs(rest)
		if err != nil {
			return err
		}
		c, err := mustClient(cfg, clientName)
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		sessions, err := runner.Sessions(context.Background(), c, 8)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(sessions)
		}
		return printSessions(out, sessions)
	case "bookmarks", "projects":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord bookmarks <client> [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		projects, err := runner.Projects(context.Background(), c)
		if err != nil {
			return err
		}
		return printProjects(out, projects)
	case "hook-status":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 || rest[1] != "codex" && rest[1] != "claude" {
			return fmt.Errorf("usage: ducklord hook-status <client> <codex|claude> [--config <path>]")
		}
		client, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		status, err := runner.HostAgentHookStatus(context.Background(), client, rest[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(status)
	case "agents":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord agents <client> <directory-path> [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		agents, err := runner.Agents(context.Background(), c, rest[1])
		if err != nil {
			return err
		}
		return printAgents(out, agents)
	case "probe":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord probe <client> [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		probe, err := runner.ProbeDucklion(context.Background(), c)
		if err != nil {
			return err
		}
		return printProbe(out, probe)
	case "install-ducklion":
		lock, err := ducklord.AcquireInstanceLock()
		if err != nil {
			return err
		}
		defer lock.Close()
		cfg, cfgPath, rest, source, dest, err := parseInstallDucklionArgs(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord install-ducklion <client> [--source <path>] [--dest <remote-path>] [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		installed, err := runner.InstallDucklion(context.Background(), c, source, dest)
		if err != nil {
			return err
		}
		c.Ducklion = installed
		replaceClient(cfg, c)
		if err := ducklord.SaveConfig(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Fprintf(out, "Installed ducklion on %s: %s\n", c.Name, installed)
		return nil
	case "read":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		clientName, sessionName, lines, err := parseReadArgs(rest)
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, clientName)
		if err != nil {
			return err
		}
		text, err := runner.Read(context.Background(), c, sessionName, lines)
		if err != nil {
			return err
		}
		fmt.Fprint(out, text)
		return nil
	case "retained":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord retained <client> [--config <path>]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		diagnostics, ok := runner.(interface {
			RetainedShells(context.Context, ducklord.Client) ([]protocol.RetainedShellSummary, error)
		})
		if !ok {
			return fmt.Errorf("retained Shell diagnostics are unavailable")
		}
		items, err := diagnostics.RetainedShells(context.Background(), c)
		if err != nil {
			return err
		}
		for _, item := range items {
			fmt.Fprintf(out, "%s  generation %d  %s  exited %s\n", item.SessionID, item.RuntimeGeneration,
				item.Handle, time.UnixMilli(item.ExitedAtMS).Local().Format(time.RFC3339))
		}
		return nil
	case "read-retained":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) < 3 {
			return fmt.Errorf("usage: ducklord read-retained <client> <session-id> <generation> [--lines N] [--config <path>]")
		}
		generation, err := strconv.ParseUint(rest[2], 10, 64)
		if err != nil || generation == 0 {
			return fmt.Errorf("invalid retained Shell generation %q", rest[2])
		}
		readArgs := append([]string{rest[0], rest[1]}, rest[3:]...)
		clientName, sessionID, lines, err := parseReadArgs(readArgs)
		if err != nil {
			return err
		}
		c, err := mustClient(cfg, clientName)
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		diagnostics, ok := runner.(interface {
			ReadRetainedShell(context.Context, ducklord.Client, string, uint64, int) (string, error)
		})
		if !ok {
			return fmt.Errorf("retained Shell diagnostics are unavailable")
		}
		text, err := diagnostics.ReadRetainedShell(context.Background(), c, sessionID, generation, lines)
		if err != nil {
			return err
		}
		fmt.Fprint(out, text)
		return nil
	case "send":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) < 3 {
			return fmt.Errorf("usage: ducklord send <client> <session> <text>")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Send(context.Background(), c, rest[1], strings.Join(rest[2:], " "))
	case "start":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) < 2 {
			return fmt.Errorf("usage: ducklord start <client> --name <name> [--kind shell | --agent <agent>] [--cwd <dir>] -- CMD [ARGS...]")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		startArgs, err := parseDucklordStartArgs(rest[1:])
		if err != nil {
			return err
		}
		_, err = runner.Start(context.Background(), c, startArgs)
		return err
	case "stop":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord stop <client> <session>")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Stop(context.Background(), c, rest[1])
	case "end", "destroy", "restart":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		operation, mode, clientName, sessionRef, err := parseDucklordLifecycle(args[0], rest)
		if err != nil {
			return err
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, clientName)
		if err != nil {
			return err
		}
		lifecycleCtx, stopLifecycleWait := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stopLifecycleWait()
		result, err := runner.Lifecycle(lifecycleCtx, c, sessionRef, operation, mode)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s (generation %d)\n", map[string]string{"end": "Ended", "destroy": "Destroyed", "restart": "Restarted"}[args[0]], result.SessionID, result.RuntimeGeneration)
		return nil
	case "yield":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		wait := false
		filtered := make([]string, 0, len(rest))
		for _, arg := range rest {
			if arg == "-w" || arg == "--wait" {
				wait = true
			} else {
				filtered = append(filtered, arg)
			}
		}
		if len(filtered) != 2 {
			return fmt.Errorf("usage: ducklord yield <client> <session> [-w|--wait]")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, filtered[0])
		if err != nil {
			return err
		}
		result, err := runner.Yield(context.Background(), c, filtered[1], wait)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Yield %s: %s (epoch %d)\n", result.SessionID, result.Decision, result.OwnershipEpoch)
		return nil
	case "attach":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord attach <client> <session>")
		}
		owner, err := ducklord.ResolveOwnerName(globalOwner, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		attach, err := runner.AttachStream(context.Background(), c, rest[1])
		if err != nil {
			return err
		}
		defer attach.Stdin.Close()
		defer attach.Stdout.Close()
		if oldState, rawErr := makeRaw(); rawErr == nil {
			defer restore(oldState)
		}
		resize := func() {
			if attach.Resize == nil {
				return
			}
			cols, rows := terminalSize()
			_ = attach.Resize(uint16(rows), uint16(cols))
		}
		resize()
		resizeSignals := make(chan os.Signal, 1)
		resizeDone := make(chan struct{})
		signal.Notify(resizeSignals, syscall.SIGWINCH)
		defer func() { signal.Stop(resizeSignals); close(resizeDone) }()
		go func() {
			for {
				select {
				case <-resizeSignals:
					resize()
				case <-resizeDone:
					return
				}
			}
		}()
		copyInputDone := make(chan struct{})
		go func() { _, _ = io.Copy(attach.Stdin, os.Stdin); close(copyInputDone) }()
		_, outputErr := io.Copy(out, attach.Stdout)
		select {
		case attachErr := <-attach.Done:
			if attachErr != nil {
				return attachErr
			}
		default:
		}
		return outputErr
	case "attach-host":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord attach-host <client> [--config <path>]")
		}
		hostCfg, err := attachHostConfig(cfg, rest[0])
		if err != nil {
			return err
		}
		return runHostTUI(hostCfg, runner, 2*time.Second, globalOwner)
	case "tui":
		cfgPath, ownerFlag, refresh, err := parseTUIFlags(args[1:])
		if err != nil {
			return err
		}
		cfg, err := loadOrEmptyConfig(cfgPath)
		if err != nil {
			return err
		}
		if globalOwner != "" && ownerFlag != "" {
			return fmt.Errorf("--name may only be specified once")
		}
		if ownerFlag == "" {
			ownerFlag = globalOwner
		}
		owner, err := ducklord.ResolveOwnerName(ownerFlag, cfg.Name)
		if err != nil {
			return err
		}
		if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
			ownerRunner.SetOwner(owner)
		}
		lock, err := ducklord.AcquireInstanceLock()
		if err != nil {
			return err
		}
		defer lock.Close()
		return runTUI(cfg, runner, cfgPath, refresh, owner)
	case "version", "--version", "-v":
		fmt.Fprintln(out, "ducklord", version.Get())
		return nil
	case "help", "--help", "-h":
		printUsage(out)
		return nil
	default:
		return fmt.Errorf("unknown ducklord command: %s", args[0])
	}
}

func printSSHHosts(out io.Writer, hosts []ducklord.SSHHost) error {
	if len(hosts) == 0 {
		fmt.Fprintln(out, "No concrete SSH hosts found.")
		return nil
	}
	fmt.Fprintf(out, "%-32s %s\n", "HOST", "SOURCE")
	for _, h := range hosts {
		fmt.Fprintf(out, "%-32s %s\n", displayField(h.Name), displayField(h.File))
	}
	return nil
}

func loadWithFlags(args []string) (*ducklord.Config, []string, error) {
	path := ""
	rest := []string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--config requires a value")
			}
			path = args[i+1]
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	cfg, err := ducklord.LoadConfig(path)
	return cfg, rest, err
}

func loadOrEmptyConfig(path string) (*ducklord.Config, error) {
	cfg, err := ducklord.LoadConfig(path)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return &ducklord.Config{}, nil
	}
	return nil, err
}

func resolvedConfigPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ducklord.DefaultConfigPath()
	}
	return path
}

func parseImportSSHHostsArgs(args []string) (cfgPath, sshConfig string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--config requires a value")
			}
			cfgPath = args[i+1]
			i++
		case "--ssh-config":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--ssh-config requires a value")
			}
			sshConfig = args[i+1]
			i++
		default:
			return "", "", fmt.Errorf("unknown import-ssh-hosts option: %s", args[i])
		}
	}
	return cfgPath, sshConfig, nil
}

func parseInstallDucklionArgs(args []string) (*ducklord.Config, string, []string, string, string, error) {
	cfgPath := ""
	source := ""
	dest := ""
	rest := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return nil, "", nil, "", "", fmt.Errorf("--config requires a value")
			}
			cfgPath = args[i+1]
			i++
		case "--source":
			if i+1 >= len(args) {
				return nil, "", nil, "", "", fmt.Errorf("--source requires a value")
			}
			source = args[i+1]
			i++
		case "--dest":
			if i+1 >= len(args) {
				return nil, "", nil, "", "", fmt.Errorf("--dest requires a value")
			}
			dest = args[i+1]
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	cfg, err := ducklord.LoadConfig(cfgPath)
	return cfg, cfgPath, rest, source, dest, err
}

func importSSHHosts(cfg *ducklord.Config, hosts []ducklord.SSHHost) int {
	added := 0
	for _, h := range hosts {
		if hasClientTarget(cfg, h.Name) {
			continue
		}
		client := ducklord.Client{Name: uniqueClientName(cfg, safeSlug(h.Name)), Host: h.Name, Group: "ssh", Ducklion: "ducklion", SSH: "ssh"}
		if err := cfg.AddClient(client); err == nil {
			added++
		}
	}
	return added
}

func hasClientTarget(cfg *ducklord.Config, target string) bool {
	for _, c := range cfg.Clients {
		if c.Target() == target || c.Host == target {
			return true
		}
	}
	return false
}

func replaceClient(cfg *ducklord.Config, client ducklord.Client) {
	for i := range cfg.Clients {
		if cfg.Clients[i].Name == client.Name {
			cfg.Clients[i] = client
			return
		}
	}
	cfg.Clients = append(cfg.Clients, client)
}

func parseTUIFlags(args []string) (string, string, time.Duration, error) {
	path := ""
	owner := ""
	refresh := 2 * time.Second
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return "", "", 0, fmt.Errorf("--config requires a value")
			}
			path = args[i+1]
			i++
		case "--name":
			if i+1 >= len(args) {
				return "", "", 0, fmt.Errorf("--name requires a value")
			}
			owner = args[i+1]
			i++
		case "--refresh":
			if i+1 >= len(args) {
				return "", "", 0, fmt.Errorf("--refresh requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d < time.Second {
				return "", "", 0, fmt.Errorf("invalid --refresh value")
			}
			refresh = d
			i++
		default:
			return "", "", 0, fmt.Errorf("unknown tui option: %s", args[i])
		}
	}
	return path, owner, refresh, nil
}

func mustClient(cfg *ducklord.Config, name string) (ducklord.Client, error) {
	c, ok := cfg.Client(name)
	if !ok {
		return ducklord.Client{}, fmt.Errorf("unknown client %q", name)
	}
	return c, nil
}

func attachHostConfig(cfg *ducklord.Config, clientName string) (*ducklord.Config, error) {
	c, err := mustClient(cfg, clientName)
	if err != nil {
		return nil, err
	}
	out := *cfg
	out.Clients = []ducklord.Client{c}
	return &out, nil
}

func printClients(out io.Writer, cfg *ducklord.Config) error {
	fmt.Fprintf(out, "%-18s %-12s %-24s %s\n", "NAME", "GROUP", "TARGET", "DUCKLION")
	for _, c := range cfg.Clients {
		fmt.Fprintf(out, "%-18s %-12s %-24s %s\n", c.Name, c.Group, c.Target(), c.Ducklion)
	}
	return nil
}

func printProjects(out io.Writer, projects []ducklord.RemoteProject) error {
	if len(projects) == 0 {
		fmt.Fprintln(out, "No remote bookmarks.")
		return nil
	}
	fmt.Fprintf(out, "%-18s %-40s %s\n", "NAME", "PATH", "SOURCE")
	for _, p := range projects {
		fmt.Fprintf(out, "%-18s %-40s %s\n", displayField(p.Name), displayField(p.Path), displayField(p.Source))
	}
	return nil
}

func printAgents(out io.Writer, agents []ducklord.RemoteAgent) error {
	if len(agents) == 0 {
		fmt.Fprintln(out, "No available agents for this directory.")
		return nil
	}
	fmt.Fprintf(out, "%-14s %s\n", "TYPE", "COMMAND")
	for _, agent := range agents {
		fmt.Fprintf(out, "%-14s %s\n", displayField(agent.Type), displayField(strings.Join(agent.Command, " ")))
	}
	return nil
}

func printProbe(out io.Writer, probe ducklord.DucklionProbe) error {
	if !probe.Available {
		fmt.Fprintln(out, "ducklion: missing")
		return nil
	}
	fmt.Fprintf(out, "ducklion: available\ncommand: %s\nversion: %s\n", probe.Command, probe.Version)
	if probe.ListOK {
		fmt.Fprintf(out, "sessions: %d\n", probe.Sessions)
	} else if probe.ListError != "" {
		fmt.Fprintf(out, "sessions: error: %s\n", probe.ListError)
	}
	return nil
}

func printSessions(out io.Writer, sessions []ducklord.RemoteSession) error {
	if len(sessions) == 0 {
		fmt.Fprintln(out, "No remote sessions.")
		return nil
	}
	fmt.Fprintf(out, "%-12s %-18s %-10s %-16s %s\n", "CLIENT", "SESSION", "STATUS", "TYPE", "LAST")
	for _, s := range sessions {
		fmt.Fprintf(out, "%-12s %-18s %-10s %-16s %s\n", displayField(s.Client), displayField(s.Name), displayField(s.Status), displayField(sessionTypeLabel(s)), truncate(sanitizeTerminalText(s.LastLine), 80))
	}
	return nil
}

func parseSessionsArgs(args []string) (client string, jsonOutput bool, err error) {
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOutput = true
		default:
			if client != "" {
				return "", false, fmt.Errorf("usage: ducklord sessions <client> [--json] [--config <path>]")
			}
			client = arg
		}
	}
	if client == "" {
		return "", false, fmt.Errorf("usage: ducklord sessions <client> [--json] [--config <path>]")
	}
	return client, jsonOutput, nil
}

func parseReadArgs(args []string) (clientName, sessionName string, lines int, err error) {
	lines = 120
	pos := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--lines", "-n":
			if i+1 >= len(args) {
				return "", "", 0, fmt.Errorf("--lines requires a value")
			}
			lines, err = strconv.Atoi(args[i+1])
			if err != nil || lines <= 0 {
				return "", "", 0, fmt.Errorf("invalid --lines value")
			}
			i++
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) != 2 {
		return "", "", 0, fmt.Errorf("usage: ducklord read <client> <session> [--lines N]")
	}
	return pos[0], pos[1], lines, nil
}

func parseCreateLine(line string) (sessionName string, args []string, err error) {
	fields, err := splitCommandLine(line)
	if err != nil {
		return "", nil, err
	}
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("session name is required")
	}
	sessionName = fields[0]
	agent := ""
	cwd := ""
	rest := fields[1:]
	commandAt := len(rest)
	for i, field := range rest {
		if field == "--" {
			commandAt = i
			break
		}
	}
	options := rest[:commandAt]
	for i := 0; i < len(options); i++ {
		switch options[i] {
		case "--agent":
			if i+1 >= len(options) {
				return "", nil, fmt.Errorf("--agent requires a value")
			}
			agent = options[i+1]
			i++
		case "--cwd", "-C":
			if i+1 >= len(options) {
				return "", nil, fmt.Errorf("%s requires a value", options[i])
			}
			cwd = options[i+1]
			i++
		default:
			return "", nil, fmt.Errorf("unknown new session option: %s", options[i])
		}
	}
	command := []string{"bash"}
	if commandAt < len(rest) {
		command = rest[commandAt+1:]
		if len(command) == 0 {
			return "", nil, fmt.Errorf("command is required after --")
		}
	}
	args, err = buildStartArgs(sessionName, agent, cwd, command)
	if err != nil {
		return "", nil, err
	}
	return sessionName, args, nil
}

func parseDucklordLifecycle(command string, args []string) (protocol.SessionLifecycleOperation, protocol.SessionLifecycleMode, string, string, error) {
	operation := protocol.SessionLifecycleOperation(command)
	if operation != protocol.SessionLifecycleEnd && operation != protocol.SessionLifecycleDestroy && operation != protocol.SessionLifecycleRestart {
		return "", "", "", "", fmt.Errorf("unknown lifecycle command %q", command)
	}
	mode := protocol.SessionLifecycleImmediate
	if operation == protocol.SessionLifecycleRestart {
		mode = protocol.SessionLifecycleWait
	}
	modeSeen := false
	positionals := make([]string, 0, 2)
	for _, arg := range args {
		switch arg {
		case "-w", "--wait":
			if modeSeen || operation == protocol.SessionLifecycleRestart {
				return "", "", "", "", fmt.Errorf("usage: ducklord %s <client> <session> [-w|--wait|-f|--force]", command)
			}
			mode = protocol.SessionLifecycleWait
			modeSeen = true
		case "-f", "--force":
			if modeSeen {
				return "", "", "", "", fmt.Errorf("usage: ducklord %s <client> <session> [-w|--wait|-f|--force]", command)
			}
			mode = protocol.SessionLifecycleForce
			modeSeen = true
		default:
			if strings.HasPrefix(arg, "-") {
				return "", "", "", "", fmt.Errorf("unknown %s option: %s", command, arg)
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 2 {
		return "", "", "", "", fmt.Errorf("usage: ducklord %s <client> <session> [-w|--wait|-f|--force]", command)
	}
	return operation, mode, positionals[0], positionals[1], nil
}

func parseDucklordStartArgs(args []string) ([]string, error) {
	name := ""
	agent := ""
	kind := model.KindAgent
	cwd := ""
	command := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			command = append([]string(nil), args[i+1:]...)
			i = len(args)
		case "--name", "-n":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s requires a value", args[i])
			}
			name = args[i+1]
			i++
		case "--agent":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--agent requires a value")
			}
			agent = args[i+1]
			i++
		case "--kind":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--kind requires a value")
			}
			kind = model.SessionKind(args[i+1])
			i++
		case "--cwd", "-C":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s requires a value", args[i])
			}
			cwd = args[i+1]
			i++
		default:
			return nil, fmt.Errorf("unknown start option: %s", args[i])
		}
	}
	return buildStartArgsKind(name, kind, agent, cwd, command)
}

func buildStartArgs(name, agent, cwd string, command []string) ([]string, error) {
	return buildStartArgsProject(name, model.KindAgent, agent, "", cwd, command)
}

func buildStartArgsKind(name string, kind model.SessionKind, agent, cwd string, command []string) ([]string, error) {
	return buildStartArgsProject(name, kind, agent, "", cwd, command)
}

func buildStartArgsProject(name string, kind model.SessionKind, agent, projectName, cwd string, command []string) ([]string, error) {
	if err := validateSessionHandle(name); err != nil {
		return nil, err
	}
	out := []string{"--name", name}
	switch kind {
	case model.KindAgent:
	case model.KindShell:
		if agent != "" {
			return nil, fmt.Errorf("shell sessions do not accept --agent")
		}
		if len(command) != 1 {
			return nil, fmt.Errorf("shell sessions require exactly one shell executable after --")
		}
		out = append(out, "--kind", string(model.KindShell))
	default:
		return nil, fmt.Errorf("invalid --kind value %q", kind)
	}
	if agent != "" {
		if !ducklord.SafeIdentifier(agent) {
			return nil, fmt.Errorf("invalid --agent value %q", agent)
		}
		out = append(out, "--agent", agent)
	}
	if cwd != "" {
		out = append(out, "--cwd", cwd)
	}
	if projectName != "" {
		out = append(out, "--project-name", projectName)
	}
	if len(command) == 0 {
		return nil, fmt.Errorf("command is required after --")
	}
	out = append(out, "--")
	out = append(out, command...)
	return out, nil
}

func safeSlug(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "session"
	}
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "-")
	}
	if out == "" {
		out = "session"
	}
	return out
}

func splitCommandLine(line string) ([]string, error) {
	var fields []string
	var b strings.Builder
	var quote rune
	escaped := false
	have := false
	for _, r := range line {
		if escaped {
			b.WriteRune(r)
			have = true
			escaped = false
			continue
		}
		if quote != '\'' && r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				have = true
				continue
			}
			b.WriteRune(r)
			have = true
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			have = true
		case ' ', '\t', '\n', '\r':
			if have {
				fields = append(fields, b.String())
				b.Reset()
				have = false
			}
		default:
			b.WriteRune(r)
			have = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("unfinished escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if have {
		fields = append(fields, b.String())
	}
	return fields, nil
}

type tuiState struct {
	modalMouseRegions    []modalMouseRegion
	modalMouseLines      map[int]modalMouseAction
	cfg                  *ducklord.Config
	cfgPath              string
	runner               remoteRunner
	refresh              time.Duration
	sessions             []ducklord.RemoteSession
	promoted             map[string]bool
	selected             int
	selectedGroupID      string
	dragSession          ducklord.SessionIdentity
	dragTargetGroup      string
	dragTargetSession    ducklord.SessionIdentity
	hashes               map[string]string
	selectedKey          string
	outputText           string
	outputErr            string
	localWarning         string
	notificationSink     notificationDelivery
	attentionDeliveryAt  map[string]time.Time
	notificationNow      func() time.Time
	notificationObserved map[string]map[model.NotificationCategory]uint64
	pendingBells         uint8
	resizeStatus         string
	outputForKey         string
	outputStale          bool
	outputFresh          bool
	outputReconnecting   bool
	terminal             *ducklord.Terminal
	terminalGeneration   uint64
	terminalOffset       uint64
	terminalCursorValid  bool
	activeAttachKey      string
	activeAttachFresh    bool
	pendingAttachKey     string
	snapshotStore        ducklord.SnapshotStore
	activityStore        ducklord.ActivityStateStore
	activityState        *ducklord.ActivityState
	focused              bool
	copyMode             bool
	copyRendering        bool
	frameOutput          frameOutput
	// terminalCookedState is the state captured before the TUI entered raw mode.
	// Notes editors temporarily restore it while they own the terminal.
	terminalCookedState                *termState
	copyTerminal                       *ducklord.Terminal
	newSessionMode                     bool
	newSessionClient                   string
	newSessionLine                     string
	newSessionSelected                 int
	newSessionErr                      string
	newSessionStarting                 bool
	newSessionStartGeneration          uint64
	newSessionStartInstance            string
	newSessionStartEpoch               uint64
	newSessionDiscovering              bool
	newSessionRequestID                uint64
	newSessionCancel                   context.CancelFunc
	createWorkers                      sync.WaitGroup
	newSessionStep                     string
	newSessionKind                     model.SessionKind
	newSessionAgent                    string
	newSessionCommand                  []string
	newSessionProjects                 []ducklord.RemoteProject
	newSessionProject                  ducklord.RemoteProject
	newSessionAgents                   []ducklord.RemoteAgent
	newSessionCWD                      string
	newSessionPathSuggestions          []string
	newSessionPathSelected             int
	newSessionPathRequestID            uint64
	newSessionPathCancel               context.CancelFunc
	newSessionPathBusy                 bool
	newSessionPathCompletion           string
	addClientMode                      bool
	addClientStep                      string
	addClientProvisionMode             string
	addClientModeSelected              int
	addClientLine                      string
	addClientSelected                  int
	addClientErr                       string
	addClientHosts                     []ducklord.SSHHost
	addClientBusy                      bool
	addClientRequestID                 uint64
	addClientCancel                    context.CancelFunc
	removeClientMode                   bool
	removeClientSelected               int
	removeClientConfirm                string
	helpMode                           bool
	helpOriginFocused                  bool
	helpOffset                         int
	helpSearchActive                   bool
	helpSearchQuery                    string
	ptyScrollOffset                    int
	shortcutMode                       bool
	shortcutStep                       string
	shortcutIndex                      int
	shortcutLine                       string
	shortcutErr                        string
	shortcutDraft                      *ducklord.Config
	notificationConfigMode             bool
	notificationConfigStep             string
	notificationConfigScope            string
	notificationConfigHost             string
	notificationConfigIndex            int
	notificationConfigChoice           int
	notificationConfigPath             string
	notificationConfigErr              string
	notificationConfigDraft            *ducklord.Config
	notificationConfigBase             []byte
	hostMenuMode                       bool
	hostMenuStep                       string
	hostMenuTarget                     string
	hostMenuIndex                      int
	hostMenuSelected                   map[string]bool
	hostMenuErr                        string
	hostMenuOldDays                    int
	hostMenuDraft                      string
	hostMenuReplaceDraft               bool
	hostMenuRequestID                  uint64
	hostResourceStatus                 protocol.HostResourceStatus
	hostSkillsIndex                    int
	hostSkillsSourceIndex              int
	hostSkillsField                    int
	hostSkillsDraftURL                 string
	hostSkillsDraftSkillID             string
	hostSkillsDraftInsecure            bool
	hostSkillsDraftTargetID            string
	hostSkillsDraftTargetPath          string
	hostSkillsSelected                 map[string]bool
	hostSkillsRemote                   []string
	hostSkillsErr                      string
	hostSkillsRepoErr                  string
	hostSkillsRepository               string
	hostSkillsPreview                  *ducklord.SkillPreview
	hostSkillsPreviewAction            string
	hostSkillsTargetIndex              int
	hostSkillsPendingID                string
	hostSkillsPendingManagement        string
	hostSkillsDraftImportPath          string
	hostSkillsDraftImportID            string
	hostSkillsBusy                     bool
	hostSkillsRequestID                uint64
	hostSkillsListRequestID            uint64
	hostSkillsCancel                   context.CancelFunc
	hostSkillsListCancel               context.CancelFunc
	hostSkillsDone                     chan hostSkillsEvent
	hostSkillsPane                     int // 0 managed repository, 1 agent tree
	hostSkillsTargetRemoteIndex        int
	hostSkillsRemoteByTarget           map[string][]string
	hostSkillsTargetExpanded           map[string]bool
	hostSkillsRenameOld                string
	hostSkillsRenameDraft              string
	hostHookInFlight                   map[string]uint64
	hostHookStatuses                   map[string]protocol.HostAgentHookStatus
	hostHookStatusRequestID            uint64
	hostHookStatusLoading              bool
	hostHookStatusErr                  string
	hostHookAgent                      string
	hostHookAction                     string
	disconnectedHosts                  map[string]bool
	hostScoped                         bool
	ownerName                          string
	listPaneWidth                      int
	autoHideList                       bool
	hostSync                           map[string]ducklord.SessionUpdate
	hostConnectionEpoch                map[string]uint64
	eventDriven                        bool
	notificationMode                   bool
	notificationIndex                  int
	notificationStaged                 map[model.NotificationCategory]bool
	notificationLevelsStaged           map[ducklord.NotificationClass]ducklord.NotificationLevel
	notificationLevelEditing           bool
	notificationLevelChoice            int
	notificationTarget                 ducklord.RemoteSession
	actionMenu                         bool
	actionIndex                        int
	actionTarget                       ducklord.RemoteSession
	actionOperation                    protocol.SessionLifecycleOperation
	actionMode                         protocol.SessionLifecycleMode
	sessionRenameMode                  bool
	sessionRenameTarget                ducklord.RemoteSession
	sessionRenameLine                  string
	sessionRenameErr                   string
	lifecycleConfirm                   protocol.SessionLifecycleOperation
	projectFiles                       projectFilesState
	projectFilesHistory                map[string][]projectFilesBatch
	lifecycleReturnToAction            bool
	lifecycleTarget                    ducklord.RemoteSession
	lifecycleMode                      protocol.SessionLifecycleMode
	lifecycleBusy                      bool
	commandPaletteMode                 bool
	commandPaletteQuery                string
	commandPaletteIndex                int
	commandPalettePreviousFocused      bool
	commandPalettePreviousAttachKey    string
	commandPalettePreviousProjectFocus bool
	commandPalettePreviousProjectID    string
	commandPalettePreviousPaneID       string
	commandPaletteWorkspaceGeneration  uint64
	commandPaletteInput                []byte
	groupMenu                          bool
	groupMenuStep                      string
	groupMenuAction                    string
	groupMenuIndex                     int
	groupMenuLine                      string
	groupMenuErr                       string
	groupMenuTarget                    string
	groupMenuSession                   ducklord.SessionIdentity
	pooledOutput                       bool
	workspacePreview                   bool
	workspaceOutput                    *ducklord.WorkspaceOutputAdapter
	workspaceNav                       *ducklord.WorkspaceState
	workspaceProjectFocus              bool
	workspaceConfigFocus               string
	workspaceConfigIdentity            ducklord.SessionIdentity
	panePrefixPending                  bool
	quickShellOrigin                   *workspacePaneIntent
	panePrefixSuffix                   string
	panePrefixDeadline                 time.Time
	panePrefixReplay                   []byte
	workspaceAttachFromProject         bool
	workspaceFocusFromProject          bool
	workspaceMouseFocus                bool
	workspacePaneMode                  bool
	workspacePaneStep                  string
	workspacePaneIndex                 int
	workspacePaneErr                   string
	workspacePaneName                  string
	workspacePaneHosts                 []string
	workspacePaneQuery                 string
	workspacePaneIntent                workspacePaneIntent
	workspacePaneSourceID              string
	workspacePaneIdentity              ducklord.SessionIdentity
	workspacePaneCandidate             ducklord.RemoteSession
	projectTransferPath                string
	projectTransferData                []byte
	projectTransfer                    ducklord.ProjectTransfer
	projectTransferPreview             ducklord.ProjectImportPreview
	projectTransferAction              string
	workspacePaneRestoreFocused        bool
	workspacePaneRestoreAttachKey      string
	workspacePaneFocusRestorePending   bool
	workspacePaneFocusRestoreLease     bool
	workspacePaneFocusRestoreKey       string
	workspacePaneFocusRestoreSession   ducklord.RemoteSession
	workspaceDragSession               ducklord.RemoteSession
	workspaceDragKind                  string
	workspaceDragProjectID             string
	workspaceDragTabID                 string
	workspaceDragMoved                 bool
	workspaceDragX                     int
	workspaceDragY                     int
	workspaceProjectOffset             int
	workspaceQuickOffset               int
	workspacePaneChanged               bool
	workspacePlacementFocusPending     bool
	workspacePlacementInputPending     bool
	notesEntries                       []ducklord.NoteEntry
	notesScope                         ducklord.NotesScope
	notesSessionID                     string
	notesSessionIdentity               ducklord.SessionIdentity
	notesEntryIndexes                  []int
	notesEntryScopes                   []ducklord.NotesScope
	notesEntryIdentities               []string
	notesEntryOrigins                  []string
	notesPickerMode                    string
	notesPickerChoices                 []string
	notesPickerLabels                  []string
	notesPickerIndex                   int
	notesQuery                         string
	notesSearchActive                  bool
	notesProjectID                     string
	notesPreviousProjectID             string
	notesPreviousPaneID                string
	notesPreviousProjectFocus          bool
	notesPreviousFocused               bool
	notesFocusRestorePending           bool
	notesPendingInputKey               string
	notesPendingInput                  []byte
	notesFocusRestoreReselect          bool
	notesPreviousAttachKey             string
	notesPreviousAttachValid           bool
	helpFocusRestorePending            bool
	helpPendingInputKey                string
	notesFormActive                    bool
	notesFormIndex                     int
	notesFormRawIndex                  int
	notesFormField                     int
	notesFormTitle                     string
	notesFormBody                      string
	notesFormCursor                    int
	workspaceNewSessionIntent          *workspacePaneIntent
	pendingWorkspacePlacements         []pendingWorkspacePlacement
	detailQuery                        string
	detailFilter                       ducklord.DetailFilter
	detailSearchFocused                bool
	detailReturnProjectFocus           bool
	detailSelected                     ducklord.SessionIdentity
	clearOutputFocus                   func()
	searchMode                         bool
	terminalSearchMode                 bool
	terminalSearchQuery                string
	terminalSearchText                 string
	terminalSearchSelected             int
	terminalBookmarkMode               bool
	terminalBookmarkLabel              string
	terminalToolInput                  []byte
	terminalBookmarkListMode           bool
	terminalBookmarkSelected           int
	terminalBookmarkPreviousFocus      string
	searchQuery                        string
	searchSelected                     int
	searchSelectedKey                  string
	searchRevision                     uint64
	searchActivatedKey                 string
	searchActivatedRevision            uint64
	searchActivatedGeneration          uint64
	searchErr                          string
	searchPendingRequestID             uint64
	searchPendingKey                   string
	searchPendingGeneration            uint64
	searchPendingRevision              uint64
}

const notesPendingInputLimit = 4096

func runTUI(cfg *ducklord.Config, runner remoteRunner, cfgPath string, refresh time.Duration, owner string) error {
	return runTUIWithOptions(cfg, runner, cfgPath, refresh, false, owner)
}

func runHostTUI(cfg *ducklord.Config, runner remoteRunner, refresh time.Duration, ownerFlag string) error {
	owner, err := ducklord.ResolveOwnerName(ownerFlag, cfg.Name)
	if err != nil {
		return err
	}
	if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
		ownerRunner.SetOwner(owner)
	}
	lock, err := ducklord.AcquireInstanceLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return runTUIWithOptions(cfg, runner, "", refresh, true, owner)
}

func runTUIWithOptions(cfg *ducklord.Config, runner remoteRunner, cfgPath string, refresh time.Duration, hostScoped bool, owner string) error {
	if limiter, ok := runner.(interface{ SetOutputSubscriptionLimit(int) error }); ok {
		if err := limiter.SetOutputSubscriptionLimit(cfg.RawOutputSubscriptionLimit()); err != nil {
			return err
		}
	}
	oldState, err := makeRaw()
	if err != nil {
		return err
	}
	defer restore(oldState)
	fmt.Print("\033[?1049h\033[?25l\033[?1002h\033[?1006h")
	defer fmt.Print(mouseCursorShape("default") + "\033[?1006l\033[?1002l\033[?25h\033[?1049l")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	activityStore := ducklord.ActivityStateStore{}
	activityState, err := activityStore.Load()
	stateWarning := ""
	var recovered *ducklord.CorruptStateRecoveredError
	var stateLoadWarning *ducklord.StateLoadWarning
	if errors.As(err, &recovered) && activityState != nil {
		stateWarning = recovered.Error()
	} else if errors.As(err, &stateLoadWarning) && activityState != nil {
		stateWarning = stateLoadWarning.Error()
	} else if err != nil {
		return fmt.Errorf("load Ducklord activity state: %w", err)
	}
	if warning := soundSetupWarning(cfg); warning != "" {
		if stateWarning != "" {
			stateWarning += "; "
		}
		stateWarning += warning
	}
	if warning := desktopSetupWarning(cfg, activityState); warning != "" {
		if stateWarning != "" {
			stateWarning += "; "
		}
		stateWarning += warning
	}
	state := &tuiState{cfg: cfg, cfgPath: cfgPath, runner: runner, refresh: refresh, hashes: map[string]string{}, hostScoped: hostScoped, ownerName: owner,
		terminalCookedState: oldState,
		snapshotStore:       ducklord.SnapshotStore{}, activityStore: activityStore, activityState: activityState, hostSync: make(map[string]ducklord.SessionUpdate), localWarning: stateWarning,
		listPaneWidth: cfg.SessionListPaneWidth(), autoHideList: cfg.SessionListAutoHide(), disconnectedHosts: make(map[string]bool),
		workspacePreview: os.Getenv("DUCKLORD_LEGACY_TUI") != "1"}
	state.notificationSink = newLocalNotificationSink()
	defer state.notificationSink.Close()
	var outputManager tuiOutputManager
	var workspaceOutput *ducklord.WorkspaceOutputAdapter
	var outputEvents <-chan ducklord.TerminalOutputEvent
	var workspaceRepaint <-chan struct{}
	if source, ok := runner.(ducklord.TerminalOutputSource); ok {
		if state.workspacePreview {
			workspaceOutput, err = ducklord.NewWorkspaceOutputAdapter(ctx, cfg.RawOutputSubscriptionLimit(), source, state.snapshotStore,
				func(selected ducklord.TerminalSelection) []ducklord.TerminalSelection {
					return state.workspaceVisibleSelections(selected)
				})
			outputManager = workspaceOutput
			workspaceRepaint = workspaceOutput.RepaintReady()
			state.workspaceOutput = workspaceOutput
			state.clearOutputFocus = workspaceOutput.ClearInputFocus
		} else {
			outputManager, err = ducklord.NewTerminalOutputManager(ctx, cfg.RawOutputSubscriptionLimit(), source, state.snapshotStore)
		}
		if err != nil {
			return err
		}
		outputEvents = outputManager.Events()
		state.pooledOutput = true
		defer outputManager.Close()
	} else {
		defer state.saveCurrentSnapshot()
	}
	sessionUpdates := make(chan hostSessionUpdate, 32)
	watchedClients := make(map[string]hostWatchState)
	var watchEpoch uint64
	watcher, eventDriven := runner.(interface {
		WatchSessionUpdates(context.Context, ducklord.Client) <-chan ducklord.SessionUpdate
	})
	watchClient := func(client ducklord.Client) {
		if !eventDriven || state.disconnectedHosts[client.Name] {
			return
		}
		if _, exists := watchedClients[client.Name]; exists {
			return
		}
		watchEpoch++
		watchCtx, cancel := context.WithCancel(ctx)
		current := hostWatchState{cancel: cancel, epoch: watchEpoch}
		watchedClients[client.Name] = current
		state.eventDriven = true
		updates := watcher.WatchSessionUpdates(watchCtx, client)
		go func() {
			for update := range updates {
				select {
				case sessionUpdates <- hostSessionUpdate{update: update, epoch: current.epoch}:
				case <-watchCtx.Done():
					return
				}
			}
		}()
	}
	watchAllClients := func() {
		for _, client := range state.cfg.Clients {
			watchClient(client)
		}
	}
	watchAllClients()
	state.refreshSessions(ctx)
	if outputManager == nil {
		state.refreshSelectedOutput(ctx)
	}
	state.render(os.Stdout)
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	resizeSignals := make(chan os.Signal, 1)
	signal.Notify(resizeSignals, syscall.SIGWINCH)
	defer signal.Stop(resizeSignals)
	input := make(chan []byte, 8)
	inputReady := make(chan struct{}, 1)
	attachOut := make(chan attachOutputEvent, 32)
	attachOutputSource := (<-chan attachOutputEvent)(attachOut)
	resizeRequests := make(chan resizeRequest, 1)
	resizeResults := make(chan resizeDoneEvent, 1)
	startDone := make(chan startDoneEvent, 1)
	hostRetentionDone := make(chan hostRetentionEvent, 1)
	hostResourceDone := make(chan hostResourceEvent, 1)
	hostHookDone := make(chan hostHookEvent, 1)
	hostHookStatusDone := make(chan hostHookStatusEvent, 1)
	hostSkillsDone := make(chan hostSkillsEvent)
	state.hostSkillsDone = hostSkillsDone
	projectFilesDone := make(chan projectFilesEvent, 8)
	state.projectFiles.done = projectFilesDone
	state.projectFiles.lifecycle = ctx
	var hostRetentionCancel context.CancelFunc
	launchHostHookStatus := func(target string) {
		state.hostHookStatusRequestID++
		requestID := state.hostHookStatusRequestID
		state.hostHookStatusLoading = true
		state.hostHookStatusErr = ""
		state.hostHookStatuses = nil
		client, ok := state.cfg.Client(target)
		if !ok || state.disconnectedHosts[target] {
			state.hostHookStatusLoading = false
			state.hostHookStatusErr = "Host is disconnected or unavailable"
			return
		}
		epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
		go func() {
			readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			statuses := make(map[string]protocol.HostAgentHookStatus, 2)
			var readErr error
			for _, agent := range []string{"codex", "claude"} {
				statuses[agent], readErr = runner.HostAgentHookStatus(readCtx, client, agent)
				if readErr != nil {
					break
				}
			}
			select {
			case hostHookStatusDone <- hostHookStatusEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, statuses: statuses, err: readErr}:
			case <-ctx.Done():
			}
		}()
	}
	createDone := make(chan createDiscoveryEvent, 1)
	pathSuggestionsDone := make(chan pathSuggestionEvent, 1)
	addClientDone := make(chan addClientDoneEvent, 1)
	lifecycleDone := make(chan lifecycleDoneEvent, 1)
	var lifecycleRequestID uint64
	previewDone := make(chan previewOutputEvent, 1)
	searchActivationTimeout := make(chan uint64, 1)
	var previewID uint64
	previewInFlight := false
	previewQueued := false
	var previewCancel context.CancelFunc
	var outputRequestID uint64
	var reconnectRequestID uint64
	var activeOutputEvent ducklord.TerminalOutputEvent
	var selectedOutputSession ducklord.RemoteSession
	selectPooledOutput := func() {
		if outputManager == nil {
			return
		}
		if state.workspacePaneMode && state.workspacePaneStep == "notes" {
			return
		}
		if len(state.sessions) == 0 {
			if workspaceOutput != nil {
				workspaceOutput.ClearVisible()
			}
			outputRequestID++
			selectedOutputSession = ducklord.RemoteSession{}
			activeOutputEvent = ducklord.TerminalOutputEvent{}
			state.outputForKey, state.outputText, state.terminal = "", "", nil
			state.outputFresh = false
			return
		}
		reconnectRequestID = 0
		state.outputReconnecting = false
		sess := state.activePTYSession()
		var workspacePriority ducklord.TerminalSelection
		if workspaceOutput != nil {
			visible := state.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: sess.Client}})
			if len(visible) == 0 {
				workspaceOutput.ClearVisible()
				outputRequestID++
				selectedOutputSession = ducklord.RemoteSession{}
				activeOutputEvent = ducklord.TerminalOutputEvent{}
				state.outputForKey = ""
				state.outputText = ""
				state.terminal = nil
				state.outputFresh = false
				state.outputErr = "current Project has no live Session pane"
				return
			}
			workspacePriority = state.workspacePreferredSelection(visible, sess)
			for _, candidate := range state.sessions {
				if candidate.Client == workspacePriority.Client.Name && candidate.InstanceID == workspacePriority.InstanceID && candidate.SessionID == workspacePriority.SessionID && candidate.RuntimeGeneration == workspacePriority.RuntimeGeneration {
					sess = candidate
					break
				}
			}
		}
		selectedOutputSession = sess
		key, keyOK := terminalOutputKey(sess)
		selectedKey := sessionKey(sess)
		if state.outputForKey != selectedKey || !keyOK || activeOutputEvent.Key != key || activeOutputEvent.Revision.RuntimeGeneration != sess.RuntimeGeneration {
			state.outputText = ""
			state.terminal = nil
			state.ptyScrollOffset = 0
			activeOutputEvent = ducklord.TerminalOutputEvent{}
			state.outputForKey = selectedKey
		}
		if workspaceOutput != nil {
			outputRequestID = workspaceOutput.Select(workspacePriority)
			state.outputForKey = selectedKey
			state.outputFresh = false
			state.outputErr = "loading live PTY output..."
			return
		}
		if sess.Client == "" || sess.InstanceID == "" || sess.SessionID == "" || sess.RuntimeGeneration == 0 || !canRead(sess) || !state.hostIsLive(sess.Client) {
			outputRequestID++
			selectedOutputSession = ducklord.RemoteSession{}
			activeOutputEvent = ducklord.TerminalOutputEvent{}
			state.outputForKey, state.outputText, state.terminal = "", "", nil
			state.outputFresh = false
			state.outputErr = "PTY output is unavailable until the host session is synchronized"
			return
		}
		client, clientErr := mustClient(cfg, sess.Client)
		if clientErr != nil {
			state.outputErr = clientErr.Error()
			return
		}
		rows, cols := state.activePTYSize()
		outputRequestID = outputManager.Select(ducklord.TerminalSelection{Client: client, InstanceID: sess.InstanceID, SessionID: sess.SessionID,
			RuntimeGeneration: sess.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
		state.outputForKey = selectedKey
		state.outputFresh = false
		state.outputErr = "loading live PTY output..."
	}
	startPreview := func() {
		if previewInFlight || !previewQueued || state.focused || len(state.sessions) == 0 {
			return
		}
		previewQueued = false
		previewInFlight = true
		id := previewID
		sess := state.currentSession()
		key, generation := sessionKey(sess), sess.RuntimeGeneration
		client, clientErr := mustClient(cfg, sess.Client)
		previewCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		previewCancel = cancel
		ref := sess.SessionID
		if ref == "" {
			ref = sess.Name
		}
		go func() {
			var text string
			err := clientErr
			if err == nil {
				if isolated, ok := runner.(interface {
					ReadPreview(context.Context, ducklord.Client, string, int) (string, error)
				}); ok {
					text, err = isolated.ReadPreview(previewCtx, client, ref, 80)
				} else {
					text, err = runner.Read(previewCtx, client, ref, 80)
				}
			}
			select {
			case previewDone <- previewOutputEvent{id: id, key: key, generation: generation, text: text, err: err}:
			case <-ctx.Done():
			}
		}()
	}
	requestPreview := func(fence bool) {
		if outputManager != nil {
			selectPooledOutput()
			return
		}
		if state.focused || len(state.sessions) == 0 {
			return
		}
		if fence || previewID == 0 {
			previewID++
		}
		previewQueued = true
		startPreview()
	}
	fencePreview := func() {
		if outputManager != nil {
			return
		}
		previewID++
		previewQueued = false
	}
	var attach *ducklord.AttachSession
	var control *ducklord.ControlSession
	var controlDone <-chan error
	var pendingControlSince time.Time
	controlOpened := make(chan controlOpenEvent, 1)
	controlID := 0
	var controlOpenCancel context.CancelFunc
	var attachCancel context.CancelFunc
	attachID := 0
	var attachCanResize bool
	var attachInitialResizeQueued bool
	var attachReplayEndOffset uint64
	var pendingFramebufferResize *resizeDoneEvent
	var resizeInFlight bool
	var queuedResize *[2]uint16
	openControlForSession := func(s ducklord.RemoteSession) bool {
		if outputManager == nil || !canAttach(s) || !state.hostIsLive(s.Client) {
			return false
		}
		c, err := mustClient(cfg, s.Client)
		if err != nil {
			state.outputErr = err.Error()
			return false
		}
		fencePreview()
		state.focused = false
		state.activeAttachKey = sessionKey(s)
		if workspaceOutput != nil {
			selectPooledOutput()
		}
		state.activeAttachFresh = state.outputFresh
		state.pendingAttachKey = ""
		controlID++
		attachID++
		resizeInFlight = false
		queuedResize = nil
		id := controlID
		if !state.canResizeCurrentSession() {
			state.focused = true
			state.outputErr = "read-only PTY view; yield control before sending input"
			return false
		}
		controlRunner, ok := runner.(interface {
			OpenControlSession(context.Context, ducklord.Client, string) (*ducklord.ControlSession, error)
		})
		if !ok {
			state.outputErr = "PTY control is unavailable"
			state.clearAttachIdentity()
			return false
		}
		state.outputErr = "opening PTY control..."
		key := sessionKey(s)
		if controlOpenCancel != nil {
			controlOpenCancel()
		}
		controlCtx, cancelControlOpen := context.WithCancel(ctx)
		controlOpenCancel = cancelControlOpen
		go func() {
			opened, openErr := controlRunner.OpenControlSession(controlCtx, c, s.SessionID)
			select {
			case controlOpened <- controlOpenEvent{id: id, key: key, control: opened, err: openErr}:
			case <-ctx.Done():
				if opened != nil {
					_ = opened.Stdin.Close()
				}
			}
		}()
		return true
	}
	var bufferedAttach []attachOutputEvent
	bufferedAttachBytes := 0
	resizeWorkerDone := make(chan struct{})
	go func() {
		defer close(resizeWorkerDone)
		runResizeWorker(ctx, resizeRequests, resizeResults)
	}()
	defer func() {
		stop()
		state.cancelCreateDiscovery()
		state.cancelPathSuggestions()
		state.createWorkers.Wait()
		<-resizeWorkerDone
	}()
	resizeCapability := func() func(uint16, uint16) (uint64, error) {
		var resize func(uint16, uint16) (uint64, error)
		expectedKey, keyOK := terminalOutputKey(state.activePTYSession())
		if outputManager != nil && control != nil && control.ResizeBarrier != nil && activeOutputEvent.Lease != 0 && keyOK && activeOutputEvent.Key == expectedKey &&
			activeOutputEvent.Revision.RuntimeGeneration == control.RuntimeGeneration && control.RuntimeGeneration == state.activePTYSession().RuntimeGeneration {
			event := activeOutputEvent
			controlSnapshot := control
			resize = func(rows, cols uint16) (uint64, error) {
				return outputManager.Resize(event, rows, cols, controlSnapshot.ResizeBarrier)
			}
		} else if attach != nil {
			resize = attach.ResizeBarrier
		}
		return resize
	}
	queueResize := func() {
		if !attachCanResize {
			return
		}
		resize := resizeCapability()
		if resize == nil {
			return
		}
		rows, cols := state.activePTYSize()
		if !attachInitialResizeQueued && !initialReplayCaughtUp(state.terminalOffset, attachReplayEndOffset) {
			// SIGWINCH may arrive while the initial replay is still being
			// consumed. Remember only the latest dimensions; starting a barrier
			// here would hide the replay until the network RPC returned.
			queuedResize = &[2]uint16{rows, cols}
			return
		}
		if resizeInFlight || pendingFramebufferResize != nil {
			queuedResize = &[2]uint16{rows, cols}
			return
		}
		request := resizeRequest{id: attachID, rows: rows, cols: cols, resize: resize}
		select {
		case resizeRequests <- request:
			resizeInFlight = true
		default:
			select {
			case <-resizeRequests:
			default:
			}
			select {
			case resizeRequests <- request:
				resizeInFlight = true
			default:
			}
		}
	}
	resetWorkspacePaneChange := func() bool {
		if !state.workspacePaneChanged {
			return false
		}
		state.workspacePaneChanged = false
		controlID++
		attachID++
		resizeInFlight = false
		pendingFramebufferResize = nil
		queuedResize = nil
		bufferedAttach = nil
		bufferedAttachBytes = 0
		attachOutputSource = attachOut
		attachCanResize = false
		attachInitialResizeQueued = false
		attachReplayEndOffset = 0
		if controlOpenCancel != nil {
			controlOpenCancel()
			controlOpenCancel = nil
		}
		if control != nil {
			_ = control.Stdin.Close()
			control, controlDone = nil, nil
		}
		if attachCancel != nil {
			attachCancel()
			attachCancel = nil
		}
		if attach != nil {
			_ = attach.Stdin.Close()
			attach = nil
		}
		state.clearAttachIdentity()
		state.focused = false
		if workspaceOutput != nil {
			selectPooledOutput()
		}
		return true
	}
	resetWorkspacePaneChangeIfNeeded := func() bool {
		// Closing a focused detach confirmation with Esc leaves the modal closed
		// while the exact source Session control is being rebound.  Do not let
		// the generic workspace reset invalidate that pending restore; once the
		// lease is accepted, normal changes can still reset as usual.
		if state.workspacePaneFocusRestorePending && !state.workspacePaneChanged {
			return false
		}
		// A focused Default detach confirmation temporarily owns the keyboard,
		// but must keep the originating PTY lease available for Esc restoration.
		// Confirming the detach closes the modal first, so the normal reset still
		// tears down the detached pane's lease.
		if state.workspacePaneMode && state.workspacePaneStep == "detach-confirm" && state.workspacePaneRestoreFocused {
			state.workspacePaneChanged = false
			return false
		}
		return resetWorkspacePaneChange()
	}
	acceptWorkspaceControl := func(restored ...ducklord.RemoteSession) bool {
		// A workspace modal owns input while it is open.  Async output/control
		// readiness must not reclaim the PTY focus that opening the modal
		// deliberately released; closeNotesModal restores it through the
		// deferred focus lease.
		if workspaceOutput == nil || control == nil || !state.workspaceControlMayAccept() {
			return false
		}
		session := state.activePTYSession()
		if len(restored) != 0 {
			session = restored[0]
		}
		// A focused Default detach modal is closed before its original PTY
		// control is restored, so the workspace pane rectangle is temporarily
		// unavailable.  The captured source session is authoritative for this
		// handoff; requiring the modal rectangle here strands Esc on the list.
		if (!state.workspacePaneFocusRestorePending && func() bool {
			_, err := state.workspacePaneRect()
			return err != nil
		}()) || !state.hostIsLive(session.Client) || !controlMatchesSession(control, session, state.ownerName) {
			_ = control.Stdin.Close()
			control, controlDone = nil, nil
			state.clearAttachIdentity()
			state.outputErr = "Session pane or writer changed before PTY control became ready"
			return false
		}
		key, ok := terminalOutputKey(session)
		if !ok || !setWorkspaceInputFocus(workspaceOutput, key) {
			return false // the visible raw-output lease may still be opening
		}
		state.focused = true
		pendingControlSince = time.Time{}
		attachCanResize = control.ResizeBarrier != nil
		if nav, err := state.workspaceNavigation(); err == nil {
			width, height := terminalSize()
			_, _ = nav.FocusVisiblePane(ducklord.CalculateWorkspaceGeometry(width, height, 4))
		}
		state.outputErr = ""
		if state.sessionFreshlyDisplayed(session) {
			state.markActivitySeen(session)
		}
		if resizeCapability() != nil && !attachInitialResizeQueued {
			attachInitialResizeQueued = true
			queueResize()
		}
		return true
	}
	restorePendingFocus := func() bool {
		restored := restorePendingNotesFocus(state, workspaceOutput, state.outputFresh)
		// The output lease and the PTY writer become ready independently.  Help
		// must not advertise terminal focus until the writer has arrived as well;
		// otherwise the first byte after closing the overlay is silently discarded
		// while control is still nil.
		restored = restorePendingHelpFocusWithAttach(state, workspaceOutput, state.outputFresh, control, attach) || restored
		if restorePendingWorkspacePaneFocus(state, workspaceOutput, state.outputFresh, control) {
			restored = true
		}
		return restored
	}
	finishAttach := func(event attachOutputEvent) {
		// Closing a focused Default detach modal can restore the existing
		// control lease while the old attach stream is still finishing.  Its
		// terminal event belongs to that old stream and must not tear down the
		// focus that Esc just returned to the source Session.
		preserveRestoredFocus := state.workspacePaneFocusRestoreLease || state.workspacePaneFocusRestorePending
		attachID++
		resizeInFlight = false
		pendingFramebufferResize = nil
		queuedResize = nil
		bufferedAttach = nil
		bufferedAttachBytes = 0
		attachOutputSource = attachOut
		wasFocused := state.focused
		if !preserveRestoredFocus {
			state.focused = false
			state.clearAttachIdentity()
		}
		attachCanResize = false
		attachInitialResizeQueued = false
		attachReplayEndOffset = 0
		attach = nil
		if attachCancel != nil {
			attachCancel()
			attachCancel = nil
		}
		requestPreview(true)
		if event.err != nil && wasFocused {
			state.outputErr = event.err.Error()
		}
	}
	var startCancel context.CancelFunc
	startID := 0
	selectPooledOutput()
	go readInput(ctx, input, inputReady)
	inputPending := false
	replayInput := make(chan []byte, 1)
	replayQueued := false
	var deferredWorkspaceInput []byte
	deferredWorkspaceReplay := false
	// Direct pane detach fences user bytes until the surviving pane's PTY
	// lease is accepted. Those bytes belong to the PTY, unlike deferred
	// workspace navigation input, so replay them with terminal focus intact.
	deferredWorkspaceInputToPTY := false
	clearWorkspacePlacementInput := func() {
		state.workspacePlacementInputPending = false
		deferredWorkspaceInput = nil
		deferredWorkspaceReplay = false
		deferredWorkspaceInputToPTY = false
	}
	panePrefixTimer := time.NewTimer(time.Hour)
	if !panePrefixTimer.Stop() {
		<-panePrefixTimer.C
	}
	stopPanePrefixTimer := func() {
		if !panePrefixTimer.Stop() {
			select {
			case <-panePrefixTimer.C:
			default:
			}
		}
	}
	resetPanePrefixTimer := func(deadline time.Time) {
		stopPanePrefixTimer()
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d < 0 {
				d = 0
			}
			panePrefixTimer.Reset(d)
		}
	}
	queuePaneReplay := func() {
		if replayQueued || len(state.panePrefixReplay) == 0 {
			return
		}
		replay := state.takePanePrefixReplay()
		replayInput <- replay
		replayQueued = true
	}
	defer stopPanePrefixTimer()
	lastOutputRender := time.Time{}
	renderOutput := func() {
		if lastOutputRender.IsZero() || time.Since(lastOutputRender) >= 50*time.Millisecond {
			state.render(os.Stdout)
			lastOutputRender = time.Now()
		}
	}
	for {
		// Inventory placement selects a newly created Session asynchronously. Queue
		// the same local attach action used by Enter so the selected pane acquires
		// a PTY control lease before the next user bytes are handled.
		if state.workspacePlacementFocusPending && !state.workspacePaneFocusRestorePending && !replayQueued {
			state.workspacePlacementFocusPending = false
			// A quick-shell placement replaces the pane owned by the current
			// control session. Release that old writer before replaying the local
			// attach action; otherwise the replayed Enter is delivered to the old
			// PTY while activeAttachKey already names the newly created Session.
			if control != nil {
				controlID++
				if controlOpenCancel != nil {
					controlOpenCancel()
					controlOpenCancel = nil
				}
				_ = control.Stdin.Close()
				control = nil
				controlDone = nil
			}
			state.focused = false
			if state.clearOutputFocus != nil {
				state.clearOutputFocus()
			}
			// Placement changes the selected workspace pane after the output
			// manager has already selected the old pane.  Select the created
			// Session before opening its writer lease, otherwise the lease
			// completion cannot transfer raw input focus to the new PTY.
			selectPooledOutput()
			replayInput <- []byte("\r")
			replayQueued = true
		}
		// A prefix command can finish while Help's asynchronous PTY focus lease
		// is still pending.  Keep it queued until the lease restores terminal
		// focus; dispatching it immediately would apply the command to the
		// navigation pane.
		if state.focused && !state.helpFocusRestorePending && !replayQueued && len(state.panePrefixReplay) != 0 {
			replayInput <- state.takePanePrefixReplay()
			replayQueued = true
		}
		// A continuously producing PTY can keep its output channel ready and
		// starve keyboard input in a Go select. Once input is queued, temporarily
		// remove the output case so controls such as Ctrl-C and Ctrl-] run first.
		attachOutputSelect := attachOutputSource
		outputEventsSelect := outputEvents
		inputSelect := (<-chan []byte)(input)
		if replayQueued {
			inputSelect = replayInput
		}
		if inputPending || len(input) != 0 || replayQueued {
			attachOutputSelect = nil
			outputEventsSelect = nil
		}
		select {
		case <-panePrefixTimer.C:
			if state.panePrefixPending && state.panePrefixSuffix != "" && !time.Now().Before(state.panePrefixDeadline) {
				command := state.panePrefixSuffix
				state.panePrefixPending, state.panePrefixSuffix = false, ""
				state.panePrefixDeadline = time.Time{}
				state.dispatchPaneCommand(command)
				if replay := state.takePanePrefixReplay(); len(replay) != 0 {
					replayInput <- replay
					replayQueued = true
				}
				state.render(os.Stdout)
			}
		case <-inputReady:
			inputPending = true
		case event := <-hostRetentionDone:
			if event.id == state.hostMenuRequestID && hostRetentionCancel != nil {
				hostRetentionCancel()
				hostRetentionCancel = nil
			}
			watch := watchedClients[event.host]
			if !state.acceptHostRetentionEvent(event, watch.epoch) {
				continue
			}
			if event.err != nil {
				state.hostMenuStep = "retention-error"
				state.hostMenuErr = sanitizeTerminalText(event.err.Error())
			} else if event.saved {
				state.hostMenuOldDays = event.days
				state.hostMenuStep = "retention-saved"
				state.hostMenuErr = ""
			} else {
				state.hostMenuOldDays = event.days
				state.hostMenuDraft = strconv.Itoa(event.days)
				state.hostMenuReplaceDraft = true
				state.hostMenuStep = "retention-edit"
				state.hostMenuErr = ""
			}
			state.render(os.Stdout)
		case event := <-hostResourceDone:
			watch := watchedClients[event.host]
			if !state.acceptHostResourceEvent(event, watch.epoch) {
				continue
			}
			if event.err != nil {
				state.hostMenuErr = sanitizeTerminalText(event.err.Error())
				state.hostMenuStep = "resources-error"
			} else {
				state.hostResourceStatus = event.status
				state.hostMenuErr = ""
				state.hostMenuStep = "resources-view"
			}
			state.render(os.Stdout)
		case event := <-hostHookDone:
			state.finishHostHookOperation(event.host, event.id)
			if event.id == state.hostMenuRequestID && hostRetentionCancel != nil {
				hostRetentionCancel()
				hostRetentionCancel = nil
			}
			watch := watchedClients[event.host]
			if !state.acceptHostHookEvent(event, watch.epoch) {
				continue
			}
			if event.err != nil {
				state.hostMenuStep = "hook-error"
				state.hostMenuErr = sanitizeTerminalText(event.err.Error())
			} else {
				state.hostMenuStep = "hook-done"
				state.hostMenuErr = hostHookResultMessage(event)
				launchHostHookStatus(event.host)
			}
			state.render(os.Stdout)
		case event := <-hostHookStatusDone:
			if event.id != state.hostHookStatusRequestID || !state.hostMenuMode || !strings.HasPrefix(state.hostMenuStep, "hook-") ||
				event.host != state.hostMenuTarget || state.disconnectedHosts[event.host] || event.epoch != watchedClients[event.host].epoch ||
				event.instanceID != state.hostSync[event.host].InstanceID {
				continue
			}
			state.hostHookStatusLoading = false
			if event.err != nil {
				state.hostHookStatusErr = "Host hook status unavailable"
			} else {
				state.hostHookStatuses = event.statuses
			}
			state.render(os.Stdout)
		case event := <-hostSkillsDone:
			if event.action == "list" {
				if event.id != state.hostSkillsListRequestID || !state.hostMenuMode || state.hostMenuStep != "skills-dashboard" {
					ducklord.CleanupSkillPreview(event.preview)
					continue
				}
				if state.hostSkillsListCancel != nil {
					state.hostSkillsListCancel()
					state.hostSkillsListCancel = nil
				}
				state.applyHostSkillsEvent(event)
				state.render(os.Stdout)
				continue
			}
			if event.id != state.hostSkillsRequestID || !state.hostSkillsBusy {
				ducklord.CleanupSkillPreview(event.preview)
				continue
			}
			state.applyHostSkillsEvent(event)
			state.render(os.Stdout)
		case event := <-projectFilesDone:
			state.applyProjectFilesEvent(event)
			if state.projectFiles.open {
				state.render(os.Stdout)
			}
		case <-workspaceRepaint:
			if control != nil && !state.focused {
				if state.workspacePaneFocusRestorePending {
					restoreSession := state.activePTYSession()
					if state.workspacePaneFocusRestoreKey != "" {
						if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
							restoreSession = captured
						} else if state.workspacePaneFocusRestoreSession.Client != "" {
							restoreSession = state.workspacePaneFocusRestoreSession
						}
					}
					if acceptWorkspaceControl(restoreSession) {
						restorePendingFocus()
					}
				} else {
					acceptWorkspaceControl()
				}
			}
			state.render(os.Stdout)
		case <-ctx.Done():
			state.cleanupHostSkills()
			state.cancelAddClientWork()
			if controlOpenCancel != nil {
				controlOpenCancel()
			}
			if control != nil {
				_ = control.Stdin.Close()
			}
			if attachCancel != nil {
				attachCancel()
			}
			if startCancel != nil {
				startCancel()
			}
			state.cancelCreateDiscovery()
			return nil
		case result := <-addClientDone:
			if result.progress {
				if state.addClientMode && state.addClientBusy && result.id == state.addClientRequestID {
					state.addClientErr = result.status
					state.render(os.Stdout)
				}
				continue
			}
			if state.completeAddClient(result) {
				state.bumpHostConnectionEpoch(result.client.Name)
				delete(state.disconnectedHosts, result.client.Name)
				watchAllClients()
			}
			state.render(os.Stdout)
		case <-ticker.C:
			// Start and inventory are separate RPCs. While a new pane is
			// awaiting its Session, poll even in event-driven mode so a missed
			// update cannot strand the placement or its deadline forever.
			hadPendingPlacement := len(state.pendingWorkspacePlacements) > 0
			if hadPendingPlacement {
				state.refreshSessions(ctx)
			}
			if workspaceOutput != nil && control != nil && !state.focused {
				if !pendingControlSince.IsZero() && time.Since(pendingControlSince) >= 15*time.Second {
					_ = control.Stdin.Close()
					control, controlDone = nil, nil
					state.clearAttachIdentity()
					state.outputErr = "PTY focus timed out waiting for Session output"
				} else {
					if state.workspacePaneFocusRestorePending {
						restoreSession := state.activePTYSession()
						if state.workspacePaneFocusRestoreKey != "" {
							if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
								restoreSession = captured
							} else if state.workspacePaneFocusRestoreSession.Client != "" {
								restoreSession = state.workspacePaneFocusRestoreSession
							}
						}
						if acceptWorkspaceControl(restoreSession) {
							restorePendingFocus()
						}
					} else {
						acceptWorkspaceControl()
					}
				}
				state.render(os.Stdout)
				continue
			}
			if !state.focused && !state.newSessionMode && !state.searchMode {
				if !state.eventDriven && !hadPendingPlacement {
					state.refreshSessions(ctx)
				}
				requestPreview(false)
			}
			state.render(os.Stdout)
		case requestID := <-searchActivationTimeout:
			if state.searchMode && state.searchPendingRequestID == requestID {
				state.searchPendingRequestID = 0
				state.searchErr = "PTY activation timed out; try again"
				selectPooledOutput()
				state.render(os.Stdout)
			}
		case event := <-outputEventsSelect:
			expectedSession := state.activePTYSession()
			if workspaceOutput != nil {
				expectedSession = selectedOutputSession
			}
			if state.searchMode && state.searchActivatedKey != "" {
				if activated, ok := state.sessionForKey(state.searchActivatedKey); ok {
					expectedSession = activated
				}
			}
			searchActivation := state.searchMode && state.searchPendingRequestID != 0 && event.RequestID == state.searchPendingRequestID
			if searchActivation {
				if state.searchPendingRevision != state.searchRevision || !state.selectableSessionMatches(state.searchPendingKey, state.searchPendingGeneration) {
					state.searchPendingRequestID = 0
					state.searchErr = "session changed while switching; try again"
					selectPooledOutput()
					state.render(os.Stdout)
					continue
				}
				expectedSession, _ = state.sessionForKey(state.searchPendingKey)
			}
			expectedKey, expectedOK := terminalOutputKey(expectedSession)
			if searchActivation && (!expectedOK || event.Key != expectedKey || event.Revision.RuntimeGeneration != expectedSession.RuntimeGeneration) {
				state.searchPendingRequestID = 0
				state.searchErr = "PTY activation returned stale session output; try again"
				selectPooledOutput()
				state.render(os.Stdout)
				continue
			}
			if event.RequestID != outputRequestID || !expectedOK || event.Key != expectedKey || event.Revision.RuntimeGeneration != expectedSession.RuntimeGeneration || event.Err != nil {
				if event.RequestID == outputRequestID && event.Err != nil {
					state.outputReconnecting = false
					if event.RequestID == reconnectRequestID {
						reconnectRequestID = 0
						state.outputStale = true
						state.outputErr = "PTY reconnect failed; previous live view retained"
						state.render(os.Stdout)
						continue
					}
					if searchActivation {
						state.searchPendingRequestID = 0
						state.searchErr = "PTY output stream unavailable"
						selectPooledOutput()
					} else {
						state.outputFresh = false
						state.outputErr = "PTY output stream unavailable"
					}
					state.render(os.Stdout)
				}
				continue
			}
			var view ducklord.PooledTerminalView
			var viewErr error
			if event.FinalView != nil {
				view = *event.FinalView
			} else {
				view, viewErr = outputManager.View(event)
				if viewErr != nil {
					if searchActivation {
						state.searchPendingRequestID = 0
						state.searchErr = "PTY framebuffer is unavailable; try again"
						selectPooledOutput()
						state.render(os.Stdout)
					}
					continue
				}
			}
			if searchActivation && (!view.Ready || view.Disconnected || view.Ended) {
				state.searchPendingRequestID = 0
				state.searchErr = "PTY output is not ready; try again"
				selectPooledOutput()
				state.render(os.Stdout)
				continue
			}
			terminal, valid := ducklord.NewTerminalFromState(view.Framebuffer, ducklord.DefaultTerminalScrollback)
			if !valid {
				if searchActivation {
					state.searchPendingRequestID = 0
					state.searchErr = "PTY framebuffer is invalid"
					selectPooledOutput()
				} else {
					state.outputFresh = false
					state.outputErr = "PTY framebuffer is invalid"
				}
				state.render(os.Stdout)
				continue
			}
			if searchActivation && !state.prepareSearchWorkspaceSelection(state.searchPendingKey) {
				selectPooledOutput()
				state.render(os.Stdout)
				continue
			}
			state.terminal = terminal
			state.ptyScrollOffset = 0
			activeOutputEvent = event
			activeOutputEvent.FinalView = nil
			state.outputText = terminal.Text()
			state.terminalGeneration = view.RuntimeGeneration
			state.terminalOffset = view.OutputOffset
			state.terminalCursorValid = view.Ready && !view.Disconnected
			state.outputStale = false
			state.outputReconnecting = false
			if event.RequestID == reconnectRequestID {
				reconnectRequestID = 0
			}
			state.outputFresh = view.Ready && !view.Disconnected
			state.outputErr = view.Error
			if view.Ended {
				state.outputErr = "PTY process ended"
			}
			if state.outputFresh {
				restorePendingFocus()
				drainNotesPendingInput(state, control)
			}
			if searchActivation {
				state.outputForKey = state.searchPendingKey
				state.activeAttachKey = state.searchPendingKey
				state.activeAttachFresh = state.outputFresh
				state.searchActivatedKey = state.searchPendingKey
				state.searchActivatedRevision = state.searchPendingRevision
				state.searchActivatedGeneration = state.searchPendingGeneration
				state.searchPendingRequestID = 0
				state.searchErr = "Active · Enter again to focus"
			}
			if control != nil && !state.focused {
				acceptWorkspaceControl()
			}
			if state.sessionFreshlyDisplayed(state.activePTYSession()) {
				state.markActivitySeen(state.activePTYSession())
			}
			if state.focused && control != nil && !attachInitialResizeQueued && resizeCapability() != nil {
				attachInitialResizeQueued = true
				queueResize()
			}
			renderOutput()
		case opened := <-controlOpened:
			paneVisible := true
			if state.workspacePreview {
				_, paneErr := state.workspacePaneRect()
				paneVisible = paneErr == nil
			}
			openedForCurrentAttach := opened.id == controlID && opened.key == state.activeAttachKey
			restoringFocusedPane := state.workspacePaneFocusRestorePending && openedForCurrentAttach
			restoreSession := state.activePTYSession()
			if restoringFocusedPane && state.workspacePaneFocusRestoreKey != "" {
				if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
					restoreSession = captured
				} else if state.workspacePaneFocusRestoreSession.Client != "" {
					restoreSession = state.workspacePaneFocusRestoreSession
				}
			}
			if state.workspacePaneMode || state.workspaceProjectFocus || !paneVisible && !restoringFocusedPane || !openedForCurrentAttach || opened.control != nil && !controlMatchesSession(opened.control, restoreSession, state.ownerName) {
				if opened.control != nil {
					_ = opened.control.Stdin.Close()
				}
				// A late result from an older attach must never clear the identity
				// captured by a newly placed quick-shell pane.  The id alone is not
				// sufficient when a replacement attach reuses the current lifecycle.
				if openedForCurrentAttach {
					if controlOpenCancel != nil {
						controlOpenCancel()
						controlOpenCancel = nil
					}
					clearWorkspacePlacementInput()
					state.clearAttachIdentity()
				}
				if !paneVisible && opened.id == controlID {
					state.clearAttachIdentity()
					state.focused = false
					state.outputErr = "Session pane became hidden; PTY control was not opened"
					state.render(os.Stdout)
				}
				continue
			}
			controlOpenCancel = nil
			if opened.err != nil {
				state.outputErr = sanitizeTerminalText(opened.err.Error())
				if openedForCurrentAttach {
					clearWorkspacePlacementInput()
				}
				state.clearAttachIdentity()
				state.render(os.Stdout)
				continue
			}
			if opened.control == nil {
				state.outputErr = "PTY control unavailable"
				if openedForCurrentAttach {
					clearWorkspacePlacementInput()
				}
				state.clearAttachIdentity()
				state.render(os.Stdout)
				continue
			}
			control = opened.control
			controlDone = control.Done
			if workspaceOutput != nil {
				var restoreSession ducklord.RemoteSession
				if state.workspacePaneFocusRestorePending && state.workspacePaneFocusRestoreKey != "" {
					if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
						restoreSession = captured
					}
					if restoreSession.Client == "" {
						restoreSession = state.workspacePaneFocusRestoreSession
					}
				}
				if restoreSession.Client == "" {
					if !acceptWorkspaceControl() {
						// Keep the placement fence until the output lease is accepted.
						// Host Skills can restore pooled output before its replacement
						// writer; clearing here drops the first command after Ctrl-C.
						if control != nil {
							pendingControlSince = time.Now()
							state.outputErr = "waiting for Session pane output before PTY input"
						}
						state.render(os.Stdout)
						continue
					}
				} else if !acceptWorkspaceControl(restoreSession) {
					// A restored focused pane may receive its control writer before
					// the output lease is ready. Keep the first post-Esc command
					// fenced until the lease is accepted; otherwise it falls through
					// to the Session list and is lost.
					if state.workspacePlacementInputPending && !restoringFocusedPane {
						clearWorkspacePlacementInput()
					}
					if control != nil {
						pendingControlSince = time.Now()
						state.outputErr = "waiting for Session pane output before PTY input"
					}
					state.render(os.Stdout)
					continue
				}
				if state.workspacePlacementInputPending {
					state.workspacePlacementInputPending = false
					if len(deferredWorkspaceInput) != 0 && !replayQueued {
						replayInput <- deferredWorkspaceInput
						deferredWorkspaceInput = nil
						deferredWorkspaceReplay = !deferredWorkspaceInputToPTY
						deferredWorkspaceInputToPTY = false
						replayQueued = true
					}
				}
				restorePendingFocus()
				drainNotesPendingInput(state, control)
				state.render(os.Stdout)
				continue
			}
			state.focused = true
			if state.workspacePlacementInputPending {
				state.workspacePlacementInputPending = false
				if len(deferredWorkspaceInput) != 0 && !replayQueued {
					replayInput <- deferredWorkspaceInput
					deferredWorkspaceInput = nil
					deferredWorkspaceReplay = !deferredWorkspaceInputToPTY
					deferredWorkspaceInputToPTY = false
					replayQueued = true
				}
			}
			attachCanResize = control.ResizeBarrier != nil
			state.outputErr = ""
			if state.sessionFreshlyDisplayed(state.activePTYSession()) {
				state.markActivitySeen(state.activePTYSession())
			}
			if resizeCapability() != nil {
				attachInitialResizeQueued = true
				queueResize()
			}
			state.render(os.Stdout)
		case controlErr := <-controlDone:
			controlDone = nil
			// The previous control stream can finish after Esc has handed the
			// saved Session back from the detach modal. Its completion belongs to
			// that stale stream; do not tear down the newly restored focus lease.
			if consumeWorkspaceRestoreControlDone(state) {
				// The restored writer itself may be the stream that just completed.
				// Reopen it against the captured Session before accepting keyboard
				// input; otherwise the UI advertises focus with no live writer.
				if state.activeAttachKey != "" {
					restoreSession := state.activePTYSession()
					if state.workspacePaneFocusRestoreKey != "" {
						if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
							restoreSession = captured
						}
					}
					state.workspacePaneFocusRestorePending = true
					state.workspacePaneFocusRestoreKey = sessionKey(restoreSession)
					state.workspacePaneFocusRestoreSession = restoreSession
					state.focused = false
					control = nil
					state.workspacePlacementInputPending = true
					openControlForSession(restoreSession)
				}
				continue
			}
			control = nil
			attachCanResize = false
			if state.focused || workspaceOutput != nil && state.activeAttachKey != "" {
				state.focused = false
				state.clearAttachIdentity()
			}
			if controlErr != nil {
				state.outputErr = "PTY control connection closed"
			}
			state.render(os.Stdout)
		case result := <-previewDone:
			if previewCancel != nil {
				previewCancel()
				previewCancel = nil
			}
			previewInFlight = false
			if state.applyPreviewOutput(result, previewID) {
				state.render(os.Stdout)
			}
			startPreview()
		case <-resizeSignals:
			queueResize()
			state.render(os.Stdout)
		case result := <-resizeResults:
			if result.id == attachID {
				resizeInFlight = false
				state.resizeStatus = ""
				if outputManager != nil {
					if result.err != nil {
						state.resizeStatus = "resize failed: " + sanitizeTerminalText(result.err.Error())
					}
					if queuedResize != nil {
						queuedResize = nil
						queueResize()
					}
					state.render(os.Stdout)
					continue
				}
				if result.err != nil {
					state.resizeStatus = "resize failed: " + sanitizeTerminalText(result.err.Error())
				} else if state.terminal != nil && result.barrier > state.terminalOffset {
					pendingFramebufferResize = &result
					state.resizeStatus = "resize pending output drain"
				} else if state.terminal != nil {
					state.terminal.Resize(int(result.rows), int(result.cols))
				}
				completion := drainBufferedAttach(state, bufferedAttach, &pendingFramebufferResize)
				bufferedAttach = nil
				bufferedAttachBytes = 0
				attachOutputSource = attachOut
				if completion != nil {
					finishAttach(*completion)
				} else if pendingFramebufferResize == nil && queuedResize != nil {
					queued := *queuedResize
					queuedResize = nil
					if resize := resizeCapability(); resize != nil {
						resizeRequests <- resizeRequest{id: attachID, rows: queued[0], cols: queued[1], resize: resize}
						resizeInFlight = true
					}
				}
				state.render(os.Stdout)
			}
		case envelope := <-sessionUpdates:
			update := envelope.update
			currentWatch, current := watchedClients[update.Client]
			if !current || currentWatch.epoch != envelope.epoch || state.disconnectedHosts[update.Client] {
				continue
			}
			previousHost := state.hostSync[update.Client]
			previousInstance := previousHost.InstanceID
			if previousInstance == "" {
				for _, session := range state.sessions {
					if session.Client == update.Client && session.InstanceID != "" {
						previousInstance = session.InstanceID
						break
					}
				}
			}
			previousActive := state.effectiveAttachKey()
			var previousWorkspaceSelections []ducklord.TerminalSelection
			if workspaceOutput != nil && update.ChangedSessionID != "" {
				selected := state.currentSession()
				previousWorkspaceSelections = state.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: selected.Client}})
			}
			if !state.applySessionUpdate(update) {
				continue
			}
			state.retryWorkspaceCreateDiscovery(ctx, createDone, previousHost, update)
			if state.invalidateCreateStart(update) {
				if startCancel != nil {
					startCancel()
					startCancel = nil
				}
				startID++ // fence a completion racing with cancellation
			}
			if outputManager != nil {
				if previousInstance != "" && update.InstanceID != "" && update.InstanceID != previousInstance {
					outputManager.ForgetHost(update.Client, previousInstance)
				} else if previousHost.InstanceID != "" && previousHost.State == "live" && update.State != "live" {
					outputManager.SyncHost(update.Client, previousHost.InstanceID, false, nil)
				}
				if update.State == "live" && update.InstanceID != "" {
					// Only an authoritative live update from the current watch epoch
					// may clear a disconnect tombstone for this instance.
					rows, cols := state.activePTYSize()
					selections := make([]ducklord.TerminalSelection, 0, len(update.Sessions))
					if client, clientErr := mustClient(cfg, update.Client); clientErr == nil {
						for _, session := range update.Sessions {
							if session.SessionID != "" && session.RuntimeGeneration != 0 && canRead(session) {
								selections = append(selections, ducklord.TerminalSelection{Client: client, InstanceID: session.InstanceID, SessionID: session.SessionID,
									RuntimeGeneration: session.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
							}
						}
					}
					outputManager.SyncHost(update.Client, update.InstanceID, true, selections)
				}
				workspaceSelectionsChangedByUpdate := false
				if workspaceOutput != nil && update.ChangedSessionID != "" {
					selected := state.currentSession()
					workspaceSelectionsChangedByUpdate = workspaceSelectionsChanged(previousWorkspaceSelections,
						state.workspaceVisibleSelections(ducklord.TerminalSelection{Client: ducklord.Client{Name: selected.Client}}))
				}
				if workspaceOutput != nil && (previousHost.State != update.State || previousInstance != update.InstanceID || workspaceSelectionsChangedByUpdate) {
					selectPooledOutput()
				}
			}
			if control != nil && (!state.hostIsLive(state.activePTYSession().Client) || !controlMatchesSession(control, state.activePTYSession(), state.ownerName)) {
				controlID++
				if controlOpenCancel != nil {
					controlOpenCancel()
					controlOpenCancel = nil
				}
				_ = control.Stdin.Close()
				control, controlDone = nil, nil
				attachCanResize = false
				state.focused = false
				state.clearAttachIdentity()
				state.outputErr = "PTY control disconnected; reconnect and focus it again"
			}
			if cancelRemovedPTYControl(state, previousActive, &control, &controlOpenCancel, &controlID) {
				controlDone = nil
				if attachCancel != nil {
					attachCancel()
					attachCancel = nil
				}
				if attach != nil {
					_ = attach.Stdin.Close()
				}
				attachID++
				resizeInFlight = false
				pendingFramebufferResize = nil
				queuedResize = nil
				bufferedAttach = nil
				bufferedAttachBytes = 0
				attachOutputSource = attachOut
				attach = nil
				attachCanResize = false
				attachInitialResizeQueued = false
				attachReplayEndOffset = 0
			}
			if outputManager != nil && !state.searchMode {
				selectPooledOutput()
			} else if !state.focused && state.activeAttachKey == "" {
				requestPreview(false)
			}
			state.render(os.Stdout)
		case result := <-createDone:
			clientName, _, args, ready := state.applyCreateDiscovery(result)
			if ready {
				client, clientErr := mustClient(cfg, clientName)
				if clientErr != nil {
					state.newSessionErr = clientErr.Error()
				} else {
					startCtx, cancel := context.WithCancel(ctx)
					startCancel = cancel
					startID++
					id := startID
					state.newSessionStarting = true
					state.newSessionStartGeneration, state.newSessionStartInstance = state.hostFingerprint(clientName)
					state.newSessionStartEpoch = state.hostConnectionEpoch[clientName]
					state.newSessionErr = "starting..."
					go func() {
						sessionID, startErr := runner.Start(startCtx, client, args)
						select {
						case startDone <- startDoneEvent{id: id, client: clientName, sessionID: sessionID, err: startErr}:
						case <-ctx.Done():
						}
					}()
				}
			}
			state.render(os.Stdout)
		case result := <-pathSuggestionsDone:
			generation, instance := state.hostFingerprint(result.client)
			if state.newSessionMode && state.newSessionStep == "path" && result.id == state.newSessionPathRequestID &&
				result.query == state.newSessionLine && result.generation == generation && result.instance == instance && state.hostIsLive(result.client) {
				state.newSessionPathBusy = false
				state.newSessionPathCancel = nil
				if result.err != nil {
					state.newSessionPathSuggestions = nil
					state.newSessionErr = sanitizeTerminalText(result.err.Error())
				} else {
					state.newSessionPathSuggestions = append([]string(nil), result.paths...)
					state.newSessionPathSelected = min(state.newSessionPathSelected, max(0, len(result.paths)-1))
					state.newSessionErr = fmt.Sprintf("%d matching remote directories", len(result.paths))
				}
				state.render(os.Stdout)
			}
		case result := <-startDone:
			if result.id != startID {
				continue
			}
			if state.disconnectedHosts[result.client] {
				startCancel = nil
				state.newSessionStarting = false
				if state.workspaceNewSessionIntent != nil {
					state.newSessionErr = "host disconnected; shell may have started—resync, then reopen Add Session pane"
				} else {
					state.newSessionErr = "host disconnected; remote start result was discarded"
				}
				state.workspaceNewSessionIntent = nil
				state.render(os.Stdout)
				continue
			}
			startCancel = nil
			state.completeNewSessionStart(ctx, result.client, result.sessionID, result.err)
			if outputManager != nil && result.err == nil {
				selectPooledOutput()
			}
			state.render(os.Stdout)
		case result := <-lifecycleDone:
			currentWatch := watchedClients[result.client]
			if result.id != lifecycleRequestID {
				continue
			}
			state.lifecycleBusy = false
			if !state.lifecycleResultCurrent(result, lifecycleRequestID, currentWatch.epoch) {
				state.outputErr = string(result.operation) + " outcome unknown after Host connection changed; resync before retrying"
				state.render(os.Stdout)
				continue
			}
			state.lifecycleConfirm = ""
			state.lifecycleTarget = ducklord.RemoteSession{}
			if result.err != nil {
				state.outputErr = string(result.operation) + " failed: " + sanitizeTerminalText(result.err.Error())
			} else {
				state.outputErr = fmt.Sprintf("%s completed for %s (generation %d)", result.operation, result.result.SessionID, result.result.RuntimeGeneration)
			}
			state.refreshSessions(ctx)
			if state.activeAttachKey == "" {
				requestPreview(false)
			}
			state.render(os.Stdout)
		case chunk := <-attachOutputSelect:
			if chunk.id != attachID {
				continue
			}
			if resizeInFlight {
				bufferedAttach = append(bufferedAttach, chunk)
				bufferedAttachBytes += len(chunk.text)
				if bufferedAttachBytes >= maxResizeBufferedBytes {
					attachOutputSource = nil
				}
				continue
			}
			if chunk.done {
				finishAttach(chunk)
				// A burst of PTY output can keep this select case ready. Avoid
				// adding another potentially blocking frame write when keyboard
				// input is already queued; the next iteration services input first.
				if len(input) == 0 {
					renderOutput()
				}
				continue
			}
			applyOrderedAttachChunk(state, chunk, &pendingFramebufferResize)
			if !attachInitialResizeQueued && attachCanResize && initialReplayCaughtUp(state.terminalOffset, attachReplayEndOffset) {
				// Show the initial replay before waiting on the remote resize
				// barrier. Bytes emitted after this point remain ordered by the
				// existing barrier buffering path.
				attachInitialResizeQueued = true
				queuedResize = nil
				queueResize()
			}
			if pendingFramebufferResize == nil && queuedResize != nil {
				queuedResize = nil
				queueResize()
			}
			// Rendering each output chunk can monopolize the event loop while a
			// command such as `yes` is producing output. Let queued controls (in
			// particular Ctrl-C and the unfocus key) run before another frame.
			if len(input) == 0 {
				renderOutput()
			}
		case b := <-inputSelect:
			replayedInput := replayQueued
			if replayQueued {
				replayQueued = false
			}
			workspaceNavigationReplay := deferredWorkspaceReplay
			deferredWorkspaceReplay = false
			if shouldRouteWorkspaceReplayAsNavigation(workspaceNavigationReplay) {
				// The replacement lease may have restored focused before the
				// deferred key arrives; route it through workspace navigation.
				state.focused = false
			}
			inputPending = false
			// A terminal tool owns every byte while its local modal is open.
			// Route it before workspace replay, modal cancellation, or PTY focus
			// handling can consume the input.
			if state.terminalSearchMode || state.terminalBookmarkMode || state.terminalBookmarkListMode {
				state.handleTerminalToolInput(b)
				state.render(os.Stdout)
				continue
			}
			workspaceProjectNavigation := shouldRouteWorkspaceProjectInputAsNavigation(state, b)
			if shouldDeferWorkspaceInput(state, replayedInput) && !workspaceProjectNavigation {
				deferredWorkspaceInput = append(deferredWorkspaceInput, b...)
				continue
			}
			var selectedActionTarget *ducklord.RemoteSession
			// Clicking another pane cancels pending focus before changing its
			// target. Late control completions remain fenced by controlID.
			if button, _, _, ok := parseSGRMouse(string(b)); ok && (button == 0 || button == 2) && strings.HasSuffix(string(b), "M") && state.workspacePreview && !state.blockingModalOpen() {
				state.panePrefixPending = false
				if handlePendingPTYInput(state, []byte("\x1b"), workspaceOutput != nil, &control, &controlOpenCancel, &controlID) {
					controlDone = nil
				}
			}
			// Central modals take ownership of Esc/Ctrl-C even when they were
			// opened from a focused terminal. Handle this before the pending
			// PTY gate so modal cancellation cannot be consumed by it.
			if state.handleCentralModalCancel(b) {
				if state.workspacePaneFocusRestorePending {
					// The original control lease is still valid when a focused
					// terminal opens the detach modal.  Keep that writer and hand
					// its visible output lease back immediately; closing it here
					// creates a needless asynchronous gap that can drop the first
					// command after Esc.
					restoreSession := state.activePTYSession()
					if state.workspacePaneFocusRestoreKey != "" {
						if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
							restoreSession = captured
						}
						if restoreSession.Client == "" {
							restoreSession = state.workspacePaneFocusRestoreSession
						}
					}
					if control != nil && controlMatchesSession(control, restoreSession, state.ownerName) {
						state.focused = false
						// Keep the saved snapshot pending until the visible output lease
						// accepts input focus. The control writer may be ready first.
						if acceptWorkspaceControl(restoreSession) {
							restorePendingFocus()
						}
						state.notesFocusRestoreReselect = false
						state.render(os.Stdout)
						continue
					}
					// The modal may have been opened while the previous control
					// lease was still connecting. Cancel that stale request before
					// replaying Enter, otherwise the replay is consumed by the
					// pending-input gate instead of reopening the saved PTY.
					// A replacement control really is asynchronous, so fence the
					// first command only on this path.
					state.workspacePlacementInputPending = true
					deferredWorkspaceInput = nil
					if controlOpenCancel != nil {
						controlOpenCancel()
						controlOpenCancel = nil
						controlID++
					}
					if control != nil {
						_ = control.Stdin.Close()
						control = nil
						controlDone = nil
					}
					state.focused = false
					// Keep the input fence up until the restored Session pane's
					// control writer is accepted. Reopen the exact source Session
					// directly; routing Esc through Project would consume the user's
					// first shell command.
					state.workspacePlacementInputPending = true
					deferredWorkspaceInput = nil
					if restoreSession.Client == "" {
						state.outputErr = "focused Session disappeared before PTY restore"
						state.workspacePaneFocusRestorePending = false
					} else {
						openControlForSession(restoreSession)
					}
				}
				state.notesFocusRestoreReselect = false
				if !state.focused && !state.workspacePaneFocusRestorePending {
					selectPooledOutput()
				}
				state.render(os.Stdout)
				continue
			}
			if state.handleCommandPaletteInput(b) {
				state.render(os.Stdout)
				continue
			}
			// A workspace pane modal owns keyboard input for its entire
			// lifetime, including when it was opened from a focused terminal.
			// Route this before the pending PTY gate and focused-terminal path so
			// navigation and confirmation keys cannot leak to the shell. Mouse
			// reports continue through the modal hit-test below.
			if state.workspacePaneMode && !strings.HasPrefix(string(b), "\x1b[<") {
				notesInput := state.workspacePaneStep == "notes"
				if state.handleWorkspacePaneInput(b) && !notesInput && state.workspacePaneStep != "notes" {
					// The create wizard takes ownership of the next byte immediately.
					// A focused Default pane may still have a deferred restore lease;
					// leave that lease attached to the workspace modal instead of
					// allowing it to consume the host selector's first Enter.
					state.workspacePaneFocusRestorePending = false
					state.workspacePaneRestoreFocused = false
					state.workspacePlacementInputPending = false
					deferredWorkspaceInput = nil
					state.beginCreate()
				}
				// Esc from a focused detach confirmation is handled by the
				// workspace-pane dispatcher, so hand the still-matching control
				// lease back immediately just as the central modal path does.
				if state.workspacePaneFocusRestorePending {
					restoreSession := state.activePTYSession()
					if state.workspacePaneFocusRestoreKey != "" {
						if captured, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
							restoreSession = captured
						}
						if restoreSession.Client == "" {
							restoreSession = state.workspacePaneFocusRestoreSession
						}
					}
					if control != nil && controlMatchesSession(control, restoreSession, state.ownerName) {
						state.focused = false
						// The saved restore snapshot remains authoritative until the
						// output focus setter accepts the handoff.
						if acceptWorkspaceControl(restoreSession) {
							restorePendingFocus()
						}
					} else {
						// The modal can close while the original writer is still
						// opening (or after it was detached). Reopen the exact
						// captured Session and keep the first shell command fenced
						// until that replacement writer is accepted.
						state.workspacePlacementInputPending = true
						deferredWorkspaceInput = nil
						if controlOpenCancel != nil {
							controlOpenCancel()
							controlOpenCancel = nil
							controlID++
						}
						if control != nil {
							_ = control.Stdin.Close()
							control = nil
							controlDone = nil
						}
						state.focused = false
						if restoreSession.Client == "" {
							state.outputErr = "focused Session disappeared before PTY restore"
							state.workspacePaneFocusRestorePending = false
						} else {
							openControlForSession(restoreSession)
						}
					}
				}
				resetWorkspacePaneChangeIfNeeded()
				state.render(os.Stdout)
				continue
			}
			// Help owns textual input for its entire lifetime, including while
			// focus restoration is pending or has already completed. Handle this
			// before the pending PTY gate so a focused close key cannot leak to the
			// originating shell. Mouse reports continue through the modal hit-test
			// below.
			if state.helpMode && !state.blockingModalOpen() && !strings.HasPrefix(string(b), "\x1b[<") {
				if state.helpSearchActive {
					if state.shortcut("help", string(b)) {
						state.closeHelp()
						if !state.helpMode {
							restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
						}
					} else {
						state.handleHelpSearchInput(b)
					}
				} else if state.shortcut("help", string(b)) {
					state.dispatchHelpAction("help")
					if !state.helpMode {
						restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
					}
				} else if string(b) == "/" {
					state.helpSearchActive = true
				}
				state.render(os.Stdout)
				continue
			}
			if !workspaceNavigationReplay && (state.notesFocusRestorePending || state.helpFocusRestorePending || state.workspacePaneFocusRestorePending) && state.outputFresh {
				restorePendingFocus()
				drainNotesPendingInput(state, control)
			}
			// Deferred workspace keys are replayed only after the replacement
			// control lease is ready. Keep them on the workspace navigation route;
			// passing them through the pending PTY gate would consume navigation
			// keys such as b before handleWorkspaceProjectInput sees them.
			pendingHandled := false
			if !workspaceNavigationReplay && !workspaceProjectNavigation {
				pendingHandled = handlePendingPTYInput(state, b, workspaceOutput != nil, &control, &controlOpenCancel, &controlID)
			}
			if pendingHandled {
				// The writer may be ready even when the last output frame is stale.
				// Confirm the Help lease before interpreting a completed prefix.
				if !state.focused && state.helpFocusRestorePending {
					restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
				}
				// Remember a prefix even if the writer opens between its two keys.
				if consumed, command := state.handlePanePrefix(b); consumed && command != "" {
					// Command palette is a local modal even when the prefix arrived
					// while a focused PTY control was being handed off.  Open it before
					// the generic focused-command path tears down PTY ownership; the
					// suffix must never be replayed to the shell.
					if command == " " {
						state.openCommandPalette()
						state.render(os.Stdout)
						continue
					}
					// A prefix command completed while PTY focus was still opening
					// must remain a local UI action. In particular, never forward
					// Ctrl-B followed by ? or o to the PTY.
					if paneNavigationCommand(command) {
						// Control readiness can race the two bytes of a focused
						// prefix command.  Preserve the focused handoff contract even
						// when the pending-input gate has just observed the writer as
						// unavailable; dispatchPaneCommand would leave the selection
						// in the list and let the next PTY sentinel be consumed there.
						if state.beginFocusedPaneNavigation(command) && !replayQueued {
							replayInput <- []byte("\r")
							replayQueued = true
						}
					} else if !state.focused && state.helpFocusRestorePending {
						state.panePrefixReplay = append([]byte(shortcutInput(state.cfg.Shortcut("pane_prefix"))), []byte(command)...)
					} else if state.focused && state.dispatchFocusedPaneCommand(command) {
						// A direct focused detach bypasses the workspace modal branch.
						// Run the same lease teardown, then replay Enter so the selected
						// surviving pane opens its control writer.
						if resetWorkspacePaneChangeIfNeeded() && state.workspaceAttachFromProject && !replayQueued {
							// The surviving pane is selected synchronously, but its
							// control lease opens asynchronously. Hold real shell input
							// until that lease has been accepted.
							state.workspacePlacementInputPending = true
							deferredWorkspaceInputToPTY = true
							deferredWorkspaceInput = nil
							replayInput <- []byte("\r")
							replayQueued = true
						}
					} else {
						state.dispatchPaneCommand(command)
					}
				}
				if control == nil {
					controlDone = nil
				}
				state.render(os.Stdout)
				continue
			}
			if state.handleDirectNotesInput(b) {
				state.render(os.Stdout)
				continue
			}
			// Project files owns mouse reports before byte-oriented modal input. This
			// prevents the SGR sequence from being swallowed as text or reaching PTY.
			if state.projectFiles.open && strings.HasPrefix(string(b), "\x1b[<") {
				if button, x, y, ok := parseSGRMouse(string(b)); ok {
					if button == 0 && strings.HasSuffix(string(b), "m") {
						state.projectFilesMouseRelease(x, y)
					} else if button == 0 && strings.HasSuffix(string(b), "M") {
						state.modalMouseInput(x, y)
					}
					state.render(os.Stdout)
					continue
				}
			}
			if state.handleProjectFilesInput(b) {
				state.render(os.Stdout)
				continue
			}
			mouseReport := strings.HasPrefix(string(b), "\x1b[<")
			var helpMouseKey []byte
			if button, x, y, ok := parseSGRMouse(string(b)); ok {
				if key, owned := state.handleHelpMouseReport(button, x, y, button == 0 && strings.HasSuffix(string(b), "M")); owned {
					if len(key) == 0 {
						state.render(os.Stdout)
						continue
					}
					helpMouseKey = key
				} else if state.blockingModalOpen() {
					if state.projectFiles.open && button == 0 && strings.HasSuffix(string(b), "m") {
						b = state.projectFilesMouseRelease(x, y)
						if len(b) == 0 {
							continue
						}
					} else {
						if button != 0 || !strings.HasSuffix(string(b), "M") {
							continue
						}
						b = state.modalMouseInput(x, y)
						if len(b) == 0 {
							continue
						}
					}
				} else {
					// Mouse reports are always local UI input. Never inject their
					// escape sequences into a focused remote PTY.
					if (button == 64 || button == 65) && state.contentPanePoint(x, y) && state.viewportTerminal() != nil {
						if button == 64 {
							state.scrollPTY(3)
						} else {
							state.scrollPTY(-3)
						}
						if state.copyMode {
							state.renderCopyMode(os.Stdout)
						} else {
							state.render(os.Stdout)
						}
						continue
					}
					if button == 64 || button == 65 {
						continue
					}
					if state.copyMode {
						continue
					}
					if state.focused {
						if !state.workspacePreview || (button != 0 && button != 2) || !strings.HasSuffix(string(b), "M") {
							continue
						}
						controlID++
						if controlOpenCancel != nil {
							controlOpenCancel()
							controlOpenCancel = nil
						}
						if control != nil {
							_ = control.Stdin.Close()
						}
						control, controlDone = nil, nil
						if attach != nil {
							_ = attach.Stdin.Close()
						}
						if attachCancel != nil {
							attachCancel()
							attachCancel = nil
						}
						attachID++
						resizeInFlight = false
						pendingFramebufferResize, queuedResize = nil, nil
						bufferedAttach, bufferedAttachBytes = nil, 0
						attachOutputSource = attachOut
						state.focused = false
						state.clearAttachIdentity()
						attachCanResize, attachInitialResizeQueued = false, false
						attachReplayEndOffset = 0
						attach = nil
					}
				}
			} else if mouseReport {
				continue
			}
			if len(helpMouseKey) != 0 {
				b = helpMouseKey
			}
			if state.copyMode {
				if copyModeExitInput(b) {
					state.exitCopyMode(os.Stdout)
					if state.sessionFreshlyDisplayed(state.currentSession()) {
						state.markActivitySeen(state.currentSession())
					}
					state.render(os.Stdout)
				} else if string(b) == "\x1b[A" || string(b) == "k" {
					if state.viewportTerminal() != nil {
						state.scrollPTY(1)
						state.renderCopyMode(os.Stdout)
					}
				} else if string(b) == "\x1b[B" || string(b) == "j" {
					state.scrollPTY(-1)
					state.renderCopyMode(os.Stdout)
				}
				continue
			}
			if state.helpSearchActive && !state.blockingModalOpen() {
				if state.shortcut("help", string(b)) {
					state.closeHelp()
					if !state.helpMode {
						// Closing Help must release its input fence even when the
						// last framebuffer event was marked stale. The visible output
						// lease can still be valid, and waiting for another event would
						// consume the first command sent to the originating PTY.
						restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
					}
				} else {
					state.handleHelpSearchInput(b)
				}
				state.render(os.Stdout)
				continue
			}
			if state.helpMode && !state.focused && !state.blockingModalOpen() && string(b) == "/" && !state.shortcut("help", string(b)) {
				state.helpSearchActive = true
				state.render(os.Stdout)
				continue
			}
			paneCommand := ""
			if consumed, command := state.handlePanePrefix(b); consumed {
				resetPanePrefixTimer(state.panePrefixDeadline)
				paneCommand = command
				if command == "" {
					state.render(os.Stdout)
					continue
				}
				if command == " " {
					// Space completes the local command-palette route. Keep the
					// focused control intact so Esc can restore the exact PTY origin.
					state.openCommandPalette()
					state.render(os.Stdout)
					continue
				}
				if strings.HasPrefix(command, "quick-shell:") {
					state.beginQuickShell(ctx, createDone, strings.TrimPrefix(command, "quick-shell:"))
					state.render(os.Stdout)
					continue
				}
				if state.focused && paneNavigationCommand(command) {
					nav, err := state.workspaceNavigation()
					if err == nil {
						width, height := terminalSize()
						var target string
						navigationKey := command
						switch command {
						case "n":
							navigationKey = "pagedown"
						case "p":
							navigationKey = "pageup"
						}
						target, err = nav.PaneNavigationTarget(ducklord.CalculateWorkspaceGeometry(width, height, 4), navigationKey)
						if err == nil && target == nav.CurrentPaneID() {
							state.render(os.Stdout)
							continue
						}
					}
					if err != nil {
						state.outputErr = err.Error()
						state.render(os.Stdout)
						continue
					}
				}
				if !state.focused {
					if paneNavigationCommand(command) {
						if !state.navigatePrefixPane(command) {
							state.render(os.Stdout)
							continue
						}
						requestPreview(true)
						b = []byte("\r")
					} else {
						dispatchPaneCommandAndRestoreHelpFocus(state, command, workspaceOutput, true, control, attach)
						queuePaneReplay()
						state.render(os.Stdout)
						continue
					}
				} else {
					if state.dispatchFocusedPaneCommand(command) {
						if resetWorkspacePaneChangeIfNeeded() && state.workspaceAttachFromProject && !replayQueued {
							// The surviving pane is selected synchronously, but its
							// control lease opens asynchronously. Hold real shell input
							// until that lease has been accepted.
							state.workspacePlacementInputPending = true
							deferredWorkspaceInputToPTY = true
							deferredWorkspaceInput = nil
							replayInput <- []byte("\r")
							replayQueued = true
						}
						state.render(os.Stdout)
						continue
					}
					b = []byte(shortcutInput(state.cfg.Shortcut("pty_unfocus")))
				}
			}
			if state.focused {
				if state.workspacePreview {
					if _, visibleErr := state.workspacePaneRect(); visibleErr != nil {
						controlID++
						attachID++
						if controlOpenCancel != nil {
							controlOpenCancel()
							controlOpenCancel = nil
						}
						if control != nil {
							_ = control.Stdin.Close()
							control, controlDone = nil, nil
						}
						if attachCancel != nil {
							attachCancel()
							attachCancel = nil
						}
						if attach != nil {
							_ = attach.Stdin.Close()
							attach = nil
						}
						state.focused = false
						state.clearAttachIdentity()
						state.outputErr = "Session pane is no longer visible; input was not sent: " + visibleErr.Error()
						state.render(os.Stdout)
						continue
					}
				}
				if !state.hostIsLive(state.activePTYSession().Client) {
					state.focused = false
					state.clearAttachIdentity()
					state.outputFresh = false
					state.outputErr = "host reconnecting; input was not sent"
					state.render(os.Stdout)
					continue
				}
				// Ctrl-] is the built-in PTY escape. Keep it recognized even if
				// an older or incomplete config omits the shortcut entry; under a
				// saturated PTY the escape must never be forwarded to the shell.
				if string(b) == "\x1d" || state.shortcut("pty_unfocus", string(b)) {
					if outputManager == nil {
						state.saveCurrentSnapshot()
					}
					controlID++
					if controlOpenCancel != nil {
						controlOpenCancel()
						controlOpenCancel = nil
					}
					if control != nil {
						_ = control.Stdin.Close()
					}
					control, controlDone = nil, nil
					if attach != nil {
						_ = attach.Stdin.Close()
					}
					if attachCancel != nil {
						attachCancel()
						attachCancel = nil
					}
					attachID++
					resizeInFlight = false
					pendingFramebufferResize = nil
					queuedResize = nil
					bufferedAttach = nil
					bufferedAttachBytes = 0
					attachOutputSource = attachOut
					state.focused = false
					state.clearAttachIdentity()
					// A busy PTY may have advanced the terminal while the cached
					// frame still describes the focused view. Force a complete
					// navigation repaint when returning from terminal focus.
					state.frameOutput = frameOutput{}
					if state.workspaceNav != nil && state.workspaceNav.InDetailMode() {
						state.syncDetailSelection()
					}
					attachCanResize = false
					attachInitialResizeQueued = false
					attachReplayEndOffset = 0
					attach = nil
					if paneNavigationCommand(paneCommand) {
						if !state.beginFocusedPaneNavigation(paneCommand) {
							state.render(os.Stdout)
							continue
						}
						requestPreview(true)
						b = []byte("\r")
					} else {
						if strings.HasPrefix(paneCommand, "quick-shell:") {
							state.beginQuickShell(ctx, createDone, strings.TrimPrefix(paneCommand, "quick-shell:"))
							state.render(os.Stdout)
							continue
						}
						requestPreview(true)
						if paneCommand != "" {
							state.dispatchPaneCommand(paneCommand)
						}
						queuePaneReplay()
						state.render(os.Stdout)
						continue
					}
				}
				if state.focused {
					activeSession := state.activePTYSession()
					if writer := focusedPTYWriter(control, attach, activeSession, state.ownerName); writer != nil {
						_, _ = writer.Write(b)
					} else if control != nil {
						controlID++
						if controlOpenCancel != nil {
							controlOpenCancel()
						}
						_ = control.Stdin.Close()
						control, controlDone = nil, nil
						attachCanResize = false
						state.outputErr = "PTY control changed; input was not sent"
					}
					continue
				}
			}
			if state.shouldDiscardModalArrow(b) {
				state.render(os.Stdout)
				continue
			}
			// Help is a pinned overlay: only its configured binding may toggle it.
			// Esc, quit, and Ctrl-C do not dismiss it.
			if state.helpMode && !state.blockingModalOpen() {
				if state.shortcut("help", string(b)) {
					state.dispatchHelpAction("help")
					if !state.helpMode {
						// See the search-close path above: focus restoration depends
						// on the lease, rather than on a fresh repaint arriving after
						// the close key.
						restorePendingHelpFocusWithAttach(state, workspaceOutput, true, control, attach)
					}
				}
				state.render(os.Stdout)
				continue
			}
			if string(b) == "\x03" && state.centralModalOpen() {
				switch {
				case state.projectFiles.open:
					state.handleProjectFilesInput(b)
				case state.workspacePaneMode:
					if state.workspacePaneStep == "notes" {
						state.closeNotesModal()
					} else {
						state.closeWorkspacePane()
					}
				case state.shortcutMode:
					state.shortcutMode = false
				case state.notificationConfigMode:
					state.closeNotificationConfig()
				case state.searchMode:
					state.closeSearch()
				case state.terminalSearchMode, state.terminalBookmarkMode, state.terminalBookmarkListMode:
					state.closeTerminalTool()
				case state.hostMenuMode:
					if strings.HasPrefix(state.hostMenuStep, "skills-") {
						state.closeHostMenuWithCtrlC()
						// Host Skills is opened from the workspace navigation route while
						// the existing PTY control remains alive. Restore that exact
						// output key here so the next byte reaches the original shell.
						if workspaceOutput != nil && state.activeAttachKey != "" {
							if key, ok := terminalOutputKey(state.activePTYSession()); ok && setWorkspaceInputFocus(workspaceOutput, key) {
								// The pooled output lease can be restored before the PTY
								// writer. Keep keyboard ownership fenced until both are
								// ready; otherwise the first command after Ctrl-C is
								// accepted visually and dropped by a nil writer.
								state.focused = false
								state.workspacePlacementInputPending = true
							}
						}
						// Keep an already-matching writer. Host Skills normally leaves
						// that writer alive while its pooled output lease is restored;
						// closing it here creates a replacement window in which the next
						// shell command can be lost.
						restoreSession := state.activePTYSession()
						if control != nil && controlMatchesSession(control, restoreSession, state.ownerName) {
							if acceptWorkspaceControl() {
								state.workspacePlacementInputPending = false
								if len(deferredWorkspaceInput) != 0 && !replayQueued {
									replayInput <- deferredWorkspaceInput
									deferredWorkspaceInput = nil
									deferredWorkspaceReplay = true
									replayQueued = true
								}
							}
						} else {
							if control != nil {
								_ = control.Stdin.Close()
								control, controlDone = nil, nil
							}
							if state.activeAttachKey != "" {
								_ = openControlForSession(restoreSession)
							}
						}
					} else {
						state.hostMenuMode = false
					}
				case state.addClientMode:
					state.cancelAddClient()
				case state.removeClientMode:
					state.cancelRemoveClient()
				case state.newSessionMode:
					if startCancel != nil {
						startCancel()
						startCancel = nil
						startID++
					}
					state.cancelCreate()
				case state.notificationMode:
					state.closeNotificationSettings()
				case state.groupMenu:
					state.closeGroupMenu()
				case state.actionMenu:
					state.closeActionMenu()
				case state.sessionRenameMode:
					state.closeSessionHandleRename()
				case state.lifecycleConfirm != "":
					state.lifecycleConfirm = ""
					state.lifecycleReturnToAction = false
					state.lifecycleTarget = ducklord.RemoteSession{}
					state.lifecycleMode = ""
				}
				state.render(os.Stdout)
				continue
			}
			if state.searchMode {
				if state.searchPendingRequestID != 0 {
					if string(b) == "\x1b" || string(b) == "\x03" {
						state.searchPendingRequestID = 0
						state.closeSearch()
						selectPooledOutput()
						state.render(os.Stdout)
						continue
					}
					state.searchErr = "switching PTY..."
					state.render(os.Stdout)
					continue
				}
				action := state.handleSearchInput(b)
				if action == "cancel" {
					state.closeSearch()
				} else if action == "activate" {
					target, ok := state.searchResult()
					key := sessionKey(target)
					if ok && state.searchResultIsActivated(target) {
						if !state.prepareSearchWorkspaceSelection(key) {
							state.render(os.Stdout)
							continue
						}
						state.closeSearch()
						b = []byte("\r")
					} else if !ok || outputManager == nil {
						state.searchErr = "PTY output activation is unavailable"
						state.render(os.Stdout)
						continue
					} else if _, valid := terminalOutputKey(target); !valid || !canRead(target) || !state.hostIsLive(target.Client) {
						state.searchErr = "selected session is unavailable"
						state.render(os.Stdout)
						continue
					} else {
						client, clientErr := mustClient(cfg, target.Client)
						if clientErr != nil {
							state.searchErr = clientErr.Error()
							state.render(os.Stdout)
							continue
						}
						rows, cols := state.activePTYSize()
						requestID := outputManager.Select(ducklord.TerminalSelection{Client: client, InstanceID: target.InstanceID, SessionID: target.SessionID,
							RuntimeGeneration: target.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
						outputRequestID = requestID
						state.searchPendingRequestID = requestID
						state.searchPendingKey = key
						state.searchPendingGeneration = target.RuntimeGeneration
						state.searchPendingRevision = state.searchRevision
						state.searchErr = "switching PTY..."
						go func(id uint64) {
							timer := time.NewTimer(15 * time.Second)
							defer timer.Stop()
							select {
							case <-timer.C:
								select {
								case searchActivationTimeout <- id:
								case <-ctx.Done():
								}
							case <-ctx.Done():
							}
						}(requestID)
						state.render(os.Stdout)
						continue
					}
				}
				if state.searchMode {
					state.render(os.Stdout)
					continue
				}
			}
			if !state.workspacePaneMode && state.workspacePreview {
				if state.syncCurrentNotes() {
					if state.handleNotesInput(b) {
						state.render(os.Stdout)
						continue
					}
				}
			}
			if state.shortcutMode {
				if state.handleShortcutInput(b) == "restart-tui" {
					return errRestartTUI
				}
				state.render(os.Stdout)
				continue
			}
			if state.notificationConfigMode {
				if state.handleNotificationConfigInput(b) == "restart-tui" {
					return errRestartTUI
				}
				state.render(os.Stdout)
				continue
			}
			if state.hostMenuMode {
				action := state.handleHostMenuInput(b)
				target := state.hostMenuTarget
				targets := []string{target}
				var connectTargets []string
				if action == "host-apply-connections" {
					targets = state.changedHostMenuTargets(false)
					connectTargets = state.changedHostMenuTargets(true)
				}
				switch action {
				case "host-resources-read":
					state.hostMenuRequestID++
					requestID := state.hostMenuRequestID
					client, ok := state.cfg.Client(target)
					if !ok || state.disconnectedHosts[target] {
						state.hostMenuStep = "resources-error"
						state.hostMenuErr = "Host is disconnected or unavailable"
						break
					}
					epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
					readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
					go func() {
						defer cancel()
						status, err := runner.HostResources(readCtx, client)
						publishHostResourceEvent(ctx, hostResourceDone, hostResourceEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, status: status, err: err})
					}()
				case "host-hooks-read":
					launchHostHookStatus(target)
				case "host-hook-save":
					client, ok := state.cfg.Client(target)
					if !ok || state.disconnectedHosts[target] {
						state.hostMenuStep = "hook-error"
						state.hostMenuErr = "Host is disconnected or unavailable"
						break
					}
					requestID := state.hostMenuRequestID + 1
					if !state.beginHostHookOperation(target, requestID) {
						state.hostMenuStep = "hook-error"
						state.hostMenuErr = "Another hook update is still running on this Host"
						break
					}
					if hostRetentionCancel != nil {
						hostRetentionCancel()
					}
					state.hostMenuRequestID = requestID
					epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
					agent, operation := state.hostHookAgent, state.hostHookAction
					saveCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
					hostRetentionCancel = cancel
					go func() {
						defer cancel()
						result, err := runner.ConfigureHostAgentHook(saveCtx, client, agent, operation)
						select {
						case hostHookDone <- hostHookEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, agent: agent, action: operation, result: result, err: err}:
						case <-ctx.Done():
						}
					}()
				case "host-retention-read":
					if hostRetentionCancel != nil {
						hostRetentionCancel()
					}
					state.hostMenuRequestID++
					requestID := state.hostMenuRequestID
					epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
					client, ok := state.cfg.Client(target)
					if !ok || state.disconnectedHosts[target] {
						state.hostMenuStep = "retention-error"
						state.hostMenuErr = "Host is disconnected or unavailable"
						break
					}
					readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
					hostRetentionCancel = cancel
					go func() {
						defer cancel()
						days, err := runner.HostLogRetention(readCtx, client)
						select {
						case hostRetentionDone <- hostRetentionEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, days: days, err: err}:
						case <-ctx.Done():
						}
					}()
				case "host-retention-save":
					if hostRetentionCancel != nil {
						hostRetentionCancel()
					}
					state.hostMenuRequestID++
					requestID := state.hostMenuRequestID
					epoch, instanceID := watchedClients[target].epoch, state.hostSync[target].InstanceID
					client, ok := state.cfg.Client(target)
					if !ok || state.disconnectedHosts[target] {
						state.hostMenuStep = "retention-error"
						state.hostMenuErr = "Host is disconnected or unavailable"
						break
					}
					days, _ := strconv.Atoi(state.hostMenuDraft)
					saveCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
					hostRetentionCancel = cancel
					go func() {
						defer cancel()
						err := runner.SetHostLogRetention(saveCtx, client, days)
						if err == nil {
							var actual int
							actual, err = runner.HostLogRetention(saveCtx, client)
							if err == nil && actual != days {
								err = fmt.Errorf("host reported %d retention days after update, expected %d", actual, days)
							}
						}
						select {
						case hostRetentionDone <- hostRetentionEvent{id: requestID, epoch: epoch, instanceID: instanceID, host: target, days: days, err: err, saved: true}:
						case <-ctx.Done():
						}
					}()
				case "host-notification-settings":
					state.hostMenuMode = false
					state.beginNotificationConfig("host", target)
				case "host-add":
					state.hostMenuMode = false
					state.beginAddClient()
				case "host-remove":
					state.hostMenuMode = false
					state.beginRemoveClient()
				case "host-disconnect", "host-reconnect", "host-apply-connections":
					for _, target := range targets {
						if target == state.hostMenuTarget && hostRetentionCancel != nil {
							hostRetentionCancel()
							hostRetentionCancel = nil
						}
						wasActive := state.hostOwnsActivePTY(target, control)
						if current, ok := watchedClients[target]; ok {
							current.cancel()
							delete(watchedClients, target)
						}
						previous := state.hostSync[target]
						state.bumpHostConnectionEpoch(target)
						state.disconnectedHosts[target] = true
						if state.newSessionMode && state.newSessionClient == target {
							state.cancelCreateDiscovery()
							state.cancelPathSuggestions()
							state.newSessionErr = "host disconnected; pending session operation was canceled"
						}
						if outputManager != nil && previous.InstanceID != "" {
							// A user disconnect is reversible and keeps the desired LRU
							// membership. ForgetHost is reserved for daemon replacement
							// and permanent host removal.
							outputManager.SyncHost(target, previous.InstanceID, false, nil)
						}
						if wasActive {
							if attachCancel != nil {
								attachCancel()
								attachCancel = nil
							}
							if controlOpenCancel != nil {
								controlOpenCancel()
								controlOpenCancel = nil
							}
							if control != nil {
								_ = control.Stdin.Close()
								control, controlDone = nil, nil
							}
							state.focused = false
							state.clearAttachIdentity()
						}
						state.hostSync[target] = ducklord.SessionUpdate{Client: target, InstanceID: previous.InstanceID, Generation: previous.Generation, Revision: previous.Revision, State: "disconnected"}
						state.hostMenuMode = false
						state.outputErr = "disconnected " + target + "; its sessions and notifications are detached"
						if action == "host-reconnect" {
							delete(state.disconnectedHosts, target)
							state.hostSync[target] = ducklord.SessionUpdate{Client: target, State: "reconnecting"}
							if client, ok := state.cfg.Client(target); ok {
								watchClient(client)
							}
							if !eventDriven {
								state.refreshSessions(ctx)
							}
							state.outputErr = "reconnecting " + target
						}
					}
					if outputManager != nil {
						selectPooledOutput()
					}
					if action == "host-apply-connections" {
						for _, target := range connectTargets {
							state.bumpHostConnectionEpoch(target)
							delete(state.disconnectedHosts, target)
							state.hostSync[target] = ducklord.SessionUpdate{Client: target, State: "connecting"}
							if client, ok := state.cfg.Client(target); ok {
								watchClient(client)
							}
							if !eventDriven {
								state.refreshSessions(ctx)
							}
						}
						state.hostMenuMode = false
						state.outputErr = fmt.Sprintf("host connections updated: %d connecting, %d disconnected", len(connectTargets), len(targets))
					}
				case "host-connect":
					for _, target := range targets {
						if !state.disconnectedHosts[target] {
							continue
						}
						state.bumpHostConnectionEpoch(target)
						delete(state.disconnectedHosts, target)
						state.hostSync[target] = ducklord.SessionUpdate{Client: target, State: "connecting"}
						if client, ok := state.cfg.Client(target); ok {
							watchClient(client)
						}
						if !eventDriven {
							state.refreshSessions(ctx)
						}
						state.hostMenuMode = false
						state.outputErr = "connecting " + target
					}
				}
				if !state.hostMenuMode && hostRetentionCancel != nil {
					hostRetentionCancel()
					hostRetentionCancel = nil
				}
				state.render(os.Stdout)
				continue
			}
			if state.addClientMode {
				if state.addClientBusy {
					if string(b) == "\x03" || string(b) == "\x1b" || string(b) == "q" {
						state.cancelAddClient()
						state.render(os.Stdout)
					}
					continue
				}
				action := state.handleAddClientInput(b)
				switch action {
				case "cancel":
					state.cancelAddClient()
				case "submit":
					if err := state.startAddClient(ctx, addClientDone); err != nil {
						state.addClientErr = err.Error()
					}
				}
				state.render(os.Stdout)
				continue
			}
			if state.removeClientMode {
				action := state.handleRemoveClientInput(b)
				if action == "remove" {
					name := state.removeClientConfirm
					instance := state.hostSync[name].InstanceID
					if err := state.removeClient(ctx, name); err != nil {
						state.outputErr = err.Error()
					} else {
						if current, ok := watchedClients[name]; ok {
							current.cancel()
							delete(watchedClients, name)
						}
						state.disconnectedHosts[name] = true
						state.bumpHostConnectionEpoch(name)
						if outputManager != nil {
							outputManager.ForgetHost(name, instance)
							selectPooledOutput()
						}
					}
				}
				state.render(os.Stdout)
				continue
			}
			if state.newSessionMode {
				if state.newSessionStarting || state.newSessionDiscovering {
					if string(b) == "\x03" {
						if state.newSessionStarting && startCancel != nil {
							startCancel()
							startCancel = nil
							startID++ // fence a Start result racing with Ctrl+C
						}
						state.cancelCreate()
						state.render(os.Stdout)
					} else if string(b) == "\x1b" {
						if state.newSessionStarting && startCancel != nil {
							startCancel()
							startCancel = nil
							startID++
							state.newSessionStarting = false
						} else {
							if state.newSessionStep == "path" && state.newSessionProject.Path != "" {
								state.newSessionLine = state.newSessionProject.Path
							}
							creatingDirectory := state.newSessionStep == "path-confirm"
							state.cancelCreateDiscovery()
							if creatingDirectory {
								state.newSessionErr = "request canceled; the directory may have been created—recheck before retrying"
							} else {
								state.newSessionErr = "operation canceled; edit or press Esc to go back"
							}
						}
						state.render(os.Stdout)
					}
					continue
				}
				beforeLine := state.newSessionLine
				action := state.handleCreateInput(b)
				if action == "" && state.newSessionStep == "path" && state.newSessionLine != beforeLine {
					state.requestPathSuggestions(ctx, pathSuggestionsDone)
				}
				switch action {
				case "cancel":
					state.cancelCreate()
				case "submit":
					_, clientName, args, ready, err := state.submitCreateStep(ctx, createDone)
					if err != nil {
						state.newSessionErr = err.Error()
						break
					}
					if !ready {
						break
					}
					if !state.hostIsLive(state.newSessionClient) {
						state.newSessionErr = "host is reconnecting; session creation is disabled until synchronization completes"
						break
					}
					c, err := mustClient(cfg, state.newSessionClient)
					if err != nil {
						return err
					}
					startCtx, cancel := context.WithCancel(ctx)
					startCancel = cancel
					startID++
					id := startID
					state.newSessionStarting = true
					state.newSessionStartGeneration, state.newSessionStartInstance = state.hostFingerprint(clientName)
					state.newSessionStartEpoch = state.hostConnectionEpoch[clientName]
					state.newSessionErr = "starting..."
					go func() {
						sessionID, err := runner.Start(startCtx, c, args)
						select {
						case startDone <- startDoneEvent{id: id, client: clientName, sessionID: sessionID, err: err}:
						case <-ctx.Done():
						}
					}()
				}
				state.render(os.Stdout)
				continue
			}
			if state.notificationMode {
				action := state.handleNotificationInput(b)
				switch action {
				case "save":
					state.saveNotificationSettings()
				case "cancel":
					state.closeNotificationSettings()
				}
				state.render(os.Stdout)
				continue
			}
			if state.groupMenu {
				state.handleGroupMenuInput(b)
				state.render(os.Stdout)
				continue
			}
			if state.sessionRenameMode {
				action := state.handleSessionHandleRenameInput(b)
				if action == "rename-submit" {
					target := state.sessionRenameTarget
					client, clientErr := mustClient(cfg, target.Client)
					if clientErr != nil {
						state.sessionRenameErr = clientErr.Error()
					} else {
						_, renameErr := runner.RenameSelected(ctx, client, target, strings.TrimSpace(state.sessionRenameLine))
						if renameErr != nil {
							state.sessionRenameErr = sanitizeTerminalText(renameErr.Error())
						} else {
							state.closeSessionHandleRename()
							state.outputErr = "Session handle renamed"
							state.refreshSessions(ctx)
						}
					}
				}
				state.render(os.Stdout)
				continue
			}
			if state.actionMenu {
				action := state.handleActionMenuInput(b)
				state.render(os.Stdout)
				if action == "" {
					continue
				}
				if action == "cancel" {
					continue
				}
				// Selection is rebound to the exact captured identity before the
				// ordinary dispatch path runs. A stale menu always fails closed.
				if !state.selectActionTarget() {
					state.outputErr = "session changed while action menu was open; reopen the menu"
					state.actionTarget = ducklord.RemoteSession{}
					state.actionOperation, state.actionMode = "", ""
					state.render(os.Stdout)
					continue
				}
				target := state.actionTarget
				selectedActionTarget = &target
				state.actionTarget = ducklord.RemoteSession{}
				if action == "lifecycle-action" {
					if state.lifecycleBusy {
						state.actionOperation, state.actionMode = "", ""
						state.outputErr = "another lifecycle operation is still pending; wait for its result"
						state.render(os.Stdout)
						continue
					}
					state.lifecycleConfirm = state.actionOperation
					state.lifecycleReturnToAction = true
					state.lifecycleMode = state.actionMode
					state.lifecycleTarget = target
					state.actionOperation, state.actionMode = "", ""
					state.render(os.Stdout)
					continue
				}
				if action == "notifications-action" {
					state.beginNotificationSettings()
					state.render(os.Stdout)
					continue
				}
				if action == "rename-handle-action" {
					state.beginSessionHandleRename(target)
					state.render(os.Stdout)
					continue
				}
				// Attach and yield reuse the canonical handlers below.
				b = nil
				switch action {
				case "attach":
					b = []byte("\r")
				case "reconnect":
					if outputManager == nil {
						state.refreshSelectedOutput(ctx)
						state.render(os.Stdout)
						continue
					}
					client, clientErr := mustClient(cfg, target.Client)
					if clientErr != nil {
						state.outputErr = clientErr.Error()
						state.render(os.Stdout)
						continue
					}
					rows, cols := state.activePTYSize()
					outputRequestID = outputManager.Reconnect(ducklord.TerminalSelection{Client: client, InstanceID: target.InstanceID, SessionID: target.SessionID,
						RuntimeGeneration: target.RuntimeGeneration, Rows: int(rows), Cols: int(cols)})
					reconnectRequestID = outputRequestID
					activeOutputEvent = ducklord.TerminalOutputEvent{}
					state.outputReconnecting = true
					state.outputStale = true
					state.outputFresh = false
					state.outputErr = "reconnecting PTY output from Ducklion..."
					state.render(os.Stdout)
					continue
				case "yield":
					b = []byte("y")
				case "yield-wait":
					b = []byte("Y")
				}
			}
			if state.lifecycleConfirm != "" {
				text := string(b)
				if text == "\x1b" || text == "q" {
					returnToAction := state.lifecycleReturnToAction
					target := state.lifecycleTarget
					state.lifecycleConfirm = ""
					state.lifecycleReturnToAction = false
					state.lifecycleTarget = ducklord.RemoteSession{}
					state.lifecycleMode = ""
					if text == "\x1b" && returnToAction {
						state.actionMenu = true
						state.actionTarget = target
					}
					state.render(os.Stdout)
					continue
				}
				mode := state.lifecycleMode
				sess := state.lifecycleTarget
				if mode == "" && sess.Kind != string(model.KindShell) && state.lifecycleConfirm == protocol.SessionLifecycleRestart {
					mode = protocol.SessionLifecycleWait
				}
				if mode == "" {
					mode = protocol.SessionLifecycleImmediate
				}
				if sess.Kind == string(model.KindShell) && text != "\r" && text != "\n" {
					continue
				}
				if state.lifecycleMode != "" && text != "\r" && text != "\n" {
					continue
				}
				if state.lifecycleMode == "" && text == "w" && state.lifecycleConfirm != protocol.SessionLifecycleRestart {
					mode = protocol.SessionLifecycleWait
				} else if state.lifecycleMode == "" && text == "f" {
					mode = protocol.SessionLifecycleForce
				} else if text != "\r" && text != "\n" {
					continue
				}
				if !state.lifecycleTargetIsCurrent() {
					state.outputErr = "session changed while awaiting confirmation; reopen the lifecycle action"
					state.lifecycleConfirm = ""
					state.lifecycleTarget = ducklord.RemoteSession{}
					state.lifecycleMode = ""
					state.render(os.Stdout)
					continue
				}
				client, clientErr := mustClient(cfg, sess.Client)
				if clientErr != nil {
					state.outputErr = clientErr.Error()
					state.lifecycleConfirm = ""
					state.lifecycleTarget = ducklord.RemoteSession{}
					continue
				}
				operation := state.lifecycleConfirm
				lifecycleRequestID++
				requestID := lifecycleRequestID
				generation, instance := state.hostFingerprint(sess.Client)
				epoch := watchedClients[sess.Client].epoch
				connectionEpoch := state.hostConnectionEpoch[sess.Client]
				state.lifecycleBusy = true
				state.lifecycleConfirm = ""
				state.lifecycleTarget = ducklord.RemoteSession{}
				state.lifecycleMode = ""
				state.outputErr = string(operation) + " accepted; Ducklion will continue it across reconnects"
				go func() {
					result, err := runner.LifecycleSelected(ctx, client, sess, operation, mode)
					select {
					case lifecycleDone <- lifecycleDoneEvent{id: requestID, watchEpoch: epoch, connectionEpoch: connectionEpoch,
						hostGeneration: generation, hostInstance: instance,
						operation: operation, client: sess.Client, result: result, err: err}:
					case <-ctx.Done():
					}
				}()
				state.render(os.Stdout)
				continue
			}
			wasWorkspaceDragging := state.workspaceDragMoved
			handledWorkspaceMouse, changedWorkspaceMouse := state.handleWorkspaceMouse(b)
			if wasWorkspaceDragging != state.workspaceDragMoved {
				shape := "default"
				if state.workspaceDragMoved {
					shape = "grabbing"
				}
				fmt.Fprint(os.Stdout, mouseCursorShape(shape))
			}
			if handledWorkspaceMouse {
				if changedWorkspaceMouse && workspaceOutput != nil {
					selectPooledOutput()
				}
				if state.workspaceMouseFocus {
					state.workspaceMouseFocus = false
					state.workspaceAttachFromProject = true
					state.workspaceFocusFromProject = true
					state.selectedGroupID = ""
					b = []byte("\r")
					if state.workspaceNav != nil && state.workspaceNav.InDetailMode() {
						b = []byte(shortcutInput(state.cfg.Shortcut("detail_focus")))
					}
				} else {
					state.render(os.Stdout)
					continue
				}
			}
			wasDetailed := state.workspaceNav != nil && state.workspaceNav.InDetailMode()
			previousDetail := state.detailSelected
			if detailAction := state.handleDetailedInput(b); detailAction != "" {
				switch detailAction {
				case "changed":
					isDetailed := state.workspaceNav != nil && state.workspaceNav.InDetailMode()
					if workspaceOutput != nil && (wasDetailed != isDetailed || previousDetail != state.detailSelected) {
						selectPooledOutput()
					}
					state.render(os.Stdout)
					continue
				case "focus", "jump":
					state.workspaceAttachFromProject = true
					state.workspaceFocusFromProject = detailAction == "jump"
					b = []byte("\r") // continue through the owner-gated attach action
				}
			}
			if handled, changed := state.handleWorkspaceProjectInput(b); handled {
				if changed && control != nil && !state.focused {
					_ = control.Stdin.Close()
					control, controlDone = nil, nil
					state.clearAttachIdentity()
				}
				if changed && workspaceOutput != nil {
					selectPooledOutput()
				}
				state.render(os.Stdout)
				continue
			}
			wasDragging := state.dragSession.Key() != ""
			action := state.handleInput(b)
			isDragging := state.dragSession.Key() != ""
			if wasDragging != isDragging {
				shape := "default"
				if isDragging {
					shape = "grabbing"
				}
				fmt.Fprint(os.Stdout, mouseCursorShape(shape))
			}
			switch action {
			case "quit":
				return nil
			case "help":
				state.dispatchHelpAction(action)
			case "project-files":
				state.openProjectFiles()
			case "shortcut-settings":
				state.beginShortcutSettings()
			case "notification-settings":
				state.beginNotificationConfig("global", "")
			case "host-actions":
				state.beginHostMenu()
			case "refresh":
				state.refreshSessions(ctx)
				if state.activeAttachKey == "" {
					requestPreview(true)
				}
			case "organize":
				state.cycleOrganizationMode()
			case "quick-sort":
				state.cycleQuickSort()
			case "quick-sort-direction":
				state.reverseQuickSortTime()
			case "groups":
				state.beginGroupMenu()
			case "reorder-up":
				state.moveSelectedSession(-1)
			case "reorder-down":
				state.moveSelectedSession(1)
			case "select":
				state.handleQuickSelection(&control, &controlDone, requestPreview)
			case "new":
				state.workspaceNewSessionIntent = nil
				state.beginCreate()
			case "search":
				state.beginSearch()
			case "notifications":
				state.beginNotificationSettings()
			case "copy-mode":
				state.enterCopyMode(os.Stdout)
				continue
			case "actions":
				state.beginActionMenu()
			case "end", "restart", "destroy":
				if state.lifecycleBusy {
					state.outputErr = "another lifecycle operation is still pending; wait for its result"
					break
				}
				sess := state.currentSession()
				if sess.SessionID == "" || !state.hostIsLive(sess.Client) {
					state.outputErr = "session lifecycle is unavailable while the host is reconnecting"
					break
				}
				if sess.Kind == string(model.KindAgent) && (sess.WriterKind != string(model.OwnerTerminal) || sess.WriterID != state.ownerName) {
					state.outputErr = "read-only session; yield control to this Ducklord before changing its lifecycle"
					break
				}
				state.lifecycleConfirm = protocol.SessionLifecycleOperation(action)
				state.lifecycleReturnToAction = false
				state.lifecycleTarget = sess
				state.lifecycleMode = ""
				state.outputErr = ""
			case "add-client":
				state.beginAddClient()
			case "remove-client":
				state.beginRemoveClient()
			case "yield", "yield-wait":
				if len(state.sessions) == 0 {
					break
				}
				sess := state.sessions[state.selected]
				if !state.hostIsLive(sess.Client) {
					state.outputErr = "host is reconnecting; yield is disabled until session state is synchronized"
					break
				}
				client, err := mustClient(cfg, sess.Client)
				if err != nil {
					state.outputErr = err.Error()
					break
				}
				var result protocol.SessionYieldResult
				if selectedActionTarget != nil {
					result, err = runner.YieldSelected(ctx, client, *selectedActionTarget, action == "yield-wait")
				} else {
					ref := sess.SessionID
					if ref == "" {
						ref = sess.Name
					}
					result, err = runner.Yield(ctx, client, ref, action == "yield-wait")
				}
				if err != nil {
					state.outputErr = err.Error()
					break
				}
				state.refreshSessions(ctx)
				state.outputErr = fmt.Sprintf("yield %s (epoch %d)", result.Decision, result.OwnershipEpoch)
			case "attach":
				if control != nil && !state.focused {
					_ = control.Stdin.Close()
					control, controlDone = nil, nil
					state.clearAttachIdentity()
				}
				if len(state.sessions) == 0 {
					continue
				}
				s := state.currentSession()
				if state.workspaceAttachFromProject || state.workspacePaneFocusRestorePending && state.workspacePaneFocusRestoreKey != "" {
					state.workspaceAttachFromProject = false
					if state.workspacePaneFocusRestorePending && state.workspacePaneFocusRestoreKey != "" {
						var ok bool
						s, ok = state.sessionForKey(state.workspacePaneFocusRestoreKey)
						if !ok {
							if state.workspacePaneFocusRestoreSession.Client == "" {
								state.outputErr = "focused Session disappeared before PTY restore"
								break
							}
							s = state.workspacePaneFocusRestoreSession
						}
					} else {
						var targetErr error
						s, targetErr = state.workspaceSelectedPaneSession()
						if targetErr != nil {
							state.outputErr = targetErr.Error()
							break
						}
					}
					state.activeAttachKey = sessionKey(s)
				}
				if state.workspacePreview {
					if _, visibleErr := state.workspacePaneRect(); visibleErr != nil {
						state.outputErr = "cannot focus hidden Session pane: " + visibleErr.Error()
						state.clearAttachIdentity()
						break
					}
				}
				if !canAttach(s) || !state.hostIsLive(s.Client) {
					if !state.hostIsLive(s.Client) {
						state.outputErr = "host is reconnecting; PTY controls are disabled"
					}
					state.clearAttachIdentity()
					continue
				}
				c, err := mustClient(cfg, s.Client)
				if err != nil {
					return err
				}
				fencePreview()
				if outputManager != nil {
					openControlForSession(s)
					break
				}
				attachCtx, cancel := context.WithCancel(ctx)
				sessionRef := s.SessionID
				if sessionRef == "" {
					sessionRef = s.Name
				}
				var session *ducklord.AttachSession
				if resumer, ok := runner.(interface {
					AttachStreamFrom(context.Context, ducklord.Client, string, ducklord.AttachResume) (*ducklord.AttachSession, error)
				}); ok && state.terminal != nil && state.outputForKey == sessionKey(s) && state.terminalCursorValid {
					session, err = resumer.AttachStreamFrom(attachCtx, c, sessionRef, ducklord.AttachResume{RuntimeGeneration: state.terminalGeneration, OutputOffset: state.terminalOffset})
				} else {
					session, err = runner.AttachStream(attachCtx, c, sessionRef)
				}
				if err != nil {
					cancel()
					state.outputErr = err.Error()
					break
				}
				attach = session
				attachCancel = cancel
				state.resizeStatus = ""
				attachID++
				resizeInFlight = false
				pendingFramebufferResize = nil
				queuedResize = nil
				bufferedAttach = nil
				bufferedAttachBytes = 0
				attachOutputSource = attachOut
				id := attachID
				state.activeAttachKey = ""
				state.activeAttachFresh = false
				state.pendingAttachKey = sessionKey(s)
				attachCanResize = state.canResizeCurrentSession() && session.ResizeBarrier != nil
				attachInitialResizeQueued = false
				attachReplayEndOffset = session.ReplayEndOffset
				state.outputForKey = state.pendingAttachKey
				if !session.ExactResume {
					state.outputText = ""
					state.terminal = nil
					state.terminalGeneration = session.RuntimeGeneration
					state.terminalOffset = session.StartOffset
					state.terminalCursorValid = false
				} else {
					state.outputStale = false
				}
				state.outputErr = ""
				state.outputStale = false
				state.outputFresh = false
				// A detach modal can reopen the saved PTY while its focus lease is
				// still pending. Keep the UI unfocused until the control writer is
				// accepted; otherwise workspaceControlMayAccept rejects that writer
				// and the next input falls through to the Session list.
				state.focused = !state.workspacePaneFocusRestorePending
				go superviseAttach(attachCtx, id, session, attachOut)
				if initialReplayCaughtUp(session.StartOffset, session.ReplayEndOffset) && attachCanResize {
					attachInitialResizeQueued = true
					queueResize()
				}
			}
			state.render(os.Stdout)
		}
	}
}

// dispatchPaneCommandAndRestoreHelpFocus closes the focus lease when a Help
// action is dispatched through the workspace prefix route (including a
// clicked Help action). Other close paths restore focus at their input site.
func dispatchPaneCommandAndRestoreHelpFocus(state *tuiState, command string, output workspaceInputFocusSetter, ready bool, control *ducklord.ControlSession, attach *ducklord.AttachSession) {
	state.dispatchPaneCommand(command)
	if command == "?" && !state.helpMode && state.helpFocusRestorePending {
		restorePendingHelpFocusWithAttach(state, output, ready, control, attach)
	}
}

type workspaceInputFocusSetter interface {
	SetInputFocus(ducklord.OutputKey) error
}

func setWorkspaceInputFocus(output workspaceInputFocusSetter, key ducklord.OutputKey) bool {
	return output != nil && output.SetInputFocus(key) == nil
}

// A control completion can arrive after the attach stream that was replaced
// by an Esc restore. Keep the restored Session focus fenced until that event
// is consumed; otherwise the late completion clears focus and routes the next
// byte to the workspace list.
func consumeWorkspaceRestoreControlDone(state *tuiState) bool {
	if !state.workspacePaneFocusRestoreLease && !state.workspacePaneFocusRestorePending {
		return false
	}
	state.workspacePaneFocusRestoreLease = false
	return true
}

func restorePendingWorkspacePaneFocus(state *tuiState, output workspaceInputFocusSetter, ready bool, control *ducklord.ControlSession) bool {
	if !state.workspacePaneFocusRestorePending || control == nil {
		return false
	}
	if !ready || output == nil {
		return false
	}
	session := state.activePTYSession()
	if state.workspacePaneFocusRestoreKey != "" {
		if restored, ok := state.sessionForKey(state.workspacePaneFocusRestoreKey); ok {
			session = restored
		} else if state.workspacePaneFocusRestoreSession.Client != "" {
			session = state.workspacePaneFocusRestoreSession
		}
	}
	key, ok := terminalOutputKey(session)
	if !ok || !setWorkspaceInputFocus(output, key) {
		return false
	}
	state.workspacePaneFocusRestorePending = false
	state.workspacePaneFocusRestoreKey = ""
	state.workspacePaneFocusRestoreLease = true
	state.focused = true
	return true
}

func restorePendingNotesFocus(state *tuiState, output workspaceInputFocusSetter, ready bool) bool {
	if !state.notesFocusRestorePending {
		return false
	}
	if !ready || output == nil {
		state.focused = false
		return false
	}
	key, ok := terminalOutputKey(state.activePTYSession())
	if !ok || !setWorkspaceInputFocus(output, key) {
		state.focused = false
		return false
	}
	state.notesFocusRestorePending = false
	state.focused = true
	state.outputErr = ""
	return true
}

func restorePendingHelpFocus(state *tuiState, output workspaceInputFocusSetter, ready bool, control *ducklord.ControlSession) bool {
	return restorePendingHelpFocusWithAttach(state, output, ready, control, nil)
}

func restorePendingHelpFocusWithAttach(state *tuiState, output workspaceInputFocusSetter, ready bool, control *ducklord.ControlSession, attach *ducklord.AttachSession) bool {
	if !state.helpFocusRestorePending || !ready || output == nil {
		return false
	}
	// ControlSession is the normal writer, but an attach stream can remain the
	// live PTY writer while control is opening or being replaced.  Treat the
	// two writers independently so a stale control event cannot block the valid
	// attach fallback.
	// Help captures the originating attach key before it relinquishes focus.
	// Use that identity when closing: selection/output bookkeeping may advance
	// while the overlay is open, so activePTYSession can already refer to a
	// different pane (or no pane at all).
	origin := state.activePTYSession()
	if state.helpPendingInputKey != "" {
		if captured, ok := state.sessionForKey(state.helpPendingInputKey); ok {
			origin = captured
		}
	}
	controlReady := control != nil && controlMatchesSession(control, origin, state.ownerName)
	attachReady := attach != nil && attach.Stdin != nil && state.helpPendingInputKey != "" && state.helpPendingInputKey == state.activeAttachKey
	if !controlReady && !attachReady {
		return false
	}
	key, ok := terminalOutputKey(origin)
	if !ok || !setWorkspaceInputFocus(output, key) {
		return false
	}
	state.helpFocusRestorePending = false
	state.helpPendingInputKey = ""
	state.focused = true
	state.outputErr = ""
	return true
}

func queueNotesPendingInput(state *tuiState, input []byte) bool {
	if !state.notesFocusRestorePending || state.notesPendingInputKey == "" || len(input) == 0 {
		return false
	}
	if len(state.notesPendingInput)+len(input) > notesPendingInputLimit {
		dropNotesPendingInput(state)
		state.notesFocusRestorePending = false
		state.outputErr = "post-Notes input was dropped while focus was restoring"
		return true
	}
	state.notesPendingInput = append(state.notesPendingInput, input...)
	return true
}

func dropNotesPendingInput(state *tuiState) {
	state.notesPendingInputKey = ""
	state.notesPendingInput = nil
}

func drainNotesPendingInput(state *tuiState, control *ducklord.ControlSession) bool {
	if state.notesFocusRestorePending || len(state.notesPendingInput) == 0 || control == nil {
		return false
	}
	if state.notesPendingInputKey == "" || state.notesPendingInputKey != state.activeAttachKey || !controlMatchesSession(control, state.activePTYSession(), state.ownerName) {
		dropNotesPendingInput(state)
		return false
	}
	queued := state.notesPendingInput
	dropNotesPendingInput(state)
	_, err := control.Stdin.Write(queued)
	return err == nil
}

// toggleHelp is the central dispatcher for opening and closing the help modal.
func (s *tuiState) toggleHelp() {
	if s.helpMode {
		s.closeHelp()
		// The output lease may have restored focus immediately before the
		// closing key is dispatched. In that case the fence is stale and must
		// not consume the first byte sent to the restored PTY. Keep it only
		// when focus is still waiting for the asynchronous lease.
		if s.helpFocusRestorePending && !s.focused {
			return
		}
		s.helpFocusRestorePending = false
		s.helpPendingInputKey = ""
		return
	}
	if s.focused {
		s.helpOriginFocused = true
		s.helpFocusRestorePending = true
		s.helpPendingInputKey = s.activeAttachKey
		s.focused = false
	} else {
		s.helpOriginFocused = false
	}
	s.helpMode = true
}

// closeHelp retires all help-local state, including the deferred focus fence.
// Help can be closed from both the pinned overlay and its search mode; keeping
// the latter as a separate assignment path can leave the next PTY byte queued
// forever after focus was restored while the close key was in flight.
func (s *tuiState) closeHelp() {
	s.helpMode = false
	s.helpOriginFocused = false
	s.helpOffset = 0
	s.helpSearchActive = false
	s.helpSearchQuery = ""
	if s.helpFocusRestorePending && !s.focused {
		return
	}
	s.helpFocusRestorePending = false
	s.helpPendingInputKey = ""
}

// dispatchHelpAction is the central event-loop action for the pinned help
// overlay. The configured help key toggles it; Esc is handled centrally.
func (s *tuiState) dispatchHelpAction(action string) bool {
	if action != "help" {
		return false
	}
	s.toggleHelp()
	return true
}

// handleCentralModalCancel is the event-loop escape path for the workspace
// modals whose close command is Esc or Ctrl-C.
func (s *tuiState) handleCentralModalCancel(input []byte) bool {
	if string(input) != "\x1b" && string(input) != "\x03" {
		return false
	}
	switch {
	case s.workspacePaneMode:
		if s.workspacePaneStep == "notes" {
			if s.notesFormActive {
				s.notesFormActive, s.outputErr = false, ""
			} else {
				s.closeNotesModal()
			}
		} else {
			// A focused terminal can open the Default Project's confirmation
			// modal after temporarily lending navigation ownership to the
			// workspace.  Cancel must return that lease to the same PTY; simply
			// closing the modal leaves the UI on the Project/session list.
			restoreFocusedPTY := s.workspacePaneStep == "detach-confirm" && s.workspacePaneRestoreFocused
			restoreAttachKey := s.workspacePaneRestoreAttachKey
			s.closeWorkspacePane()
			if restoreFocusedPTY {
				s.beginFocusedDefaultDetachRestore(restoreAttachKey)
			}
		}
	default:
		return false
	}
	return true
}

func initialReplayCaughtUp(currentOffset, replayEndOffset uint64) bool {
	return currentOffset >= replayEndOffset
}

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

func (s *tuiState) currentKey() string {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		return ""
	}
	return sessionKey(s.sessions[s.selected])
}

func (s *tuiState) selectedClientName() string {
	if s.selectedGroupID != "" && s.organizationMode() == ducklord.OrganizationHost {
		if _, ok := s.cfg.Client(s.selectedGroupID); ok {
			return s.selectedGroupID
		}
	}
	if len(s.sessions) > 0 && s.selected >= 0 && s.selected < len(s.sessions) && s.sessions[s.selected].Client != "" {
		return s.sessions[s.selected].Client
	}
	if s.cfg != nil && len(s.cfg.Clients) > 0 {
		return s.cfg.Clients[0].Name
	}
	return ""
}

func (s *tuiState) completeNewSessionStart(ctx context.Context, clientName, sessionID string, err error) {
	s.newSessionStarting = false
	if err != nil {
		if s.workspaceNewSessionIntent != nil {
			message := sanitizeTerminalText(err.Error())
			s.cancelCreate()
			s.newSessionErr = message
			s.outputErr = message
			return
		}
		s.newSessionErr = err.Error()
		return
	}
	intent := s.workspaceNewSessionIntent
	s.workspaceNewSessionIntent = nil
	s.newSessionMode = false
	s.newSessionLine = ""
	s.newSessionErr = ""
	s.outputErr = ""
	if intent != nil {
		generation, instance := s.hostFingerprint(clientName)
		if !s.hostIsLive(clientName) || generation != s.newSessionStartGeneration || instance != s.newSessionStartInstance ||
			s.hostConnectionEpoch[clientName] != s.newSessionStartEpoch {
			s.outputErr = "shell started, but host changed before pane placement; find it in Default Project"
		} else {
			s.pendingWorkspacePlacements = append(s.pendingWorkspacePlacements, pendingWorkspacePlacement{
				intent: *intent, client: clientName, instanceID: instance, sessionID: sessionID,
				generation: generation, connectionEpoch: s.newSessionStartEpoch, createdAt: time.Now(),
			})
		}
	}
	s.refreshSessions(ctx)
	for index, session := range s.sessions {
		if session.Client == clientName && session.InstanceID == s.newSessionStartInstance && session.SessionID == sessionID {
			s.selected = index
			s.selectedKey = s.currentKey()
			break
		}
	}
	if s.outputErr == "" {
		for _, pending := range s.pendingWorkspacePlacements {
			if pending.client == clientName && pending.instanceID == s.newSessionStartInstance && pending.sessionID == sessionID &&
				pending.generation == s.newSessionStartGeneration && pending.connectionEpoch == s.newSessionStartEpoch {
				s.outputErr = "shell started; waiting for session synchronization before placing its pane"
				break
			}
		}
	}
	if !s.pooledOutput {
		placementMessage := s.outputErr
		s.refreshSelectedOutput(ctx)
		if placementMessage != "" {
			s.outputErr = placementMessage
		}
	}
}

// A successful Start may precede the next authoritative inventory. Preserve
// its exact pane intent until that Session appears, while fencing Host changes.
func (s *tuiState) reconcilePendingWorkspacePlacements() {
	if len(s.pendingWorkspacePlacements) == 0 {
		return
	}
	remaining := s.pendingWorkspacePlacements[:0]
	for _, pending := range s.pendingWorkspacePlacements {
		generation, instance := s.hostFingerprint(pending.client)
		if !s.hostIsLive(pending.client) || generation != pending.generation || instance != pending.instanceID ||
			s.hostConnectionEpoch[pending.client] != pending.connectionEpoch {
			s.outputErr = "shell started, but Host changed before pane placement; find it in Default Project"
			continue
		}
		if time.Since(pending.createdAt) > 30*time.Second {
			s.outputErr = "shell started, but its Session was not synchronized; find it in Default Project"
			continue
		}
		found := false
		for _, session := range s.sessions {
			if session.Client != pending.client || session.InstanceID != pending.instanceID || session.SessionID != pending.sessionID {
				continue
			}
			found = true
			s.outputErr = ""
			if err := s.placeCreatedWorkspacePane(pending.intent, session); err != nil {
				s.outputErr = "shell started, but pane placement failed: " + sanitizeTerminalText(err.Error())
			} else if s.outputErr == "" {
				s.outputErr = "Session pane created"
			}
			break
		}
		if !found {
			remaining = append(remaining, pending)
		}
	}
	s.pendingWorkspacePlacements = remaining
}

func (s *tuiState) refreshSelectedOutput(ctx context.Context) {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		s.outputText = ""
		s.outputErr = ""
		s.outputForKey = ""
		return
	}
	sess := s.sessions[s.selected]
	key := sessionKey(sess)
	s.selectedKey = key
	if s.outputForKey != key {
		s.outputText = ""
		s.terminal = nil
		s.ptyScrollOffset = 0
		s.outputErr = ""
		s.outputStale = false
		s.outputFresh = false
		if snapshot, err := s.snapshotStore.Load(sess.InstanceID, sess.SessionID); err == nil {
			if state, decodeErr := ducklord.DecodeTerminalRenderState(snapshot.Payload); decodeErr == nil {
				s.terminalGeneration = state.RuntimeGeneration
				s.terminalOffset = state.OutputOffset
				s.terminalCursorValid = state.ResumeCursorValid
				if state.Framebuffer != nil {
					s.terminal, _ = ducklord.NewTerminalFromState(*state.Framebuffer, ducklord.DefaultTerminalScrollback)
					s.ptyScrollOffset = 0
				}
				if s.terminal != nil {
					s.outputText = s.terminal.Text()
				} else {
					s.outputText = state.Text
				}
				s.outputStale = true
			}
		}
	}
	s.outputForKey = key
	if !s.hostIsLive(sess.Client) {
		s.outputFresh = false
		s.outputErr = "host reconnecting; showing last synchronized output"
		return
	}
	if !canRead(sess) {
		s.outputFresh = false
		s.outputErr = sess.Error
		if s.outputErr == "" {
			s.outputErr = sess.Status
		}
		return
	}
	if sess.Status == string(model.StatusRunning) && s.outputStale && s.terminal != nil && s.terminalCursorValid && s.terminalGeneration == sess.RuntimeGeneration {
		s.outputFresh = false
		s.outputErr = "saved terminal state; press enter to resume from its exact output position"
		return
	}
	c, err := mustClient(s.cfg, sess.Client)
	if err != nil {
		s.outputFresh = false
		if !s.outputStale {
			s.outputText = ""
		}
		s.outputErr = err.Error()
		return
	}
	ref := sess.SessionID
	if ref == "" {
		ref = sess.Name
	}
	text, err := s.runner.Read(ctx, c, ref, 80)
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	rows, cols := s.activePTYSize()
	s.terminal = ducklord.NewTerminal(int(rows), int(cols), ducklord.DefaultTerminalScrollback)
	s.ptyScrollOffset = 0
	s.terminal.Write([]byte(text))
	s.outputText = s.terminal.Text()
	s.outputErr = ""
	s.outputStale = false
	s.outputFresh = true
	s.terminalGeneration = sess.RuntimeGeneration
	s.terminalOffset = 0
	s.terminalCursorValid = false
	if s.sessionFreshlyDisplayed(sess) {
		s.markActivitySeen(sess)
	}
}

func (s *tuiState) applyAttachOutput(text string, runtimeGeneration, outputOffset uint64) {
	s.outputStale = false
	if s.terminal == nil {
		rows, cols := s.activePTYSize()
		s.terminal = ducklord.NewTerminal(int(rows), int(cols), ducklord.DefaultTerminalScrollback)
		s.ptyScrollOffset = 0
	}
	s.terminal.Write([]byte(text))
	s.terminalGeneration = runtimeGeneration
	s.terminalOffset = outputOffset
	s.terminalCursorValid = runtimeGeneration != 0
	s.outputFresh = true
	if s.pendingAttachKey != "" {
		s.activeAttachKey = s.pendingAttachKey
		s.pendingAttachKey = ""
	}
	s.activeAttachFresh = true
	if s.sessionFreshlyDisplayed(s.currentSession()) {
		s.markActivitySeen(s.currentSession())
	}
}

func applyOrderedAttachChunk(state *tuiState, chunk attachOutputEvent, pending **resizeDoneEvent) {
	if chunk.runtimeGeneration != 0 {
		if chunk.outputOffset < chunk.startOffset || chunk.outputOffset-chunk.startOffset != uint64(len(chunk.text)) ||
			state.terminalGeneration != 0 && chunk.runtimeGeneration != state.terminalGeneration || chunk.startOffset != state.terminalOffset {
			state.outputErr = "PTY output stream lost byte continuity; detach and reattach to rebuild the view"
			*pending = nil
			state.resizeStatus = ""
			return
		}
	}
	resize := *pending
	if resize != nil && resize.id == chunk.id && chunk.runtimeGeneration == state.terminalGeneration && resize.barrier > chunk.startOffset && resize.barrier < chunk.outputOffset {
		split := int(resize.barrier - chunk.startOffset)
		state.applyAttachOutput(chunk.text[:split], chunk.runtimeGeneration, resize.barrier)
		state.terminal.Resize(int(resize.rows), int(resize.cols))
		state.applyAttachOutput(chunk.text[split:], chunk.runtimeGeneration, chunk.outputOffset)
		*pending = nil
		state.resizeStatus = ""
		return
	}
	if resize != nil && resize.id == chunk.id && chunk.startOffset >= resize.barrier {
		if state.terminal != nil {
			state.terminal.Resize(int(resize.rows), int(resize.cols))
		}
		*pending = nil
		state.resizeStatus = ""
	}
	state.applyAttachOutput(chunk.text, chunk.runtimeGeneration, chunk.outputOffset)
	if resize != nil && resize.id == chunk.id && chunk.runtimeGeneration == state.terminalGeneration && chunk.outputOffset == resize.barrier {
		state.terminal.Resize(int(resize.rows), int(resize.cols))
		*pending = nil
		state.resizeStatus = ""
	}
}

func drainBufferedAttach(state *tuiState, events []attachOutputEvent, pending **resizeDoneEvent) *attachOutputEvent {
	for index := range events {
		event := events[index]
		if event.done {
			return &event
		}
		applyOrderedAttachChunk(state, event, pending)
	}
	return nil
}

func (s *tuiState) activity() *ducklord.ActivityState {
	if s.activityState == nil {
		s.activityState = ducklord.NewActivityState()
	}
	return s.activityState
}

func (s *tuiState) markActivitySeen(session ducklord.RemoteSession) {
	selectedKey := s.currentKey()
	next := s.activity().Clone()
	activityChanged := next.MarkSeen(session)
	if activityChanged {
		if err := s.activityStore.Save(next); err != nil {
			s.outputErr = "local state: " + err.Error()
			return
		}
		s.activityState = next
	}
	for i := range s.sessions {
		if sessionKey(s.sessions[i]) == sessionKey(session) {
			s.sessions[i].Unread = false
			s.sessions[i].Updated = false
		}
	}
	s.applyOrganizationOrder()
	s.restoreSelection(selectedKey)
}

func (s *tuiState) saveActivityState() error {
	err := s.activityStore.Save(s.activity())
	if err != nil && s.outputErr == "" {
		s.outputErr = "local state: " + err.Error()
	}
	return err
}

func (s *tuiState) currentSession() ducklord.RemoteSession {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		return ducklord.RemoteSession{}
	}
	return s.sessions[s.selected]
}

func (s *tuiState) activePTYSession() ducklord.RemoteSession {
	if s.workspacePreview && s.activeAttachKey != "" {
		if session, ok := s.sessionForKey(s.activeAttachKey); ok {
			return session
		}
		if sessionKey(s.workspacePaneFocusRestoreSession) == s.activeAttachKey {
			return s.workspacePaneFocusRestoreSession
		}
		return ducklord.RemoteSession{}
	}
	if s.workspacePreview && s.workspaceNav != nil && s.workspaceNav.InDetailMode() {
		identity := s.workspaceNav.DetailSelection()
		for _, session := range s.sessions {
			if candidate, ok := ducklord.IdentityFromSession(session); ok && candidate == identity {
				return session
			}
		}
		return ducklord.RemoteSession{}
	}
	return s.currentSession()
}

func (s *tuiState) hostOwnsActivePTY(host string, control *ducklord.ControlSession) bool {
	if s.activePTYSession().Client == host || control != nil && control.ClientKey == host {
		return true
	}
	if s.pendingAttachKey != "" {
		if pending, ok := s.sessionForKey(s.pendingAttachKey); ok && pending.Client == host {
			return true
		}
	}
	return false
}

func (s *tuiState) acceptHostRetentionEvent(event hostRetentionEvent, watchEpoch uint64) bool {
	return s.hostMenuMode && s.hostMenuTarget == event.host && s.hostMenuRequestID == event.id && !s.disconnectedHosts[event.host] &&
		watchEpoch == event.epoch && s.hostSync[event.host].InstanceID == event.instanceID
}

func (s *tuiState) acceptHostResourceEvent(event hostResourceEvent, watchEpoch uint64) bool {
	return s.hostMenuMode && s.hostMenuTarget == event.host && s.hostMenuRequestID == event.id && strings.HasPrefix(s.hostMenuStep, "resources-") &&
		!s.disconnectedHosts[event.host] && watchEpoch == event.epoch && s.hostSync[event.host].InstanceID == event.instanceID
}

func (s *tuiState) acceptHostHookEvent(event hostHookEvent, watchEpoch uint64) bool {
	return s.hostMenuMode && s.hostMenuStep == "hook-saving" && s.hostMenuTarget == event.host && s.hostMenuRequestID == event.id &&
		s.hostHookAgent == event.agent && s.hostHookAction == event.action && !s.disconnectedHosts[event.host] &&
		watchEpoch == event.epoch && s.hostSync[event.host].InstanceID == event.instanceID
}

func (s *tuiState) beginHostHookOperation(host string, id uint64) bool {
	if s.hostHookInFlight == nil {
		s.hostHookInFlight = make(map[string]uint64)
	}
	if _, exists := s.hostHookInFlight[host]; exists {
		return false
	}
	s.hostHookInFlight[host] = id
	return true
}

func (s *tuiState) finishHostHookOperation(host string, id uint64) {
	if s.hostHookInFlight[host] == id {
		delete(s.hostHookInFlight, host)
	}
}

func (s *tuiState) canResizeCurrentSession() bool {
	session := s.activePTYSession()
	if session.Kind == string(model.KindShell) {
		return true
	}
	return session.WriterKind == string(model.OwnerTerminal) && session.WriterID == s.ownerName
}

func controlMatchesSession(control *ducklord.ControlSession, session ducklord.RemoteSession, owner string) bool {
	if control == nil || session.Client != control.ClientKey || session.InstanceID != control.InstanceID || session.SessionID != control.SessionID ||
		session.OwnershipEpoch != control.OwnershipEpoch || session.RuntimeGeneration != control.RuntimeGeneration {
		return false
	}
	return session.Kind == string(model.KindShell) || session.WriterKind == string(model.OwnerTerminal) && session.WriterID == owner
}

func focusedPTYWriter(control *ducklord.ControlSession, attach *ducklord.AttachSession, session ducklord.RemoteSession, owner string) io.WriteCloser {
	if control != nil && control.Stdin != nil && controlMatchesSession(control, session, owner) {
		return control.Stdin
	}
	if attach != nil && attach.Stdin != nil {
		return attach.Stdin
	}
	return nil
}

func (s *tuiState) activePTYSize() (rows, cols uint16) {
	width, height := terminalSize()
	if s.workspacePreview {
		pane, err := s.workspacePaneRectAt(width, height)
		if err != nil {
			return 1, 1
		}
		return uint16(max(1, min(200, pane.Height-1))), uint16(max(1, min(500, pane.Width)))
	}
	layout := calculateTUILayout(width, true, s.listPaneWidth, s.autoHideList)
	contentWidth := layout.contentWidth
	contentHeight := height - 4
	if contentWidth < 40 {
		contentWidth = 40
	} else if contentWidth > 500 {
		contentWidth = 500
	}
	if contentHeight < 5 {
		contentHeight = 5
	} else if contentHeight > 200 {
		contentHeight = 200
	}
	return uint16(contentHeight), uint16(contentWidth)
}

type tuiLayout struct {
	width, menuWidth, contentX, contentWidth int
	overlay, showList                        bool
}

func calculateTUILayout(width int, ptyFocused bool, configuredListWidth int, autoHide bool) tuiLayout {
	if width < 1 {
		width = 1
	}
	menuWidth := configuredListWidth
	if menuWidth < 20 || menuWidth > 80 {
		menuWidth = ducklord.DefaultSessionListWidth
	}
	if ptyFocused && autoHide {
		return tuiLayout{width: width, menuWidth: menuWidth, contentX: 1, contentWidth: width, showList: false}
	}
	if width < menuWidth+3+40 {
		contentWidth := width
		if menuWidth > width {
			menuWidth = width
		}
		return tuiLayout{width: width, menuWidth: menuWidth, contentX: 1, contentWidth: contentWidth, overlay: true, showList: true}
	}
	return tuiLayout{width: width, menuWidth: menuWidth, contentX: menuWidth + 4, contentWidth: width - menuWidth - 3, showList: true}
}

func (s *tuiState) saveCurrentSnapshot() {
	session := s.currentSession()
	if sessionKey(session) != s.outputForKey {
		return
	}
	s.saveSnapshot(session, s.outputText)
}

func (s *tuiState) saveSnapshot(session ducklord.RemoteSession, text string) {
	if session.InstanceID == "" || session.SessionID == "" || sessionKey(session) != s.outputForKey || text == "" && s.terminal == nil {
		return
	}
	renderState := ducklord.TerminalRenderState{Text: sanitizeTerminalText(text)}
	if s.terminal != nil && sessionKey(session) == s.outputForKey {
		framebuffer := s.terminal.SnapshotState()
		renderState.Framebuffer = &framebuffer
		renderState.Text = s.terminal.Text()
		renderState.RuntimeGeneration = s.terminalGeneration
		renderState.OutputOffset = s.terminalOffset
		renderState.ResumeCursorValid = s.terminalCursorValid
	}
	payload, err := ducklord.EncodeTerminalRenderState(renderState)
	if err == nil {
		err = s.snapshotStore.Save(ducklord.TerminalSnapshot{InstanceID: session.InstanceID, SessionID: session.SessionID, Payload: payload})
	}
	if err != nil {
		s.outputErr = "snapshot: " + err.Error()
	}
}

func (s *tuiState) render(out io.Writer) {
	s.modalMouseRegions = nil
	for s.pendingBells > 0 {
		_, _ = out.Write([]byte{'\a'})
		s.pendingBells--
	}
	if s.copyMode {
		return
	}
	var frame strings.Builder
	destination := out
	out = &frame
	defer func() {
		width, height := terminalSize()
		if s.copyRendering {
			status := modalCellTruncate(" COPY MODE  Drag to select · j/k browse · Esc/q/v/Ctrl+C exits", max(1, width))
			fmt.Fprintf(&frame, "\033[2;1H\033[2K%s%s%s", modalSelected, status, modalReset)
		}
		s.frameOutput.write(destination, frame.String(), width, height)
	}()
	if s.workspacePreview {
		s.renderWorkspacePreview(out)
		return
	}
	width, height := terminalSize()
	modalHeight := height
	layout := calculateTUILayout(width, s.focused, s.listPaneWidth, s.autoHideList)
	renderWidth := layout.width
	if height < 12 {
		height = 12
	}
	menuWidth := layout.menuWidth
	contentX := layout.contentX
	contentWidth := layout.contentWidth
	fmt.Fprint(out, "\033[?25l\033[H\033[2J")
	status := ""
	if s.resizeStatus != "" {
		status = "  " + s.resizeStatus
	}
	header := fmt.Sprintf("ducklord remote agents  owner:%s%s%s", displayField(s.ownerName), s.hostSyncLabel(), truncate(status, renderWidth/2))
	fmt.Fprintln(out, truncate(header, renderWidth))
	if s.localWarning != "" {
		fmt.Fprintln(out, truncate("warning: "+sanitizeTerminalText(s.localWarning), renderWidth))
	} else if s.focused {
		fmt.Fprintln(out, truncate("session focus  keys go to session  ctrl-] menu", renderWidth))
	} else if s.addClientMode {
		fmt.Fprintln(out, truncate("add ducklion host: ssh config number/name or user@host  enter add  esc cancel", renderWidth))
	} else if s.newSessionMode {
		fmt.Fprintln(out, truncate(s.createHeader()+"  enter next  esc cancel", renderWidth))
	} else if s.searchMode {
		fmt.Fprintln(out, truncate("search sessions  type to filter  ↑/↓ move  enter activate  esc close", renderWidth))
	} else if s.actionMenu {
		fmt.Fprintln(out, truncate("session actions  ↑/↓ or j/k move  enter choose  esc close", renderWidth))
	} else if s.groupMenu {
		fmt.Fprintln(out, truncate("custom groups  ↑/↓ choose  enter confirm  esc close", renderWidth))
	} else if s.lifecycleConfirm != "" {
		fmt.Fprintln(out, truncate("lifecycle confirmation open; use the modal controls", renderWidth))
	} else {
		fmt.Fprintln(out, truncate(s.cfg.Shortcut("help")+" help · Enter focus/group · "+s.cfg.Shortcut("pty_unfocus")+" leave PTY", renderWidth))
	}
	fmt.Fprintln(out, strings.Repeat("-", renderWidth))
	if len(s.sessions) == 0 {
		fmt.Fprintln(out, truncate("No sessions.", renderWidth))
		s.renderCreateModal(out, width, modalHeight)
		s.renderSearchModal(out, width, modalHeight)
		s.renderActionModal(out, width, modalHeight)
		s.renderSessionHandleRenameModal(out, width, modalHeight)
		s.renderAddClientModal(out, width, modalHeight)
		s.renderRemoveClientModal(out, width, modalHeight)
		s.renderHelpModal(out, width, modalHeight)
		s.renderShortcutModal(out, width, modalHeight)
		s.renderNotificationConfigModal(out, width, modalHeight)
		s.renderHostModal(out, width, modalHeight)
		s.renderGroupModal(out, width, modalHeight)
		s.renderNotificationModal(out, width, modalHeight)
		s.renderLifecycleModal(out, width, modalHeight)
		s.renderCommandPalette(out, width, modalHeight)
		s.renderProjectFilesModal(out, width, modalHeight)
		return
	}
	if layout.overlay {
		s.renderContent(out, contentX, contentWidth, height)
	}
	if layout.showList {
		separator := " | content"
		if layout.overlay {
			separator = ""
		}
		clear := "\033[K"
		if layout.overlay {
			clear = ""
		}
		heading := truncate("sessions ["+string(s.organizationMode())+"]", menuWidth)
		fmt.Fprintf(out, "\033[4;1H%-*s%s%s", menuWidth, heading, separator, clear)
	} else {
		fmt.Fprintf(out, "\033[4;1H%s\033[K", truncate("content", renderWidth))
	}
	row := 5
	for _, listRow := range s.sessionListRows() {
		if !layout.showList {
			break
		}
		if row > height {
			break
		}
		if listRow.isGroup {
			prefix := " "
			if s.selectedGroupID == listRow.groupID {
				prefix = ">"
			}
			disclosure := "▾"
			if s.groupCollapsed(listRow.groupID) {
				disclosure = "▸"
			}
			groupLabel := prefix + disclosure + " [" + displayField(listRow.groupLabel) + "]"
			if s.groupHasUnread(listRow.groupID) {
				groupLabel += " •"
			}
			separator := " |"
			if layout.overlay {
				separator = ""
			}
			clear := "\033[K"
			if layout.overlay {
				clear = ""
			}
			plain := modalCellPad(modalCellTruncate(groupLabel, menuWidth), menuWidth)
			if s.organizationMode() == ducklord.OrganizationHost {
				plain = s.hostRowStyle(listRow.groupID) + plain + modalReset
			}
			fmt.Fprintf(out, "\033[%d;1H%s%s%s", row, plain, separator, clear)
			row++
			continue
		}
		i, sess := listRow.sessionIndex, s.sessions[listRow.sessionIndex]
		prefix := " "
		if s.selectedGroupID == "" && i == s.selected {
			prefix = ">"
		}
		mark := " "
		if sessionNeedsAttention(sess) {
			mark = "💀"
		} else if sessionKey(sess) == s.activeAttachKey {
			mark = "●"
		} else if sess.Unread {
			mark = "•"
		} else if sess.Updated {
			mark = "*"
		}
		markColumn := mark + " "
		if modalCellWidth(mark) >= 2 {
			markColumn = mark
		}
		line := fmt.Sprintf("%s%s%-12s %-18s %-9s %-14s %s", prefix, markColumn, displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), displayField(sessionTypeLabel(sess)), sess.LastLine)
		if s.organizationMode() == ducklord.OrganizationHost {
			line = fmt.Sprintf("%s%s%-18s %-9s %-14s %s", prefix, markColumn, displayField(sess.Name), displayField(sess.Status), displayField(sessionTypeLabel(sess)), sess.LastLine)
		}
		if sess.Error != "" {
			errorMark := "!"
			if sessionNeedsAttention(sess) {
				errorMark = "💀"
			}
			errorColumn := errorMark + " "
			if modalCellWidth(errorMark) >= 2 {
				errorColumn = errorMark
			}
			line = fmt.Sprintf("%s%s%-12s %-18s %-9s %s", prefix, errorColumn, displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), sess.Error)
			if s.organizationMode() == ducklord.OrganizationHost {
				line = fmt.Sprintf("%s%s%-18s %-9s %s", prefix, errorColumn, displayField(sess.Name), displayField(sess.Status), sess.Error)
			}
		}
		separator := " |"
		if layout.overlay {
			separator = ""
		}
		clear := "\033[K"
		if layout.overlay {
			clear = ""
		}
		plain := modalCellPad(modalCellTruncate(line, menuWidth), menuWidth)
		fmt.Fprintf(out, "\033[%d;1H%s%s%s%s%s", row, s.hostRowStyle(sess.Client), plain, modalReset, separator, clear)
		row++
	}
	if !layout.overlay {
		s.renderContent(out, contentX, contentWidth, height)
	}
	s.renderCreateModal(out, width, modalHeight)
	s.renderSearchModal(out, width, modalHeight)
	s.renderActionModal(out, width, modalHeight)
	s.renderSessionHandleRenameModal(out, width, modalHeight)
	s.renderAddClientModal(out, width, modalHeight)
	s.renderRemoveClientModal(out, width, modalHeight)
	s.renderHelpModal(out, width, modalHeight)
	s.renderShortcutModal(out, width, modalHeight)
	s.renderNotificationConfigModal(out, width, modalHeight)
	s.renderHostModal(out, width, modalHeight)
	s.renderGroupModal(out, width, modalHeight)
	s.renderNotificationModal(out, width, modalHeight)
	s.renderLifecycleModal(out, width, modalHeight)
	s.renderCommandPalette(out, width, modalHeight)
	s.renderProjectFilesModal(out, width, modalHeight)
	if s.focused && s.terminal != nil && !s.outputStale && s.ptyScrollOffset == 0 {
		if cursorRow, cursorCol, visible := s.terminal.CursorPosition(height-5, contentWidth); visible {
			fmt.Fprintf(out, "\033[%d;%dH\033[?25h", 6+cursorRow, contentX+cursorCol)
		}
	}
}

func (s *tuiState) groupHasUnread(group string) bool {
	for _, session := range s.sessions {
		if s.organizationGroupID(session) == group && session.Unread {
			return true
		}
	}
	return false
}

func (s *tuiState) hostSyncLabel() string {
	if len(s.hostSync) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.hostSync))
	for name := range s.hostSync {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		update := s.hostSync[name]
		state := strings.ToUpper(update.State)
		if state == "" {
			state = "SYNCING"
		}
		if update.Gap {
			state = "RESYNCED-GAP"
		}
		parts = append(parts, fmt.Sprintf("%s:%s r%d", displayField(name), state, update.Revision))
	}
	return "  " + strings.Join(parts, "  ")
}

const (
	modalSurface  = "\033[38;2;211;222;235;48;2;24;34;49m"
	modalReset    = "\033[0m"
	modalBorder   = "\033[38;2;78;116;139m"
	modalTitle    = "\033[1;38;2;129;212;250m"
	modalMuted    = "\033[38;2;148;163;184m"
	modalStatus   = "\033[38;2;250;204;21m"
	modalInput    = "\033[38;2;167;243;208m"
	modalSelected = "\033[1;38;2;255;255;255;48;2;36;83;107m"
	modalDisabled = "\033[38;2;100;116;139m"
	modalDanger   = "\033[1;38;2;255;255;255;48;2;190;24;93m"
)

func mouseCursorShape(shape string) string {
	if shape != "grabbing" {
		shape = "default"
	}
	return "\033]22;" + shape + "\033\\"
}

func shortcutInput(binding string) string {
	switch binding {
	case "up":
		return "\x1b[A"
	case "down":
		return "\x1b[B"
	case "right":
		return "\x1b[C"
	case "left":
		return "\x1b[D"
	case "pageup":
		return "\x1b[5~"
	case "pagedown":
		return "\x1b[6~"
	}
	if binding == "enter" {
		return "\r"
	}
	if strings.HasPrefix(binding, "ctrl-") {
		runes := []rune(strings.TrimPrefix(binding, "ctrl-"))
		if len(runes) == 1 {
			return string(byte(unicode.ToUpper(runes[0])) & 0x1f)
		}
	}
	return binding
}

func (s *tuiState) shortcut(action, input string) bool {
	return input == shortcutInput(s.cfg.Shortcut(action))
}

func (s *tuiState) sessionShortcut(input string) bool {
	for _, action := range []string{"session_actions", "session_notifications", "session_yield", "session_yield_wait", "session_end", "session_restart", "session_destroy"} {
		if s.shortcut(action, input) {
			return true
		}
	}
	return false
}

type modalRenderLine struct {
	style string
	text  string
}

func renderModalBox(out io.Writer, cols, rows int, lines []modalRenderLine) {
	renderModalBoxWidth(out, cols, rows, 72, lines)
}

// renderModalBoxWidth keeps the rendered frame, its clipping, and mouse layout
// on the same width for wide modals such as Project files.
func renderModalBoxWidth(out io.Writer, cols, rows, preferredWidth int, lines []modalRenderLine) {
	renderModalBoxWidthInternal(out, cols, rows, preferredWidth, lines, false)
}

// renderModalBoxWidthANSI is reserved for callers that compose trusted SGR
// styling with independently sanitized dynamic text.
func renderModalBoxWidthANSI(out io.Writer, cols, rows, preferredWidth int, lines []modalRenderLine) {
	renderModalBoxWidthInternal(out, cols, rows, preferredWidth, lines, true)
}

func renderModalBoxWidthInternal(out io.Writer, cols, rows, preferredWidth int, lines []modalRenderLine, allowANSI bool) {
	if cols < 8 || rows < 3 || len(lines) == 0 {
		return
	}
	maxLines := rows - 2
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	boxWidth := min(preferredWidth, max(8, cols-2))
	innerWidth := boxWidth - 2
	boxHeight := len(lines) + 2
	top := max(1, (rows-boxHeight)/2+1)
	left := max(1, (cols-boxWidth)/2+1)
	fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
	for index, line := range lines {
		textValue := modalDisplayText(line.text)
		if allowANSI {
			textValue = modalDisplayANSI(line.text)
		}
		text := modalCellPad(modalCellTruncate(textValue, innerWidth), innerWidth)
		fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", top+index+1, left, modalSurface+modalBorder, modalSurface+line.style, text, modalReset+modalSurface+modalBorder, modalReset)
	}
	fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+boxHeight-1, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
}

func (s *tuiState) renderAddClientModal(out io.Writer, cols, rows int) {
	if !s.addClientMode {
		return
	}
	s.resetModalMouse()
	if s.addClientStep == "mode" {
		standaloneStyle, integratedStyle := "", ""
		if s.addClientModeSelected == 0 {
			standaloneStyle = modalSelected
		} else {
			integratedStyle = modalSelected
		}
		lines := []modalRenderLine{
			{modalTitle, "  Add Ducklion host · who manages Ducklion?"},
			{standaloneStyle, "  Standalone · Use Ducklion without Duckway proxy"},
			{integratedStyle, "  Duckway proxy · Duckway installs and manages Ducklion"},
			{modalMuted, "  Already installed: verify and connect; no reinstall."},
			{modalMuted, "  Missing: Standalone installs; proxy requires remote setup."},
			{modalMuted, "  Wrong mode or stopped daemon: host is not added."},
			{modalMuted, "  ↑/↓ choose   Enter continue   Esc cancel"},
		}
		s.modalChoice(1, &s.addClientModeSelected, 0, "\r")
		s.modalChoice(2, &s.addClientModeSelected, 1, "\r")
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	selected := -1
	if len(s.addClientHosts) > 0 && s.addClientLine == "" {
		selected = min(max(s.addClientSelected, 0), len(s.addClientHosts)-1)
	}
	choices := s.addClientHosts
	maxChoices := max(1, rows-7)
	start := max(0, selected-maxChoices/2)
	if len(choices) > maxChoices {
		start = min(start, len(choices)-maxChoices)
		choices = choices[start : start+maxChoices]
	}
	lines := []modalRenderLine{{modalTitle, "  Add Ducklion host · " + s.addClientProvisionMode}}
	if len(choices) == 0 {
		lines = append(lines, modalRenderLine{modalMuted, "  No SSH config hosts found"})
	}
	for index, host := range choices {
		if !s.addClientBusy {
			s.modalChoice(len(lines), &s.addClientSelected, start+index, "\r")
			a := s.modalMouseLines[len(lines)]
			a.before = func() { s.addClientLine = "" }
			s.modalMouseLines[len(lines)] = a
		}
		style, prefix := "", "  "
		if start+index == selected {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%d  %s", prefix, start+index+1, host.Name)})
	}
	status := s.addClientErr
	if status == "" {
		status = "Choose an SSH host or enter user@host"
	}
	help := "  ↑/↓ select   Enter add   Esc back"
	if s.addClientBusy {
		help = "  Connecting in background   Esc cancel"
	}
	lines = append(lines,
		modalRenderLine{modalStatus, "  " + status},
		modalRenderLine{modalInput, "  host › " + s.addClientLine},
		modalRenderLine{modalMuted, help})
	if rows < 7 {
		choice := "No SSH config hosts"
		if selected >= 0 {
			choice = fmt.Sprintf("› %d  %s", selected+1, choices[selected-start].Name)
		} else if s.addClientLine != "" {
			choice = "Custom SSH target"
		}
		lines = []modalRenderLine{{modalSelected, choice}, {modalInput, "host › " + s.addClientLine}}
		s.modalMouseLines = make(map[int]modalMouseAction)
		if selected >= 0 && !s.addClientBusy {
			s.modalChoice(0, &s.addClientSelected, selected, "\r")
		}
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderRemoveClientModal(out io.Writer, cols, rows int) {
	if !s.removeClientMode || len(s.cfg.Clients) == 0 {
		return
	}
	s.resetModalMouse()
	selected := min(max(s.removeClientSelected, 0), len(s.cfg.Clients)-1)
	client := s.cfg.Clients[selected]
	lines := []modalRenderLine{{modalTitle, "  Remove Ducklion host"}}
	if s.removeClientConfirm != "" {
		lines = append(lines,
			modalRenderLine{modalDanger, "  REMOVE HOST CONFIGURATION?"},
			modalRenderLine{modalInput, "  " + displayField(client.Name) + " · " + displayField(client.Host)},
			modalRenderLine{modalMuted, "  Remote sessions will not be destroyed"},
			modalRenderLine{modalMuted, "  Enter remove   Esc back   Ctrl+C cancel"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	for index, candidate := range s.cfg.Clients {
		s.modalChoice(len(lines), &s.removeClientSelected, index, "\r")
		style, prefix := "", "  "
		if index == selected {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%d  %s · %s", prefix, index+1, displayField(candidate.Name), displayField(candidate.Host))})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ select   Enter continue   Esc/Ctrl+C cancel"})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderHelpModal(out io.Writer, cols, rows int) {
	if !s.helpMode || s.blockingModalOpen() {
		return
	}
	s.resetModalMouse()
	entries := helpCatalog(s.workspacePreview)
	query := strings.ToLower(strings.TrimSpace(s.helpSearchQuery))
	results := []modalRenderLine{}
	helpMouseActions := make(map[string]string)
	category := ""
	categoryEntries := []modalRenderLine{}
	flushCategory := func() {
		if len(categoryEntries) == 0 {
			return
		}
		results = append(results, modalRenderLine{modalStatus, "  " + category})
		results = append(results, categoryEntries...)
		categoryEntries = nil
	}
	for _, entry := range entries {
		if entry.category != "" {
			flushCategory()
			category = entry.category
		}
		shortcut, keyInput := "", ""
		if strings.HasPrefix(entry.action, "prefix+") {
			suffix := strings.TrimPrefix(entry.action, "prefix+")
			displaySuffix := suffix
			switch suffix {
			case "slash":
				displaySuffix = "/"
			case "space":
				displaySuffix = "Space"
			case "up":
				displaySuffix = "↑"
			case "down":
				displaySuffix = "↓"
			case "left":
				displaySuffix = "←"
			case "right":
				displaySuffix = "→"
			}
			quickSequence := suffix == "--" || suffix == "\\\\" || suffix == "tt"
			if quickSequence {
				displaySuffix = suffix
			}
			shortcut = s.cfg.Shortcut("pane_prefix") + " " + displaySuffix
			if suffix == "slash" {
				keyInput = shortcutInput(s.cfg.Shortcut("pane_prefix")) + "/"
			} else if suffix == "space" {
				keyInput = shortcutInput(s.cfg.Shortcut("pane_prefix")) + " "
			} else if quickSequence {
				keyInput = shortcutInput(s.cfg.Shortcut("pane_prefix")) + suffix
			} else {
				keyInput = shortcutInput(s.cfg.Shortcut("pane_prefix")) + shortcutInput(suffix)
			}
		} else if entry.action != "" {
			shortcut = s.cfg.Shortcut(entry.action)
			keyInput = shortcutInput(shortcut)
		}
		searchText := strings.ToLower(category + " " + entry.action + " " + entry.label + " " + entry.detail + " " + shortcut)
		if query == "" || strings.Contains(searchText, query) {
			text := "  " + fmt.Sprintf("%-14s %s", shortcut, entry.label)
			if entry.detail != "" {
				text += " · " + entry.detail
			}
			style := modalMuted
			if entry.action != "" && keyInput != "" {
				style = modalInput
				available := s.helpActionAvailable(entry.action)
				if s.helpOriginFocused && s.helpFocusRestorePending {
					available = s.helpActionAvailableFromTerminalOrigin(entry.action)
				}
				if available {
					style = modalSelected
				}
			}
			wrapped := wrapHelpEntry(text, min(68, max(1, cols-4)), "                  ")
			for _, line := range wrapped {
				categoryEntries = append(categoryEntries, modalRenderLine{style, line})
			}
			quickSequence := entry.action == "prefix+--" || entry.action == "prefix+\\\\" || entry.action == "prefix+tt"
			if keyInput != "" && !quickSequence && s.helpActionAvailable(entry.action) && strings.HasPrefix(entry.action, "prefix+") {
				helpMouseActions[wrapped[0]] = keyInput
			}
		}
	}
	flushCategory()
	mouseHelp := "Click a prefix shortcut to send its sequence · drag reorder · right-click focus/toggle"
	if s.workspacePreview {
		mouseHelp = "Click a prefix shortcut to send its sequence · right-click target: config · drag Session into Terminal: add pane"
	}
	if query == "" || strings.Contains(strings.ToLower("mouse "+mouseHelp), query) {
		results = append(results, modalRenderLine{modalStatus, "  MOUSE"})
		results = append(results, helpWrappedLines("  "+mouseHelp, min(68, max(1, cols-4)), "  ")...)
	}
	if query == "" || strings.Contains("modals choose enter confirm esc back ctrl+c close", query) {
		results = append(results, modalRenderLine{modalStatus, "  MODAL CONTROLS"})
		results = append(results, helpWrappedLines("  ↑/↓ choose · Enter confirm · Esc back · Ctrl+C close; forms may use Tab to move fields", min(68, max(1, cols-4)), "  ")...)
	}
	if query == "" || strings.Contains("host connect disconnect reconnect sessions notifications", query) {
		results = append(results, modalRenderLine{modalMuted, "  Host connect/disconnect/reconnect affects all host sessions and notifications"})
	}
	if len(results) == 0 {
		results = append(results, modalRenderLine{modalMuted, "  No matching shortcuts"})
	}
	helpKey := s.cfg.Shortcut("help")
	searchHint := "  / search · " + helpKey + " close help"
	if s.helpSearchActive {
		searchHint = "  Search: " + s.helpSearchQuery + "_ · ↑/↓ browse · Esc clear"
	} else if s.helpSearchQuery != "" {
		searchHint = "  Filter: " + s.helpSearchQuery + "  (/ edit · search then Esc clears)"
	}
	maxVisible := max(1, rows-5)
	s.helpOffset = min(max(s.helpOffset, 0), max(0, len(results)-maxVisible))
	lines := []modalRenderLine{{modalTitle, "  Keyboard shortcuts and feature guide"}, {modalInput, searchHint}}
	lines = append(lines, results[s.helpOffset:min(len(results), s.helpOffset+maxVisible)]...)
	footer := "  Pinned · / search · Enter pins search · " + helpKey + " close"
	if s.helpSearchQuery != "" {
		footer = "  Filtered · / edit · search then Esc clears · " + helpKey + " close"
	}
	lines = append(lines, modalRenderLine{modalMuted, footer})
	for i, line := range lines {
		if key := helpMouseActions[line.text]; key != "" {
			s.modalChoice(i, nil, 0, key)
			a := s.modalMouseLines[i]
			a.before = func() { s.helpSearchActive = false }
			s.modalMouseLines[i] = a
		}
	}
	if !s.helpSearchActive {
		s.modalChoice(1, nil, 0, "/")
	}
	s.renderModalBox(out, cols, rows, lines)
	if cols >= 8 && rows >= 3 && len(lines) <= rows-2 {
		width := min(72, max(8, cols-2))
		left := max(1, (cols-width)/2+1) + 1
		row := max(1, (rows-len(lines)-2)/2+1) + len(lines)
		footer := lines[len(lines)-1].text
		for _, button := range []struct{ label, key string }{{"/ search", "/"}, {helpKey + " close", shortcutInput(helpKey)}} {
			start := strings.LastIndex(footer, button.label)
			if start < 0 {
				continue
			}
			column := modalCellWidth(footer[:start])
			if column+modalCellWidth(button.label) <= width-2 {
				s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left + column, left + column + modalCellWidth(button.label) - 1, row, modalMouseAction{key: button.key}})
			}
		}
	}
}

func wrapHelpEntry(text string, width int, continuationIndent string) []string {
	lines := helpWrappedLines(text, width, continuationIndent)
	out := make([]string, len(lines))
	for i := range lines {
		out[i] = lines[i].text
	}
	return out
}

func helpWrappedLines(text string, width int, continuationIndent string) []modalRenderLine {
	if width < 1 {
		return []modalRenderLine{{modalMuted, text}}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []modalRenderLine{{modalMuted, ""}}
	}
	lines := []modalRenderLine{}
	line := words[0]
	lineWidth := width
	for _, word := range words[1:] {
		if modalCellWidth(line)+1+modalCellWidth(word) > lineWidth {
			lines = append(lines, modalRenderLine{modalMuted, line})
			line = continuationIndent + word
			lineWidth = max(1, width-modalCellWidth(continuationIndent))
			continue
		}
		line += " " + word
	}
	return append(lines, modalRenderLine{modalMuted, line})
}

func (s *tuiState) handleHelpSearchInput(input []byte) {
	switch string(input) {
	case "\x1b", "\x03":
		s.helpSearchActive, s.helpSearchQuery, s.helpOffset = false, "", 0
	case "\r", "\n":
		s.helpSearchActive = false
	case "\x1b[A":
		s.helpOffset = max(0, s.helpOffset-1)
	case "\x1b[B":
		s.helpOffset++
	case "\b", "\x7f":
		s.helpSearchQuery = trimLastRune(s.helpSearchQuery)
		s.helpOffset = 0
	default:
		if utf8.Valid(input) && !strings.ContainsRune(string(input), '\x1b') {
			for _, r := range string(input) {
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && len(s.helpSearchQuery)+utf8.RuneLen(r) <= 256 {
					s.helpSearchQuery += string(r)
				}
			}
			s.helpOffset = 0
		}
	}
}

func shortcutActions() []string {
	actions := make([]string, 0, len(ducklord.DefaultShortcuts))
	for action := range ducklord.DefaultShortcuts {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}

func trimLastRune(text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return text
	}
	return string(runes[:len(runes)-1])
}

func (s *tuiState) beginShortcutSettings() {
	if s.cfgPath == "" {
		s.outputErr = "shortcut settings are unavailable without a config path"
		return
	}
	s.shortcutMode, s.shortcutStep, s.shortcutIndex = true, "list", 0
	s.shortcutLine, s.shortcutErr = "", ""
	s.shortcutDraft = s.cfg.Clone()
}

func (s *tuiState) renderShortcutModal(out io.Writer, cols, rows int) {
	if !s.shortcutMode {
		return
	}
	s.resetModalMouse()
	actions := shortcutActions()
	lines := []modalRenderLine{{modalTitle, "  Configure keyboard shortcuts"}}
	switch s.shortcutStep {
	case "edit":
		action := actions[s.shortcutIndex]
		lines = append(lines, modalRenderLine{modalStatus, "  " + action}, modalRenderLine{modalInput, "  binding › " + s.shortcutLine}, modalRenderLine{modalMuted, "  Examples: x, ?, ctrl-k, ctrl-] · Enter save · Esc back"})
	case "restart":
		lines = append(lines, modalRenderLine{modalStatus, "  Shortcut saved. Restart Ducklord TUI now?"}, modalRenderLine{modalInput, "  y / Enter  restart now"}, modalRenderLine{modalMuted, "  n / Esc     keep running with current bindings"})
		s.modalChoice(2, nil, 0, "\r")
		s.modalChoice(3, nil, 0, "\x1b")
	default:
		visible := max(3, rows-8)
		start := max(0, min(s.shortcutIndex-visible/2, len(actions)-visible))
		for index := start; index < min(len(actions), start+visible); index++ {
			s.modalChoice(len(lines), &s.shortcutIndex, index, "\r")
			style, prefix := modalInput, "  "
			if index == s.shortcutIndex {
				style, prefix = modalSelected, "› "
			}
			lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%-24s %s", prefix, actions[index], s.shortcutDraft.Shortcut(actions[index]))})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter edit · Esc/Ctrl+C close"})
	}
	if s.shortcutErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + sanitizeTerminalText(s.shortcutErr)})
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) handleShortcutInput(input []byte) string {
	actions := shortcutActions()
	text := string(input)
	switch s.shortcutStep {
	case "restart":
		if text == "y" || text == "Y" || text == "\r" || text == "\n" {
			return "restart-tui"
		}
		if text == "n" || text == "N" || text == "\x1b" || text == "\x03" {
			s.shortcutMode = false
			s.outputErr = "shortcuts saved; restart Ducklord to load them"
		}
		return ""
	case "edit":
		if text == "\x1b" || text == "\x03" {
			s.shortcutStep, s.shortcutLine, s.shortcutErr = "list", "", ""
			return ""
		}
		if text == "\x7f" || text == "\b" {
			s.shortcutLine = trimLastRune(s.shortcutLine)
			return ""
		}
		if text == "\r" || text == "\n" {
			action := actions[s.shortcutIndex]
			candidate := s.shortcutDraft.Clone()
			candidate.Shortcuts[action] = strings.TrimSpace(s.shortcutLine)
			if err := ducklord.SaveConfig(s.cfgPath, candidate); err != nil {
				s.shortcutErr = err.Error()
				return ""
			}
			s.shortcutDraft, s.shortcutStep, s.shortcutErr = candidate, "restart", ""
			return ""
		}
		for _, r := range text {
			if !unicode.IsControl(r) && len(s.shortcutLine) < 16 {
				s.shortcutLine += string(r)
			}
		}
	default:
		switch text {
		case "\x1b", "\x03":
			s.shortcutMode = false
		case "j", "\x1b[B":
			s.shortcutIndex = min(len(actions)-1, s.shortcutIndex+1)
		case "k", "\x1b[A":
			s.shortcutIndex = max(0, s.shortcutIndex-1)
		case "\r", "\n":
			s.shortcutStep = "edit"
			s.shortcutLine = s.shortcutDraft.Shortcut(actions[s.shortcutIndex])
			s.shortcutErr = ""
		}
	}
	return ""
}

var hostActions = []string{"Connections", "Reconnect", "PTY log retention", "Agent notification hooks", "Skills", "Notification defaults", "Add host", "Remove host", "Resources"}

func (s *tuiState) renderHostModal(out io.Writer, cols, rows int) {
	if !s.hostMenuMode {
		return
	}
	s.resetModalMouse()
	if s.hostMenuStep == "host-list" {
		lines := []modalRenderLine{{modalTitle, "  Hosts"}, {modalMuted, "  Choose a host to view its settings"}}
		if len(s.cfg.Clients) == 0 {
			lines = append(lines, modalRenderLine{modalMuted, "  (no hosts configured)"})
		} else {
			for index, client := range s.cfg.Clients {
				s.modalChoice(len(lines), &s.hostMenuIndex, index, "\r")
				style, prefix := modalInput, "  "
				if index == s.hostMenuIndex {
					style, prefix = modalSelected, "› "
				}
				state := "connected"
				if s.disconnectedHosts[client.Name] {
					state = "disconnected"
				}
				lines = append(lines, modalRenderLine{style, prefix + displayField(client.Name) + " · " + state})
			}
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter settings · Esc close"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if strings.HasPrefix(s.hostMenuStep, "skills-") {
		s.renderHostSkillsModal(out, cols, rows)
		return
	}
	if strings.HasPrefix(s.hostMenuStep, "retention-") {
		lines := []modalRenderLine{{modalTitle, "  Host PTY log retention · " + displayField(s.hostMenuTarget)}}
		switch s.hostMenuStep {
		case "retention-loading":
			lines = append(lines, modalRenderLine{modalMuted, "  Reading current Host setting…"})
		case "retention-edit":
			lines = append(lines, modalRenderLine{modalInput, fmt.Sprintf("  Current: %d days", s.hostMenuOldDays)}, modalRenderLine{modalSelected, "  New days (1–3650): " + s.hostMenuDraft + "▏"}, modalRenderLine{modalMuted, "  Enter review · Esc back"})
		case "retention-confirm":
			lines = append(lines, modalRenderLine{modalSelected, fmt.Sprintf("  Change %d → %s days?", s.hostMenuOldDays, s.hostMenuDraft)}, modalRenderLine{modalDanger, "  Shortening retention may delete expired PTY logs now."}, modalRenderLine{modalMuted, "  Enter confirm · Esc edit"})
		case "retention-saving":
			lines = append(lines, modalRenderLine{modalMuted, "  Saving and verifying Host setting…"})
		case "retention-saved":
			lines = append(lines, modalRenderLine{modalStatus, fmt.Sprintf("  Saved: %d days", s.hostMenuOldDays)}, modalRenderLine{modalMuted, "  Enter or Esc close"})
		case "retention-error":
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr}, modalRenderLine{modalMuted, "  Enter retry · Esc back"})
		}
		if s.hostMenuErr != "" && s.hostMenuStep != "retention-error" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if strings.HasPrefix(s.hostMenuStep, "resources-") {
		lines := []modalRenderLine{{modalTitle, "  Host resources · " + displayField(s.hostMenuTarget)}}
		if s.hostMenuStep == "resources-loading" {
			lines = append(lines, modalRenderLine{modalMuted, "  Reading Host resources…"})
		} else if s.hostMenuErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr})
			if s.hostMenuStep == "resources-error" {
				lines = append(lines, modalRenderLine{modalMuted, "  Enter retry · Esc back"})
			}
		} else {
			r := s.hostResourceStatus
			lines = append(lines,
				modalRenderLine{modalStatus, fmt.Sprintf("  OS / arch: %s / %s", r.GOOS, r.GOARCH)},
				modalRenderLine{modalStatus, fmt.Sprintf("  CPU: %d · heap: %d bytes", r.CPUCount, r.HeapAllocBytes)},
				modalRenderLine{modalStatus, fmt.Sprintf("  Uptime: %s", (time.Duration(r.UptimeSeconds) * time.Second).String())},
				modalRenderLine{modalStatus, fmt.Sprintf("  Sessions: %d · PTYs: %d", r.ManagedSessionCount, r.ActivePTYCount)},
				modalRenderLine{modalMuted, "  r refresh · Esc back"})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if strings.HasPrefix(s.hostMenuStep, "hook-") {
		lines := []modalRenderLine{{modalTitle, "  Agent notification hooks · " + displayField(s.hostMenuTarget)}}
		if s.hostHookStatusLoading {
			lines = append(lines, modalRenderLine{modalMuted, "  Reading Host hook status…"})
		} else if s.hostHookStatusErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostHookStatusErr})
		} else if len(s.hostHookStatuses) > 0 {
			for _, agent := range []string{"codex", "claude"} {
				if status, ok := s.hostHookStatuses[agent]; ok {
					lines = append(lines, modalRenderLine{modalStatus, hostHookStatusLine(status)})
					if agent == "codex" && status.Installed && status.Activation != "operational" {
						lines = append(lines, modalRenderLine{modalMuted, "  Review Codex /hooks; awaiting a valid session callback."})
					}
				}
			}
		}
		switch s.hostMenuStep {
		case "hook-select":
			choices := []string{"Install Codex Stop hook", "Remove Codex Stop hook", "Install Claude Stop hooks", "Remove Claude Stop hooks"}
			for index, choice := range choices {
				s.modalChoice(len(lines), &s.hostMenuIndex, index, "\r")
				style, prefix := modalInput, "  "
				if index == s.hostMenuIndex {
					style, prefix = modalSelected, "› "
				}
				lines = append(lines, modalRenderLine{style, prefix + choice})
			}
			lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter review · Esc back"})
		case "hook-confirm":
			path := "~/.claude/settings.json"
			if s.hostHookAgent == "codex" {
				path = "~/.codex/hooks.json"
			}
			lines = append(lines,
				modalRenderLine{modalSelected, "  " + strings.ToUpper(s.hostHookAction[:1]) + s.hostHookAction[1:] + " " + s.hostHookAgent + " hook on this Host?"},
				modalRenderLine{modalInput, "  Edit: " + path},
				modalRenderLine{modalMuted, "  Existing settings are preserved; changed files get a private backup."},
				modalRenderLine{modalMuted, "  Only Ducklion's own hook entry is changed."})
			if s.hostHookAgent == "codex" && s.hostHookAction == "install" {
				lines = append(lines, modalRenderLine{modalDanger, "  Codex activation pending until trusted with /hooks."})
			}
			lines = append(lines, modalRenderLine{modalMuted, "  Enter confirm · Esc choose again"})
		case "hook-saving":
			lines = append(lines, modalRenderLine{modalMuted, "  Updating Host hook configuration…"})
		case "hook-done":
			lines = append(lines, modalRenderLine{modalStatus, "  " + s.hostMenuErr}, modalRenderLine{modalMuted, "  Enter or Esc close"})
		case "hook-error":
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr}, modalRenderLine{modalMuted, "  Enter retry · Esc back"})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	if s.hostMenuStep == "connections" {
		lines := []modalRenderLine{{modalTitle, "  Host connections · checked means connected"}}
		for index, client := range s.cfg.Clients {
			s.modalChoice(len(lines), &s.hostMenuIndex, index, " ")
			style, cursor := modalInput, "  "
			changed := s.hostMenuSelected[client.Name] == s.disconnectedHosts[client.Name]
			changeMark := " "
			if changed {
				style, changeMark = modalStatus, "◆"
			}
			if index == s.hostMenuIndex {
				style, cursor = modalSelected, "› "
			}
			check := "[ ]"
			if s.hostMenuSelected[client.Name] {
				check = "[✓]"
			}
			state := "connected"
			if s.disconnectedHosts[client.Name] {
				state = "disconnected"
			}
			intent := ""
			if changed && s.hostMenuSelected[client.Name] {
				intent = " → will connect"
			} else if changed {
				intent = " → will disconnect"
			}
			lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%s%s %-18s %s%s", cursor, changeMark, check, displayField(client.Name), state, intent)})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ◆ changed · Space toggle · Enter apply all changes · Esc back"})
		if s.hostMenuErr != "" {
			lines = append(lines, modalRenderLine{modalDanger, "  " + s.hostMenuErr})
		}
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	lines := []modalRenderLine{{modalTitle, "  Host actions · " + displayField(s.hostMenuTarget)}}
	for index, label := range hostActions {
		if !s.hostScoped || index < 5 {
			s.modalChoice(len(lines), &s.hostMenuIndex, index, "\r")
		}
		style, prefix := "", "  "
		if s.hostScoped && index >= 5 {
			style, label = modalDisabled, label+" (unavailable in host-scoped mode)"
		}
		if index == s.hostMenuIndex {
			prefix = "› "
			if style == "" {
				style = modalSelected
			}
		}
		lines = append(lines, modalRenderLine{style, prefix + label})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  Host connect state covers every session and notification on that host"}, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter run · Esc host list"})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) beginHostMenu() {
	if len(s.cfg.Clients) == 0 {
		s.outputErr = "no host selected"
		return
	}
	s.hostMenuTarget = s.selectedClientName()
	s.hostMenuIndex = 0
	for i, client := range s.cfg.Clients {
		if client.Name == s.hostMenuTarget {
			s.hostMenuIndex = i
			break
		}
	}
	s.hostMenuMode, s.hostMenuStep = true, "host-list"
	s.hostMenuSelected = make(map[string]bool)
	s.hostMenuErr = ""
}

func (s *tuiState) handleHostMenuInput(input []byte) string {
	if s.hostMenuStep == "host-list" {
		switch string(input) {
		case "\x1b", "\x03":
			s.hostMenuMode = false
			return "cancel"
		case "j", "\x1b[B":
			s.hostMenuIndex = min(len(s.cfg.Clients)-1, s.hostMenuIndex+1)
		case "k", "\x1b[A":
			s.hostMenuIndex = max(0, s.hostMenuIndex-1)
		case "\r", "\n":
			if len(s.cfg.Clients) > 0 {
				s.hostMenuTarget = s.cfg.Clients[s.hostMenuIndex].Name
				s.hostMenuStep, s.hostMenuIndex = "actions", 0
			}
		}
		return ""
	}
	if strings.HasPrefix(s.hostMenuStep, "resources-") {
		switch string(input) {
		case "r":
			s.hostMenuStep = "resources-loading"
			return "host-resources-read"
		case "\r", "\n":
			if s.hostMenuStep == "resources-error" {
				s.hostMenuStep = "resources-loading"
				return "host-resources-read"
			}
		case "\x1b":
			s.hostMenuStep, s.hostMenuIndex = "actions", 8
		case "\x03":
			s.hostMenuMode = false
		}
		return ""
	}
	if strings.HasPrefix(s.hostMenuStep, "skills-") {
		return s.handleHostSkillsInput(input)
	}
	if strings.HasPrefix(s.hostMenuStep, "hook-") {
		switch string(input) {
		case "\x03":
			if s.hostMenuStep != "hook-saving" {
				s.hostMenuMode = false
			}
		case "\x1b":
			switch s.hostMenuStep {
			case "hook-confirm":
				s.hostMenuStep = "hook-select"
			case "hook-done":
				s.hostMenuMode = false
			case "hook-saving":
			default:
				s.hostMenuStep, s.hostMenuIndex = "actions", 3
			}
		case "j", "\x1b[B":
			if s.hostMenuStep == "hook-select" {
				s.hostMenuIndex = min(3, s.hostMenuIndex+1)
			}
		case "k", "\x1b[A":
			if s.hostMenuStep == "hook-select" {
				s.hostMenuIndex = max(0, s.hostMenuIndex-1)
			}
		case "\r", "\n":
			switch s.hostMenuStep {
			case "hook-select":
				s.hostHookAgent, s.hostHookAction = []string{"codex", "codex", "claude", "claude"}[s.hostMenuIndex], []string{"install", "remove", "install", "remove"}[s.hostMenuIndex]
				s.hostMenuStep = "hook-confirm"
			case "hook-confirm":
				s.hostMenuStep = "hook-saving"
				return "host-hook-save"
			case "hook-error":
				s.hostMenuStep = "hook-saving"
				return "host-hook-save"
			case "hook-done":
				s.hostMenuMode = false
			}
		}
		return ""
	}
	if strings.HasPrefix(s.hostMenuStep, "retention-") {
		switch string(input) {
		case "\x03":
			if s.hostMenuStep != "retention-saving" {
				s.hostMenuMode = false
			}
		case "\x1b":
			if s.hostMenuStep == "retention-confirm" {
				s.hostMenuStep = "retention-edit"
			} else if s.hostMenuStep == "retention-saved" {
				s.hostMenuMode = false
			} else if s.hostMenuStep != "retention-saving" {
				s.hostMenuStep, s.hostMenuIndex = "actions", 2
			}
		case "\x7f", "\b":
			if s.hostMenuStep == "retention-edit" && len(s.hostMenuDraft) > 0 {
				if s.hostMenuReplaceDraft {
					s.hostMenuDraft = ""
				} else {
					s.hostMenuDraft = s.hostMenuDraft[:len(s.hostMenuDraft)-1]
				}
				s.hostMenuReplaceDraft = false
				s.hostMenuErr = ""
			}
		case "\r", "\n":
			switch s.hostMenuStep {
			case "retention-saved":
				s.hostMenuMode = false
			case "retention-error":
				s.hostMenuStep, s.hostMenuErr = "retention-loading", ""
				return "host-retention-read"
			case "retention-edit":
				days, err := strconv.Atoi(s.hostMenuDraft)
				if err != nil || days < 1 || days > 3650 {
					s.hostMenuErr = "Enter a number from 1 to 3650"
				} else if days == s.hostMenuOldDays {
					s.hostMenuStep, s.hostMenuIndex = "actions", 3
				} else {
					s.hostMenuStep = "retention-confirm"
				}
			case "retention-confirm":
				s.hostMenuStep = "retention-saving"
				return "host-retention-save"
			}
		default:
			if s.hostMenuStep == "retention-edit" && len(input) == 1 && input[0] >= '0' && input[0] <= '9' && len(s.hostMenuDraft) < 4 {
				if s.hostMenuReplaceDraft {
					s.hostMenuDraft = string(input)
				} else {
					s.hostMenuDraft += string(input)
				}
				s.hostMenuReplaceDraft = false
				s.hostMenuErr = ""
			}
		}
		return ""
	}
	if s.hostMenuStep == "connections" {
		if len(s.cfg.Clients) == 0 {
			return ""
		}
		switch string(input) {
		case "\x03":
			s.hostMenuMode = false
			return "cancel"
		case "\x1b":
			s.hostMenuStep, s.hostMenuIndex = "actions", 0
			return ""
		case "j", "\x1b[B":
			s.hostMenuIndex = min(len(s.cfg.Clients)-1, s.hostMenuIndex+1)
		case "k", "\x1b[A":
			s.hostMenuIndex = max(0, s.hostMenuIndex-1)
		case " ":
			name := s.cfg.Clients[s.hostMenuIndex].Name
			s.hostMenuSelected[name] = !s.hostMenuSelected[name]
			s.hostMenuErr = ""
		case "\r", "\n":
			if len(s.changedHostMenuTargets(true))+len(s.changedHostMenuTargets(false)) == 0 {
				s.hostMenuErr = "no connection changes selected"
				return ""
			}
			return "host-apply-connections"
		}
		return ""
	}
	switch string(input) {
	case "\x1b", "\x03":
		if string(input) == "\x03" {
			s.hostMenuMode = false
			return "cancel"
		}
		s.hostMenuStep = "host-list"
		for i, client := range s.cfg.Clients {
			if client.Name == s.hostMenuTarget {
				s.hostMenuIndex = i
				break
			}
		}
		return ""
	case "j", "\x1b[B":
		s.hostMenuIndex = min(len(hostActions)-1, s.hostMenuIndex+1)
	case "k", "\x1b[A":
		s.hostMenuIndex = max(0, s.hostMenuIndex-1)
	case "\r", "\n":
		action := []string{"host-connections", "host-reconnect", "host-retention-read", "host-hooks", "host-skills", "host-notification-settings", "host-add", "host-remove", "host-resources-read"}[s.hostMenuIndex]
		if s.hostScoped && (action == "host-add" || action == "host-remove" || action == "host-notification-settings") {
			s.outputErr = "host configuration changes are unavailable in host-scoped mode"
			return ""
		}
		if action == "host-connections" {
			s.hostMenuStep = "connections"
			s.hostMenuIndex = 0
			s.hostMenuErr = ""
			for _, client := range s.cfg.Clients {
				s.hostMenuSelected[client.Name] = !s.disconnectedHosts[client.Name]
			}
			if s.hostMenuTarget != "" {
				for index, client := range s.cfg.Clients {
					if client.Name == s.hostMenuTarget {
						s.hostMenuIndex = index
						break
					}
				}
			}
			return ""
		}
		if action == "host-resources-read" {
			s.hostMenuStep = "resources-loading"
			s.hostMenuErr = ""
			return action
		}
		if action == "host-retention-read" {
			s.hostMenuStep = "retention-loading"
			s.hostMenuErr = ""
		}
		if action == "host-hooks" {
			s.hostMenuStep, s.hostMenuIndex = "hook-select", 0
			s.hostMenuErr = ""
			return "host-hooks-read"
		}
		if action == "host-skills" {
			s.beginHostSkills()
			return ""
		}
		return action
	}
	return ""
}

func (s *tuiState) changedHostMenuTargets(desired bool) []string {
	targets := make([]string, 0, len(s.cfg.Clients))
	for _, client := range s.cfg.Clients {
		currentDesired := !s.disconnectedHosts[client.Name]
		if s.hostMenuSelected[client.Name] == desired && desired != currentDesired {
			targets = append(targets, client.Name)
		}
	}
	return targets
}

func (s *tuiState) renderGroupModal(out io.Writer, cols, rows int) {
	if !s.groupMenu {
		return
	}
	s.resetModalMouse()
	lines := []modalRenderLine{{modalTitle, "  Custom groups"}}
	if s.groupMenuStep == "name" {
		verb := "Create group"
		if s.groupMenuAction == "rename" {
			verb = "Rename group"
		}
		lines = append(lines,
			modalRenderLine{modalStatus, "  " + verb + " · 1–64 Unicode characters"},
			modalRenderLine{modalInput, "  name › " + s.groupMenuLine})
	} else {
		labels := make([]string, 0)
		if s.groupMenuStep == "action" {
			for _, action := range groupMenuActions {
				labels = append(labels, action.label)
			}
		} else {
			for _, group := range s.groupMenuChoices() {
				label := group.Name
				if group.ID != ducklord.UngroupedGroupID {
					label = s.customGroupDisplayName(group)
				}
				labels = append(labels, label)
			}
		}
		if len(labels) == 0 {
			lines = append(lines, modalRenderLine{modalMuted, "  No custom groups"})
		}
		selected := min(max(s.groupMenuIndex, 0), max(0, len(labels)-1))
		start := 0
		maxChoices := max(1, rows-5)
		if len(labels) > maxChoices {
			start = max(0, selected-maxChoices/2)
			start = min(start, len(labels)-maxChoices)
			labels = labels[start : start+maxChoices]
		}
		for index, label := range labels {
			absoluteIndex := start + index
			s.modalChoice(len(lines), &s.groupMenuIndex, absoluteIndex, "\r")
			style, prefix := "", "  "
			if absoluteIndex == selected {
				style, prefix = modalSelected, "› "
			}
			if s.groupMenuStep == "action" && groupMenuActions[absoluteIndex].id == "delete" {
				style = modalDanger
				if absoluteIndex != selected {
					style = modalStatus
				}
			}
			lines = append(lines, modalRenderLine{style, prefix + label})
		}
	}
	status := s.groupMenuErr
	if status == "" {
		status = "Changes are local to Ducklord"
	}
	footer := "  ↑/↓ or j/k choose · Enter confirm · Esc cancel"
	if s.groupMenuStep == "name" {
		footer = "  Type name · Enter confirm · Esc cancel"
	}
	lines = append(lines, modalRenderLine{modalStatus, "  " + status}, modalRenderLine{modalMuted, footer})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) customGroupDisplayName(group ducklord.CustomGroup) string {
	duplicates := make([]string, 0)
	for _, candidate := range s.activity().Organization.Groups {
		if candidate.Name == group.Name {
			duplicates = append(duplicates, candidate.ID)
		}
	}
	if len(duplicates) < 2 {
		return group.Name
	}
	prefixLength := 4
	for ; prefixLength < len(group.ID); prefixLength++ {
		prefix := group.ID[:prefixLength]
		unique := true
		for _, id := range duplicates {
			if id != group.ID && strings.HasPrefix(id, prefix) {
				unique = false
				break
			}
		}
		if unique {
			break
		}
	}
	return group.Name + " · " + group.ID[:min(prefixLength, len(group.ID))]
}

type sessionAction struct {
	ID             string
	Key            string
	Label          string
	Enabled        bool
	DisabledReason string
	Danger         bool
	Operation      protocol.SessionLifecycleOperation
	Mode           protocol.SessionLifecycleMode
}

func actionOwnerLabel(session ducklord.RemoteSession) string {
	if session.Kind == string(model.KindShell) {
		return "shared"
	}
	if session.WriterID == "" {
		return "none"
	}
	return session.WriterKind + ":" + session.WriterID
}

func (s *tuiState) sessionActions(session ducklord.RemoteSession) []sessionAction {
	live := session.SessionID != "" && s.hostIsLive(session.Client)
	attachEnabled := live && canAttach(session)
	attachLabel := "Open PTY"
	if session.Kind == string(model.KindAgent) && (session.WriterKind != string(model.OwnerTerminal) || session.WriterID != s.ownerName) {
		attachLabel = "View PTY (read-only)"
	}
	actions := []sessionAction{
		{ID: "attach", Key: "o", Label: attachLabel, Enabled: attachEnabled, DisabledReason: "session is not running or host is reconnecting"},
		{ID: "reconnect", Key: "c", Label: "Reconnect PTY output", Enabled: attachEnabled, DisabledReason: "session is not running or host is reconnecting"},
	}
	if session.Kind == string(model.KindAgent) {
		adapterHealthy := session.AdapterState == string(model.AdapterHealthy)
		nonOwner := session.WriterKind != string(model.OwnerTerminal) || session.WriterID != s.ownerName
		yieldReady := live && adapterHealthy && nonOwner
		actions = append(actions,
			sessionAction{ID: "yield", Key: "y", Label: "Yield control now", Enabled: yieldReady && session.TaskState == string(model.TaskIdle), DisabledReason: actionDisabledReason(live, adapterHealthy, nonOwner, session.TaskState == string(model.TaskIdle))},
			sessionAction{ID: "yield-wait", Key: "Y", Label: "Yield when idle", Enabled: yieldReady, DisabledReason: actionDisabledReason(live, adapterHealthy, nonOwner, true)},
		)
	}
	actions = append(actions, sessionAction{ID: "notifications", Key: "n", Label: "Notification settings (local)", Enabled: session.InstanceID != "" && session.SessionID != "", DisabledReason: "session identity is unavailable"})
	renameAction := sessionAction{ID: "rename-handle", Key: "h", Label: "Rename handle", Enabled: session.InstanceID != "" && session.SessionID != "", DisabledReason: "session identity is unavailable"}
	lifecycleEnabled := live && !s.lifecycleBusy && (session.Kind == string(model.KindShell) || session.WriterKind == string(model.OwnerTerminal) && session.WriterID == s.ownerName)
	lifecycleReason := "host is reconnecting"
	if s.lifecycleBusy {
		lifecycleReason = "another lifecycle operation is pending"
	} else if live && session.Kind == string(model.KindAgent) {
		lifecycleReason = "yield control to this Ducklord first"
	}
	addLifecycle := func(id, key, label string, operation protocol.SessionLifecycleOperation, mode protocol.SessionLifecycleMode, allowed bool, danger bool) {
		reason := lifecycleReason
		if lifecycleEnabled && !allowed {
			reason = "current task or adapter state does not support this mode"
		}
		actions = append(actions, sessionAction{ID: id, Key: key, Label: label, Enabled: lifecycleEnabled && allowed, DisabledReason: reason, Danger: danger, Operation: operation, Mode: mode})
	}
	if session.Kind == string(model.KindShell) {
		addLifecycle("restart", "r", "Restart shell immediately", protocol.SessionLifecycleRestart, protocol.SessionLifecycleImmediate, true, false)
		addLifecycle("end", "e", "End shell immediately", protocol.SessionLifecycleEnd, protocol.SessionLifecycleImmediate, true, true)
		addLifecycle("destroy", "x", "Destroy shell and logs", protocol.SessionLifecycleDestroy, protocol.SessionLifecycleImmediate, true, true)
		actions = append(actions, renameAction)
		return actions
	}
	idle := session.TaskState == string(model.TaskIdle)
	adapterHealthy := session.AdapterState == string(model.AdapterHealthy)
	addLifecycle("restart", "r", "Restart now", protocol.SessionLifecycleRestart, protocol.SessionLifecycleImmediate, idle, false)
	addLifecycle("restart-wait", "", "Restart when idle", protocol.SessionLifecycleRestart, protocol.SessionLifecycleWait, adapterHealthy, false)
	addLifecycle("restart-force", "", "Restart and force-cancel task", protocol.SessionLifecycleRestart, protocol.SessionLifecycleForce, adapterHealthy, true)
	addLifecycle("end", "e", "End now", protocol.SessionLifecycleEnd, protocol.SessionLifecycleImmediate, idle, true)
	addLifecycle("end-wait", "", "End when idle", protocol.SessionLifecycleEnd, protocol.SessionLifecycleWait, adapterHealthy, true)
	addLifecycle("end-force", "", "End and force-cancel task", protocol.SessionLifecycleEnd, protocol.SessionLifecycleForce, adapterHealthy, true)
	addLifecycle("destroy", "x", "Destroy now", protocol.SessionLifecycleDestroy, protocol.SessionLifecycleImmediate, idle, true)
	addLifecycle("destroy-wait", "", "Destroy when idle", protocol.SessionLifecycleDestroy, protocol.SessionLifecycleWait, adapterHealthy, true)
	addLifecycle("destroy-force", "", "Destroy and force-cancel task", protocol.SessionLifecycleDestroy, protocol.SessionLifecycleForce, adapterHealthy, true)
	actions = append(actions, renameAction)
	return actions
}

func actionDisabledReason(live, adapterHealthy, nonOwner, idle bool) string {
	switch {
	case !live:
		return "host is reconnecting"
	case !adapterHealthy:
		return "agent adapter is not healthy"
	case !nonOwner:
		return "this Ducklord already controls the session"
	case !idle:
		return "agent task is running; use Yield when idle"
	default:
		return "unavailable"
	}
}

func (s *tuiState) renderCreateModal(out io.Writer, cols, rows int) {
	if !s.newSessionMode || cols < 8 || rows < 3 {
		return
	}
	s.resetModalMouse()
	if cols < 20 || rows < 7 {
		choices := s.createModalChoices()
		selected := s.createModalSelectedIndex(len(choices))
		if s.newSessionStep == "path" {
			selected = -1
			if len(choices) > 0 {
				selected = min(max(s.newSessionPathSelected, 0), len(choices)-1)
			}
		}
		choice := ""
		if selected >= 0 {
			choice = "› " + choices[selected]
		}
		boxWidth := max(8, cols-2)
		innerWidth := boxWidth - 2
		boxHeight := min(4, rows)
		top := max(1, (rows-boxHeight)/2+1)
		left := max(1, (cols-boxWidth)/2+1)
		writeRow := func(row int, style, text string) {
			text = modalCellPad(modalCellTruncate(modalDisplayText(text), innerWidth), innerWidth)
			fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", row, left, modalSurface+modalBorder, modalSurface+style, text, modalReset+modalSurface+modalBorder, modalReset)
		}
		writePrompt := func(row int) {
			s.renderCreatePromptRow(out, row, left, innerWidth, "")
		}
		fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
		if boxHeight == 3 {
			s.renderCreatePromptRow(out, top+1, left, innerWidth, choice+"  ")
			s.createMouseRegion(left+1, top+1, min(innerWidth, modalCellWidth(choice)), selected)
		} else {
			writeRow(top+1, modalSelected, choice)
			s.createMouseRegion(left+1, top+1, innerWidth, selected)
			writePrompt(top + 2)
		}
		fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+boxHeight-1, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
		return
	}
	boxWidth := min(72, cols-4)
	innerWidth := boxWidth - 2
	allChoices := s.createModalChoices()
	selected := s.createModalSelectedIndex(len(allChoices))
	if s.newSessionStep == "path" {
		selected = -1
		if len(allChoices) > 0 {
			selected = min(max(s.newSessionPathSelected, 0), len(allChoices)-1)
		}
	}
	choices := allChoices
	maxChoices := max(1, rows-7)
	hiddenBefore, hiddenAfter := 0, 0
	if len(choices) > maxChoices {
		start := max(0, selected-maxChoices/2)
		start = min(start, len(choices)-maxChoices)
		hiddenBefore = start
		hiddenAfter = len(choices) - start - maxChoices
		choices = choices[start : start+maxChoices]
		selected -= start
	}
	boxHeight := len(choices) + 6
	top := max(1, (rows-boxHeight)/2+1)
	left := max(1, (cols-boxWidth)/2+1)
	writeRow := func(row int, style, text string) {
		text = modalCellPad(modalCellTruncate(modalDisplayText(text), innerWidth), innerWidth)
		fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", row, left, modalSurface+modalBorder, modalSurface+style, text, modalReset+modalSurface+modalBorder, modalReset)
	}
	fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
	writeRow(top+1, modalTitle, "  "+s.createHeader())
	for i, choice := range choices {
		prefix, style := "  ", ""
		if i == selected && !s.newSessionDiscovering && !s.newSessionStarting {
			prefix, style = "› ", modalSelected
		}
		writeRow(top+2+i, style, prefix+choice)
		s.createMouseRegion(left+1, top+2+i, innerWidth, hiddenBefore+i)
	}
	status := s.newSessionErr
	if hiddenBefore+hiddenAfter > 0 {
		status = fmt.Sprintf("%s  (%d above, %d below)", status, hiddenBefore, hiddenAfter)
	}
	writeRow(top+2+len(choices), modalStatus, "  "+status)
	s.renderCreatePromptRow(out, top+3+len(choices), left, innerWidth, "  ")
	writeRow(top+4+len(choices), modalMuted, "  ↑/↓ select   Enter continue   Esc back   Ctrl+C close")
	s.modalHintRegions("  ↑/↓ select   Enter continue   Esc back   Ctrl+C close", left+1, top+4+len(choices), innerWidth)
	fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+5+len(choices), left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
}

func (s *tuiState) renderCreatePromptRow(out io.Writer, row, left, innerWidth int, indent string) {
	prefix := modalDisplayText(indent + s.createPromptLabel() + " › " + s.newSessionLine)
	ghost := ""
	if s.newSessionLine == "" {
		switch s.newSessionStep {
		case "handle", "project-name":
			ghost = modalDisplayText(defaultSessionHandle(s.newSessionCWD))
			if s.newSessionStep == "project-name" {
				ghost = modalDisplayText(projectregistry.DefaultProjectName(filepath.Base(s.newSessionCWD)))
			}
		}
	}
	prefix = modalCellTruncate(prefix, innerWidth)
	remaining := max(0, innerWidth-modalCellWidth(prefix))
	ghost = modalCellTruncate(ghost, remaining)
	padding := strings.Repeat(" ", max(0, remaining-modalCellWidth(ghost)))
	fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s%s%s%s│%s", row, left, modalSurface+modalBorder, modalSurface+modalInput, prefix, modalMuted, ghost, padding, modalReset+modalSurface+modalBorder, modalReset)
}

func (s *tuiState) renderActionModal(out io.Writer, cols, rows int) {
	if !s.actionMenu || cols < 8 || rows < 3 {
		return
	}
	s.resetModalMouse()
	actions := s.sessionActions(s.actionTarget)
	if len(actions) == 0 {
		return
	}
	selected := min(max(s.actionIndex, 0), len(actions)-1)
	if cols < 20 || rows < 7 {
		action := actions[selected]
		text := "› " + action.Label
		style := modalSelected
		if !action.Enabled {
			style = modalDisabled
		} else if action.Danger {
			style = modalDanger
		}
		fmt.Fprintf(out, "\033[%d;1H%s%s%s\033[K", max(1, rows/2), modalTitle, modalCellTruncate("[ Session actions ]", cols), modalReset)
		fmt.Fprintf(out, "\033[%d;1H%s%s%s\033[K", min(rows, max(1, rows/2)+1), style, modalCellTruncate(text, cols), modalReset)
		if action.Enabled {
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{1, cols, min(rows, max(1, rows/2)+1), modalMouseAction{selection: &s.actionIndex, index: selected, key: "\r"}})
		}
		return
	}
	boxWidth := min(72, cols-4)
	innerWidth := boxWidth - 2
	maxChoices := max(1, rows-7)
	start := 0
	if len(actions) > maxChoices {
		start = max(0, selected-maxChoices/2)
		start = min(start, len(actions)-maxChoices)
	}
	end := min(len(actions), start+maxChoices)
	visible := actions[start:end]
	visibleSelected := selected - start
	boxHeight := len(visible) + 6
	top := max(1, (rows-boxHeight)/2+1)
	left := max(1, (cols-boxWidth)/2+1)
	writeRow := func(row int, style, value string) {
		value = modalCellPad(modalCellTruncate(modalDisplayText(value), innerWidth), innerWidth)
		fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", row, left, modalSurface+modalBorder, modalSurface+style, value, modalReset+modalSurface+modalBorder, modalReset)
	}
	fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
	title := fmt.Sprintf("  Session actions · %s / %s", s.actionTarget.Client, s.actionTarget.Name)
	writeRow(top+1, modalTitle, title)
	for i, action := range visible {
		prefix, style := "  ", ""
		if !action.Enabled {
			style = modalDisabled
		}
		if i == visibleSelected {
			prefix = "› "
			if action.Enabled && action.Danger {
				style = modalDanger
			} else if action.Enabled {
				style = modalSelected
			}
		}
		label := action.Label
		if action.Key != "" {
			label = "[" + action.Key + "] " + label
		}
		writeRow(top+2+i, style, prefix+label)
		if action.Enabled {
			s.modalMouseRegions = append(s.modalMouseRegions, modalMouseRegion{left + 1, left + innerWidth, top + 2 + i, modalMouseAction{selection: &s.actionIndex, index: start + i, key: "\r"}})
		}
	}
	status := fmt.Sprintf("  %s · %s · owner %s", s.actionTarget.Kind, s.actionTarget.Status, actionOwnerLabel(s.actionTarget))
	if !actions[selected].Enabled {
		status = "  unavailable: " + actions[selected].DisabledReason
	}
	writeRow(top+2+len(visible), modalStatus, status)
	writeRow(top+3+len(visible), modalMuted, "  ↑/↓ or j/k select   Enter choose   Esc close")
	s.modalHintRegions("  ↑/↓ or j/k select   Enter choose   Esc close", left+1, top+3+len(visible), innerWidth)
	writeRow(top+4+len(visible), modalMuted, fmt.Sprintf("  ID %s · generation %d", s.actionTarget.SessionID, s.actionTarget.RuntimeGeneration))
	fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+5+len(visible), left, modalSurface+modalBorder, strings.Repeat("─", innerWidth), modalReset)
}

func (s *tuiState) createModalChoices() []string {
	switch s.newSessionStep {
	case "kind":
		return []string{"1  Agent session", "2  Shell session"}
	case "host":
		hosts := s.workspaceCreateHosts()
		choices := make([]string, 0, len(hosts))
		for i, client := range hosts {
			choices = append(choices, fmt.Sprintf("%d  %s  %s", i+1, displayField(client.Name), displayField(client.Target())))
		}
		return choices
	case "project":
		choices := make([]string, 0, len(s.newSessionProjects)+1)
		for i, project := range s.newSessionProjects {
			choices = append(choices, fmt.Sprintf("%d  %s  %s", i+1, displayField(project.Name), displayField(project.Path)))
		}
		choices = append(choices, fmt.Sprintf("%d  Browse remote path…", len(choices)+1))
		return choices
	case "path":
		choices := make([]string, 0, len(s.newSessionPathSuggestions))
		for _, path := range s.newSessionPathSuggestions {
			choices = append(choices, displayField(path))
		}
		return choices
	case "path-confirm":
		return []string{"1  Create directory recursively", "2  Back without creating"}
	case "project-policy":
		return []string{"1  Add path to bookmarks", "2  Use path once"}
	case "project-name":
		return nil
	case "agent":
		choices := make([]string, 0, len(s.newSessionAgents))
		for i, agent := range s.newSessionAgents {
			choices = append(choices, fmt.Sprintf("%d  %s", i+1, displayField(agent.Type)))
		}
		return choices
	case "handle":
		return nil
	default:
		return nil
	}
}

func (s *tuiState) createModalSelectedIndex(choiceCount int) int {
	if choiceCount == 0 {
		return -1
	}
	return min(max(s.newSessionSelected, 0), choiceCount-1)
}

func modalDisplayText(value string) string {
	value = sanitizeTerminalText(value)
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(value, "�"))
}

func modalRuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	switch textwidth.LookupRune(r).Kind() {
	case textwidth.EastAsianWide, textwidth.EastAsianFullwidth:
		return 2
	default:
		return 1
	}
}

// modalDisplayANSI preserves only SGR color sequences while sanitizing all
// other control bytes. It is used by the few views that color independent
// regions within one modal line.
func modalDisplayANSI(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); {
		if value[i] == '\x1b' && i+1 < len(value) && value[i+1] == '[' {
			j := i + 2
			for j < len(value) && (value[j] >= '0' && value[j] <= '9' || value[j] == ';' || value[j] == ':') {
				j++
			}
			if j < len(value) && value[j] == 'm' {
				b.WriteString(value[i : j+1])
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		if r >= ' ' && r != '\x7f' && !unicode.IsControl(r) {
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

func modalANSISequence(value string, i int) int {
	if i+1 >= len(value) || value[i] != '\x1b' || value[i+1] != '[' {
		return i
	}
	j := i + 2
	for j < len(value) && (value[j] >= '0' && value[j] <= '9' || value[j] == ';' || value[j] == ':') {
		j++
	}
	if j < len(value) && value[j] == 'm' {
		return j + 1
	}
	return i
}

func modalCellWidth(value string) int {
	width := 0
	for i := 0; i < len(value); {
		if next := modalANSISequence(value, i); next != i {
			i = next
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		width += modalRuneWidth(r)
		i += size
	}
	return width
}

func modalCellTruncate(value string, cells int) string {
	if cells <= 0 {
		return ""
	}
	if modalCellWidth(value) <= cells {
		return value
	}
	ellipsis := "…"
	limit := max(0, cells-modalCellWidth(ellipsis))
	var b strings.Builder
	used := 0
	for i := 0; i < len(value); {
		if next := modalANSISequence(value, i); next != i {
			b.WriteString(value[i:next])
			i = next
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		w := modalRuneWidth(r)
		if used+w > limit {
			break
		}
		b.WriteString(value[i : i+size])
		used += w
		i += size
	}
	b.WriteString(ellipsis)
	if strings.Contains(value, "\x1b[") {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func modalCellPad(value string, cells int) string {
	return value + strings.Repeat(" ", max(0, cells-modalCellWidth(value)))
}

func (s *tuiState) renderContent(out io.Writer, x, width, height int) {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		return
	}
	sess := s.sessions[s.selected]
	header := fmt.Sprintf("%s / %s  %s  %s", displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), displayField(sessionTypeLabel(sess)))
	if sess.Status == string(model.StatusStopped) && sess.RetainedOutputBytes > 0 {
		header += fmt.Sprintf("  retained:%s until %s", formatByteCount(sess.RetainedOutputBytes), formatRetainedUntil(sess.RetainedOutputUntilMS))
	}
	if s.focused {
		header += "  [focus]"
	}
	if s.outputStale {
		if s.outputReconnecting {
			header += "  [RECONNECTING PTY…]"
		} else {
			header += "  [STALE SNAPSHOT]"
		}
	}
	fmt.Fprintf(out, "\033[5;%dH%s\033[K", x, truncate(header, width))
	startRow := 6
	if s.outputErr != "" && s.outputText == "" && s.terminal == nil {
		for i, line := range wrapDisplayText("error: "+sanitizeTerminalText(s.outputErr), width) {
			if startRow+i > height {
				break
			}
			fmt.Fprintf(out, "\033[%d;%dH%s\033[K", startRow+i, x, line)
		}
		return
	}
	lines := tailLines(strings.Split(strings.TrimRight(s.outputText, "\n"), "\n"), height-startRow+1)
	styled := false
	if terminal := s.viewportTerminal(); terminal != nil {
		lines = terminal.RenderLinesOffset(height-startRow+1, width, s.ptyScrollOffset)
		styled = true
	}
	for i, line := range lines {
		if styled {
			fmt.Fprintf(out, "\033[%d;%dH%s\033[K", startRow+i, x, line)
		} else {
			fmt.Fprintf(out, "\033[%d;%dH%s\033[K", startRow+i, x, truncate(line, width))
		}
	}
}

func (s *tuiState) lifecycleTargetIsCurrent() bool {
	target := s.lifecycleTarget
	if target.Client == "" || target.InstanceID == "" || target.SessionID == "" || !s.hostIsLive(target.Client) {
		return false
	}
	for _, session := range s.sessions {
		if session.Client != target.Client || session.InstanceID != target.InstanceID || session.SessionID != target.SessionID {
			continue
		}
		return sameSessionRevision(session, target)
	}
	return false
}

func notificationLabel(category model.NotificationCategory) string {
	switch category {
	case model.NotificationTerminalAttention:
		return "Terminal attention (BEL / OSC)"
	case model.NotificationTaskCompleted:
		return "Task completed"
	case model.NotificationTaskFailed:
		return "Task failed"
	case model.NotificationTaskCancelled:
		return "Task cancelled"
	case model.NotificationTaskTimeout:
		return "Task timeout"
	case model.NotificationApprovalRequired:
		return "Approval required"
	case model.NotificationAgentNeedsInput:
		return "Agent needs input"
	case model.NotificationUnexpectedProcessExit:
		return "Unexpected process exit"
	default:
		return string(category)
	}
}

func (s *tuiState) renderSearchModal(out io.Writer, cols, rows int) {
	if !s.searchMode {
		return
	}
	s.resetModalMouse()
	results := s.searchResults()
	totalResults := len(results)
	lines := []modalRenderLine{
		{modalTitle, "  Search sessions"},
		{modalInput, "  search › " + s.searchQuery},
	}
	maxResults := max(1, (rows-7)/2)
	start := 0
	if len(results) > maxResults {
		start = min(max(0, s.searchSelected-maxResults/2), len(results)-maxResults)
		results = results[start : start+maxResults]
	}
	if len(results) == 0 {
		lines = append(lines, modalRenderLine{modalStatus, "  No matching sessions"})
	} else {
		currentGroup := "\x00"
		for index, session := range results {
			absolute := start + index
			groupID := s.organizationGroupID(session)
			if groupID != currentGroup {
				groupLabel := "  [" + displayField(s.organizationGroupLabel(session)) + "]"
				if s.groupHasUnread(groupID) {
					groupLabel += " •"
				}
				lines = append(lines, modalRenderLine{modalMuted, groupLabel})
				currentGroup = groupID
			}
			style, prefix := "", "  "
			if absolute == s.searchSelected {
				style, prefix = modalSelected, "› "
			}
			mark := " "
			if sessionKey(session) == s.activeAttachKey {
				mark = "●"
			} else if session.Unread {
				mark = "•"
			}
			s.modalChoice(len(lines), &s.searchSelected, absolute, "\r")
			lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%s %-12s %-18s %-8s %s", prefix, mark,
				displayField(session.Client), displayField(session.Name), displayField(session.Kind), displayField(session.AgentType))})
		}
	}
	status := fmt.Sprintf("  %d match", totalResults)
	if totalResults != 1 {
		status += "es"
	}
	if s.searchErr != "" {
		status = "  " + sanitizeTerminalText(s.searchErr)
	}
	lines = append(lines, modalRenderLine{modalStatus, status}, modalRenderLine{modalMuted, "  ↑/↓ select · Enter activate · Esc close"})
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderNotificationModal(out io.Writer, cols, rows int) {
	if !s.notificationMode {
		return
	}
	s.resetModalMouse()
	target := s.notificationTarget
	categories := model.NotificationCategories()
	if s.notificationLevelEditing {
		class := notificationConfigClasses[s.notificationIndex-len(categories)]
		lines := []modalRenderLine{{modalTitle, fmt.Sprintf("  Notification delivery · %s / %s", target.Client, target.Name)},
			{modalStatus, "  " + notificationClassLabel(class)}}
		lines = append(lines, modalRenderLine{modalInput, choiceLine(s.notificationLevelChoice == 0, "inherit Host")})
		s.modalChoice(len(lines)-1, &s.notificationLevelChoice, 0, "\r")
		for i, level := range notificationConfigLevels {
			s.modalChoice(len(lines), &s.notificationLevelChoice, i+1, "\r")
			lines = append(lines, modalRenderLine{modalInput, choiceLine(s.notificationLevelChoice == i+1, string(level))})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter stage · Esc back"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	selected := min(max(s.notificationIndex, 0), len(categories)+len(notificationConfigClasses)-1)
	maxChoices := max(1, rows-8)
	start := max(0, selected-maxChoices/2)
	start = min(start, max(0, len(categories)+len(notificationConfigClasses)-maxChoices))
	lines := []modalRenderLine{{modalTitle, fmt.Sprintf("  Notifications · %s / %s", target.Client, target.Name)}}
	for index := start; index < min(len(categories)+len(notificationConfigClasses), start+maxChoices); index++ {
		s.modalChoice(len(lines), &s.notificationIndex, index, "\r")
		label := ""
		if index < len(categories) {
			category := categories[index]
			check := " "
			if s.notificationStaged[category] {
				check = "x"
			}
			label = fmt.Sprintf("[%s] %s", check, notificationLabel(category))
		} else {
			class := notificationConfigClasses[index-len(categories)]
			level := "inherit Host"
			if override := s.notificationLevelsStaged[class]; override != "" {
				level = string(override)
			}
			label = fmt.Sprintf("%s delivery: %s", notificationClassLabel(class), level)
		}
		style, prefix := "", "  "
		if index == selected {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, prefix + label})
	}
	status := fmt.Sprintf("  ID %s · changes are staged", target.SessionID)
	if s.outputErr != "" {
		status = "  Not saved: " + s.outputErr
	}
	lines = append(lines, modalRenderLine{modalStatus, status},
		modalRenderLine{modalMuted, "  a/r all · x none · Space toggle source · Enter edit delivery"},
		modalRenderLine{modalMuted, "  ↑/↓ select · s save · Esc cancel"})
	if rows < 7 {
		lines = lines[:min(len(lines), 2)]
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderLifecycleModal(out io.Writer, cols, rows int) {
	if s.lifecycleConfirm == "" {
		return
	}
	s.resetModalMouse()
	target := s.lifecycleTarget
	verb := strings.ToUpper(string(s.lifecycleConfirm))
	style := modalTitle
	if s.lifecycleConfirm == protocol.SessionLifecycleDestroy || s.lifecycleConfirm == protocol.SessionLifecycleEnd {
		style = modalDanger
	}
	detail := "Restart keeps the session identity and starts a new runtime generation."
	switch s.lifecycleConfirm {
	case protocol.SessionLifecycleDestroy:
		detail = "Destroy permanently removes this PTY and its retained logs."
	case protocol.SessionLifecycleEnd:
		detail = "End keeps the session metadata and retained logs for recovery."
	}
	help := "Enter wait for idle · f force-cancel task · Esc cancel"
	if s.lifecycleConfirm != protocol.SessionLifecycleRestart {
		help = "Enter now · w wait for idle · f force-cancel task · Esc cancel"
	}
	if s.lifecycleMode != "" {
		help = "Enter confirm selected mode · Esc cancel"
	}
	if target.Kind == string(model.KindShell) {
		switch s.lifecycleConfirm {
		case protocol.SessionLifecycleRestart:
			detail = "Shell restarts immediately; this Session pane stays."
			help = "Enter restart process immediately · Esc cancel"
		case protocol.SessionLifecycleEnd:
			detail = "Shell and Session pane disappear; retained PTY logs remain."
			help = "Enter end process immediately · Esc cancel"
		case protocol.SessionLifecycleDestroy:
			detail = "Shell and Session pane disappear; retained PTY logs are removed."
			help = "Enter destroy process immediately · Esc cancel"
		}
	}
	if rows < 7 {
		warning := verb + ": " + detail + "  " + help
		s.renderModalBox(out, cols, rows, []modalRenderLine{{style, warning}})
		return
	}
	lines := []modalRenderLine{
		{style, "  " + verb + " SESSION"},
		{modalInput, fmt.Sprintf("  %s [%s]", target.Name, target.SessionID)},
		{modalMuted, fmt.Sprintf("  Host %s · generation %d", target.Client, target.RuntimeGeneration)},
		{modalStatus, "  " + detail},
		{modalMuted, "  " + help},
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) beginNotificationSettings() {
	session := s.currentSession()
	if session.InstanceID == "" || session.SessionID == "" {
		s.outputErr = "notification settings require a synchronized Ducklion session"
		return
	}
	s.notificationMode = true
	s.notificationIndex = 0
	s.notificationLevelEditing = false
	s.notificationTarget = session
	s.outputErr = ""
	s.notificationStaged = make(map[model.NotificationCategory]bool)
	for _, category := range model.NotificationCategories() {
		s.notificationStaged[category] = s.activity().Enabled(session.InstanceID, session.SessionID, category)
	}
	s.notificationLevelsStaged = make(map[ducklord.NotificationClass]ducklord.NotificationLevel)
	if identity, ok := ducklord.IdentityFromSession(session); ok {
		for class, level := range s.activity().Sessions[identity.Key()].NotificationLevels {
			s.notificationLevelsStaged[class] = level
		}
	}
}

func (s *tuiState) closeNotificationSettings() {
	s.notificationMode = false
	s.notificationStaged = nil
	s.notificationLevelsStaged = nil
	s.notificationLevelEditing = false
	s.notificationTarget = ducklord.RemoteSession{}
}

func (s *tuiState) beginActionMenu() {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		s.outputErr = "no session selected"
		return
	}
	s.beginSessionActionMenu(s.sessions[s.selected])
}

func (s *tuiState) beginSessionActionMenu(target ducklord.RemoteSession) {
	if target.InstanceID == "" || target.SessionID == "" {
		s.outputErr = "session identity is unavailable while the host reconnects"
		return
	}
	s.actionMenu = true
	s.actionTarget = target
	s.actionIndex = 0
	s.actionOperation, s.actionMode = "", ""
	s.outputErr = ""
}

func (s *tuiState) closeActionMenu() {
	s.actionMenu = false
	s.actionIndex = 0
	s.actionTarget = ducklord.RemoteSession{}
	s.actionOperation, s.actionMode = "", ""
}

func (s *tuiState) handleActionMenuInput(input []byte) string {
	actions := s.sessionActions(s.actionTarget)
	if len(actions) == 0 {
		s.closeActionMenu()
		return "cancel"
	}
	s.actionIndex = min(max(s.actionIndex, 0), len(actions)-1)
	text := string(input)
	for index, action := range actions {
		if action.Key != "" && text == action.Key && action.Enabled {
			s.actionIndex = index
			return s.chooseActionMenuItem(action)
		}
	}
	switch text {
	case "\x1b", "q":
		s.closeActionMenu()
		return "cancel"
	case "j", "\x1b[B":
		for next := s.actionIndex + 1; next < len(actions); next++ {
			if actions[next].Enabled {
				s.actionIndex = next
				break
			}
		}
	case "k", "\x1b[A":
		for next := s.actionIndex - 1; next >= 0; next-- {
			if actions[next].Enabled {
				s.actionIndex = next
				break
			}
		}
	case "\r", "\n":
		action := actions[s.actionIndex]
		if !action.Enabled {
			return ""
		}
		return s.chooseActionMenuItem(action)
	}
	return ""
}

func (s *tuiState) chooseActionMenuItem(action sessionAction) string {
	switch action.ID {
	case "attach", "reconnect", "yield", "yield-wait":
		s.actionMenu = false
		s.actionOperation, s.actionMode = action.Operation, action.Mode
		return action.ID
	case "notifications":
		s.actionMenu = false
		s.actionOperation, s.actionMode = action.Operation, action.Mode
		return "notifications-action"
	case "rename-handle":
		s.actionMenu = false
		return "rename-handle-action"
	default:
		if action.Operation != "" {
			s.actionMenu = false
			s.actionOperation, s.actionMode = action.Operation, action.Mode
			return "lifecycle-action"
		}
		return ""
	}
}

func sameSessionRevision(left, right ducklord.RemoteSession) bool {
	return left.Client == right.Client && left.InstanceID == right.InstanceID && left.SessionID == right.SessionID &&
		left.Kind == right.Kind && left.WriterKind == right.WriterKind && left.WriterID == right.WriterID &&
		left.OwnershipEpoch == right.OwnershipEpoch && left.RuntimeGeneration == right.RuntimeGeneration &&
		left.Status == right.Status && left.TaskState == right.TaskState && left.AdapterState == right.AdapterState
}

func (s *tuiState) selectActionTarget() bool {
	if !s.hostIsLive(s.actionTarget.Client) {
		return false
	}
	for i, session := range s.sessions {
		if sameSessionRevision(session, s.actionTarget) {
			s.selected = i
			s.selectedKey = sessionKey(session)
			return true
		}
	}
	return false
}

func (s *tuiState) handleNotificationInput(input []byte) string {
	categories := model.NotificationCategories()
	if s.notificationLevelEditing {
		switch string(input) {
		case "\x1b", "\x03":
			s.notificationLevelEditing = false
		case "j", "\x1b[B":
			s.notificationLevelChoice = min(len(notificationConfigLevels), s.notificationLevelChoice+1)
		case "k", "\x1b[A":
			s.notificationLevelChoice = max(0, s.notificationLevelChoice-1)
		case "\r", "\n":
			class := notificationConfigClasses[s.notificationIndex-len(categories)]
			if s.notificationLevelChoice == 0 {
				delete(s.notificationLevelsStaged, class)
			} else {
				s.notificationLevelsStaged[class] = notificationConfigLevels[s.notificationLevelChoice-1]
			}
			s.notificationLevelEditing = false
		}
		return ""
	}
	switch string(input) {
	case "\x1b", "q":
		return "cancel"
	case "\r", "\n":
		if s.notificationIndex < len(categories) {
			category := categories[s.notificationIndex]
			s.notificationStaged[category] = !s.notificationStaged[category]
			return ""
		}
		class := notificationConfigClasses[s.notificationIndex-len(categories)]
		s.notificationLevelChoice = 0
		for i, level := range notificationConfigLevels {
			if s.notificationLevelsStaged[class] == level {
				s.notificationLevelChoice = i + 1
			}
		}
		s.notificationLevelEditing = true
	case "s":
		return "save"
	case "j", "\x1b[B":
		if s.notificationIndex < len(categories)+len(notificationConfigClasses)-1 {
			s.notificationIndex++
		}
	case "k", "\x1b[A":
		if s.notificationIndex > 0 {
			s.notificationIndex--
		}
	case " ":
		if s.notificationIndex < len(categories) {
			category := categories[s.notificationIndex]
			s.notificationStaged[category] = !s.notificationStaged[category]
		}
	case "a", "r":
		for _, category := range categories {
			s.notificationStaged[category] = true
		}
	case "x":
		for _, category := range categories {
			s.notificationStaged[category] = false
		}
	}
	return ""
}

func (s *tuiState) saveNotificationSettings() {
	session := s.notificationTarget
	found := false
	for _, current := range s.sessions {
		if current.Client == session.Client && current.InstanceID == session.InstanceID && current.SessionID == session.SessionID {
			found = true
			break
		}
	}
	if !found {
		s.outputErr = "notification target changed; reopen settings"
		return
	}
	next := s.activity().Clone()
	for _, category := range model.NotificationCategories() {
		if err := next.SetEnabled(session.InstanceID, session.SessionID, category, s.notificationStaged[category]); err != nil {
			s.outputErr = err.Error()
			return
		}
	}
	if err := ducklord.ValidateNotificationLevels(s.notificationLevelsStaged); err != nil {
		s.outputErr = err.Error()
		return
	}
	identity, ok := ducklord.IdentityFromSession(session)
	if !ok {
		s.outputErr = "notification target identity is unavailable"
		return
	}
	entry := next.Sessions[identity.Key()]
	entry.NotificationLevels = make(map[ducklord.NotificationClass]ducklord.NotificationLevel, len(s.notificationLevelsStaged))
	for class, level := range s.notificationLevelsStaged {
		entry.NotificationLevels[class] = level
	}
	next.Sessions[identity.Key()] = entry
	if err := s.activityStore.Save(next); err != nil {
		s.outputErr = "notification state: " + err.Error()
		return
	}
	s.activityState = next
	s.closeNotificationSettings()
	s.outputErr = "notification settings saved"
}

func wrapDisplayText(text string, width int) []string {
	if width < 1 {
		return nil
	}
	runes := []rune(text)
	lines := make([]string, 0, (len(runes)+width-1)/width)
	for len(runes) > 0 {
		n := width
		if n > len(runes) {
			n = len(runes)
		}
		lines = append(lines, string(runes[:n]))
		runes = runes[n:]
	}
	return lines
}

func (s *tuiState) beginSearch() {
	s.searchMode = true
	s.searchQuery = ""
	s.searchSelected = 0
	s.searchSelectedKey = ""
	s.searchRevision++
	s.searchActivatedKey = ""
	s.searchActivatedRevision = 0
	s.searchActivatedGeneration = 0
	s.searchErr = ""
	s.syncSearchSelection()
}

func (s *tuiState) closeSearch() {
	s.searchMode = false
	s.searchQuery = ""
	s.searchSelected = 0
	s.searchSelectedKey = ""
	s.searchActivatedKey = ""
	s.searchActivatedRevision = 0
	s.searchActivatedGeneration = 0
	s.searchErr = ""
	s.searchPendingRequestID = 0
	s.searchPendingKey = ""
	s.searchPendingGeneration = 0
	s.searchPendingRevision = 0
}

func (s *tuiState) searchResult() (ducklord.RemoteSession, bool) {
	results := s.searchResults()
	if len(results) == 0 || s.searchSelected < 0 || s.searchSelected >= len(results) {
		return ducklord.RemoteSession{}, false
	}
	return results[s.searchSelected], true
}

func (s *tuiState) selectSessionKey(key string) bool {
	for index, session := range s.sessions {
		if sessionKey(session) == key {
			s.selected = index
			s.selectedKey = key
			return true
		}
	}
	return false
}

func (s *tuiState) sessionForKey(key string) (ducklord.RemoteSession, bool) {
	for _, session := range s.sessions {
		if sessionKey(session) == key {
			return session, true
		}
	}
	return ducklord.RemoteSession{}, false
}

func (s *tuiState) selectableSessionMatches(key string, generation uint64) bool {
	session, ok := s.sessionForKey(key)
	return ok && session.RuntimeGeneration == generation && canRead(session) && s.hostIsLive(session.Client)
}

func (s *tuiState) searchResultIsActivated(session ducklord.RemoteSession) bool {
	key := sessionKey(session)
	return s.searchActivatedKey == key && s.searchActivatedRevision == s.searchRevision &&
		s.searchActivatedGeneration == session.RuntimeGeneration && s.outputForKey == key && s.outputFresh
}

func (s *tuiState) searchResults() []ducklord.RemoteSession {
	terms := strings.Fields(strings.ToLower(modalDisplayText(s.searchQuery)))
	if len(terms) == 0 {
		return append([]ducklord.RemoteSession(nil), s.sessions...)
	}
	results := make([]ducklord.RemoteSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		fields := strings.ToLower(strings.Join([]string{
			modalDisplayText(session.Name), modalDisplayText(session.ProjectName), modalDisplayText(session.Client),
			modalDisplayText(s.organizationGroupLabel(session)), modalDisplayText(session.Kind), modalDisplayText(session.AgentType), modalDisplayText(sessionTypeLabel(session)),
		}, "\n"))
		matched := true
		for _, term := range terms {
			if !strings.Contains(fields, term) {
				matched = false
				break
			}
		}
		if matched {
			results = append(results, session)
		}
	}
	return results
}

func (s *tuiState) syncSearchSelection() {
	results := s.searchResults()
	if len(results) == 0 {
		s.searchSelected = 0
		s.searchSelectedKey = ""
		return
	}
	if s.searchSelectedKey != "" {
		for index, session := range results {
			if sessionKey(session) == s.searchSelectedKey {
				s.searchSelected = index
				return
			}
		}
	}
	s.searchSelected = min(max(s.searchSelected, 0), len(results)-1)
	s.searchSelectedKey = sessionKey(results[s.searchSelected])
}

func (s *tuiState) handleSearchInput(input []byte) string {
	results := s.searchResults()
	switch string(input) {
	case "\x1b":
		return "cancel"
	case "\x1b[B":
		if s.searchSelected < len(results)-1 {
			s.searchSelected++
			s.searchSelectedKey = sessionKey(results[s.searchSelected])
		}
	case "\x1b[A":
		if s.searchSelected > 0 {
			s.searchSelected--
			s.searchSelectedKey = sessionKey(results[s.searchSelected])
		}
	case "\r", "\n":
		if len(results) > 0 {
			return "activate"
		}
		return ""
	case "\b", "\x7f":
		if s.searchQuery != "" {
			runes := []rune(s.searchQuery)
			s.searchQuery = string(runes[:len(runes)-1])
			s.searchRevision++
			s.searchSelected = 0
			s.searchSelectedKey = ""
			s.searchActivatedKey = ""
			s.searchActivatedGeneration = 0
		}
	default:
		if utf8.Valid(input) {
			for _, r := range string(input) {
				if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && len(s.searchQuery)+utf8.RuneLen(r) <= 1024 {
					s.searchQuery += string(r)
				}
			}
			s.searchRevision++
			s.searchSelected = 0
			s.searchSelectedKey = ""
			s.searchActivatedKey = ""
			s.searchActivatedGeneration = 0
		}
	}
	if string(input) == "\x1b[A" || string(input) == "\x1b[B" {
		s.searchRevision++
		s.searchActivatedKey = ""
		s.searchActivatedRevision = 0
		s.searchActivatedGeneration = 0
	}
	s.syncSearchSelection()
	return ""
}

func (s *tuiState) handleInput(b []byte) string {
	text := string(b)
	if !strings.HasPrefix(text, "\x1b[<") {
		s.dragSession, s.dragTargetGroup, s.dragTargetSession = ducklord.SessionIdentity{}, "", ducklord.SessionIdentity{}
	}
	switch {
	case s.shortcut("quit", text) || text == "\x03":
		return "quit"
	case s.shortcut("help", text):
		return "help"
	case s.shortcut("shortcut_settings", text) && !s.hostScoped:
		return "shortcut-settings"
	case s.shortcut("notification_settings", text) && !s.hostScoped:
		return "notification-settings"
	case s.shortcut("host_actions", text):
		return "host-actions"
	case s.selectedGroupID != "" && (s.sessionShortcut(text) || s.shortcut("list_reorder_up", text) || s.shortcut("list_reorder_down", text)):
		s.outputErr = "select a session row for this action"
		return "group-select"
	case !s.workspacePreview && text == "\x1b[D":
		if s.selectedGroupID == "" {
			s.selectedGroupID = s.organizationGroupID(s.currentSession())
			return "group-select"
		}
		s.setSelectedGroupCollapsed(true)
		return "group-toggle"
	case !s.workspacePreview && text == "\x1b[C":
		if s.selectedGroupID != "" {
			s.setSelectedGroupCollapsed(false)
			return "group-toggle"
		}
		return "group-select"
	case s.shortcut("refresh", text):
		return "refresh"
	case text == "f" && !s.focused:
		return "project-files"
	case s.shortcut("list_search", text):
		return "search"
	case s.workspacePreview && s.shortcut("list_sort", text):
		return "quick-sort"
	case s.workspacePreview && s.shortcut("list_sort_direction", text):
		return "quick-sort-direction"
	case !s.workspacePreview && s.shortcut("list_organize", text):
		return "organize"
	case !s.workspacePreview && s.shortcut("list_groups", text):
		return "groups"
	case !s.workspacePreview && s.shortcut("list_reorder_up", text):
		return "reorder-up"
	case !s.workspacePreview && s.shortcut("list_reorder_down", text):
		return "reorder-down"
	case s.shortcut("session_yield", text):
		return "yield"
	case s.shortcut("session_yield_wait", text):
		return "yield-wait"
	case s.shortcut("session_end", text):
		return "end"
	case s.shortcut("session_restart", text):
		return "restart"
	case s.shortcut("session_destroy", text):
		return "destroy"
	case s.shortcut("session_create", text) && !s.hostScoped:
		return "new"
	case s.shortcut("session_notifications", text):
		return "notifications"
	case s.shortcut("pty_copy", text):
		return "copy-mode"
	case s.shortcut("session_actions", text):
		return "actions"
	case s.shortcut("host_add", text) && !s.hostScoped:
		return "add-client"
	case s.shortcut("host_remove", text) && !s.hostScoped:
		return "remove-client"
	case text == "\r":
		if s.selectedGroupID != "" {
			s.toggleSelectedGroup()
			return "group-toggle"
		}
		return "attach"
	case text == "j" || text == "\x1b[B":
		if s.moveListSelection(1) {
			if s.selectedGroupID != "" {
				return "group-select"
			}
			return "select"
		}
	case text == "k" || text == "\x1b[A":
		if s.moveListSelection(-1) {
			if s.selectedGroupID != "" {
				return "group-select"
			}
			return "select"
		}
	case strings.HasPrefix(text, "\x1b[<32;") && strings.HasSuffix(text, "M"):
		if row, ok := s.listRowForMouse(text); ok && s.dragSession.Key() != "" {
			s.dragTargetGroup, s.dragTargetSession = row.groupID, ducklord.SessionIdentity{}
			if row.isGroup {
				s.selectedGroupID = row.groupID
				return "group-select"
			}
			s.dragTargetSession, _ = ducklord.IdentityFromSession(s.sessions[row.sessionIndex])
			s.selectedGroupID, s.selected = "", row.sessionIndex
			return "select"
		}
	case strings.HasPrefix(text, "\x1b[<0;") && strings.HasSuffix(text, "m"):
		if row, ok := s.listRowForMouse(text); ok && row.groupID == s.dragTargetGroup && s.dragSession.Key() != "" {
			if !row.isGroup {
				released, valid := ducklord.IdentityFromSession(s.sessions[row.sessionIndex])
				if !valid || released != s.dragTargetSession {
					s.dragSession, s.dragTargetGroup, s.dragTargetSession = ducklord.SessionIdentity{}, "", ducklord.SessionIdentity{}
					s.outputErr = "session list changed during drag; try again"
					return "group-drop"
				}
			}
			if row.isGroup && s.organizationMode() != ducklord.OrganizationCustom {
				s.dragSession, s.dragTargetGroup, s.dragTargetSession = ducklord.SessionIdentity{}, "", ducklord.SessionIdentity{}
				s.outputErr = "drop onto a session to reorder inside this group"
				return "group-drop"
			}
			before := ducklord.SessionIdentity{}
			if !row.isGroup {
				before, _ = ducklord.IdentityFromSession(s.sessions[row.sessionIndex])
			}
			s.moveDraggedSessionBefore(row.groupID, before)
			return "group-drop"
		}
		s.dragSession, s.dragTargetGroup, s.dragTargetSession = ducklord.SessionIdentity{}, "", ducklord.SessionIdentity{}
	case strings.HasPrefix(text, "\x1b[<0;") && strings.HasSuffix(text, "M"):
		if session, ok := s.selectListRowForMouse(text); ok {
			if session {
				s.selectedKey = s.currentKey()
				s.dragSession, _ = ducklord.IdentityFromSession(s.currentSession())
				return "select"
			}
			return "group-select"
		}
		if s.contentPaneClicked(text) {
			return "attach"
		}
	case strings.HasPrefix(text, "\x1b[<2;") && strings.HasSuffix(text, "M"):
		if session, ok := s.selectListRowForMouse(text); ok {
			if !session {
				s.toggleSelectedGroup()
				return "group-toggle"
			}
			s.selectedKey = s.currentKey()
		}
		return "attach"
	}
	return ""
}

func copyModeExitInput(input []byte) bool {
	text := string(input)
	return text == "q" || text == "v" || text == "\x03" || text == "\x1b"
}

func (s *tuiState) enterCopyMode(out io.Writer) {
	if s.copyMode {
		return
	}
	s.copyMode = true
	if s.terminal != nil {
		s.copyTerminal, _ = ducklord.NewTerminalFromState(s.terminal.SnapshotState(), ducklord.DefaultTerminalScrollback)
	}
	// Native terminal selection requires all mouse reporting to be disabled.
	fmt.Fprint(out, "\033[?1000l\033[?1002l\033[?1003l\033[?1006l")
	s.renderCopyMode(out)
}

func (s *tuiState) renderCopyMode(out io.Writer) {
	if !s.copyMode {
		return
	}
	s.copyMode = false
	s.copyRendering = true
	s.render(out)
	s.copyRendering = false
	s.copyMode = true
}

func (s *tuiState) exitCopyMode(out io.Writer) {
	if !s.copyMode {
		return
	}
	s.copyMode = false
	s.copyTerminal = nil
	// Restore the encoding before event tracking so no mouse report can arrive
	// in an unexpected format during the transition.
	fmt.Fprint(out, "\033[?1000l\033[?1006h\033[?1002h")
}

func (s *tuiState) viewportTerminal() *ducklord.Terminal {
	if s.copyTerminal != nil {
		return s.copyTerminal
	}
	return s.terminal
}

func (s *tuiState) scrollPTY(delta int) {
	terminal := s.viewportTerminal()
	if terminal == nil {
		return
	}
	s.ptyScrollOffset = min(len(terminal.Scrollback), max(0, s.ptyScrollOffset+delta))
}

func (s *tuiState) beginAddClient() {
	hosts, err := ducklord.LoadSSHConfigHosts("")
	if err != nil {
		s.outputErr = err.Error()
		return
	}
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
		installedPath, err := runner.InstallDucklion(ctx, client, "", "")
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
	return s.projectFiles.open || s.commandPaletteMode || s.workspacePaneMode || s.shortcutMode || s.notificationConfigMode || s.hostMenuMode || s.searchMode || s.terminalSearchMode || s.terminalBookmarkMode || s.terminalBookmarkListMode || s.addClientMode || s.removeClientMode || s.newSessionMode || s.notificationMode || s.groupMenu || s.actionMenu || s.sessionRenameMode || s.lifecycleConfirm != ""
}

// workspaceControlMayAccept gates asynchronous PTY ownership. Workspace
// modals keep ownership until they close and the deferred focus lease runs.
func (s *tuiState) workspaceControlMayAccept() bool {
	return !s.focused && !s.workspacePaneMode && !s.newSessionMode && !s.helpMode && !s.projectFiles.open
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

func sessionKey(sess ducklord.RemoteSession) string {
	if sess.InstanceID != "" && sess.SessionID != "" {
		return sess.Client + "/" + sess.InstanceID + "/" + sess.SessionID
	}
	return sess.Group + "/" + sess.Client + "/" + sess.Name
}

func terminalOutputKey(sess ducklord.RemoteSession) (ducklord.OutputKey, bool) {
	key := ducklord.OutputKey{ClientKey: sess.Client, InstanceID: sess.InstanceID, SessionID: sess.SessionID}
	return key, key.ClientKey != "" && key.InstanceID != "" && key.SessionID != "" && sess.RuntimeGeneration != 0
}

func canAttach(sess ducklord.RemoteSession) bool {
	return canRead(sess) && sess.Status == "running"
}

func canRead(sess ducklord.RemoteSession) bool {
	return sess.Error == "" && sess.Status != "error" && sess.Name != "(offline)"
}

func (s *tuiState) selectListRowForMouse(seq string) (bool, bool) {
	selected, ok := s.listRowForMouse(seq)
	if !ok {
		return false, false
	}
	if selected.isGroup {
		s.selectedGroupID = selected.groupID
		return false, true
	}
	s.selectedGroupID = ""
	s.selected = selected.sessionIndex
	return true, true
}

func (s *tuiState) listRowForMouse(seq string) (sessionListRow, bool) {
	if !strings.HasPrefix(seq, "\x1b[<") || len(seq) < 7 {
		return sessionListRow{}, false
	}
	parts := strings.FieldsFunc(strings.TrimPrefix(seq, "\x1b[<"), func(r rune) bool { return r == ';' || r == 'M' || r == 'm' })
	if len(parts) != 3 {
		return sessionListRow{}, false
	}
	x, err := strconv.Atoi(parts[1])
	if err != nil {
		return sessionListRow{}, false
	}
	width, _ := terminalSize()
	layout := calculateTUILayout(width, s.focused, s.listPaneWidth, s.autoHideList)
	if !layout.showList || x > layout.menuWidth+2 {
		return sessionListRow{}, false
	}
	y, err := strconv.Atoi(parts[2])
	if err != nil {
		return sessionListRow{}, false
	}
	row := y - 5
	rows := s.sessionListRows()
	if row < 0 || row >= len(rows) {
		return sessionListRow{}, false
	}
	return rows[row], true
}

// sessionIndexForMouse remains the non-mutating compatibility helper used by
// tests and callers that only care about session rows.
func (s *tuiState) sessionIndexForMouse(seq string) (int, bool) {
	oldSelected, oldGroup := s.selected, s.selectedGroupID
	session, ok := s.selectListRowForMouse(seq)
	index := s.selected
	s.selected, s.selectedGroupID = oldSelected, oldGroup
	return index, ok && session
}

func (s *tuiState) contentPaneClicked(seq string) bool {
	if !strings.HasPrefix(seq, "\x1b[<0;") || !strings.HasSuffix(seq, "M") {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b[<0;"), "M"), ";")
	if len(parts) != 2 {
		return false
	}
	x, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	y, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	width, height := terminalSize()
	layout := calculateTUILayout(width, s.focused, s.listPaneWidth, s.autoHideList)
	contentStart := 1
	if layout.showList {
		contentStart = layout.menuWidth + 3
	}
	return x >= contentStart && x <= width && y >= 4 && y <= height
}

func parseSGRMouse(seq string) (button, x, y int, ok bool) {
	if len(seq) > 64 || !strings.HasPrefix(seq, "\x1b[<") || len(seq) < 7 || seq[len(seq)-1] != 'M' && seq[len(seq)-1] != 'm' {
		return 0, 0, 0, false
	}
	parts := strings.Split(seq[3:len(seq)-1], ";")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	values := []*int{&button, &x, &y}
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 10000 {
			return 0, 0, 0, false
		}
		*values[index] = value
	}
	return button, x, y, true
}

func (s *tuiState) contentPanePoint(x, y int) bool {
	width, height := terminalSize()
	if s.workspacePreview {
		pane := ducklord.CalculateWorkspaceGeometry(width, height, 4).Terminal
		return x >= pane.X && x < pane.X+pane.Width && y >= pane.Y+2 && y < pane.Y+pane.Height
	}
	layout := calculateTUILayout(width, s.focused, s.listPaneWidth, s.autoHideList)
	contentStart := 1
	if layout.showList {
		contentStart = layout.menuWidth + 3
	}
	return x >= contentStart && x <= width && y >= 5 && y <= height
}

func readInput(ctx context.Context, ch chan<- []byte, ready chan<- struct{}) {
	raw := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(raw)
				return
			}
			select {
			case raw <- append([]byte(nil), buf[:n]...):
			case <-ctx.Done():
				return
			}
		}
	}()
	var pending []byte
	var escapeTimer *time.Timer
	var escapeTimeout <-chan time.Time
	stopEscapeTimer := func() {
		if escapeTimer != nil && !escapeTimer.Stop() {
			select {
			case <-escapeTimer.C:
			default:
			}
		}
		escapeTimeout = nil
	}
	emit := func(event []byte) bool {
		select {
		case ready <- struct{}{}:
		default:
		}
		select {
		case ch <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		select {
		case <-ctx.Done():
			stopEscapeTimer()
			return
		case data, open := <-raw:
			if !open {
				return
			}
			stopEscapeTimer()
			pending = append(pending, data...)
		case <-escapeTimeout:
			escapeTimeout = nil
			if len(pending) > 0 && pending[0] == 0x1b {
				if !emit([]byte{0x1b}) {
					return
				}
				pending = pending[1:]
			}
		}
		for len(pending) > 0 {
			event, rest, ok := nextInputEvent(pending)
			if !ok {
				pending = rest
				if len(pending) <= 2 && pending[0] == 0x1b {
					escapeTimer = time.NewTimer(inputEscapeAmbiguityTimeout)
					escapeTimeout = escapeTimer.C
				}
				break
			}
			pending = rest
			if !emit(event) {
				return
			}
		}
	}
}

func nextInputEvent(pending []byte) (event, rest []byte, ok bool) {
	if len(pending) == 0 {
		return nil, nil, false
	}
	if pending[0] != 0x1b {
		if pending[0] < utf8.RuneSelf {
			return append([]byte(nil), pending[:1]...), pending[1:], true
		}
		if !utf8.FullRune(pending) {
			return nil, pending, false
		}
		_, size := utf8.DecodeRune(pending)
		return append([]byte(nil), pending[:size]...), pending[size:], true
	}
	if len(pending) == 1 || len(pending) == 2 && pending[1] == '[' {
		return nil, pending, false
	}
	if len(pending) >= 3 && pending[1] == '[' && (pending[2] == 'A' || pending[2] == 'B' || pending[2] == 'C' || pending[2] == 'D') {
		return append([]byte(nil), pending[:3]...), pending[3:], true
	}
	if len(pending) >= 3 && pending[1] == '[' && (pending[2] == '5' || pending[2] == '6') {
		if len(pending) == 3 {
			return nil, pending, false
		}
		if pending[3] == '~' {
			return append([]byte(nil), pending[:4]...), pending[4:], true
		}
	}
	if len(pending) >= 4 && pending[1] == '[' && pending[2] == '<' {
		for i := 3; i < len(pending); i++ {
			if pending[i] == 'M' || pending[i] == 'm' {
				return append([]byte(nil), pending[:i+1]...), pending[i+1:], true
			}
		}
		if len(pending) > 64 {
			return append([]byte(nil), pending...), nil, true
		}
		return nil, pending, false
	}
	// Preserve Alt/Meta key chords as one event. In particular, this prevents
	// the Esc prefix from closing copy mode and its suffix from becoming a
	// normal-mode shortcut such as q or c.
	if len(pending) >= 2 && pending[1] != '[' {
		if pending[1] < utf8.RuneSelf {
			// C0 controls are complete input events in their own right. In
			// particular, do not merge Esc + Ctrl-] into an Alt-style chord:
			// after a modal consumes Esc, Ctrl-] must still unfocus the PTY.
			if pending[1] < 0x20 || pending[1] == 0x7f {
				return append([]byte(nil), pending[:1]...), pending[1:], true
			}
			return append([]byte(nil), pending[:2]...), pending[2:], true
		}
		if !utf8.FullRune(pending[1:]) {
			return nil, pending, false
		}
		_, size := utf8.DecodeRune(pending[1:])
		return append([]byte(nil), pending[:1+size]...), pending[1+size:], true
	}
	return append([]byte(nil), pending[:1]...), pending[1:], true
}

func superviseAttach(ctx context.Context, id int, session *ducklord.AttachSession, out chan<- attachOutputEvent) {
	stdoutDone := make(chan error, 1)
	go readAttachOutput(ctx, id, session, out, stdoutDone)
	var stdoutErr error
	var commandErr error
	stdoutOpen := true
	commandOpen := session.Done != nil
	for stdoutOpen || commandOpen {
		select {
		case err := <-stdoutDone:
			stdoutErr = err
			stdoutOpen = false
			_ = session.Stdin.Close()
		case err := <-session.Done:
			commandErr = err
			commandOpen = false
		case <-ctx.Done():
			return
		}
	}
	if commandErr != nil {
		select {
		case out <- attachOutputEvent{id: id, done: true, err: commandErr}:
		case <-ctx.Done():
		}
		return
	}
	select {
	case out <- attachOutputEvent{id: id, done: true, err: stdoutErr}:
	case <-ctx.Done():
	}
}

func runResizeWorker(ctx context.Context, requests <-chan resizeRequest, results chan<- resizeDoneEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-requests:
			// Collapse queued SIGWINCH bursts before starting the single RPC.
			for {
				select {
				case newer := <-requests:
					request = newer
				default:
					goto resize
				}
			}
		resize:
			barrier, err := request.resize(request.rows, request.cols)
			select {
			case results <- resizeDoneEvent{id: request.id, rows: request.rows, cols: request.cols, barrier: barrier, err: err}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func readAttachOutput(ctx context.Context, id int, session *ducklord.AttachSession, out chan<- attachOutputEvent, done chan<- error) {
	buf := make([]byte, 4096)
	for {
		n, err := session.Stdout.Read(buf)
		if n > 0 {
			offset := uint64(0)
			if session.OutputOffset != nil {
				offset = session.OutputOffset()
			}
			startOffset := offset
			if offset >= uint64(n) {
				startOffset = offset - uint64(n)
			}
			select {
			case out <- attachOutputEvent{id: id, text: string(buf[:n]), runtimeGeneration: session.RuntimeGeneration, startOffset: startOffset, outputOffset: offset}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			select {
			case done <- err:
			case <-ctx.Done():
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func appendOutputText(current, chunk string, maxLines int) string {
	const maxOutputBytes = 1 << 20
	if chunk == "" {
		return current
	}
	text := applyInteractiveText(current, chunk)
	if len(text) > maxOutputBytes {
		text = strings.ToValidUTF8(text[len(text)-maxOutputBytes:], " ")
	}
	endsWithNewline := strings.HasSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	text = strings.Join(tailLines(lines, maxLines), "\n")
	if endsWithNewline {
		text += "\n"
	}
	return text
}

func applyInteractiveText(current, chunk string) string {
	current = strings.ToValidUTF8(current, " ")
	chunk = strings.ToValidUTF8(chunk, " ")
	out := []rune(current)
	for _, r := range chunk {
		switch {
		case r == '\b' || r == 0x7f:
			if len(out) > 0 && out[len(out)-1] != '\n' {
				out = out[:len(out)-1]
			}
		case r == '\n' || r == '\t':
			out = append(out, r)
		case r == '\r':
			if len(out) == 0 || out[len(out)-1] != '\n' {
				out = append(out, '\n')
			}
		case r < 0x20 || r >= 0x80 && r <= 0x9f:
			out = append(out, ' ')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

func sanitizeTerminalText(s string) string {
	s = strings.ToValidUTF8(s, " ")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
			b.WriteRune('\n')
		case r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f || unicode.Is(unicode.Cf, r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func displayField(s string) string {
	return sanitizeTerminalText(s)
}

func sessionTypeLabel(session ducklord.RemoteSession) string {
	if session.Kind == string(model.KindShell) {
		if session.DetectedForeground == "codex" || session.DetectedForeground == "claude" {
			return session.DetectedForeground + "?" // process detection is advisory, not an exact task state
		}
		if session.DetectedForeground == "other_agent" {
			return "other agent?" // conservative executable detection, not an exact task state
		}
		return "shell"
	}
	if session.AgentType == "" {
		return "agent"
	}
	return "agent:" + session.AgentType
}

func sessionNeedsAttention(session ducklord.RemoteSession) bool {
	return session.AdapterState == string(model.AdapterUnhealthy) ||
		session.Status == string(model.StatusStopped) && (session.ExitSuccess == nil || !*session.ExitSuccess)
}

func formatByteCount(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f KiB", float64(bytes)/1024)
}

func formatRetainedUntil(timestampMS int64) string {
	if timestampMS <= 0 {
		return "unknown"
	}
	return time.UnixMilli(timestampMS).Local().Format("Jan 02 15:04")
}

func tailLines(lines []string, max int) []string {
	if max <= 0 {
		return nil
	}
	if len(lines) <= max {
		return lines
	}
	return lines[len(lines)-max:]
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if modalCellWidth(s) <= n {
		return s
	}
	return modalCellTruncate(s, n)
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, `ducklord - remote agent session TUI

Usage:
  ducklord [--name <owner>] tui [--config <path>] [--refresh 2s]
  ducklord clients [--config <path>]
  ducklord ssh-hosts [--config-file <ssh_config>]
  ducklord import-ssh-hosts [--ssh-config <ssh_config>] [--config <path>]
  ducklord sessions <client> [--config <path>]
  ducklord bookmarks <client> [--config <path>] (alias: projects)
  ducklord hook-status <client> <codex|claude> [--config <path>]
  ducklord agents <client> <directory-path> [--config <path>]
  ducklord probe <client> [--config <path>]
  ducklord install-ducklion <client> [--source <path>] [--dest <remote-path>] [--config <path>]
  ducklord tui [--config <path>] [--name <owner>] [--refresh 2s]
  ducklord attach-host <client> [--config <path>]
  ducklord attach <client> <session> [--config <path>]
  ducklord read <client> <session> [--lines N] [--config <path>]
  ducklord retained <client> [--config <path>]
  ducklord read-retained <client> <session-id> <generation> [--lines N] [--config <path>]
  ducklord send <client> <session> <text> [--config <path>]
  ducklord start <client> --name <name> [--kind shell | --agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
  ducklord stop <client> <session> [--config <path>]
  ducklord end <client> <session> [-w|--wait|-f|--force] [--config <path>]
  ducklord restart <client> <session> [-f|--force] [--config <path>]
  ducklord destroy <client> <session> [-w|--wait|-f|--force] [--config <path>]
  ducklord yield <client> <session> [-w|--wait] [--config <path>]
  ducklord version`)
}
