package main

import (
	"context"
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

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
	"github.com/hackerduck/duckway/internal/version"
)

type remoteRunner interface {
	Sessions(context.Context, ducklord.Client, int) ([]ducklord.RemoteSession, error)
	Read(context.Context, ducklord.Client, string, int) (string, error)
	Send(context.Context, ducklord.Client, string, string) error
	Start(context.Context, ducklord.Client, []string) (string, error)
	Stop(context.Context, ducklord.Client, string) error
	Lifecycle(context.Context, ducklord.Client, string, protocol.SessionLifecycleOperation, protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error)
	Yield(context.Context, ducklord.Client, string, bool) (protocol.SessionYieldResult, error)
	Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error)
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
}

type lifecycleDoneEvent struct {
	operation protocol.SessionLifecycleOperation
	result    protocol.SessionLifecycleResult
	err       error
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
		if len(rest) != 1 {
			return fmt.Errorf("usage: ducklord sessions <client> [--config <path>]")
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
		sessions, err := runner.Sessions(context.Background(), c, 8)
		if err != nil {
			return err
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
	return buildStartArgsKind(name, model.KindAgent, agent, cwd, command)
}

func buildStartArgsKind(name string, kind model.SessionKind, agent, cwd string, command []string) ([]string, error) {
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
	hashes                    map[string]string
	selectedKey               string
	outputText                string
	outputErr                 string
	localWarning              string
	resizeStatus              string
	outputForKey              string
	outputStale               bool
	outputFresh               bool
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
	newSessionMode            bool
	newSessionClient          string
	newSessionLine            string
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
	addClientMode             bool
	addClientLine             string
	addClientErr              string
	addClientHosts            []ducklord.SSHHost
	hostScoped                bool
	ownerName                 string
	listPaneWidth             int
	autoHideList              bool
	hostSync                  map[string]ducklord.SessionUpdate
	eventDriven               bool
	notificationMode          bool
	notificationIndex         int
	notificationStaged        map[model.NotificationCategory]bool
	lifecycleConfirm          protocol.SessionLifecycleOperation
	lifecycleBusy             bool
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
	fmt.Print("\033[?1049h\033[?25l\033[?1000h\033[?1006h")
	defer fmt.Print("\033[?1006l\033[?1000l\033[?25h\033[?1049l")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	activityStore := ducklord.ActivityStateStore{}
	activityState, err := activityStore.Load()
	stateWarning := ""
	var recovered *ducklord.CorruptStateRecoveredError
	if errors.As(err, &recovered) && activityState != nil {
		stateWarning = recovered.Error()
	} else if err != nil {
		return fmt.Errorf("load Ducklord activity state: %w", err)
	}
	state := &tuiState{cfg: cfg, cfgPath: cfgPath, runner: runner, refresh: refresh, hashes: map[string]string{}, hostScoped: hostScoped, ownerName: owner,
		snapshotStore: ducklord.SnapshotStore{}, activityStore: activityStore, activityState: activityState, hostSync: make(map[string]ducklord.SessionUpdate), localWarning: stateWarning,
		listPaneWidth: cfg.SessionListPaneWidth(), autoHideList: cfg.SessionListAutoHide()}
	defer state.saveCurrentSnapshot()
	sessionUpdates := make(chan ducklord.SessionUpdate, 32)
	if watcher, ok := runner.(interface {
		WatchSessionUpdates(context.Context, ducklord.Client) <-chan ducklord.SessionUpdate
	}); ok {
		state.eventDriven = true
		for _, client := range cfg.Clients {
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
	}
	state.refreshSessions(ctx)
	state.refreshSelectedOutput(ctx)
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
	lifecycleDone := make(chan lifecycleDoneEvent, 1)
	previewDone := make(chan previewOutputEvent, 1)
	var previewID uint64
	previewInFlight := false
	previewQueued := false
	var previewCancel context.CancelFunc
	var startPreview func()
	startPreview = func() {
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
		previewID++
		previewQueued = false
	}
	var attach *ducklord.AttachSession
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
		state.createWorkers.Wait()
		<-resizeWorkerDone
	}()
	queueResize := func() {
		if attach == nil || attach.ResizeBarrier == nil || !attachCanResize {
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
		request := resizeRequest{id: attachID, rows: rows, cols: cols, resize: attach.ResizeBarrier}
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
	go readInput(ctx, input)
	for {
		select {
		case <-ctx.Done():
			if attachCancel != nil {
				attachCancel()
			}
			if startCancel != nil {
				startCancel()
			}
			state.cancelCreateDiscovery()
			return nil
		case <-ticker.C:
			if !state.focused && !state.newSessionMode {
				if !state.eventDriven {
					state.refreshSessions(ctx)
				}
				requestPreview(false)
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
					resizeRequests <- resizeRequest{id: attachID, rows: queued[0], cols: queued[1], resize: attach.ResizeBarrier}
					resizeInFlight = true
				}
				state.render(os.Stdout)
			}
		case update := <-sessionUpdates:
			previousActive := state.effectiveAttachKey()
			if state.invalidateCreateStart(update) {
				if startCancel != nil {
					startCancel()
					startCancel = nil
				}
				startID++ // fence a completion racing with cancellation
			}
			state.applySessionUpdate(update)
			if previousActive != "" && state.effectiveAttachKey() == "" && attach != nil {
				if attachCancel != nil {
					attachCancel()
					attachCancel = nil
				}
				_ = attach.Stdin.Close()
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
			if !state.focused && state.activeAttachKey == "" {
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
		case result := <-startDone:
			if result.id != startID {
				continue
			}
			startCancel = nil
			state.completeNewSessionStart(ctx, result.client, result.sessionID, result.err)
			state.render(os.Stdout)
		case result := <-lifecycleDone:
			state.lifecycleBusy = false
			state.lifecycleConfirm = ""
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
			if state.focused {
				if isDetachInput(b) {
					state.saveCurrentSnapshot()
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
				if attach != nil {
					_, _ = attach.Stdin.Write(b)
				}
				continue
			}
			if state.addClientMode {
				action := state.handleLineInput(b, &state.addClientLine)
				switch action {
				case "cancel":
					state.cancelAddClient()
				case "submit":
					if err := state.submitAddClient(ctx); err != nil {
						state.addClientErr = err.Error()
					}
				}
				state.render(os.Stdout)
				continue
			}
			if state.newSessionMode {
				if state.newSessionStarting || state.newSessionDiscovering {
					if string(b) == "\x03" || string(b) == "\x1b" || string(b) == "q" {
						if state.newSessionStarting && startCancel != nil {
							startCancel()
							state.newSessionErr = "canceling start..."
						} else {
							state.cancelCreate()
						}
						state.render(os.Stdout)
					}
					continue
				}
				action := state.handleCreateInput(b)
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
					state.notificationMode = false
					state.notificationStaged = nil
				}
				state.render(os.Stdout)
				continue
			}
			if state.lifecycleConfirm != "" {
				text := string(b)
				if text == "\x1b" || text == "q" {
					state.lifecycleConfirm = ""
					state.render(os.Stdout)
					continue
				}
				mode := protocol.SessionLifecycleImmediate
				sess := state.currentSession()
				if sess.Kind != string(model.KindShell) && state.lifecycleConfirm == protocol.SessionLifecycleRestart {
					mode = protocol.SessionLifecycleWait
				}
				if sess.Kind == string(model.KindShell) && text != "\r" && text != "\n" {
					continue
				}
				if text == "w" && state.lifecycleConfirm != protocol.SessionLifecycleRestart {
					mode = protocol.SessionLifecycleWait
				} else if text == "f" {
					mode = protocol.SessionLifecycleForce
				} else if text != "\r" && text != "\n" {
					continue
				}
				client, clientErr := mustClient(cfg, sess.Client)
				if clientErr != nil {
					state.outputErr = clientErr.Error()
					state.lifecycleConfirm = ""
					continue
				}
				ref, operation := sess.SessionID, state.lifecycleConfirm
				if ref == "" {
					ref = sess.Name
				}
				state.lifecycleBusy = true
				state.lifecycleConfirm = ""
				state.outputErr = string(operation) + " accepted; Ducklion will continue it across reconnects"
				go func() {
					result, err := runner.Lifecycle(ctx, client, ref, operation, mode)
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
			case "notifications":
				state.beginNotificationSettings()
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
				state.outputErr = ""
			case "add-client":
				state.beginAddClient()
			case "remove-client":
				if err := state.removeSelectedClient(ctx); err != nil {
					state.outputErr = err.Error()
				}
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
				ref := sess.SessionID
				if ref == "" {
					ref = sess.Name
				}
				result, err := runner.Yield(ctx, client, ref, action == "yield-wait")
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
		return all[i].Name < all[j].Name
	})
	activityChanged := false
	for i := range all {
		key := all[i].Client + "/" + all[i].Name
		if all[i].TailHash != "" && s.hashes[key] != "" && s.hashes[key] != all[i].TailHash {
			all[i].Updated = true
		}
		if all[i].TailHash != "" {
			s.hashes[key] = all[i].TailHash
		}
		all[i].LastLine = sanitizeTerminalText(all[i].LastLine)
		all[i].Error = sanitizeTerminalText(all[i].Error)
		var changed bool
		all[i].Unread, changed = s.activity().Reconcile(all[i], sessionKey(all[i]) == s.activeAttachKey && s.activeAttachFresh)
		activityChanged = activityChanged || changed
	}
	if activityChanged {
		s.saveActivityState()
	}
	s.sessions = all
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
}

func (s *tuiState) applySessionUpdate(update ducklord.SessionUpdate) {
	if update.Client == "" {
		return
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
	if update.State != "live" {
		if s.currentSession().Client == update.Client {
			s.outputFresh = false
		}
		return // retain the last authoritative rows while reconnecting
	}
	oldKey := s.currentKey()
	all := make([]ducklord.RemoteSession, 0, len(s.sessions)+len(update.Sessions))
	for _, session := range s.sessions {
		if session.Client != update.Client {
			all = append(all, session)
		}
	}
	activityChanged := false
	for _, session := range update.Sessions {
		activeFresh := sessionKey(session) == s.activeAttachKey && s.activeAttachFresh
		unread, changed := s.activity().Reconcile(session, activeFresh)
		session.Unread = unread
		activityChanged = activityChanged || changed
		if session.SessionID == update.ChangedSessionID && sessionKey(session) != oldKey {
			session.Updated = true
		}
		all = append(all, session)
	}
	if activityChanged {
		s.saveActivityState()
	}
	sortRemoteSessions(all)
	s.sessions = all
	s.restoreSelection(oldKey)
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
	return true
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
		return sessions[i].Name < sessions[j].Name
	})
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
	s.refreshSelectedOutput(ctx)
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
	if sessionKey(s.currentSession()) == s.activeAttachKey {
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
	if s.activity().MarkSeen(session) {
		for i := range s.sessions {
			if sessionKey(s.sessions[i]) == sessionKey(session) {
				s.sessions[i].Unread = false
			}
		}
		s.saveActivityState()
	}
}

func (s *tuiState) saveActivityState() {
	if err := s.activityStore.Save(s.activity()); err != nil && s.outputErr == "" {
		s.outputErr = "notification state: " + err.Error()
	}
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
	s.saveSnapshot(s.currentSession(), s.outputText)
}

func (s *tuiState) saveSnapshot(session ducklord.RemoteSession, text string) {
	if session.InstanceID == "" || session.SessionID == "" || text == "" && s.terminal == nil {
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
	width, height := terminalSize()
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
	} else if s.lifecycleConfirm != "" {
		verb := string(s.lifecycleConfirm)
		if s.currentSession().Kind == string(model.KindShell) {
			fmt.Fprintln(out, truncate(verb+" shell session now?  enter terminate process immediately  esc cancel", renderWidth))
		} else if s.lifecycleConfirm == protocol.SessionLifecycleRestart {
			fmt.Fprintln(out, truncate("restart selected session?  enter wait for idle  f force-cancel task  esc cancel", renderWidth))
		} else {
			fmt.Fprintln(out, truncate(verb+" selected session?  enter now  w wait for idle  f force-cancel task  esc cancel", renderWidth))
		}
	} else if s.hostScoped {
		fmt.Fprintln(out, truncate("j/k move  enter focus  y/Y yield  E end  R restart  X destroy  n notifications  r refresh  q quit", renderWidth))
	} else {
		fmt.Fprintln(out, truncate("j/k move  enter focus  y/Y yield  E end  R restart  X destroy  n notifications  c new  a add  d remove  r refresh  q quit", renderWidth))
	}
	fmt.Fprintln(out, strings.Repeat("-", renderWidth))
	if len(s.sessions) == 0 {
		fmt.Fprintln(out, truncate("No sessions.", renderWidth))
		s.renderAddClientChoices(out, 5, 1, menuWidth, height)
		s.renderCreateChoices(out, 5, 1, menuWidth, height)
		s.renderAddClientPrompt(out, 4, 1, renderWidth)
		s.renderCreatePrompt(out, 4, 1, renderWidth)
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
		heading := truncate("sessions", menuWidth)
		fmt.Fprintf(out, "\033[4;1H%-*s%s%s", menuWidth, heading, separator, clear)
	} else {
		fmt.Fprintf(out, "\033[4;1H%s\033[K", truncate("content", renderWidth))
	}
	currentGroup := "\000"
	row := 5
	for i, sess := range s.sessions {
		if !layout.showList {
			break
		}
		if row > height {
			break
		}
		group := displayField(sess.Group)
		if group == "" {
			group = "default"
		}
		if group != currentGroup {
			groupLabel := "[" + group + "]"
			if s.groupHasUnread(sess.Group) {
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
			fmt.Fprintf(out, "\033[%d;1H%-*s%s%s", row, menuWidth, truncate(groupLabel, menuWidth), separator, clear)
			row++
			currentGroup = group
			if row > height {
				break
			}
		}
		prefix := " "
		if i == s.selected {
			prefix = ">"
		}
		mark := " "
		if sessionKey(sess) == s.activeAttachKey {
			mark = "●"
		} else if sess.Unread {
			mark = "•"
		} else if sess.Updated {
			mark = "*"
		}
		line := fmt.Sprintf("%s%s %-12s %-18s %-9s %-14s %s", prefix, mark, displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), displayField(sessionTypeLabel(sess)), sess.LastLine)
		if sess.Error != "" {
			line = fmt.Sprintf("%s! %-12s %-18s %-9s %s", prefix, displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), sess.Error)
		}
		separator := " |"
		if layout.overlay {
			separator = ""
		}
		clear := "\033[K"
		if layout.overlay {
			clear = ""
		}
		fmt.Fprintf(out, "\033[%d;1H%-*s%s%s", row, menuWidth, truncate(line, menuWidth), separator, clear)
		row++
	}
	s.renderAddClientChoices(out, row, 1, menuWidth, height)
	s.renderCreateChoices(out, row, 1, menuWidth, height)
	if !layout.overlay {
		s.renderContent(out, contentX, contentWidth, height)
	}
	s.renderAddClientPrompt(out, height, 1, renderWidth)
	s.renderCreatePrompt(out, height, 1, renderWidth)
	if s.focused && s.terminal != nil && !s.outputStale {
		if cursorRow, cursorCol, visible := s.terminal.CursorPosition(height-5, contentWidth); visible {
			fmt.Fprintf(out, "\033[%d;%dH\033[?25h", 6+cursorRow, contentX+cursorCol)
		}
	}
}

func (s *tuiState) groupHasUnread(group string) bool {
	for _, session := range s.sessions {
		if session.Group == group && session.Unread {
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

func (s *tuiState) renderAddClientChoices(out io.Writer, row, x, width, height int) {
	if !s.addClientMode || row > height-2 {
		return
	}
	for i, host := range s.addClientHosts {
		if row > height-2 {
			return
		}
		line := fmt.Sprintf("%d %s", i+1, displayField(host.Name))
		fmt.Fprintf(out, "\033[%d;%dH%-*s |\033[K", row, x, width, truncate(line, width))
		row++
	}
}

func (s *tuiState) renderAddClientPrompt(out io.Writer, row, x, width int) {
	if !s.addClientMode {
		return
	}
	prompt := fmt.Sprintf("host> %s", s.addClientLine)
	fmt.Fprintf(out, "\033[%d;%dH%s\033[K", row, x, truncate(prompt, width))
	if s.addClientErr != "" && row > 1 {
		fmt.Fprintf(out, "\033[%d;%dH%s\033[K", row-1, x, truncate("status: "+sanitizeTerminalText(s.addClientErr), width))
	}
}

func (s *tuiState) renderCreateChoices(out io.Writer, row, x, width, height int) {
	if !s.newSessionMode || row > height-2 {
		return
	}
	switch s.newSessionStep {
	case "kind":
		fmt.Fprintf(out, "\033[%d;%dH%-*s |\033[K", row, x, width, truncate("1 agent", width))
		if row+1 <= height-2 {
			fmt.Fprintf(out, "\033[%d;%dH%-*s |\033[K", row+1, x, width, truncate("2 shell", width))
		}
	case "host":
		for i, client := range s.cfg.Clients {
			if row > height-2 {
				return
			}
			line := fmt.Sprintf("%d %s %s", i+1, displayField(client.Name), displayField(client.Target()))
			fmt.Fprintf(out, "\033[%d;%dH%-*s |\033[K", row, x, width, truncate(line, width))
			row++
		}
	case "project":
		for i, project := range s.newSessionProjects {
			if row > height-2 {
				return
			}
			line := fmt.Sprintf("%d %s %s", i+1, displayField(project.Name), displayField(project.Path))
			fmt.Fprintf(out, "\033[%d;%dH%-*s |\033[K", row, x, width, truncate(line, width))
			row++
		}
	case "agent":
		for i, agent := range s.newSessionAgents {
			if row > height-2 {
				return
			}
			line := fmt.Sprintf("%d %s", i+1, displayField(agent.Type))
			fmt.Fprintf(out, "\033[%d;%dH%-*s |\033[K", row, x, width, truncate(line, width))
			row++
		}
	}
}

func (s *tuiState) renderCreatePrompt(out io.Writer, row, x, width int) {
	if !s.newSessionMode {
		return
	}
	prompt := fmt.Sprintf("%s> %s", s.createPromptLabel(), s.newSessionLine)
	fmt.Fprintf(out, "\033[%d;%dH%s\033[K", row, x, truncate(prompt, width))
	if s.newSessionErr != "" && row > 1 {
		fmt.Fprintf(out, "\033[%d;%dH%s\033[K", row-1, x, truncate("status: "+sanitizeTerminalText(s.newSessionErr), width))
	}
}

func (s *tuiState) renderContent(out io.Writer, x, width, height int) {
	if len(s.sessions) == 0 || s.selected < 0 || s.selected >= len(s.sessions) {
		return
	}
	sess := s.sessions[s.selected]
	if s.lifecycleConfirm != "" {
		status := "waiting for confirmation"
		if s.lifecycleBusy {
			status = "request accepted; waiting for durable completion"
		}
		lines := []string{
			strings.ToUpper(string(s.lifecycleConfirm)) + " SESSION",
			"Session: " + displayField(sess.Name) + "  [" + displayField(sess.SessionID) + "]",
			"Host: " + displayField(sess.Client),
			"Status: " + status,
		}
		switch s.lifecycleConfirm {
		case protocol.SessionLifecycleDestroy:
			lines = append(lines, "Destroy permanently removes this PTY and its retained logs.")
		case protocol.SessionLifecycleEnd:
			lines = append(lines, "End keeps the session metadata and retained logs for recovery.")
		default:
			lines = append(lines, "Restart keeps the session identity and starts a new runtime generation.")
		}
		if sess.Kind == string(model.KindShell) {
			lines = append(lines, "Shell lifecycle terminates the current process immediately; it never waits for idle.")
		}
		for i, line := range lines {
			fmt.Fprintf(out, "\033[%d;%dH%s\033[K", 5+i, x, truncate(line, width))
		}
		return
	}
	if s.notificationMode {
		s.renderNotificationSettings(out, x, width, height, sess)
		return
	}
	header := fmt.Sprintf("%s / %s  %s  %s", displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), displayField(sessionTypeLabel(sess)))
	if sess.Status == string(model.StatusStopped) && sess.RetainedOutputBytes > 0 {
		header += fmt.Sprintf("  retained:%s until %s", formatByteCount(sess.RetainedOutputBytes), formatRetainedUntil(sess.RetainedOutputUntilMS))
	}
	if s.focused {
		header += "  [focus]"
	}
	if s.outputStale {
		header += "  [STALE SNAPSHOT]"
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

func (s *tuiState) renderNotificationSettings(out io.Writer, x, width, height int, session ducklord.RemoteSession) {
	header := fmt.Sprintf("Notifications · %s / %s", displayField(session.Client), displayField(session.Name))
	fmt.Fprintf(out, "\033[5;%dH%s\033[K", x, truncate(header, width))
	fmt.Fprintf(out, "\033[6;%dH%s\033[K", x, truncate("j/k move · Space toggle · a all · x none · r defaults · Enter save · Esc cancel", width))
	for i, category := range model.NotificationCategories() {
		row := 8 + i
		if row > height {
			break
		}
		cursor := " "
		if i == s.notificationIndex {
			cursor = ">"
		}
		check := " "
		if s.notificationStaged[category] {
			check = "x"
		}
		line := fmt.Sprintf("%s [%s] %s", cursor, check, notificationLabel(category))
		fmt.Fprintf(out, "\033[%d;%dH%s\033[K", row, x, truncate(line, width))
	}
	if s.outputErr != "" {
		row := 8 + len(model.NotificationCategories()) + 1
		if row <= height {
			fmt.Fprintf(out, "\033[%d;%dH%s\033[K", row, x, truncate("Not saved: "+sanitizeTerminalText(s.outputErr), width))
		}
	}
}

func (s *tuiState) beginNotificationSettings() {
	session := s.currentSession()
	if session.InstanceID == "" || session.SessionID == "" {
		s.outputErr = "notification settings require a synchronized Ducklion session"
		return
	}
	s.notificationMode = true
	s.notificationIndex = 0
	s.notificationStaged = make(map[model.NotificationCategory]bool)
	for _, category := range model.NotificationCategories() {
		s.notificationStaged[category] = s.activity().Enabled(session.InstanceID, session.SessionID, category)
	}
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
	session := s.currentSession()
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
	s.notificationMode = false
	s.notificationStaged = nil
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

func (s *tuiState) handleInput(b []byte) string {
	text := string(b)
	switch {
	case text == "q" || text == "\x03":
		return "quit"
	case text == "r":
		return "refresh"
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
	case text == "a" && !s.hostScoped:
		return "add-client"
	case text == "d" && !s.hostScoped:
		return "remove-client"
	case text == "\r" || text == "\n":
		return "attach"
	case text == "j" || text == "\x1b[B":
		if s.selected < len(s.sessions)-1 {
			s.selected++
			s.selectedKey = s.currentKey()
			return "select"
		}
	case text == "k" || text == "\x1b[A":
		if s.selected > 0 {
			s.selected--
			s.selectedKey = s.currentKey()
			return "select"
		}
	case strings.HasPrefix(text, "\x1b[<0;"):
		if idx, ok := s.sessionIndexForMouse(text); ok {
			s.selected = idx
			s.selectedKey = s.currentKey()
			return "select"
		}
	case strings.HasPrefix(text, "\x1b[<2;"):
		if idx, ok := s.sessionIndexForMouse(text); ok {
			s.selected = idx
			s.selectedKey = s.currentKey()
		}
		return "attach"
	}
	return ""
}

func (s *tuiState) beginAddClient() {
	hosts, err := ducklord.LoadSSHConfigHosts("")
	if err != nil {
		s.outputErr = err.Error()
		return
	}
	s.addClientMode = true
	s.addClientLine = ""
	s.addClientErr = ""
	s.addClientHosts = hosts
	s.newSessionMode = false
	s.outputErr = ""
}

func (s *tuiState) cancelAddClient() {
	s.addClientMode = false
	s.addClientLine = ""
	s.addClientErr = ""
	s.addClientHosts = nil
}

func (s *tuiState) submitAddClient(ctx context.Context) error {
	client, err := s.clientFromAddLine(strings.TrimSpace(s.addClientLine))
	if err != nil {
		return err
	}
	probe, err := s.runner.ProbeDucklion(ctx, client)
	if err != nil {
		return err
	}
	var installErr error
	installed := false
	if !probe.Available {
		installedPath, err := s.runner.InstallDucklion(ctx, client, "", "")
		if err != nil {
			installErr = err
		} else {
			installed = true
			client.Ducklion = installedPath
			probe, err = s.runner.ProbeDucklion(ctx, client)
			if err != nil {
				return err
			}
		}
	}
	if probe.Available && probe.Command != "" {
		client.Ducklion = probe.Command
	}
	next := s.cfg.Clone()
	if err := next.AddClient(client); err != nil {
		return err
	}
	if err := ducklord.SaveConfig(s.cfgPath, next); err != nil {
		return err
	}
	s.cfg = next
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

func (s *tuiState) removeSelectedClient(ctx context.Context) error {
	clientName := s.selectedClientName()
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
	s.cfg = next
	s.outputText = ""
	s.outputForKey = ""
	s.refreshSessions(ctx)
	s.refreshSelectedOutput(ctx)
	s.outputErr = fmt.Sprintf("removed host entry %s from config", clientName)
	return nil
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
	s.outputErr = ""
}

func (s *tuiState) cancelCreate() {
	s.cancelCreateDiscovery()
	s.newSessionMode = false
	s.newSessionLine = ""
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
}

func (s *tuiState) cancelCreateDiscovery() {
	if s.newSessionCancel != nil {
		s.newSessionCancel()
		s.newSessionCancel = nil
	}
	s.newSessionRequestID++
	s.newSessionDiscovering = false
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
		result.kind, result.client, result.project = event.kind, event.client, event.project
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
		s.newSessionErr = "loading projects..."
		s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "projects", client: clientName}, func(workCtx context.Context) createDiscoveryEvent {
			projects, projectErr := s.runner.Projects(workCtx, client)
			return createDiscoveryEvent{projects: projects, err: projectErr}
		})
		return "", "", nil, false, nil
	case "project":
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
		s.newSessionErr = "checking available runtime..."
		s.beginCreateDiscovery(ctx, done, createDiscoveryEvent{kind: "agents", client: s.newSessionClient, project: project}, func(workCtx context.Context) createDiscoveryEvent {
			agents, agentErr := s.runner.Agents(workCtx, client, project.Path)
			return createDiscoveryEvent{agents: agents, err: agentErr}
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
			projects, projectErr := s.runner.Projects(workCtx, client)
			if projectErr != nil {
				return createDiscoveryEvent{sessionName: name, failureStep: "project", err: fmt.Errorf("project revalidation failed: %w", projectErr)}
			}
			if !containsProject(projects, project) {
				return createDiscoveryEvent{sessionName: name, failureStep: "project", err: fmt.Errorf("selected project is no longer available")}
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
				startArgs, agentErr = buildStartArgsKind(name, model.KindShell, "", project.Path, resolved.Command)
			} else {
				startArgs, agentErr = buildStartArgs(name, resolved.Type, project.Path, resolved.Command)
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

func (s *tuiState) applyCreateDiscovery(event createDiscoveryEvent) (clientName, sessionName string, args []string, ready bool) {
	if !s.newSessionMode || !s.newSessionDiscovering || event.id != s.newSessionRequestID {
		return "", "", nil, false
	}
	generation, instance := s.hostFingerprint(event.client)
	if generation != event.generation || instance != event.instance || !s.hostIsLive(event.client) {
		s.cancelCreateDiscovery()
		s.newSessionStep = "host"
		s.newSessionErr = "host changed while loading; choose it again"
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
			s.newSessionStep = "project"
		case "validate":
			s.newSessionStep = event.failureStep
			if s.newSessionStep == "" {
				s.newSessionStep = "handle"
			}
		}
		return "", "", nil, false
	}
	switch event.kind {
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
			s.newSessionStep = "host"
			s.newSessionErr = "this host has no configured projects"
			return "", "", nil, false
		}
		s.newSessionProjects = append([]ducklord.RemoteProject(nil), projects...)
		s.newSessionStep = "project"
		s.newSessionErr = fmt.Sprintf("host: %s; choose a configured project", event.client)
	case "agents":
		if s.newSessionKind == model.KindShell {
			shell, ok := findRemoteAgent(event.agents, "shell")
			if !ok {
				s.newSessionStep, s.newSessionErr = "project", "remote shell is unavailable"
				return "", "", nil, false
			}
			s.newSessionAgent, s.newSessionCommand = shell.Type, append([]string(nil), shell.Command...)
			s.newSessionStep, s.newSessionErr = "handle", fmt.Sprintf("shell project: %s; empty handle uses %s", event.project.Path, defaultSessionHandle(event.project.Path))
			return "", "", nil, false
		}
		agents := make([]ducklord.RemoteAgent, 0, len(event.agents))
		for _, agent := range event.agents {
			if agent.Type != "shell" {
				agents = append(agents, agent)
			}
		}
		if len(agents) == 0 {
			s.newSessionStep, s.newSessionErr = "project", "this project has no available agent runtime"
			return "", "", nil, false
		}
		s.newSessionAgents = agents
		s.newSessionStep, s.newSessionErr = "agent", fmt.Sprintf("project: %s; choose an available agent", event.project.Path)
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
	case "agent":
		return fmt.Sprintf("new session: host=%s project=%s  choose agent", displayField(s.newSessionClient), displayField(s.newSessionCWD))
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
	case "agent":
		return "agent"
	case "handle":
		return "handle"
	default:
		return "host"
	}
}

func (s *tuiState) handleCreateInput(b []byte) string {
	return s.handleLineInput(b, &s.newSessionLine)
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
	if len(b) == 1 && b[0] >= 0x20 && b[0] != 0x7f {
		*line += string(b)
	}
	return ""
}

func sessionKey(sess ducklord.RemoteSession) string {
	if sess.InstanceID != "" && sess.SessionID != "" {
		return sess.InstanceID + "/" + sess.SessionID
	}
	return sess.Group + "/" + sess.Client + "/" + sess.Name
}

func canAttach(sess ducklord.RemoteSession) bool {
	return canRead(sess) && sess.Status == "running"
}

func canRead(sess ducklord.RemoteSession) bool {
	return sess.Error == "" && sess.Status != "error" && sess.Name != "(offline)"
}

func (s *tuiState) sessionIndexForMouse(seq string) (int, bool) {
	if strings.HasPrefix(seq, "\x1b[<0;") || strings.HasPrefix(seq, "\x1b[<2;") {
		seq = seq[len("\x1b[<0;"):]
	}
	parts := strings.FieldsFunc(seq, func(r rune) bool { return r == ';' || r == 'M' || r == 'm' })
	if len(parts) < 2 {
		return 0, false
	}
	x, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, false
	}
	width, _ := terminalSize()
	layout := calculateTUILayout(width, s.focused, s.listPaneWidth, s.autoHideList)
	if !layout.showList || x > layout.menuWidth+2 {
		return 0, false
	}
	y, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	row := 5
	currentGroup := "\000"
	for i, sess := range s.sessions {
		group := sess.Group
		if group == "" {
			group = "default"
		}
		if group != currentGroup {
			row++
			currentGroup = group
		}
		if y == row {
			return i, true
		}
		row++
	}
	return 0, false
}

func readInput(ctx context.Context, ch chan<- []byte) {
	buf := make([]byte, 64)
	var pending []byte
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		pending = append(pending, buf[:n]...)
		for {
			event, rest, ok := nextInputEvent(pending)
			if !ok {
				pending = rest
				break
			}
			pending = rest
			select {
			case ch <- event:
			case <-ctx.Done():
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
		return append([]byte(nil), pending[:1]...), pending[1:], true
	}
	if len(pending) >= 3 && pending[1] == '[' && (pending[2] == 'A' || pending[2] == 'B') {
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
		case r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f:
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
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
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
