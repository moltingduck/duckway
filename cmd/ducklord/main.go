package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
	"github.com/hackerduck/duckway/internal/version"
)

type remoteRunner interface {
	Sessions(context.Context, ducklord.Client, int) ([]ducklord.RemoteSession, error)
	Read(context.Context, ducklord.Client, string, int) (string, error)
	Send(context.Context, ducklord.Client, string, string) error
	Start(context.Context, ducklord.Client, []string) error
	Stop(context.Context, ducklord.Client, string) error
	Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error)
	ProbeDucklion(context.Context, ducklord.Client) (ducklord.DucklionProbe, error)
	InstallDucklion(context.Context, ducklord.Client, string, string) (string, error)
	Attach(ducklord.Client, string) error
	AttachStream(context.Context, ducklord.Client, string) (*ducklord.AttachSession, error)
}

type attachOutputEvent struct {
	id   int
	text string
}

type attachDoneEvent struct {
	id  int
	err error
}

type startDoneEvent struct {
	id      int
	client  string
	session string
	err     error
}

func main() {
	if err := run(os.Args[1:], os.Stdout, ducklord.Runner{}); err != nil {
		log.Fatal(err)
	}
}

func run(args []string, out io.Writer, runner remoteRunner) error {
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
			return fmt.Errorf("usage: ducklord start <client> --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		startArgs, err := parseDucklordStartArgs(rest[1:])
		if err != nil {
			return err
		}
		return runner.Start(context.Background(), c, startArgs)
	case "stop":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord stop <client> <session>")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Stop(context.Background(), c, rest[1])
	case "attach":
		cfg, rest, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 2 {
			return fmt.Errorf("usage: ducklord attach <client> <session>")
		}
		c, err := mustClient(cfg, rest[0])
		if err != nil {
			return err
		}
		return runner.Attach(c, rest[1])
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
		return runHostTUI(hostCfg, runner, 2*time.Second)
	case "tui":
		cfgPath, refresh, err := parseTUIFlags(args[1:])
		if err != nil {
			return err
		}
		cfg, err := ducklord.LoadConfig(cfgPath)
		if err != nil {
			return err
		}
		return runTUI(cfg, runner, cfgPath, refresh)
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

func parseTUIFlags(args []string) (string, time.Duration, error) {
	path := ""
	refresh := 2 * time.Second
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return "", 0, fmt.Errorf("--config requires a value")
			}
			path = args[i+1]
			i++
		case "--refresh":
			if i+1 >= len(args) {
				return "", 0, fmt.Errorf("--refresh requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d < time.Second {
				return "", 0, fmt.Errorf("invalid --refresh value")
			}
			refresh = d
			i++
		default:
			return "", 0, fmt.Errorf("unknown tui option: %s", args[i])
		}
	}
	return path, refresh, nil
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
	fmt.Fprintf(out, "%-12s %-18s %-10s %-12s %s\n", "CLIENT", "SESSION", "STATUS", "AGENT", "LAST")
	for _, s := range sessions {
		fmt.Fprintf(out, "%-12s %-18s %-10s %-12s %s\n", displayField(s.Client), displayField(s.Name), displayField(s.Status), displayField(s.AgentType), truncate(sanitizeTerminalText(s.LastLine), 80))
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

func parseDucklordStartArgs(args []string) ([]string, error) {
	name := ""
	agent := ""
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
	return buildStartArgs(name, agent, cwd, command)
}

func buildStartArgs(name, agent, cwd string, command []string) ([]string, error) {
	if !ducklord.SafeIdentifier(name) {
		return nil, fmt.Errorf("invalid session name %q", name)
	}
	out := []string{"--name", name}
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

func parseCreateAgentChoice(input string) (string, []string, error) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "", "1", "shell", "bash", "sh":
		return "shell", []string{"bash"}, nil
	case "2", "codex":
		return "codex", []string{"codex"}, nil
	case "3", "claude", "claude_code", "claude-code":
		return "claude_code", []string{"claude"}, nil
	default:
		return "", nil, fmt.Errorf("unknown agent %q; choose shell, codex, or claude", input)
	}
}

func createSessionName(agent, cwd string) string {
	base := strings.TrimSpace(cwd)
	base = strings.TrimRight(base, "/")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == "" || base == "." || base == "~" {
		base = "home"
	}
	return safeSlug(agent + "-" + base)
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
	cfg                *ducklord.Config
	cfgPath            string
	runner             remoteRunner
	refresh            time.Duration
	sessions           []ducklord.RemoteSession
	selected           int
	hashes             map[string]string
	selectedKey        string
	outputText         string
	outputErr          string
	outputForKey       string
	focused            bool
	newSessionMode     bool
	newSessionClient   string
	newSessionLine     string
	newSessionErr      string
	newSessionStarting bool
	newSessionStep     string
	newSessionAgent    string
	newSessionCommand  []string
	newSessionProjects []ducklord.RemoteProject
	addClientMode      bool
	addClientLine      string
	addClientErr       string
	addClientHosts     []ducklord.SSHHost
	hostScoped         bool
}

func runTUI(cfg *ducklord.Config, runner remoteRunner, cfgPath string, refresh time.Duration) error {
	return runTUIWithOptions(cfg, runner, cfgPath, refresh, false)
}

func runHostTUI(cfg *ducklord.Config, runner remoteRunner, refresh time.Duration) error {
	return runTUIWithOptions(cfg, runner, "", refresh, true)
}

func runTUIWithOptions(cfg *ducklord.Config, runner remoteRunner, cfgPath string, refresh time.Duration, hostScoped bool) error {
	oldState, err := makeRaw()
	if err != nil {
		return err
	}
	defer restore(oldState)
	fmt.Print("\033[?1049h\033[?25l\033[?1000h\033[?1006h")
	defer fmt.Print("\033[?1006l\033[?1000l\033[?25h\033[?1049l")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	state := &tuiState{cfg: cfg, cfgPath: cfgPath, runner: runner, refresh: refresh, hashes: map[string]string{}, hostScoped: hostScoped}
	state.refreshSessions(ctx)
	state.refreshSelectedOutput(ctx)
	state.render(os.Stdout)
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	input := make(chan []byte, 8)
	attachOut := make(chan attachOutputEvent, 32)
	attachDone := make(chan attachDoneEvent, 1)
	startDone := make(chan startDoneEvent, 1)
	var attach *ducklord.AttachSession
	var attachCancel context.CancelFunc
	attachID := 0
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
			return nil
		case <-ticker.C:
			if !state.focused && !state.newSessionMode {
				state.refreshSessions(ctx)
				state.refreshSelectedOutput(ctx)
			}
			state.render(os.Stdout)
		case result := <-startDone:
			if result.id != startID {
				continue
			}
			startCancel = nil
			state.completeNewSessionStart(ctx, result.client, result.session, result.err)
			state.render(os.Stdout)
		case chunk := <-attachOut:
			if chunk.id != attachID {
				continue
			}
			state.outputText = appendOutputText(state.outputText, chunk.text, 120)
			state.render(os.Stdout)
		case result := <-attachDone:
			if result.id != attachID {
				continue
			}
			if result.err != nil && state.focused {
				state.outputErr = result.err.Error()
			}
			state.focused = false
			attach = nil
			if attachCancel != nil {
				attachCancel()
				attachCancel = nil
			}
			state.render(os.Stdout)
		case b := <-input:
			if state.focused {
				if isDetachInput(b) {
					if attach != nil {
						_ = attach.Stdin.Close()
					}
					if attachCancel != nil {
						attachCancel()
						attachCancel = nil
					}
					attachID++
					state.focused = false
					attach = nil
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
				if state.newSessionStarting {
					if string(b) == "\x03" || string(b) == "\x1b" || string(b) == "q" {
						if startCancel != nil {
							startCancel()
						}
						state.newSessionErr = "canceling start..."
						state.render(os.Stdout)
					}
					continue
				}
				action := state.handleCreateInput(b)
				switch action {
				case "cancel":
					state.cancelCreate()
				case "submit":
					sessionName, clientName, args, ready, err := state.submitCreateStep(ctx)
					if err != nil {
						state.newSessionErr = err.Error()
						break
					}
					if !ready {
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
					state.newSessionErr = "starting..."
					go func() {
						err := runner.Start(startCtx, c, args)
						select {
						case startDone <- startDoneEvent{id: id, client: clientName, session: sessionName, err: err}:
						case <-ctx.Done():
						}
					}()
				}
				state.render(os.Stdout)
				continue
			}
			action := state.handleInput(b)
			switch action {
			case "quit":
				return nil
			case "refresh":
				state.refreshSessions(ctx)
				state.refreshSelectedOutput(ctx)
			case "select":
				state.refreshSelectedOutput(ctx)
			case "new":
				state.beginCreate()
			case "add-client":
				state.beginAddClient()
			case "attach":
				if len(state.sessions) == 0 {
					continue
				}
				s := state.sessions[state.selected]
				if !canAttach(s) {
					continue
				}
				c, err := mustClient(cfg, s.Client)
				if err != nil {
					return err
				}
				attachCtx, cancel := context.WithCancel(ctx)
				session, err := runner.AttachStream(attachCtx, c, s.Name)
				if err != nil {
					cancel()
					state.outputErr = err.Error()
					break
				}
				attach = session
				attachCancel = cancel
				attachID++
				id := attachID
				state.focused = true
				go superviseAttach(attachCtx, id, session, attachOut, attachDone)
			}
			state.render(os.Stdout)
		}
	}
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
	for i := range all {
		key := all[i].Client + "/" + all[i].Name
		if all[i].TailHash != "" && s.hashes[key] != "" && s.hashes[key] != all[i].TailHash {
			all[i].Updated = true
			fmt.Print("\a")
		}
		if all[i].TailHash != "" {
			s.hashes[key] = all[i].TailHash
		}
		all[i].LastLine = sanitizeTerminalText(all[i].LastLine)
		all[i].Error = sanitizeTerminalText(all[i].Error)
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

func (s *tuiState) selectSession(clientName, sessionName string) {
	for i, sess := range s.sessions {
		if sess.Client == clientName && sess.Name == sessionName {
			s.selected = i
			s.selectedKey = s.currentKey()
			return
		}
	}
}

func (s *tuiState) completeNewSessionStart(ctx context.Context, clientName, sessionName string, err error) {
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
	s.selectSession(clientName, sessionName)
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
	s.outputForKey = key
	if !canRead(sess) {
		s.outputText = ""
		s.outputErr = sess.Error
		if s.outputErr == "" {
			s.outputErr = sess.Status
		}
		return
	}
	c, err := mustClient(s.cfg, sess.Client)
	if err != nil {
		s.outputText = ""
		s.outputErr = err.Error()
		return
	}
	text, err := s.runner.Read(ctx, c, sess.Name, 80)
	if err != nil {
		s.outputText = ""
		s.outputErr = err.Error()
		return
	}
	s.outputText = sanitizeTerminalText(text)
	s.outputErr = ""
}

func (s *tuiState) render(out io.Writer) {
	width, height := terminalSize()
	renderWidth := width
	if renderWidth < 80 {
		renderWidth = 80
	}
	if height < 12 {
		height = 12
	}
	menuWidth := menuWidthFor(renderWidth)
	contentX := menuWidth + 4
	contentWidth := renderWidth - contentX + 1
	fmt.Fprint(out, "\033[H\033[2J")
	fmt.Fprintln(out, "ducklord remote agents")
	if s.focused {
		fmt.Fprintln(out, "session focus  keys go to session  ctrl-] menu")
	} else if s.addClientMode {
		fmt.Fprintln(out, "add ducklion host: ssh config number/name or user@host  enter add  esc cancel")
	} else if s.newSessionMode {
		fmt.Fprintf(out, "%s  enter next  esc cancel\n", truncate(s.createHeader(), renderWidth))
	} else if s.hostScoped {
		fmt.Fprintln(out, "j/k or arrows move  enter/right-click focus session  r refresh  q quit")
	} else {
		fmt.Fprintln(out, "j/k or arrows move  enter/right-click focus session  a add host  n new  r refresh  q quit")
	}
	fmt.Fprintln(out, strings.Repeat("-", renderWidth))
	if len(s.sessions) == 0 {
		fmt.Fprintln(out, "No sessions.")
		s.renderAddClientChoices(out, 5, 1, menuWidth, height)
		s.renderCreateChoices(out, 5, 1, menuWidth, height)
		s.renderAddClientPrompt(out, 4, 1, renderWidth)
		s.renderCreatePrompt(out, 4, 1, renderWidth)
		return
	}
	fmt.Fprintf(out, "\033[4;1H%-*s | %s\033[K", menuWidth, "sessions", "content")
	currentGroup := "\000"
	row := 5
	for i, sess := range s.sessions {
		if row > height {
			break
		}
		group := displayField(sess.Group)
		if group == "" {
			group = "default"
		}
		if group != currentGroup {
			fmt.Fprintf(out, "\033[%d;1H%-*s |\033[K", row, menuWidth, "["+group+"]")
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
		if sess.Updated {
			mark = "*"
		}
		line := fmt.Sprintf("%s%s %-12s %-18s %-9s %-10s %s", prefix, mark, displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), displayField(sess.AgentType), sess.LastLine)
		if sess.Error != "" {
			line = fmt.Sprintf("%s! %-12s %-18s %-9s %s", prefix, displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), sess.Error)
		}
		fmt.Fprintf(out, "\033[%d;1H%-*s |\033[K", row, menuWidth, truncate(line, menuWidth))
		row++
	}
	s.renderAddClientChoices(out, row, 1, menuWidth, height)
	s.renderCreateChoices(out, row, 1, menuWidth, height)
	s.renderContent(out, contentX, contentWidth, height)
	s.renderAddClientPrompt(out, height, 1, renderWidth)
	s.renderCreatePrompt(out, height, 1, renderWidth)
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
	header := fmt.Sprintf("%s / %s  %s  %s", displayField(sess.Client), displayField(sess.Name), displayField(sess.Status), displayField(sess.AgentType))
	if s.focused {
		header += "  [focus]"
	}
	fmt.Fprintf(out, "\033[5;%dH%s\033[K", x, truncate(header, width))
	startRow := 6
	if s.outputErr != "" {
		fmt.Fprintf(out, "\033[%d;%dH%s\033[K", startRow, x, truncate("error: "+sanitizeTerminalText(s.outputErr), width))
		return
	}
	lines := tailLines(strings.Split(strings.TrimRight(s.outputText, "\n"), "\n"), height-startRow+1)
	for i, line := range lines {
		fmt.Fprintf(out, "\033[%d;%dH%s\033[K", startRow+i, x, truncate(line, width))
	}
}

func (s *tuiState) handleInput(b []byte) string {
	text := string(b)
	switch {
	case text == "q" || text == "\x03":
		return "quit"
	case text == "r":
		return "refresh"
	case text == "n" && !s.hostScoped:
		return "new"
	case text == "a" && !s.hostScoped:
		return "add-client"
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
	if err := s.cfg.AddClient(client); err != nil {
		return err
	}
	if err := ducklord.SaveConfig(s.cfgPath, s.cfg); err != nil {
		return err
	}
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
	s.newSessionStep = "agent"
	s.newSessionAgent = ""
	s.newSessionCommand = nil
	s.newSessionProjects = nil
	s.outputErr = ""
}

func (s *tuiState) cancelCreate() {
	s.newSessionMode = false
	s.newSessionLine = ""
	s.newSessionErr = ""
	s.newSessionStarting = false
	s.newSessionStep = ""
	s.newSessionAgent = ""
	s.newSessionCommand = nil
	s.newSessionProjects = nil
}

func (s *tuiState) submitCreateStep(ctx context.Context) (sessionName, clientName string, args []string, ready bool, err error) {
	line := strings.TrimSpace(s.newSessionLine)
	switch s.newSessionStep {
	case "", "agent":
		agent, command, err := parseCreateAgentChoice(line)
		if err != nil {
			return "", "", nil, false, err
		}
		s.newSessionAgent = agent
		s.newSessionCommand = command
		s.newSessionStep = "host"
		s.newSessionLine = ""
		s.newSessionErr = fmt.Sprintf("agent: %s", agent)
		return "", "", nil, false, nil
	case "host":
		clientName, err := s.resolveCreateClient(line)
		if err != nil {
			return "", "", nil, false, err
		}
		client, err := mustClient(s.cfg, clientName)
		if err != nil {
			return "", "", nil, false, err
		}
		projects, projectErr := s.runner.Projects(ctx, client)
		s.newSessionClient = clientName
		s.newSessionProjects = projects
		s.newSessionStep = "project"
		s.newSessionLine = ""
		if projectErr != nil {
			s.newSessionErr = "project list failed; enter a cwd path manually: " + projectErr.Error()
		} else if len(projects) == 0 {
			s.newSessionErr = "no duckway projects found; enter a cwd path manually"
		} else {
			s.newSessionErr = fmt.Sprintf("host: %s; choose project number/name/path", clientName)
		}
		return "", "", nil, false, nil
	case "project":
		cwd, err := s.resolveCreateProject(line)
		if err != nil {
			return "", "", nil, false, err
		}
		name := createSessionName(s.newSessionAgent, cwd)
		args, err := buildStartArgs(name, s.newSessionAgent, cwd, s.newSessionCommand)
		if err != nil {
			return "", "", nil, false, err
		}
		return name, s.newSessionClient, args, true, nil
	default:
		return "", "", nil, false, fmt.Errorf("unknown create step %q", s.newSessionStep)
	}
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

func (s *tuiState) resolveCreateProject(input string) (string, error) {
	if input == "" {
		if len(s.newSessionProjects) == 1 {
			return s.newSessionProjects[0].Path, nil
		}
		return "", fmt.Errorf("project path, name, or number is required")
	}
	if n, err := strconv.Atoi(input); err == nil {
		if n < 1 || n > len(s.newSessionProjects) {
			return "", fmt.Errorf("project number out of range")
		}
		return s.newSessionProjects[n-1].Path, nil
	}
	for _, project := range s.newSessionProjects {
		if input == project.Name || input == project.Path {
			return project.Path, nil
		}
	}
	if strings.HasPrefix(input, "/") || strings.HasPrefix(input, "~") || strings.HasPrefix(input, ".") {
		return input, nil
	}
	return "", fmt.Errorf("unknown project %q; enter a project number or full cwd path", input)
}

func (s *tuiState) createHeader() string {
	switch s.newSessionStep {
	case "host":
		return fmt.Sprintf("new session: agent=%s  host number/name", displayField(s.newSessionAgent))
	case "project":
		return fmt.Sprintf("new session: agent=%s host=%s  project number/name/path", displayField(s.newSessionAgent), displayField(s.newSessionClient))
	default:
		return "new session: agent  1 shell  2 codex  3 claude"
	}
}

func (s *tuiState) createPromptLabel() string {
	switch s.newSessionStep {
	case "host":
		return "host"
	case "project":
		return "project"
	default:
		return "agent"
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
	if x > menuWidthFor(width)+2 {
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

func menuWidthFor(width int) int {
	if width < 80 {
		width = 80
	}
	if width >= 120 {
		return 44
	}
	return 36
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

func superviseAttach(ctx context.Context, id int, session *ducklord.AttachSession, out chan<- attachOutputEvent, done chan<- attachDoneEvent) {
	stdoutDone := make(chan error, 1)
	go readAttachOutput(ctx, id, session.Stdout, out, stdoutDone)
	var stdoutErr error
	var commandErr error
	stdoutOpen := true
	commandOpen := session.Done != nil
	for stdoutOpen || commandOpen {
		select {
		case err := <-stdoutDone:
			stdoutErr = err
			stdoutOpen = false
		case err := <-session.Done:
			commandErr = err
			commandOpen = false
		case <-ctx.Done():
			return
		}
	}
	if commandErr != nil {
		select {
		case done <- attachDoneEvent{id: id, err: commandErr}:
		case <-ctx.Done():
		}
		return
	}
	select {
	case done <- attachDoneEvent{id: id, err: stdoutErr}:
	case <-ctx.Done():
	}
}

func readAttachOutput(ctx context.Context, id int, r io.Reader, out chan<- attachOutputEvent, done chan<- error) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			select {
			case out <- attachOutputEvent{id: id, text: string(buf[:n])}:
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
	if chunk == "" {
		return current
	}
	text := applyInteractiveText(current, chunk)
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
  ducklord clients [--config <path>]
  ducklord ssh-hosts [--config-file <ssh_config>]
  ducklord import-ssh-hosts [--ssh-config <ssh_config>] [--config <path>]
  ducklord sessions <client> [--config <path>]
  ducklord projects <client> [--config <path>]
  ducklord probe <client> [--config <path>]
  ducklord install-ducklion <client> [--source <path>] [--dest <remote-path>] [--config <path>]
  ducklord tui [--config <path>] [--refresh 2s]
  ducklord attach-host <client> [--config <path>]
  ducklord attach <client> <session> [--config <path>]
  ducklord read <client> <session> [--lines N] [--config <path>]
  ducklord send <client> <session> <text> [--config <path>]
  ducklord start <client> --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
  ducklord stop <client> <session> [--config <path>]
  ducklord version`)
}
