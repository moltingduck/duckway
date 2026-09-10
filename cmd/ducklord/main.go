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
	Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error)
	SuggestProjectPaths(context.Context, ducklord.Client, string) ([]string, error)
	EnsureDirectory(context.Context, ducklord.Client, string, bool) (ducklord.RemoteDirectoryStatus, error)
	AddProject(context.Context, ducklord.Client, string, string) (ducklord.RemoteProject, error)
	Agents(context.Context, ducklord.Client, string) ([]ducklord.RemoteAgent, error)
	ProbeDucklion(context.Context, ducklord.Client) (ducklord.DucklionProbe, error)
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
}

type pathSuggestionEvent struct {
	id, generation          uint64
	client, instance, query string
	paths                   []string
	err                     error
}

type lifecycleDoneEvent struct {
	operation protocol.SessionLifecycleOperation
	result    protocol.SessionLifecycleResult
	err       error
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
	defer runner.Close()
	if err := run(os.Args[1:], os.Stdout, runner); err != nil {
		log.Fatal(err)
	}
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
	case "projects":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord projects <client> [--config <path>]")
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
	case "agents":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord agents <client> <project-path> [--config <path>]")
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
		fmt.Fprintln(out, "No remote projects.")
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
		fmt.Fprintln(out, "No available agents for this project.")
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
	cfg                       *ducklord.Config
	cfgPath                   string
	runner                    remoteRunner
	refresh                   time.Duration
	sessions                  []ducklord.RemoteSession
	selected                  int
	selectedGroupID           string
	dragSession               ducklord.SessionIdentity
	dragTargetGroup           string
	dragTargetSession         ducklord.SessionIdentity
	hashes                    map[string]string
	selectedKey               string
	outputText                string
	outputErr                 string
	localWarning              string
	resizeStatus              string
	outputForKey              string
	outputStale               bool
	outputFresh               bool
	outputReconnecting        bool
	terminal                  *ducklord.Terminal
	terminalGeneration        uint64
	terminalOffset            uint64
	terminalCursorValid       bool
	activeAttachKey           string
	activeAttachFresh         bool
	pendingAttachKey          string
	snapshotStore             ducklord.SnapshotStore
	activityStore             ducklord.ActivityStateStore
	activityState             *ducklord.ActivityState
	focused                   bool
	copyMode                  bool
	newSessionMode            bool
	newSessionClient          string
	newSessionLine            string
	newSessionSelected        int
	newSessionErr             string
	newSessionStarting        bool
	newSessionStartGeneration uint64
	newSessionStartInstance   string
	newSessionDiscovering     bool
	newSessionRequestID       uint64
	newSessionCancel          context.CancelFunc
	createWorkers             sync.WaitGroup
	newSessionStep            string
	newSessionKind            model.SessionKind
	newSessionAgent           string
	newSessionCommand         []string
	newSessionProjects        []ducklord.RemoteProject
	newSessionProject         ducklord.RemoteProject
	newSessionAgents          []ducklord.RemoteAgent
	newSessionCWD             string
	newSessionPathSuggestions []string
	newSessionPathSelected    int
	newSessionPathRequestID   uint64
	newSessionPathCancel      context.CancelFunc
	newSessionPathBusy        bool
	newSessionPathCompletion  string
	addClientMode             bool
	addClientLine             string
	addClientSelected         int
	addClientErr              string
	addClientHosts            []ducklord.SSHHost
	addClientBusy             bool
	addClientRequestID        uint64
	addClientCancel           context.CancelFunc
	removeClientMode          bool
	removeClientSelected      int
	removeClientConfirm       string
	hostScoped                bool
	ownerName                 string
	listPaneWidth             int
	autoHideList              bool
	hostSync                  map[string]ducklord.SessionUpdate
	eventDriven               bool
	notificationMode          bool
	notificationIndex         int
	notificationStaged        map[model.NotificationCategory]bool
	notificationTarget        ducklord.RemoteSession
	actionMenu                bool
	actionIndex               int
	actionTarget              ducklord.RemoteSession
	actionOperation           protocol.SessionLifecycleOperation
	actionMode                protocol.SessionLifecycleMode
	lifecycleConfirm          protocol.SessionLifecycleOperation
	lifecycleReturnToAction   bool
	lifecycleTarget           ducklord.RemoteSession
	lifecycleMode             protocol.SessionLifecycleMode
	lifecycleBusy             bool
	groupMenu                 bool
	groupMenuStep             string
	groupMenuAction           string
	groupMenuIndex            int
	groupMenuLine             string
	groupMenuErr              string
	groupMenuTarget           string
	groupMenuSession          ducklord.SessionIdentity
	pooledOutput              bool
	searchMode                bool
	searchQuery               string
	searchSelected            int
	searchSelectedKey         string
	searchRevision            uint64
	searchActivatedKey        string
	searchActivatedRevision   uint64
	searchActivatedGeneration uint64
	searchErr                 string
	searchPendingRequestID    uint64
	searchPendingKey          string
	searchPendingGeneration   uint64
	searchPendingRevision     uint64
}

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
	defer fmt.Print("\033[?1006l\033[?1002l\033[?25h\033[?1049l")

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
	state := &tuiState{cfg: cfg, cfgPath: cfgPath, runner: runner, refresh: refresh, hashes: map[string]string{}, hostScoped: hostScoped, ownerName: owner,
		snapshotStore: ducklord.SnapshotStore{}, activityStore: activityStore, activityState: activityState, hostSync: make(map[string]ducklord.SessionUpdate), localWarning: stateWarning,
		listPaneWidth: cfg.SessionListPaneWidth(), autoHideList: cfg.SessionListAutoHide()}
	var outputManager *ducklord.TerminalOutputManager
	var outputEvents <-chan ducklord.TerminalOutputEvent
	if source, ok := runner.(ducklord.TerminalOutputSource); ok {
		outputManager, err = ducklord.NewTerminalOutputManager(ctx, cfg.RawOutputSubscriptionLimit(), source, state.snapshotStore)
		if err != nil {
			return err
		}
		outputEvents = outputManager.Events()
		state.pooledOutput = true
		defer outputManager.Close()
	} else {
		defer state.saveCurrentSnapshot()
	}
	sessionUpdates := make(chan ducklord.SessionUpdate, 32)
	watchedClients := make(map[string]bool)
	watcher, eventDriven := runner.(interface {
		WatchSessionUpdates(context.Context, ducklord.Client) <-chan ducklord.SessionUpdate
	})
	watchClient := func(client ducklord.Client) {
		key := bridgeKeyForTUI(client)
		if !eventDriven || watchedClients[key] {
			return
		}
		watchedClients[key] = true
		state.eventDriven = true
		updates := watcher.WatchSessionUpdates(ctx, client)
		go func() {
			for update := range updates {
				select {
				case sessionUpdates <- update:
				case <-ctx.Done():
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
	attachOut := make(chan attachOutputEvent, 32)
	attachOutputSource := (<-chan attachOutputEvent)(attachOut)
	resizeRequests := make(chan resizeRequest, 1)
	resizeResults := make(chan resizeDoneEvent, 1)
	startDone := make(chan startDoneEvent, 1)
	createDone := make(chan createDiscoveryEvent, 1)
	pathSuggestionsDone := make(chan pathSuggestionEvent, 1)
	addClientDone := make(chan addClientDoneEvent, 1)
	lifecycleDone := make(chan lifecycleDoneEvent, 1)
	previewDone := make(chan previewOutputEvent, 1)
	searchActivationTimeout := make(chan uint64, 1)
	var previewID uint64
	previewInFlight := false
	previewQueued := false
	var previewCancel context.CancelFunc
	var outputRequestID uint64
	var reconnectRequestID uint64
	var activeOutputEvent ducklord.TerminalOutputEvent
	selectPooledOutput := func() {
		if outputManager == nil || len(state.sessions) == 0 {
			return
		}
		reconnectRequestID = 0
		state.outputReconnecting = false
		sess := state.currentSession()
		key, keyOK := terminalOutputKey(sess)
		selectedKey := sessionKey(sess)
		if state.outputForKey != selectedKey || !keyOK || activeOutputEvent.Key != key || activeOutputEvent.Revision.RuntimeGeneration != sess.RuntimeGeneration {
			state.outputText = ""
			state.terminal = nil
			activeOutputEvent = ducklord.TerminalOutputEvent{}
			state.outputForKey = selectedKey
		}
		if sess.Client == "" || sess.InstanceID == "" || sess.SessionID == "" || sess.RuntimeGeneration == 0 || !canRead(sess) || !state.hostIsLive(sess.Client) {
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
	controlOpened := make(chan controlOpenEvent, 1)
	controlID := 0
	var controlOpenCancel context.CancelFunc
	var attachCancel context.CancelFunc
	attachID := 0
	attachCanResize := false
	attachInitialResizeQueued := false
	var attachReplayEndOffset uint64
	var pendingFramebufferResize *resizeDoneEvent
	resizeInFlight := false
	var queuedResize *[2]uint16
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
		expectedKey, keyOK := terminalOutputKey(state.currentSession())
		if outputManager != nil && control != nil && control.ResizeBarrier != nil && activeOutputEvent.Lease != 0 && keyOK && activeOutputEvent.Key == expectedKey &&
			activeOutputEvent.Revision.RuntimeGeneration == control.RuntimeGeneration && control.RuntimeGeneration == state.currentSession().RuntimeGeneration {
			resize = func(rows, cols uint16) (uint64, error) {
				return outputManager.Resize(activeOutputEvent, rows, cols, control.ResizeBarrier)
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
	finishAttach := func(event attachOutputEvent) {
		attachID++
		resizeInFlight = false
		pendingFramebufferResize = nil
		queuedResize = nil
		bufferedAttach = nil
		bufferedAttachBytes = 0
		attachOutputSource = attachOut
		wasFocused := state.focused
		state.focused = false
		state.clearAttachIdentity()
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
	go readInput(ctx, input)
	for {
		select {
		case <-ctx.Done():
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
				watchAllClients()
			}
			state.render(os.Stdout)
		case <-ticker.C:
			if !state.focused && !state.newSessionMode && !state.searchMode {
				if !state.eventDriven {
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
		case event := <-outputEvents:
			expectedSession := state.currentSession()
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
			state.terminal = terminal
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
			if state.sessionFreshlyDisplayed(state.currentSession()) {
				state.markActivitySeen(state.currentSession())
			}
			if state.focused && control != nil && !attachInitialResizeQueued && resizeCapability() != nil {
				attachInitialResizeQueued = true
				queueResize()
			}
			state.render(os.Stdout)
		case opened := <-controlOpened:
			if opened.id != controlID || opened.key != state.activeAttachKey || opened.control != nil && !controlMatchesSession(opened.control, state.currentSession(), state.ownerName) {
				if opened.control != nil {
					_ = opened.control.Stdin.Close()
				}
				continue
			}
			controlOpenCancel = nil
			if opened.err != nil {
				state.outputErr = sanitizeTerminalText(opened.err.Error())
				state.clearAttachIdentity()
				state.render(os.Stdout)
				continue
			}
			control = opened.control
			controlDone = control.Done
			state.focused = true
			attachCanResize = control.ResizeBarrier != nil
			state.outputErr = ""
			if resizeCapability() != nil {
				attachInitialResizeQueued = true
				queueResize()
			}
			state.render(os.Stdout)
		case controlErr := <-controlDone:
			controlDone = nil
			control = nil
			attachCanResize = false
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
		case update := <-sessionUpdates:
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
			if state.invalidateCreateStart(update) {
				if startCancel != nil {
					startCancel()
					startCancel = nil
				}
				startID++ // fence a completion racing with cancellation
			}
			state.applySessionUpdate(update)
			if outputManager != nil {
				if previousInstance != "" && update.InstanceID != "" && update.InstanceID != previousInstance {
					outputManager.ForgetHost(update.Client, previousInstance)
				} else if previousHost.InstanceID != "" && previousHost.State == "live" && update.State != "live" {
					outputManager.SyncHost(update.Client, previousHost.InstanceID, false, nil)
				}
				if update.State == "live" && update.InstanceID != "" {
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
			}
			if control != nil && (!state.hostIsLive(state.currentSession().Client) || !controlMatchesSession(control, state.currentSession(), state.ownerName)) {
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
			if previousActive != "" && state.effectiveAttachKey() == "" {
				controlID++
				if control != nil {
					_ = control.Stdin.Close()
				}
				control, controlDone = nil, nil
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
			startCancel = nil
			state.completeNewSessionStart(ctx, result.client, result.sessionID, result.err)
			if outputManager != nil && result.err == nil {
				selectPooledOutput()
			}
			state.render(os.Stdout)
		case result := <-lifecycleDone:
			state.lifecycleBusy = false
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
		case chunk := <-attachOutputSource:
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
				state.render(os.Stdout)
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
			state.render(os.Stdout)
		case b := <-input:
			var selectedActionTarget *ducklord.RemoteSession
			if state.copyMode {
				if copyModeExitInput(b) {
					state.exitCopyMode(os.Stdout)
					if state.sessionFreshlyDisplayed(state.currentSession()) {
						state.markActivitySeen(state.currentSession())
					}
					state.render(os.Stdout)
				}
				continue
			}
			if state.focused {
				if !state.hostIsLive(state.currentSession().Client) {
					state.focused = false
					state.clearAttachIdentity()
					state.outputFresh = false
					state.outputErr = "host reconnecting; input was not sent"
					state.render(os.Stdout)
					continue
				}
				if isDetachInput(b) {
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
					attachCanResize = false
					attachInitialResizeQueued = false
					attachReplayEndOffset = 0
					attach = nil
					requestPreview(true)
					state.render(os.Stdout)
					continue
				}
				if control != nil && controlMatchesSession(control, state.currentSession(), state.ownerName) {
					_, _ = control.Stdin.Write(b)
				} else if control != nil {
					controlID++
					if controlOpenCancel != nil {
						controlOpenCancel()
					}
					_ = control.Stdin.Close()
					control, controlDone = nil, nil
					attachCanResize = false
					state.outputErr = "PTY control changed; input was not sent"
				} else if attach != nil {
					_, _ = attach.Stdin.Write(b)
				}
				continue
			}
			if (string(b) == "\x1b[D" || string(b) == "\x1b[C") && state.centralModalOpen() {
				state.render(os.Stdout)
				continue
			}
			if string(b) == "\x03" && state.centralModalOpen() {
				switch {
				case state.searchMode:
					state.closeSearch()
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
						state.selectSessionKey(key)
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
					} else if outputManager != nil {
						outputManager.ForgetHost(name, instance)
						selectPooledOutput()
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
				state.lifecycleBusy = true
				state.lifecycleConfirm = ""
				state.lifecycleTarget = ducklord.RemoteSession{}
				state.lifecycleMode = ""
				state.outputErr = string(operation) + " accepted; Ducklion will continue it across reconnects"
				go func() {
					result, err := runner.LifecycleSelected(ctx, client, sess, operation, mode)
					select {
					case lifecycleDone <- lifecycleDoneEvent{operation: operation, result: result, err: err}:
					case <-ctx.Done():
					}
				}()
				state.render(os.Stdout)
				continue
			}
			action := state.handleInput(b)
			switch action {
			case "quit":
				return nil
			case "refresh":
				state.refreshSessions(ctx)
				if state.activeAttachKey == "" {
					requestPreview(true)
				}
			case "organize":
				state.cycleOrganizationMode()
			case "groups":
				state.beginGroupMenu()
			case "reorder-up":
				state.moveSelectedSession(-1)
			case "reorder-down":
				state.moveSelectedSession(1)
			case "select":
				// List navigation owns the preview pane. Never leave a stale
				// attach identity pointing at the previously selected session.
				state.clearAttachIdentity()
				state.outputForKey = state.currentKey()
				state.outputText = ""
				state.terminal = nil
				state.outputErr = "loading live preview..."
				requestPreview(true)
			case "new":
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
				if len(state.sessions) == 0 {
					continue
				}
				s := state.sessions[state.selected]
				if !canAttach(s) || !state.hostIsLive(s.Client) {
					if !state.hostIsLive(s.Client) {
						state.outputErr = "host is reconnecting; PTY controls are disabled"
					}
					continue
				}
				c, err := mustClient(cfg, s.Client)
				if err != nil {
					return err
				}
				fencePreview()
				if outputManager != nil {
					state.focused = false
					state.activeAttachKey = sessionKey(s)
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
						break
					}
					controlRunner, ok := runner.(interface {
						OpenControlSession(context.Context, ducklord.Client, string) (*ducklord.ControlSession, error)
					})
					if !ok {
						state.outputErr = "PTY control is unavailable"
						break
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
				state.focused = true
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

func initialReplayCaughtUp(currentOffset, replayEndOffset uint64) bool {
	return currentOffset >= replayEndOffset
}

func (s *tuiState) refreshSessions(ctx context.Context) {
	oldKey := s.currentKey()
	var all []ducklord.RemoteSession
	for _, c := range s.cfg.Clients {
		sessions, err := s.runner.Sessions(ctx, c, 8)
		if err != nil {
			all = append(all, ducklord.RemoteSession{Client: c.Name, Group: c.Group, Name: "(offline)", Status: "error", Error: err.Error()})
			continue
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
		var changed bool
		all[i].Unread, changed = s.activity().Reconcile(all[i], s.sessionFreshlyDisplayed(all[i]))
		activityChanged = activityChanged || changed
	}
	s.sessions = all
	organizationChanged := s.applyOrganizationOrder()
	if activityChanged || organizationChanged {
		_ = s.saveActivityState()
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
}

func (s *tuiState) applySessionUpdate(update ducklord.SessionUpdate) {
	if update.Client == "" {
		return
	}
	if s.cfg != nil {
		if _, configured := s.cfg.Client(update.Client); !configured {
			return
		}
	}
	previous := s.hostSync[update.Client]
	if update.Generation < previous.Generation || update.Generation == previous.Generation && update.InstanceID == previous.InstanceID && update.Revision < previous.Revision {
		return
	}
	s.hostSync[update.Client] = update
	if s.newSessionMode && s.newSessionDiscovering && s.newSessionClient == update.Client &&
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
		if s.currentSession().Client == update.Client {
			s.outputFresh = false
		}
		return // retain the last authoritative rows while reconnecting
	}
	oldKey := s.currentKey()
	previousActivity := make(map[string]map[model.NotificationCategory]uint64)
	for _, session := range s.sessions {
		if identity, ok := ducklord.IdentityFromSession(session); session.Client == update.Client && ok {
			previousActivity[identity.Key()] = session.ActivitySequences
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
		if before, ok := previousActivity[identity.Key()]; identityOK && ok {
			if session.ActivitySequences[model.NotificationTaskFailed] > before[model.NotificationTaskFailed] && s.activity().Enabled(session.InstanceID, session.SessionID, model.NotificationTaskFailed) {
				s.outputErr = sanitizeTerminalText(session.Name) + ": agent turn failed"
			} else if session.ActivitySequences[model.NotificationTaskCompleted] > before[model.NotificationTaskCompleted] && s.activity().Enabled(session.InstanceID, session.SessionID, model.NotificationTaskCompleted) {
				s.outputErr = sanitizeTerminalText(session.Name) + ": agent turn completed"
			}
		}
		activeFresh := s.sessionFreshlyDisplayed(session)
		unread, changed := s.activity().Reconcile(session, activeFresh)
		session.Unread = unread
		activityChanged = activityChanged || changed
		if session.SessionID == update.ChangedSessionID && sessionKey(session) != oldKey && !activeFresh {
			session.Updated = true
		}
		all = append(all, session)
	}
	if activityChanged {
		_ = s.saveActivityState()
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
}

func (s *tuiState) effectiveAttachKey() string {
	if s.activeAttachKey != "" {
		return s.activeAttachKey
	}
	return s.pendingAttachKey
}

func (s *tuiState) clearAttachIdentity() {
	s.activeAttachKey = ""
	s.activeAttachFresh = false
	s.pendingAttachKey = ""
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
	return identityOK && !s.copyMode && !s.centralModalOpen() && s.outputFresh && !s.outputStale && s.terminal != nil &&
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
	group := s.organizationGroupID(selected)
	targetIndex := s.selected + direction
	for targetIndex >= 0 && targetIndex < len(s.sessions) && s.organizationGroupID(s.sessions[targetIndex]) != group {
		targetIndex += direction
	}
	if targetIndex < 0 || targetIndex >= len(s.sessions) {
		s.outputErr = "session is already at the group boundary"
		return
	}
	selectedIdentity, selectedOK := ducklord.IdentityFromSession(selected)
	targetIdentity, targetOK := ducklord.IdentityFromSession(s.sessions[targetIndex])
	if !selectedOK || !targetOK {
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
	key := sessionKey(selected)
	s.applyOrganizationOrder()
	s.restoreSelection(key)
	if err := s.saveActivityState(); err != nil {
		s.outputErr = "session order changed locally but was not saved: " + err.Error()
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
	if len(s.sessions) > 0 && s.selected >= 0 && s.selected < len(s.sessions) && s.sessions[s.selected].Client != "" {
		return s.sessions[s.selected].Client
	}
	if s.cfg != nil && len(s.cfg.Clients) > 0 {
		return s.cfg.Clients[0].Name
	}
	return ""
}

func (s *tuiState) selectSession(clientName, sessionID string) {
	for i, sess := range s.sessions {
		if sess.Client == clientName && strings.EqualFold(sess.SessionID, sessionID) {
			s.selected = i
			s.selectedKey = s.currentKey()
			return
		}
	}
}

func (s *tuiState) completeNewSessionStart(ctx context.Context, clientName, sessionID string, err error) {
	s.newSessionStarting = false
	if err != nil {
		s.newSessionErr = err.Error()
		return
	}
	s.newSessionMode = false
	s.newSessionLine = ""
	s.newSessionErr = ""
	s.outputErr = ""
	s.refreshSessions(ctx)
	s.selectSession(clientName, sessionID)
	if !s.pooledOutput {
		s.refreshSelectedOutput(ctx)
	}
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

func (s *tuiState) canResizeCurrentSession() bool {
	session := s.currentSession()
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

func (s *tuiState) activePTYSize() (rows, cols uint16) {
	width, height := terminalSize()
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
	if s.copyMode {
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
	} else if s.hostScoped {
		fmt.Fprintln(out, truncate("j/k rows  enter focus/group  m actions  g groups  drag→group  / search  v copy  o organize  y/Y yield  E end  R restart  X destroy  n notify  r refresh  q quit", renderWidth))
	} else {
		fmt.Fprintln(out, truncate("j/k rows  enter focus/group  m actions  g groups  drag→group  / search  v copy  o organize  y/Y yield  E end  R restart  X destroy  n notify  c new  a add  d remove  r refresh  q quit", renderWidth))
	}
	fmt.Fprintln(out, strings.Repeat("-", renderWidth))
	if len(s.sessions) == 0 {
		fmt.Fprintln(out, truncate("No sessions.", renderWidth))
		s.renderCreateModal(out, width, modalHeight)
		s.renderSearchModal(out, width, modalHeight)
		s.renderActionModal(out, width, modalHeight)
		s.renderAddClientModal(out, width, modalHeight)
		s.renderRemoveClientModal(out, width, modalHeight)
		s.renderGroupModal(out, width, modalHeight)
		s.renderNotificationModal(out, width, modalHeight)
		s.renderLifecycleModal(out, width, modalHeight)
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
				plain = s.hostRowColor(listRow.groupID) + plain + modalReset
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
		fmt.Fprintf(out, "\033[%d;1H%s%s%s%s%s", row, s.hostRowColor(sess.Client), plain, modalReset, separator, clear)
		row++
	}
	if !layout.overlay {
		s.renderContent(out, contentX, contentWidth, height)
	}
	s.renderCreateModal(out, width, modalHeight)
	s.renderSearchModal(out, width, modalHeight)
	s.renderActionModal(out, width, modalHeight)
	s.renderAddClientModal(out, width, modalHeight)
	s.renderRemoveClientModal(out, width, modalHeight)
	s.renderGroupModal(out, width, modalHeight)
	s.renderNotificationModal(out, width, modalHeight)
	s.renderLifecycleModal(out, width, modalHeight)
	if s.focused && s.terminal != nil && !s.outputStale {
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
	modalReset    = "\033[0m"
	modalBorder   = "\033[38;2;86;182;194m"
	modalTitle    = "\033[1;38;2;129;212;250m"
	modalMuted    = "\033[38;2;148;163;184m"
	modalStatus   = "\033[38;2;250;204;21m"
	modalInput    = "\033[38;2;167;243;208m"
	modalSelected = "\033[1;38;2;255;255;255;48;2;37;99;235m"
	modalDisabled = "\033[38;2;100;116;139m"
	modalDanger   = "\033[1;38;2;255;255;255;48;2;190;24;93m"
)

type modalRenderLine struct {
	style string
	text  string
}

func renderModalBox(out io.Writer, cols, rows int, lines []modalRenderLine) {
	if cols < 8 || rows < 3 || len(lines) == 0 {
		return
	}
	maxLines := rows - 2
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	boxWidth := min(72, max(8, cols-2))
	innerWidth := boxWidth - 2
	boxHeight := len(lines) + 2
	top := max(1, (rows-boxHeight)/2+1)
	left := max(1, (cols-boxWidth)/2+1)
	fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalBorder, strings.Repeat("─", innerWidth), modalReset)
	for index, line := range lines {
		text := modalCellPad(modalCellTruncate(modalDisplayText(line.text), innerWidth), innerWidth)
		fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", top+index+1, left, modalBorder, line.style, text, modalReset+modalBorder, modalReset)
	}
	fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+boxHeight-1, left, modalBorder, strings.Repeat("─", innerWidth), modalReset)
}

func (s *tuiState) renderAddClientModal(out io.Writer, cols, rows int) {
	if !s.addClientMode {
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
	lines := []modalRenderLine{{modalTitle, "  Add Ducklion host"}}
	if len(choices) == 0 {
		lines = append(lines, modalRenderLine{modalMuted, "  No SSH config hosts found"})
	}
	for index, host := range choices {
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
	help := "  ↑/↓ select   Enter add   Esc cancel"
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
	}
	renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderRemoveClientModal(out io.Writer, cols, rows int) {
	if !s.removeClientMode || len(s.cfg.Clients) == 0 {
		return
	}
	selected := min(max(s.removeClientSelected, 0), len(s.cfg.Clients)-1)
	client := s.cfg.Clients[selected]
	lines := []modalRenderLine{{modalTitle, "  Remove Ducklion host"}}
	if s.removeClientConfirm != "" {
		lines = append(lines,
			modalRenderLine{modalDanger, "  REMOVE HOST CONFIGURATION?"},
			modalRenderLine{modalInput, "  " + displayField(client.Name) + " · " + displayField(client.Host)},
			modalRenderLine{modalMuted, "  Remote sessions will not be destroyed"},
			modalRenderLine{modalMuted, "  Enter remove   Esc back   Ctrl+C cancel"})
		renderModalBox(out, cols, rows, lines)
		return
	}
	for index, candidate := range s.cfg.Clients {
		style, prefix := "", "  "
		if index == selected {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%d  %s · %s", prefix, index+1, displayField(candidate.Name), displayField(candidate.Host))})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ select   Enter continue   Esc/Ctrl+C cancel"})
	renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderGroupModal(out io.Writer, cols, rows int) {
	if !s.groupMenu {
		return
	}
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
	lines = append(lines, modalRenderLine{modalStatus, "  " + status}, modalRenderLine{modalMuted, "  ↑/↓ or j/k choose · Enter confirm · Esc cancel"})
	renderModalBox(out, cols, rows, lines)
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
			fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", row, left, modalBorder, style, text, modalReset+modalBorder, modalReset)
		}
		writePrompt := func(row int) {
			s.renderCreatePromptRow(out, row, left, innerWidth, "")
		}
		fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalBorder, strings.Repeat("─", innerWidth), modalReset)
		if boxHeight == 3 {
			s.renderCreatePromptRow(out, top+1, left, innerWidth, choice+"  ")
		} else {
			writeRow(top+1, modalSelected, choice)
			writePrompt(top + 2)
		}
		fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+boxHeight-1, left, modalBorder, strings.Repeat("─", innerWidth), modalReset)
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
		fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", row, left, modalBorder, style, text, modalReset+modalBorder, modalReset)
	}
	fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalBorder, strings.Repeat("─", innerWidth), modalReset)
	writeRow(top+1, modalTitle, "  "+s.createHeader())
	for i, choice := range choices {
		prefix, style := "  ", ""
		if i == selected && !s.newSessionDiscovering && !s.newSessionStarting {
			prefix, style = "› ", modalSelected
		}
		writeRow(top+2+i, style, prefix+choice)
	}
	status := s.newSessionErr
	if hiddenBefore+hiddenAfter > 0 {
		status = fmt.Sprintf("%s  (%d above, %d below)", status, hiddenBefore, hiddenAfter)
	}
	writeRow(top+2+len(choices), modalStatus, "  "+status)
	s.renderCreatePromptRow(out, top+3+len(choices), left, innerWidth, "  ")
	writeRow(top+4+len(choices), modalMuted, "  ↑/↓ select   Enter continue   Esc back   Ctrl+C close")
	fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+5+len(choices), left, modalBorder, strings.Repeat("─", innerWidth), modalReset)
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
	fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s%s%s%s│%s", row, left, modalBorder, modalInput, prefix, modalMuted, ghost, padding, modalReset+modalBorder, modalReset)
}

func (s *tuiState) renderActionModal(out io.Writer, cols, rows int) {
	if !s.actionMenu || cols < 8 || rows < 3 {
		return
	}
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
		fmt.Fprintf(out, "\033[%d;%dH%s│%s%s%s│%s", row, left, modalBorder, style, value, modalReset+modalBorder, modalReset)
	}
	fmt.Fprintf(out, "\033[%d;%dH%s╭%s╮%s", top, left, modalBorder, strings.Repeat("─", innerWidth), modalReset)
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
	}
	status := fmt.Sprintf("  %s · %s · owner %s", s.actionTarget.Kind, s.actionTarget.Status, actionOwnerLabel(s.actionTarget))
	if !actions[selected].Enabled {
		status = "  unavailable: " + actions[selected].DisabledReason
	}
	writeRow(top+2+len(visible), modalStatus, status)
	writeRow(top+3+len(visible), modalMuted, "  ↑/↓ or j/k select   Enter choose   Esc close")
	writeRow(top+4+len(visible), modalMuted, fmt.Sprintf("  ID %s · generation %d", s.actionTarget.SessionID, s.actionTarget.RuntimeGeneration))
	fmt.Fprintf(out, "\033[%d;%dH%s╰%s╯%s", top+5+len(visible), left, modalBorder, strings.Repeat("─", innerWidth), modalReset)
}

func (s *tuiState) createModalChoices() []string {
	switch s.newSessionStep {
	case "kind":
		return []string{"1  Agent session", "2  Shell session"}
	case "host":
		choices := make([]string, 0, len(s.cfg.Clients))
		for i, client := range s.cfg.Clients {
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
		return []string{"1  Add path to Duckway projects", "2  Use path once"}
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

func modalCellWidth(value string) int {
	width := 0
	for _, r := range value {
		width += modalRuneWidth(r)
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
	for _, r := range value {
		w := modalRuneWidth(r)
		if used+w > limit {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + ellipsis
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
	if s.terminal != nil {
		lines = s.terminal.RenderLines(height-startRow+1, width)
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
	renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderNotificationModal(out io.Writer, cols, rows int) {
	if !s.notificationMode {
		return
	}
	target := s.notificationTarget
	categories := model.NotificationCategories()
	selected := min(max(s.notificationIndex, 0), len(categories)-1)
	maxChoices := max(1, rows-7)
	start := max(0, selected-maxChoices/2)
	if len(categories) > maxChoices {
		start = min(start, len(categories)-maxChoices)
		categories = categories[start : start+maxChoices]
	}
	lines := []modalRenderLine{{modalTitle, fmt.Sprintf("  Notifications · %s / %s", target.Client, target.Name)}}
	for index, category := range categories {
		check := " "
		if s.notificationStaged[category] {
			check = "x"
		}
		style, prefix := "", "  "
		if start+index == selected {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s[%s] %s", prefix, check, notificationLabel(category))})
	}
	status := fmt.Sprintf("  ID %s · changes are staged", target.SessionID)
	if s.outputErr != "" {
		status = "  Not saved: " + s.outputErr
	}
	lines = append(lines, modalRenderLine{modalStatus, status}, modalRenderLine{modalMuted, "  Space toggle · a all · x none · r defaults · Enter save · Esc cancel"})
	if rows < 7 {
		category := categories[selected-start]
		check := " "
		if s.notificationStaged[category] {
			check = "x"
		}
		lines = []modalRenderLine{{modalSelected, fmt.Sprintf("› [%s] %s", check, notificationLabel(category))}, {modalMuted, "Space toggle · Enter save · Esc"}}
	}
	renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderLifecycleModal(out io.Writer, cols, rows int) {
	if s.lifecycleConfirm == "" {
		return
	}
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
		detail = "Shell process ends immediately; it never waits for idle."
		help = "Enter terminate process immediately · Esc cancel"
	}
	if rows < 7 {
		warning := verb + ": " + detail + "  " + help
		renderModalBox(out, cols, rows, []modalRenderLine{{style, warning}})
		return
	}
	lines := []modalRenderLine{
		{style, "  " + verb + " SESSION"},
		{modalInput, fmt.Sprintf("  %s [%s]", target.Name, target.SessionID)},
		{modalMuted, fmt.Sprintf("  Host %s · generation %d", target.Client, target.RuntimeGeneration)},
		{modalStatus, "  " + detail},
		{modalMuted, "  " + help},
	}
	renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) beginNotificationSettings() {
	session := s.currentSession()
	if session.InstanceID == "" || session.SessionID == "" {
		s.outputErr = "notification settings require a synchronized Ducklion session"
		return
	}
	s.notificationMode = true
	s.notificationIndex = 0
	s.notificationTarget = session
	s.outputErr = ""
	s.notificationStaged = make(map[model.NotificationCategory]bool)
	for _, category := range model.NotificationCategories() {
		s.notificationStaged[category] = s.activity().Enabled(session.InstanceID, session.SessionID, category)
	}
}

func (s *tuiState) closeNotificationSettings() {
	s.notificationMode = false
	s.notificationStaged = nil
	s.notificationTarget = ducklord.RemoteSession{}
}

func (s *tuiState) beginActionMenu() {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		s.outputErr = "no session selected"
		return
	}
	target := s.sessions[s.selected]
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
	switch string(input) {
	case "\x1b", "q":
		return "cancel"
	case "\r", "\n":
		return "save"
	case "j", "\x1b[B":
		if s.notificationIndex < len(categories)-1 {
			s.notificationIndex++
		}
	case "k", "\x1b[A":
		if s.notificationIndex > 0 {
			s.notificationIndex--
		}
	case " ":
		category := categories[s.notificationIndex]
		s.notificationStaged[category] = !s.notificationStaged[category]
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
			modalDisplayText(s.organizationGroupLabel(session)), modalDisplayText(session.Kind), modalDisplayText(session.AgentType),
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
	case text == "q" || text == "\x03":
		return "quit"
	case s.selectedGroupID != "" && (text == "g" || text == "y" || text == "Y" || text == "E" || text == "R" || text == "X" || text == "n" || text == "m" || text == "d" || text == "\x0b" || text == "\n"):
		s.outputErr = "select a session row for this action"
		return "group-select"
	case text == "\x1b[D":
		if s.selectedGroupID == "" {
			s.selectedGroupID = s.organizationGroupID(s.currentSession())
			return "group-select"
		}
		s.setSelectedGroupCollapsed(true)
		return "group-toggle"
	case text == "\x1b[C":
		if s.selectedGroupID != "" {
			s.setSelectedGroupCollapsed(false)
			return "group-toggle"
		}
		return "group-select"
	case text == "r":
		return "refresh"
	case text == "/":
		return "search"
	case text == "o":
		return "organize"
	case text == "g":
		return "groups"
	case text == "\x0b":
		return "reorder-up"
	case text == "\n":
		return "reorder-down"
	case text == "y":
		return "yield"
	case text == "Y":
		return "yield-wait"
	case text == "E":
		return "end"
	case text == "R":
		return "restart"
	case text == "X":
		return "destroy"
	case text == "c" && !s.hostScoped:
		return "new"
	case text == "n":
		return "notifications"
	case text == "v":
		return "copy-mode"
	case text == "m":
		return "actions"
	case text == "a" && !s.hostScoped:
		return "add-client"
	case text == "d" && !s.hostScoped:
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
	cols, _ := terminalSize()
	status := modalCellTruncate(" COPY MODE  Drag to select PTY text; use terminal Copy; Esc/q/v/Ctrl+C exits", max(1, cols))
	// Stop event tracking before disabling its SGR encoding. This hands mouse
	// drags back to the terminal emulator for native selection.
	fmt.Fprintf(out, "\033[?1002l\033[?1006l\033[2;1H\033[2K%s%s%s", modalSelected, status, modalReset)
}

func (s *tuiState) exitCopyMode(out io.Writer) {
	if !s.copyMode {
		return
	}
	s.copyMode = false
	// Restore the encoding before event tracking so no mouse report can arrive
	// in an unexpected format during the transition.
	fmt.Fprint(out, "\033[?1006h\033[?1002h")
}

func (s *tuiState) beginAddClient() {
	hosts, err := ducklord.LoadSSHConfigHosts("")
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	s.addClientMode = true
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
	result := runAddClient(ctx, s.runner, client)
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
	go func() {
		result := runAddClientWithProgress(workCtx, s.runner, client, func(status string) {
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
	result := addClientDoneEvent{client: client}
	probe, err := runner.ProbeDucklion(ctx, client)
	if err != nil {
		result.err = err
		return result
	}
	var installErr error
	installed := false
	if !probe.Available {
		if progress != nil {
			progress("installing ducklion on " + client.Name + "...")
		}
		installedPath, err := runner.InstallDucklion(ctx, client, "", "")
		if err != nil {
			installErr = err
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
	if probe.Available && probe.Command != "" {
		client.Ducklion = probe.Command
	}
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
	clientName := s.selectedClientName()
	if clientName == "" {
		s.outputErr = "no ducklord client configured"
		return
	}
	s.newSessionMode = true
	s.newSessionClient = clientName
	s.newSessionLine = ""
	s.newSessionSelected = 0
	s.newSessionErr = ""
	s.newSessionStarting = false
	s.cancelCreateDiscovery()
	s.newSessionStep = "kind"
	s.newSessionKind = model.KindAgent
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

func (s *tuiState) cancelCreate() {
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

func (s *tuiState) invalidateCreateStart(update ducklord.SessionUpdate) bool {
	if !s.newSessionStarting || update.Client != s.newSessionClient {
		return false
	}
	if update.State == "live" && update.Generation == s.newSessionStartGeneration && update.InstanceID == s.newSessionStartInstance {
		return false
	}
	s.newSessionStarting = false
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
		for i, client := range s.cfg.Clients {
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
		if !s.hostIsLive(clientName) {
			return "", "", nil, false, fmt.Errorf("host %s is reconnecting; wait for synchronization", clientName)
		}
		client, err := mustClient(s.cfg, clientName)
		if err != nil {
			return "", "", nil, false, err
		}
		s.newSessionClient = clientName
		s.newSessionLine = ""
		s.newSessionSelected = 0
		s.newSessionErr = "loading projects..."
		s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "projects", client: clientName}, func(workCtx context.Context) createDiscoveryEvent {
			projects, projectErr := s.runner.Projects(workCtx, client)
			return createDiscoveryEvent{projects: projects, err: projectErr}
		})
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
			return "", "", nil, false, fmt.Errorf("choose 1 (add project) or 2 (use once)")
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
		s.newSessionErr = "adding remote project..."
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
		s.newSessionErr = "revalidating project and runtime..."
		project, agentType, kind := s.newSessionProject, s.newSessionAgent, s.newSessionKind
		s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "validate", client: s.newSessionClient, project: project, sessionName: name}, func(workCtx context.Context) createDiscoveryEvent {
			if project.Source != "path" {
				projects, projectErr := s.runner.Projects(workCtx, client)
				if projectErr != nil {
					return createDiscoveryEvent{sessionName: name, failureStep: "project", err: fmt.Errorf("project revalidation failed: %w", projectErr)}
				}
				if !containsProject(projects, project) {
					return createDiscoveryEvent{sessionName: name, failureStep: "project", err: fmt.Errorf("selected project is no longer available")}
				}
			}
			agents, agentErr := s.runner.Agents(workCtx, client, project.Path)
			if agentErr != nil {
				return createDiscoveryEvent{sessionName: name, failureStep: "agent", err: fmt.Errorf("runtime revalidation failed: %w", agentErr)}
			}
			resolved, ok := findRemoteAgent(agents, agentType)
			if !ok {
				return createDiscoveryEvent{sessionName: name, failureStep: "agent", err: fmt.Errorf("selected runtime %s is no longer available", agentType)}
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
	if generation != event.generation || instance != event.instance || !s.hostIsLive(event.client) {
		s.cancelCreateDiscovery()
		s.newSessionStep = "host"
		if event.kind == "add-project" && event.project.Path != "" {
			s.newSessionErr = "project was added, but the host changed; choose it again"
		} else {
			s.newSessionErr = "host changed while loading; choose it again"
		}
		return "", "", nil, false
	}
	s.newSessionDiscovering = false
	s.newSessionCancel = nil
	if event.err != nil {
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
				s.newSessionErr = "project was added, but runtime discovery failed: " + event.err.Error()
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
	case "path-status":
		if event.directory.Exists {
			s.newSessionStep, s.newSessionSelected = "project-policy", 0
			s.newSessionErr = "directory exists; add it to projects or use it once"
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
			s.newSessionErr = "directory created recursively; add it to projects or use it once"
		} else {
			s.newSessionErr = "directory already exists; add it to projects or use it once"
		}
	case "projects":
		projects := event.projects
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
			s.newSessionErr = "no configured projects; type an absolute remote directory path"
			return "", "", nil, false
		}
		s.newSessionProjects = append([]ducklord.RemoteProject(nil), projects...)
		s.newSessionStep = "project"
		s.newSessionSelected = 0
		s.newSessionErr = fmt.Sprintf("host: %s; choose a configured project", event.client)
	case "add-project":
		s.newSessionProject = event.project
		s.newSessionCWD = event.project.Path
		s.newSessionProjects = appendRemoteProjectOnce(s.newSessionProjects, event.project)
		s.newSessionStep = "project"
		s.newSessionLine = event.project.Name
		s.newSessionErr = "project added; press Enter to continue"
	case "agents":
		if s.newSessionKind == model.KindShell {
			shells := make([]ducklord.RemoteAgent, 0, 3)
			for _, agent := range event.agents {
				if agent.Type == "shell" || agent.Type == "zsh" || agent.Type == "bash" || agent.Type == "sh" {
					shells = append(shells, agent)
				}
			}
			if len(shells) == 0 {
				s.newSessionStep, s.newSessionErr = "project", "remote shell is unavailable"
				return "", "", nil, false
			}
			if len(shells) == 1 && shells[0].Type == "shell" {
				s.newSessionAgent, s.newSessionCommand = shells[0].Type, append([]string(nil), shells[0].Command...)
				s.newSessionStep, s.newSessionErr = "handle", fmt.Sprintf("shell project: %s; empty handle uses %s", event.project.Path, defaultSessionHandle(event.project.Path))
				return "", "", nil, false
			}
			s.newSessionAgents = shells
			s.newSessionStep, s.newSessionErr = "agent", fmt.Sprintf("project: %s; choose zsh, bash, or sh", event.project.Path)
			s.newSessionSelected = 0
			return "", "", nil, false
		}
		agents := make([]ducklord.RemoteAgent, 0, len(event.agents))
		for _, agent := range event.agents {
			if agent.Type != "shell" && agent.Type != "zsh" && agent.Type != "bash" && agent.Type != "sh" {
				agents = append(agents, agent)
			}
		}
		if len(agents) == 0 {
			s.newSessionStep, s.newSessionErr = "project", "this project has no available agent runtime"
			return "", "", nil, false
		}
		s.newSessionAgents = agents
		s.newSessionStep, s.newSessionErr = "agent", fmt.Sprintf("project: %s; choose an available agent", event.project.Path)
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
	return "", nil, fmt.Errorf("agent %q is not available for this project", input)
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
	if len(s.cfg.Clients) == 0 {
		return "", fmt.Errorf("no ducklord client configured")
	}
	if input == "" {
		return s.newSessionClient, nil
	}
	if n, err := strconv.Atoi(input); err == nil {
		if n < 1 || n > len(s.cfg.Clients) {
			return "", fmt.Errorf("host number out of range")
		}
		return s.cfg.Clients[n-1].Name, nil
	}
	if _, ok := s.cfg.Client(input); !ok {
		return "", fmt.Errorf("unknown host %q", input)
	}
	return input, nil
}

func (s *tuiState) resolveCreateProject(input string) (ducklord.RemoteProject, error) {
	if input == "" {
		if len(s.newSessionProjects) == 1 {
			return s.newSessionProjects[0], nil
		}
		return ducklord.RemoteProject{}, fmt.Errorf("project name or number is required")
	}
	if n, err := strconv.Atoi(input); err == nil {
		if n < 1 || n > len(s.newSessionProjects) {
			return ducklord.RemoteProject{}, fmt.Errorf("project number out of range")
		}
		return s.newSessionProjects[n-1], nil
	}
	for _, project := range s.newSessionProjects {
		if input == project.Name || input == project.Path {
			return project, nil
		}
	}
	return ducklord.RemoteProject{}, fmt.Errorf("unknown project %q; choose a configured project", input)
}

func (s *tuiState) createHeader() string {
	switch s.newSessionStep {
	case "kind":
		return "new session: choose agent or shell"
	case "host":
		return "new session: choose host number/name"
	case "project":
		return fmt.Sprintf("new %s session: host=%s  choose configured project", s.newSessionKind, displayField(s.newSessionClient))
	case "path":
		return fmt.Sprintf("new %s session: browse remote path on %s", s.newSessionKind, displayField(s.newSessionClient))
	case "path-confirm":
		return fmt.Sprintf("CREATE REMOTE DIRECTORY · %s · %s", displayField(s.newSessionClient), displayField(s.newSessionCWD))
	case "project-policy":
		return "remote path selected: add to projects or use once"
	case "project-name":
		return "add remote path to Duckway projects"
	case "agent":
		label := "agent"
		if s.newSessionKind == model.KindShell {
			label = "shell"
		}
		return fmt.Sprintf("new session: host=%s project=%s  choose %s", displayField(s.newSessionClient), displayField(s.newSessionCWD), label)
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
		return "project"
	case "path":
		return "path"
	case "path-confirm":
		return "choice"
	case "project-policy":
		return "choice"
	case "project-name":
		return "project name"
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
		if s.newSessionStep == "kind" {
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

func (s *tuiState) centralModalOpen() bool {
	return s.searchMode || s.addClientMode || s.removeClientMode || s.newSessionMode || s.notificationMode || s.groupMenu || s.actionMenu || s.lifecycleConfirm != ""
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
		for index, client := range s.cfg.Clients {
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

func bridgeKeyForTUI(client ducklord.Client) string {
	return strings.Join([]string{client.Name, client.Host, client.User, client.SSH, client.Ducklion}, "\x00")
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

func readInput(ctx context.Context, ch chan<- []byte) {
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
	if len(pending) >= 4 && pending[1] == '[' && pending[2] == '<' {
		for i := 3; i < len(pending); i++ {
			if pending[i] == 'M' || pending[i] == 'm' {
				return append([]byte(nil), pending[:i+1]...), pending[i+1:], true
			}
		}
		return nil, pending, false
	}
	// Preserve Alt/Meta key chords as one event. In particular, this prevents
	// the Esc prefix from closing copy mode and its suffix from becoming a
	// normal-mode shortcut such as q or c.
	if len(pending) >= 2 && pending[1] != '[' {
		if pending[1] < utf8.RuneSelf {
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

func isDetachInput(b []byte) bool {
	return len(b) == 1 && b[0] == 0x1d
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
  ducklord projects <client> [--config <path>]
  ducklord agents <client> <project-path> [--config <path>]
  ducklord probe <client> [--config <path>]
  ducklord install-ducklion <client> [--source <path>] [--dest <remote-path>] [--config <path>]
  ducklord tui [--config <path>] [--name <owner>] [--refresh 2s]
  ducklord attach-host <client> [--config <path>]
  ducklord attach <client> <session> [--config <path>]
  ducklord read <client> <session> [--lines N] [--config <path>]
  ducklord send <client> <session> <text> [--config <path>]
  ducklord start <client> --name <name> [--kind shell | --agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
  ducklord stop <client> <session> [--config <path>]
  ducklord end <client> <session> [-w|--wait|-f|--force] [--config <path>]
  ducklord restart <client> <session> [-f|--force] [--config <path>]
  ducklord destroy <client> <session> [-w|--wait|-f|--force] [--config <path>]
  ducklord yield <client> <session> [-w|--wait] [--config <path>]
  ducklord version`)
}
