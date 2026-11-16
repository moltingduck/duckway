package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

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
