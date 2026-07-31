package main

import (
	"context"
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
	"golang.org/x/sys/unix"
)

type remoteRunner interface {
	Sessions(context.Context, ducklord.Client, int) ([]ducklord.RemoteSession, error)
	Read(context.Context, ducklord.Client, string, int) (string, error)
	Send(context.Context, ducklord.Client, string, string) error
	Start(context.Context, ducklord.Client, []string) error
	Stop(context.Context, ducklord.Client, string) error
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
	case "clients":
		cfg, _, err := loadWithFlags(args[1:])
		if err != nil {
			return err
		}
		return printClients(out, cfg)
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
	case "tui":
		cfgPath, refresh, err := parseTUIFlags(args[1:])
		if err != nil {
			return err
		}
		cfg, err := ducklord.LoadConfig(cfgPath)
		if err != nil {
			return err
		}
		return runTUI(cfg, runner, refresh)
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

func printClients(out io.Writer, cfg *ducklord.Config) error {
	fmt.Fprintf(out, "%-18s %-12s %-24s %s\n", "NAME", "GROUP", "TARGET", "DUCKLION")
	for _, c := range cfg.Clients {
		fmt.Fprintf(out, "%-18s %-12s %-24s %s\n", c.Name, c.Group, c.Target(), c.Ducklion)
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
}

func runTUI(cfg *ducklord.Config, runner remoteRunner, refresh time.Duration) error {
	oldState, err := makeRaw()
	if err != nil {
		return err
	}
	defer restore(oldState)
	fmt.Print("\033[?1049h\033[?25l\033[?1000h\033[?1006h")
	defer fmt.Print("\033[?1006l\033[?1000l\033[?25h\033[?1049l")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	state := &tuiState{cfg: cfg, runner: runner, refresh: refresh, hashes: map[string]string{}}
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
					state.newSessionMode = false
					state.newSessionLine = ""
					state.newSessionErr = ""
				case "submit":
					sessionName, args, err := parseCreateLine(state.newSessionLine)
					if err != nil {
						state.newSessionErr = err.Error()
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
					clientName := state.newSessionClient
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
	} else if s.newSessionMode {
		fmt.Fprintf(out, "new session on %s  <name> [--agent shell] [--cwd DIR] [-- CMD...]  enter create  esc cancel\n", displayField(s.newSessionClient))
	} else {
		fmt.Fprintln(out, "j/k or arrows move  enter/right-click focus session  n new  r refresh  q quit")
	}
	fmt.Fprintln(out, strings.Repeat("-", renderWidth))
	if len(s.sessions) == 0 {
		fmt.Fprintln(out, "No sessions.")
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
	s.renderContent(out, contentX, contentWidth, height)
	s.renderCreatePrompt(out, height, 1, renderWidth)
}

func (s *tuiState) renderCreatePrompt(out io.Writer, row, x, width int) {
	if !s.newSessionMode {
		return
	}
	prompt := fmt.Sprintf("new> %s", s.newSessionLine)
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
	case text == "n":
		return "new"
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
	s.outputErr = ""
}

func (s *tuiState) handleCreateInput(b []byte) string {
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
		if s.newSessionLine != "" {
			runes := []rune(s.newSessionLine)
			s.newSessionLine = string(runes[:len(runes)-1])
		}
		return ""
	}
	if len(b) == 1 && b[0] >= 0x20 && b[0] != 0x7f {
		s.newSessionLine += string(b)
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

func makeRaw() (*unix.Termios, error) {
	fd := int(os.Stdin.Fd())
	oldState, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	newState := *oldState
	newState.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG
	newState.Iflag &^= unix.ICRNL | unix.IXON
	newState.Cc[unix.VMIN] = 1
	newState.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &newState); err != nil {
		return nil, err
	}
	return oldState, nil
}

func restore(state *unix.Termios) {
	if state != nil {
		_ = unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TCSETS, state)
	}
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

func terminalSize() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		return 120, 32
	}
	return int(ws.Col), int(ws.Row)
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
  ducklord sessions <client> [--config <path>]
  ducklord tui [--config <path>] [--refresh 2s]
  ducklord attach <client> <session> [--config <path>]
  ducklord read <client> <session> [--lines N] [--config <path>]
  ducklord send <client> <session> <text> [--config <path>]
  ducklord start <client> --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
  ducklord stop <client> <session> [--config <path>]
  ducklord version`)
}
